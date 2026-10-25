package main

// `kintsugi schedule` — scheduled tasks from a cron expression (I-56).
//
//	A schedule stores a cron expression and a task, and fires the task when the
//	expression comes due. `run` is the foreground driver that watches for due
//	jobs; `trigger` fires one immediately, which is how an operator tests a job
//	without waiting a day for it.
//
// # One registry, not two
//
// A firing is a background run in internal/hub — the registry
// task(background=true), agent_output and the TUI roster already use. The
// cadence logic lives in internal/loop; this file is the CLI surface and the
// runner wiring, nothing else. There is deliberately no second scheduler
// abstraction: `kintsugi schedule run` competes with nothing, because the hub is
// where a run's lifecycle lives.
//
// # Injection safety, and where it is enforced
//
// A scheduled prompt is data, never authority. Three gates, in order:
//
//  1. Write time. `kintsugi schedule add` refuses a payload that tries to grant a
//     permission, widen the declared scope, or write authorization/scope
//     context. The refusal is logged with a class and a rule label only; the
//     matched span never reaches the log, so it cannot be replayed.
//  2. Fire time. The same check runs again before each firing, because the
//     jobs file is on disk and anything on disk can be edited — including by an
//     agent holding a write tool. A payload that grew an authority grab since
//     it was stored never reaches a model.
//  3. Turn time. The runner checks once more and refuses, so no caller of the
//     runner can skip the property, and every scheduled turn carries
//     loop.InjectionNote telling the model the same rule.
//
// What a scheduled payload cannot do, by construction: it cannot change the
// permission matrix, the [scope] block, the sandbox posture, the attestation,
// or a hook. None of those are read from the task text anywhere in the
// harness; they are read from configuration the operator wrote. The task text
// becomes a user turn and nothing else.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"kintsugi/internal/hub"
	"kintsugi/internal/loop"
	"kintsugi/internal/notify"
)

func cmdSchedule(ws string, args []string) {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, scheduleUsage())
		os.Exit(2)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		scheduleList(ws, rest)
	case "add":
		scheduleAdd(ws, rest)
	case "remove", "rm":
		scheduleRemove(ws, rest)
	case "pause":
		schedulePause(ws, rest, true)
	case "resume":
		schedulePause(ws, rest, false)
	case "trigger":
		scheduleTrigger(ws, rest)
	case "run":
		scheduleRun(ws, rest)
	case "help", "-h", "--help":
		fmt.Print(scheduleUsage())
	default:
		fmt.Fprintf(os.Stderr, "unknown schedule subcommand %q\n\n%s", sub, scheduleUsage())
		os.Exit(2)
	}
}

// scheduleList prints the workspace's jobs with their state and next firing.
func scheduleList(ws string, args []string) {
	fs := flag.NewFlagSet("schedule list", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, scheduleUsage()) }
	fs.Parse(args)

	store, err := loop.OpenStore(ws)
	if err != nil {
		fail("%v", err)
	}
	jobs := store.Jobs()
	if len(jobs) == 0 {
		fmt.Printf("(no scheduled jobs in %s)\n", store.Path())
		return
	}
	fmt.Println(loop.FormatJobList(jobs, time.Now()))
	fmt.Printf("\nfile %s\n", store.Path())
}

// scheduleAdd validates and stores a job.
func scheduleAdd(ws string, args []string) {
	fs := flag.NewFlagSet("schedule add", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, scheduleUsage()) }
	fs.Parse(args)

	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Fprintf(os.Stderr, "usage: kintsugi schedule add \"<cron>\" \"<task>\"\n\n%s", scheduleUsage())
		os.Exit(2)
	}
	expr := rest[0]
	task := strings.TrimSpace(strings.Join(rest[1:], " "))

	store, err := loop.OpenStore(ws)
	if err != nil {
		fail("%v", err)
	}
	// Add runs both the cron parser and the injection guard. A refusal is a
	// loop.Refusal, whose Error() states the rule and never the payload text.
	job, err := store.Add(expr, task)
	if err != nil {
		// The refusal class and rule label go to the session log; the matched
		// span does not, by construction (Refusal has no such field).
		if ref, ok := err.(loop.Refusal); ok {
			recordScheduleRefusal(job.ID, ref)
		}
		fail("%v", err)
	}
	fmt.Printf("added %s · %s · next %s\n", job.ID, job.Cron, humanRelative(job.NextAt))
	fmt.Printf("  %s\n", clipLine(job.Task, 100))
	fmt.Printf("file %s\n", store.Path())
	fmt.Println("· run `kintsugi schedule run` to fire it, or `kintsugi schedule trigger " + job.ID + "` to test it now")
}

