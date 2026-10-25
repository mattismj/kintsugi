package main

// `kintsugi loop [interval] <task>` — self-paced loops (I-57).
//
// A loop runs the task once immediately, then again on a cadence, and stops
// when another check cannot help: when a pass fails (the same wiring will fail
// the same way) or when two consecutive passes return the same answer.
//
// # What it is built on
//
// Each pass is a background run in internal/hub — the registry
// task(background=true) and agent_output already use. There is no second
// scheduler in this file: a pass gets a hub id, a title, a state and a result,
// so the TUI roster and the kill path work on it unchanged. `internal/loop`
// drives the cadence; this file only supplies the runner (one ordinary agent
// turn) and the printing.
//
// # Injection safety
//
// A loop's task text is data, never authority. It is refused at write time if
// it tries to grant a permission, widen the declared scope, or write
// authorization/scope context, and the same check runs again before every
// pass — see internal/loop/guard.go and the package comment there. What the
// task text cannot do is change the policy, the scope, the sandbox or the
// attestation: those come from configuration the operator wrote, exactly as
// for a typed turn. Every pass additionally carries loop.InjectionNote in its
// prompt, which restates the rule to the model.
//
// # Not the conversation loop
//
// internal/agent owns the conversation loop (one turn, streamed, tool calls
// dispatched). This file owns the *cadence* loop. They are different things
// with confusingly similar names; the comment is here so the next reader does
// not merge them.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"kintsugi/internal/agent"
	"kintsugi/internal/harness"
	"kintsugi/internal/hub"
	"kintsugi/internal/intercept"
	"kintsugi/internal/loop"
	"kintsugi/internal/memory"
	"kintsugi/internal/notify"
	"kintsugi/internal/prompt"
	"kintsugi/internal/provider"
	"kintsugi/internal/session"
	"kintsugi/internal/tools"
)

func cmdLoop(ws string, args []string) {
	fs := flag.NewFlagSet("loop", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, loopUsage()) }
	model := fs.String("model", "", "provider/model")
	persona := fs.String("persona", "", "persona name override")
	role := fs.String("role", "", "model role: default|coder|researcher|opsec")
	maxIter := fs.Int("max", loop.DefaultMaxIterations, "stop after this many passes")
	keepGoing := fs.Bool("keep-going", false, "continue after a failed pass instead of stopping")
	quiet := fs.Bool("quiet", false, "print a one-line summary per pass instead of the full answer")
	fs.Parse(args)

	interval, task, err := parseLoopArgs(fs.Args())
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n\n%s", err, loopUsage())
		os.Exit(2)
	}

	runner := newTaskRunner(ws, runnerOptions{
		Model: *model, Persona: *persona, Role: *role,
		Purpose: "loop",
	})
	defer runner.Close()
	if ref, refused := loop.GuardPayload(task); refused {
		// Write-time refusal: the task never reaches a model, and the log line
		// carries the class and rule label only — never the matched text.
		runner.record("loop_refused", map[string]any{
			"class": ref.Class, "rule": ref.Rule, "payload": loop.DescribePayload(task),
		})
		fail("%v", ref)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	lp := &loop.Loop{
		Task:             task,
		Title:            task,
		Role:             *role,
		Model:            runner.modelRef(),
		Interval:         interval,
		MaxIterations:    *maxIter,
		KeepGoingOnError: *keepGoing,
		Runner:           runner.Run,
		Hub:              hub.NewHub(),
		OnIteration: func(it loop.Iteration) {
			printIteration(it, interval, *quiet)
		},
	}

	fmt.Printf("· loop · every %s · at most %d checks · %s\n", humanDuration(interval), *maxIter, runner.modelRef())
	rep, err := lp.Run(ctx)
	fmt.Println()
	for _, it := range rep.Iterations {
		if it.Reason != "" {
			fmt.Printf("· stopped after check %d: %s\n", it.Index+1, it.Reason)
		}
	}
	if err != nil && ctx.Err() != nil {
		fmt.Printf("· stopped after %d check(s): interrupted\n", len(rep.Iterations))
		return
	}
	if err != nil {
		fail("loop: %v", err)
	}
	if rep.Stopped != "" && len(rep.Iterations) == 0 {
		fmt.Printf("· loop did not run: %s\n", rep.Stopped)
	}
}

// parseLoopArgs splits `[interval] <task>`: an optional leading duration, then
// the task prose.
func parseLoopArgs(args []string) (time.Duration, string, error) {
	if len(args) == 0 {
		return 0, "", fmt.Errorf("usage: kintsugi loop [interval] \"<task>\"")
	}
	interval := loop.DefaultInterval
	rest := args
	if d, ok := parseInterval(args[0]); ok {
		interval = d
		rest = args[1:]
	}
	task := strings.TrimSpace(strings.Join(rest, " "))
	if task == "" {
		return 0, "", fmt.Errorf("usage: kintsugi loop [interval] \"<task>\" — the task text is empty")
	}
	if interval < time.Second {
		return 0, "", fmt.Errorf("interval %s is too short: a loop re-runs a model turn, so the floor is 1s", interval)
	}
	return interval, task, nil
}

