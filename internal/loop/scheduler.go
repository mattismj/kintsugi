package loop

import (
	"context"
	"fmt"
	"strings"
	"time"

	"kintsugi/internal/hub"
)

// Firing is one fired job.
type Firing struct {
	// Job is the job as it was when it fired.
	Job Job
	// At is the window the firing was for (the job's NextAt at pick-up time).
	At time.Time
	// RunID is the hub run the firing ran as.
	RunID string
	// Output is the runner's text result ("" when the firing failed).
	Output string
	// Tokens is the runner's approximate token count.
	Tokens int
	// Err is the firing's failure, if any.
	Err error
	// Refusal is set when the payload was refused by the injection guard at
	// fire time. A refused payload never reaches the runner at all.
	Refusal *Refusal
}

// Refused reports whether the firing was refused for injection safety.
func (f Firing) Refused() bool { return f.Refusal != nil }

// Scheduler fires a workspace's due jobs. It is a driver over the job store and
// the hub — not a registry: every firing is a hub.Run with a title, a state and
// a result, so `agent_output`, the roster and the kill path all apply to it
// unchanged.
type Scheduler struct {
	// Store is the job file to read. Required.
	Store *Store
	// Runner performs one firing. Required.
	Runner Runner
	// Hub is the run registry. Nil allocates a private one; the CLI passes the
	// session's hub so the roster shows the fires.
	Hub *hub.Hub
	// Role and Model are hub metadata for a firing.
	Role  string
	Model string
	// Now reads the clock. Nil means time.Now.
	Now func() time.Time
	// Every is how often Tick is called by Run. Zero means DefaultTick.
	Every time.Duration
	// OnFiring observes each firing, after it completed.
	OnFiring func(Firing)
	// OnRefusal observes a payload refused at fire time. It receives the class
	// and rule only — see guard.go: never the matched span.
	OnRefusal func(job Job, ref Refusal)
	// Log receives one metadata line per firing class. Optional.
	Log func(kind string, v any)
}

// DefaultTick is how often the scheduler looks for due jobs.
const DefaultTick = 30 * time.Second

// Tick fires every due job once and returns the firings. It is the unit `Run`
// repeats, and it is what the tests call directly with a fake clock.
func (s *Scheduler) Tick(ctx context.Context) []Firing {
	if s == nil || s.Store == nil || s.Runner == nil {
		return nil
	}
	now := s.now()()
	var out []Firing
	for _, job := range s.Store.Due(now) {
		out = append(out, s.fire(ctx, job))
	}
	return out
}

// Run calls Tick on a cadence until the context is cancelled.
func (s *Scheduler) Run(ctx context.Context) error {
	every := s.Every
	if every <= 0 {
		every = DefaultTick
	}
	// Fire what is already due before the first wait, so a scheduler started
	// after a window was missed does not sit idle for a whole tick.
	s.Tick(ctx)
	for {
		if err := SleepContext(ctx, every); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		s.Tick(ctx)
	}
}