// scheduleRemove deletes a job.
func scheduleRemove(ws string, args []string) {
	if len(args) == 0 {
		fail("usage: kintsugi schedule remove <id>")
	}
	store, err := loop.OpenStore(ws)
	if err != nil {
		fail("%v", err)
	}
	job, ok := store.Get(args[0])
	if !ok {
		fail("unknown job %q (run `kintsugi schedule list`)", args[0])
	}
	if err := store.Remove(args[0]); err != nil {
		fail("%v", err)
	}
	fmt.Printf("removed %s · %s\n", job.ID, job.Cron)
}

// schedulePause pauses or resumes a job.
func schedulePause(ws string, args []string, paused bool) {
	if len(args) == 0 {
		verb := "pause"
		if !paused {
			verb = "resume"
		}
		fail("usage: kintsugi schedule %s <id>", verb)
	}
	store, err := loop.OpenStore(ws)
	if err != nil {
		fail("%v", err)
	}
	job, err := store.SetPaused(args[0], paused)
	if err != nil {
		fail("%v", err)
	}
	state := "active"
	if job.Paused {
		state = "paused"
	}
	fmt.Printf("%s · %s · %s\n", job.ID, job.Cron, state)
	if !job.NextAt.IsZero() {
		fmt.Printf("  next %s\n", humanRelative(job.NextAt))
	}
}

// scheduleTrigger fires one job immediately, without touching its schedule.
// It is the operator's test path: the same runner, the same guards, the same
// hub, one firing.
func scheduleTrigger(ws string, args []string) {
	fs := flag.NewFlagSet("schedule trigger", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, scheduleUsage()) }
	model := fs.String("model", "", "provider/model")
	role := fs.String("role", "", "model role: default|coder|researcher|opsec")
	fs.Parse(args)
	if len(fs.Args()) == 0 {
		fail("usage: kintsugi schedule trigger <id>")
	}

	store, err := loop.OpenStore(ws)
	if err != nil {
		fail("%v", err)
	}
	job, ok := store.Get(fs.Args()[0])
	if !ok {
		fail("unknown job %q (run `kintsugi schedule list`)", fs.Args()[0])
	}

	runner := newTaskRunner(ws, runnerOptions{Model: *model, Role: *role, Purpose: "schedule"})
	defer runner.Close()

	// Injection safety, gate 2, ahead of the fire: a job that was stored before
	// it could be checked (a hand-edited file) never reaches a model here.
	if ref, refused := loop.GuardPayload(job.Task); refused {
		runner.record("schedule_refused", map[string]any{
			"job": job.ID, "class": ref.Class, "rule": ref.Rule, "payload": loop.DescribePayload(job.Task),
		})
		fail("job %s: %v", job.ID, ref)
	}

	sched := &loop.Scheduler{
		Store:  store,
		Runner: runner.Run,
		Hub:    hub.NewHub(),
		Role:   *role,
		Model:  runner.modelRef(),
		Log:    runner.record,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("· trigger %s · %s\n", job.ID, job.Cron)
	// Force one firing: Due() is time-based, and a trigger is explicit
	// operator intent, so the job's window is taken as now.
	job.NextAt = time.Now().Add(-time.Second)
	if err := storeForceSetNext(store, job); err != nil {
		fail("%v", err)
	}
	firings := sched.Tick(ctx)
	if len(firings) == 0 {
		fail("job %s did not fire · check `kintsugi schedule list`", job.ID)
	}
	f := firings[0]
	if f.Refused() {
		fail("job %s: %v", job.ID, *f.Refusal)
	}
	if f.Err != nil {
		fail("job %s: %v", job.ID, f.Err)
	}
	runner.raise(notify.KindDone, "schedule "+job.ID+" done")
	fmt.Printf("· run %s · %dtok\n\n%s\n", f.RunID, f.Tokens, strings.TrimSpace(f.Output))
}