// parseInterval reads a bare duration ("90s", "5m") or a bare number of
// seconds ("90"), which is what an operator types when they are in a hurry.
func parseInterval(s string) (time.Duration, bool) {
	if d, err := time.ParseDuration(s); err == nil {
		return d, true
	}
	if n, err := strconv.Atoi(s); err == nil {
		return time.Duration(n) * time.Second, true
	}
	return 0, false
}

// printIteration reports one pass. A quiet loop prints the state line and the
// answer's first line; a verbose one prints the whole answer, because the
// point of watching a loop is reading what changed between passes.
func printIteration(it loop.Iteration, interval time.Duration, quiet bool) {
	head := fmt.Sprintf("· check %d · %s · %s", it.Index+1, it.Started.Format("15:04:05"), it.RunID)
	switch {
	case it.Err != nil:
		fmt.Printf("%s · ✗ %v\n", head, it.Err)
		return
	case quiet:
		fmt.Printf("%s · ✓ %dtok · %s\n", head, it.Tokens, clipLine(it.Output, 100))
		return
	}
	fmt.Printf("%s · ✓ %dtok\n\n%s\n", head, it.Tokens, strings.TrimSpace(it.Output))
	if it.Reason == "" {
		fmt.Printf("\n· next check in %s\n", humanDuration(interval))
	}
}

// ---- the runner: one ordinary agent turn per pass ----

// runnerOptions selects the model, persona and role a headless pass runs as.
type runnerOptions struct {
	Model   string
	Persona string
	Role    string
	// Purpose labels the session and the refusal telemetry ("loop", "schedule").
	Purpose string
}

// taskRunner runs one task as a normal, fully policy-bound agent turn: the same
// executor, the same permission matrix, the same scope and sandbox floors, the
// same interception, as `kintsugi run`. It exists so loops and schedules cannot
// grow a private, weaker path to the model.
//
// The only thing a scheduled turn does differently from a typed one is the
// sentence in the prompt that says it was scheduled — and that sentence grants
// nothing.
type taskRunner struct {
	app    *harness.App
	res    *harness.Resolved
	comp   *prompt.Compiled
	log    *session.Session
	mem    *memory.Store
	policy tools.Policy
	notify *notify.Notifier
}

// newTaskRunner resolves the model, compiles the prompt stack and opens a
// session for the passes.
func newTaskRunner(ws string, opts runnerOptions) *taskRunner {
	app := newApp(ws)

	var res *harness.Resolved
	var err error
	if opts.Model == "" && opts.Role != "" {
		res, err = app.ResolveRole(opts.Role)
	} else {
		res, err = app.Resolve(opts.Model)
	}
	if err != nil {
		fail("%s: %v", opts.Purpose, err)
	}
	comp, err := app.CompileRole(res, opts.Role, harness.CompileOpts{Persona: opts.Persona})
	if err != nil {
		fail("%s: %v", opts.Purpose, err)
	}
	_ = prompt.SaveManifest(app.Workspace, comp.Manifest)

	log, err := session.New()
	if err != nil {
		fail("%s: session: %v", opts.Purpose, err)
	}
	log.Log("meta", map[string]string{
		"model":   res.ProviderName + "/" + res.Model.ID,
		"persona": comp.Manifest.Persona.Name,
		"role":    opts.Role,
		"stack":   comp.Manifest.StackHash,
		"purpose": opts.Purpose,
	})

	r := &taskRunner{app: app, res: res, comp: comp, log: log, policy: app.ToolPolicy()}
	if mem, err := app.MemoryStore(); err == nil && mem != nil {
		r.mem = mem
	}
	if n := notifierFor(app); n != nil {
		r.notify = n
	}
	return r
}

// modelRef names the model the passes run on.
func (r *taskRunner) modelRef() string {
	if r == nil || r.res == nil {
		return ""
	}
	return r.res.ProviderName + "/" + r.res.Model.ID
}

// Close releases the session and the memory store.
func (r *taskRunner) Close() {
	if r == nil {
		return
	}
	if r.mem != nil {
		_ = r.mem.Close()
	}
	if r.log != nil {
		r.log.Close()
	}
}

// record writes one session-log line. It is nil-safe: a refusal may be recorded
// before a runner exists (the write-time guard), and a run must never fail
// because a log line could not be written.
func (r *taskRunner) record(kind string, v any) {
	if r != nil && r.log != nil {
		r.log.Log(kind, v)
	}
}