// fire runs one job.
func (s *Scheduler) fire(ctx context.Context, job Job) Firing {
	f := Firing{Job: job, At: job.NextAt}

	// Injection safety, second gate: the payload was checked when it was
	// written, and it is checked again here. The jobs file is on disk, so
	// anything on disk can change — including by an agent with a write tool. A
	// payload that has grown an authority grab since it was stored is refused
	// before the runner is ever called.
	if ref, bad := GuardPayload(job.Task); bad {
		f.Refusal = &ref
		if s.OnRefusal != nil {
			s.OnRefusal(job, ref)
		}
		s.log("schedule_refused", map[string]any{
			"job": job.ID, "class": ref.Class, "rule": ref.Rule, "payload": DescribePayload(job.Task),
		})
		// The window is consumed: a refused payload is not retried every tick
		// forever. The operator is told once and can fix or remove the job.
		_ = s.Store.RecordFiring(job.ID, f.At, ref)
		return f
	}

	h := s.Hub
	if h == nil {
		h = hub.NewHub()
		s.Hub = h
	}
	title := fmt.Sprintf("schedule %s · %s", job.ID, clipLine(job.Task, loopTitleMax))
	task, runner := job.Task, s.Runner
	id := h.Spawn(title, s.Role, s.Model, func(runCtx context.Context) (*hub.Result, int, error) {
		return runner(runCtx, task)
	})
	f.RunID = id

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = h.Kill(id)
		case <-done:
		}
	}()
	_, waitErr := h.WaitContext(ctx, id)
	close(done)

	run, ok := h.Status(id)
	switch {
	case !ok:
		f.Err = fmt.Errorf("run %s disappeared", id)
	case run.State == hub.StateFailed:
		f.Err = fmt.Errorf("%s", run.Error)
	case run.State == hub.StateKilled && ctx.Err() == nil:
		f.Err = fmt.Errorf("the firing was killed")
	case waitErr != nil:
		f.Err = waitErr
	}
	if ok {
		f.Tokens = run.Tokens
		if run.Result != nil {
			f.Output = run.Result.Content
		}
	}

	_ = s.Store.RecordFiring(job.ID, f.At, f.Err)
	if f.Err != nil {
		s.log("schedule_failed", map[string]any{"job": job.ID, "error": f.Err.Error()})
	} else {
		s.log("schedule_fired", map[string]any{"job": job.ID, "run": id, "tokens": f.Tokens})
	}
	if s.OnFiring != nil {
		s.OnFiring(f)
	}
	return f
}

// log writes one metadata line, defaulting to a no-op.
func (s *Scheduler) log(kind string, v any) {
	if s.Log != nil {
		s.Log(kind, v)
	}
}

// now reads the clock, defaulting to the wall clock.
func (s *Scheduler) now() func() time.Time {
	if s.Now != nil {
		return s.Now
	}
	return time.Now
}

// FormatJobList renders the jobs for `kintsugi schedule list`.
func FormatJobList(jobs []Job, now time.Time) string {
	if len(jobs) == 0 {
		return "(no scheduled jobs)"
	}
	ordered := make([]Job, len(jobs))
	copy(ordered, jobs)
	// Display order is id order, not file order: an operator looks a job up by
	// the id the listing printed last time.
	for i := 0; i < len(ordered); i++ {
		for j := i + 1; j < len(ordered); j++ {
			if idLess(ordered[j].ID, ordered[i].ID) {
				ordered[i], ordered[j] = ordered[j], ordered[i]
			}
		}
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("%-8s %-16s %-6s %-16s %-16s %s\n",
		"id", "cron", "state", "next", "last", "task"))
	for _, j := range ordered {
		state := "active"
		if j.Paused {
			state = "paused"
		}
		next := "—"
		if !j.NextAt.IsZero() {
			next = relative(j.NextAt, now)
		}
		last := "never"
		if !j.LastFired.IsZero() {
			last = relative(j.LastFired, now)
		}
		task := clipLine(j.Task, 48)
		b.WriteString(fmt.Sprintf("%-8s %-16s %-6s %-16s %-16s %s\n", j.ID, j.Cron, state, next, last, task))
		if j.LastError != "" {
			b.WriteString(fmt.Sprintf("%-8s ✗ %s\n", "", clipLine(j.LastError, 90)))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// relative renders a moment as a distance from now: "in 4m", "2h ago".
func relative(t, now time.Time) string {
	d := t.Sub(now)
	if d < 0 {
		return humanDuration(-d) + " ago"
	}
	return "in " + humanDuration(d)
}

// humanDuration renders a duration in at most two units.
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()+0.5))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		h, m := int(d.Hours()), int(d.Minutes())%60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%02dm", h, m)
	default:
		days, h := int(d.Hours()/24), int(d.Hours())%24
		if h == 0 {
			return fmt.Sprintf("%dd", days)
		}
		return fmt.Sprintf("%dd%02dh", days, h)
	}
}