// storeForceSetNext writes one job's NextAt so it is due immediately. It is the
// trigger's one write; nothing else in the command sets a window by hand.
func storeForceSetNext(store *loop.Store, job loop.Job) error {
	return store.SetNextAt(job.ID, job.NextAt)
}

// scheduleRun is the foreground scheduler: it fires due jobs and waits for the
// next window. It runs until interrupted.
func scheduleRun(ws string, args []string) {
	fs := flag.NewFlagSet("schedule run", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, scheduleUsage()) }
	model := fs.String("model", "", "provider/model")
	role := fs.String("role", "", "model role: default|coder|researcher|opsec")
	every := fs.Duration("every", loop.DefaultTick, "how often to look for due jobs")
	fs.Parse(args)

	store, err := loop.OpenStore(ws)
	if err != nil {
		fail("%v", err)
	}
	runner := newTaskRunner(ws, runnerOptions{Model: *model, Role: *role, Purpose: "schedule"})
	defer runner.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sched := &loop.Scheduler{
		Store:  store,
		Runner: runner.Run,
		Hub:    hub.NewHub(),
		Role:   *role,
		Model:  runner.modelRef(),
		Now:    time.Now,
		Every:  *every,
		Log:    runner.record,
		OnFiring: func(f loop.Firing) {
			switch {
			case f.Refused():
				// Class and rule only: the payload's own words are not logged.
				fmt.Printf("· %s refused (%s: %s)\n", f.Job.ID, f.Refusal.Class, f.Refusal.Rule)
			case f.Err != nil:
				fmt.Printf("· %s ✗ %v\n", f.Job.ID, f.Err)
				runner.raise(notify.KindQuota, "schedule "+f.Job.ID+" failed")
			default:
				fmt.Printf("· %s fired · run %s · %dtok\n", f.Job.ID, f.RunID, f.Tokens)
				runner.raise(notify.KindDone, "schedule "+f.Job.ID+" done")
			}
		},
	}

	fmt.Printf("· schedule run · %d job(s) · ticking every %s · %s\n",
		len(store.Jobs()), humanDuration(*every), runner.modelRef())
	fmt.Println("· ctrl-c stops the scheduler; jobs stay in " + store.Path())
	if err := sched.Run(ctx); err != nil && ctx.Err() == nil {
		fail("schedule run: %v", err)
	}
	fmt.Println("\n· schedule run stopped")
}

// recordScheduleRefusal reports a write-time refusal. It is a metadata line —
// class, rule label, and the job id — and there is no matched-text field to
// write: loop.Refusal does not carry one.
func recordScheduleRefusal(jobID string, ref loop.Refusal) {
	fmt.Fprintf(os.Stderr, "warn: schedule_refused · %s · class %s · rule %s\n", jobID, ref.Class, ref.Rule)
}

// humanRelative renders a moment as a distance from now.
func humanRelative(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Until(t)
	if d < 0 {
		return humanDuration(-d) + " ago"
	}
	return "in " + humanDuration(d)
}

// scheduleUsage is the command's help, as a string so a test can assert it.
func scheduleUsage() string {
	return `kintsugi schedule — scheduled tasks from a cron expression

  kintsugi schedule list                  jobs, state, next and last firing
  kintsugi schedule add "<cron>" "<task>" store a job (5-field cron, or @daily …)
  kintsugi schedule trigger <id>          fire a job now, without touching its cron
  kintsugi schedule pause <id>            stop it firing; keeps the job and its window
  kintsugi schedule resume <id>           start it firing again
  kintsugi schedule remove <id>           delete the job
  kintsugi schedule run                   foreground scheduler: fire due jobs until ctrl-c

  cron fields: minute hour day-of-month month day-of-week
               (*, n, a-b, */n, a-b/n, comma lists; MON/DEC names; @hourly
               @daily @midnight @weekly @monthly)
  flags: --model provider/model · --role default|coder|researcher|opsec
         --every 30s (schedule run: how often to look for due jobs)

A firing is a background run in the session hub, so it shows in the TUI roster
and can be killed there. Delivery is local: the answer is printed and, when a
notification backend is configured, one terminal notification is raised.

The task text is data, never authority. A payload that tries to grant a
permission, widen the declared scope, or write authorization/scope context is
refused when it is added, again when it fires, and again inside the runner —
and the refusal is logged as a class and a rule, never as the text that tried.
`
}