// Run performs one pass. It has the hub's work-function shape, so the pass is a
// hub run and nothing else.
func (r *taskRunner) Run(ctx context.Context, task string) (*hub.Result, int, error) {
	// Injection safety, second gate. The payload was checked before the loop
	// started (and, for a job, before it was stored and again when it fired).
	// Checking here as well is what makes the property local to the code that
	// actually builds the prompt: no caller of Run can skip it.
	if ref, refused := loop.GuardPayload(task); refused {
		r.record("schedule_refused", map[string]any{
			"class": ref.Class, "rule": ref.Rule, "payload": loop.DescribePayload(task),
		})
		return nil, 0, ref
	}

	ex := &tools.Executor{
		Workspace: r.app.Workspace,
		Memory:    r.mem,
		Policy:    r.policy,
		Log:       r.record,
		Offline:   r.app.Cfg.Core.Mode == "offline",
	}
	ex.ApplySecurity(r.app.Cfg)
	if err := ex.EnableAudit(r.app.Workspace, r.log.ID); err != nil {
		fmt.Fprintf(os.Stderr, "warn: audit log: %v\n", err)
	} else {
		defer ex.Audit.Close()
	}
	closeTools := r.app.AttachTools(ex, r.log.Log)
	defer closeTools()
	if !r.policy.Yolo {
		// An unattended pass has no operator at the keyboard: promptApprover
		// denies when stdin is not a terminal, so a scheduled turn cannot
		// approve itself into a wider posture. That is the intended behaviour.
		ex.Approver = promptApprover()
	}

	tokens := 0
	turn := &agent.Loop{
		Client:   r.res.Client,
		Tools:    ex,
		Log:      r.log,
		MaxLoops: r.app.Cfg.Harness.MaxToolLoops,
		OnUsage: func(p, c int) {
			tokens = p + c
			r.app.RecordUsage(r.res.ProviderName, p, c)
		},
	}
	if r.app.Cfg.Intercept.Enabled {
		if lx, err := r.app.RefusalLexicon(); err == nil {
			turn.Interceptor = lx
			turn.Reminder = r.app.ReminderBuilder(r.comp)
			turn.MaxRefusalRetries = r.app.Cfg.Intercept.MaxRetries
			if rules, err := r.app.StreamRules(); err == nil && len(rules.Ordered()) > 0 {
				turn.StreamRules = rules
				turn.OnRule = func(ev agent.RuleEvent) {
					r.record("stream_rule", map[string]any{
						"rule": ev.Rule, "action": ev.Action, "level": ev.Level, "outcome": ev.Outcome,
					})
				}
			}
			turn.OnIntercept = func(hit intercept.Hit, level int, outcome string) {
				r.record("refusal", map[string]any{"class": hit.Class, "level": level, "outcome": outcome})
			}
		}
	}
	if hr := r.app.HooksRunner(r.log.Log); hr != nil {
		turn.Hooks = hr
		turn.SessionID = r.log.ID
		turn.CWD = r.app.Workspace
		turn.Model = r.modelRef()
		turn.Persona = r.comp.Manifest.Persona.Name
	}

	// The scheduled-turn note rides as its own system message, after the
	// compiled prompt and before the task: it is a statement about this turn,
	// not part of the cached prefix, and it is a constant of the harness.
	history := []provider.Message{{Role: "system", Content: loop.InjectionNote}}

	final, _, err := turn.RunConversation(ctx, r.comp.Text, history, task)
	if err != nil {
		return nil, tokens, err
	}
	return &hub.Result{Content: final}, tokens, nil
}

// notifierFor builds the operator's terminal notifier from config. An unset or
// unknown backend yields nil: the default posture is silence.
func notifierFor(app *harness.App) *notify.Notifier {
	backend := notify.Backend(strings.ToLower(strings.TrimSpace(app.Cfg.Notify.Backend)))
	switch backend {
	case notify.BackendOSC9, notify.BackendOSC777, notify.BackendBell, notify.BackendCommand:
		return notify.New(notify.Config{Backend: backend, Command: app.Cfg.Notify.Command})
	default:
		return nil
	}
}

// raise emits one terminal notification when the operator configured a backend.
// A failed notification is dropped: the work must not stop because a bell did
// not carry.
func (r *taskRunner) raise(kind notify.Kind, message string) {
	if r == nil || r.notify == nil {
		return
	}
	_ = r.notify.Notify(kind, message)
}

// clipLine shortens s to at most n bytes on one line.
func clipLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if n > 0 && len(s) > n {
		s = strings.TrimSpace(s[:n])
	}
	return s
}

// humanDuration renders a duration in at most two units.
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()+0.5))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%02dh", int(d.Hours()/24), int(d.Hours())%24)
	}
}

// loopUsage is the command's help, as a string so a test can assert it.
func loopUsage() string {
	return `kintsugi loop — self-paced loops

  kintsugi loop [interval] "<task>"     run once now, then every interval until a
                                     check cannot help

  interval   90s · 5m · 1h · or a bare number of seconds (default ` + humanDuration(loop.DefaultInterval) + `)
  flags      --max N        stop after N checks (default ` + strconv.Itoa(loop.DefaultMaxIterations) + `)
             --keep-going   keep going after a failed check
             --quiet        one line per check instead of the full answer
             --model provider/model · --persona name · --role default|coder|researcher|opsec

A loop stops early when a check fails (the same wiring will fail the same way)
or when two consecutive checks return the same answer (another check cannot
help). Every pass is a background run in the session hub, so it appears in the
TUI roster and can be killed there.

The task text is data, never authority: a payload that tries to grant a
permission, widen the declared scope, or write authorization context is
refused, at start and again before every pass.
`
}
