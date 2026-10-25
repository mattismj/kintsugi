// Package loop runs self-paced loops (I-57) and cron-scheduled jobs (I-56).
//
// # One registry, not two
//
// Both features schedule work, and both run each pass as a background run in
// internal/hub — the same registry task(background=true), agent_output and the
// TUI roster already use. There is deliberately no second scheduler
// abstraction in this package: a loop iteration or a fired job is a hub.Run
// with a title, a role, a model, a state and a result, so an operator can see
// it in the roster and kill it with the machinery that already exists.
//
// # Injection safety: a scheduled prompt is data, never authority
//
// This is the non-negotiable rule of the whole package, and it is enforced in
// two places, not documented and hoped for:
//
//   - loop.GuardPayload (guard.go) scans a job's or a loop's task text for an
//     attempt to grant a permission, widen the declared scope, or write
//     authorization/scope context (CONTEXT.md, [authorization], an attestation).
//     A payload that tries is refused, and the refusal is logged with the
//     class and the rule label — never with the matched span, so the attempt
//     cannot be replayed into a prompt.
//   - The guard runs at write time (the CLI will not store such a job) *and*
//     at fire time (a hand-edited jobs file, or a file edited by an agent,
//     cannot smuggle one in later). Defense in depth, because the file is on
//     disk and anything on disk can change.
//
// What the package does NOT do is read any of that text into a system prompt,
// a hook decision, a permission rule or an attestation. Task text is passed to
// the runner as a user turn and nothing else: the runner's policy, scope and
// sandbox come from configuration the operator wrote, exactly as they would
// for a typed turn. A scheduled payload therefore has precisely the authority
// of a line typed into the prompt — which is to say, none.
package loop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"kintsugi/internal/hub"
)

// Defaults for a loop that names neither an interval nor a ceiling. Ten checks
// at five minutes is a quarter-hour of watching: past that, a human should be
// reading the output rather than paying for another pass.
const (
	DefaultInterval      = 5 * time.Minute
	DefaultMaxIterations = 10
)

// Runner performs one pass of a loop or one firing of a job. It has the shape
// of the hub's work function on purpose, so a pass is a hub run and nothing
// else: the text result, an approximate token count, and an error.
type Runner func(ctx context.Context, task string) (*hub.Result, int, error)

// Iteration is one completed pass of a loop.
type Iteration struct {
	// Index is the zero-based pass number.
	Index int
	// RunID is the hub run the pass ran as.
	RunID string
	// Started is the pass's start time, read from the loop's clock.
	Started time.Time
	// Output is the runner's text result ("" when the pass failed).
	Output string
	// Tokens is the runner's approximate token count.
	Tokens int
	// Err is the pass's failure, if any. The loop normally stops on the first
	// failure: another pass with the same broken wiring cannot help.
	Err error
	// Reason is why the loop stopped after this pass ("" means it did not).
	Reason string
}

// Failed reports whether the pass failed.
func (i Iteration) Failed() bool { return i.Err != nil }

// Report is what one loop run did.
type Report struct {
	Iterations []Iteration
	// Stopped explains why the loop ended. It is always set on a normal return.
	Stopped string
}

// Last returns the final pass, if the loop ran at least once.
func (r Report) Last() (Iteration, bool) {
	if len(r.Iterations) == 0 {
		return Iteration{}, false
	}
	return r.Iterations[len(r.Iterations)-1], true
}

// Loop is a self-paced loop: run once immediately, then again on a cadence,
// ending when another check cannot help.
type Loop struct {
	// Task is the text each pass runs. It is data handed to the runner as a
	// user turn — see the package comment.
	Task string
	// Title, Role and Model are hub metadata, so the roster names the pass.
	Title string
	Role  string
	Model string
	// Interval is the wait between passes. Zero means DefaultInterval.
	Interval time.Duration
	// MaxIterations is the ceiling on passes. Zero means DefaultMaxIterations.
	MaxIterations int
	// KeepGoingOnError continues after a failed pass instead of stopping. The
	// default (false) is the honest one: a pass that could not run says more
	// about the wiring than another identical attempt would.
	KeepGoingOnError bool
	// Runner performs one pass. Required.
	Runner Runner
	// Hub is the run registry. Nil allocates a private one, which is what a
	// test wants; the CLI passes the session's hub so the roster sees it.
	Hub *hub.Hub
	// Now reads the clock. Nil means time.Now.
	Now func() time.Time
	// Wait blocks for the cadence. Nil means a timer. This is the seam a fake
	// clock replaces, and the only place the loop sleeps.
	Wait func(ctx context.Context, d time.Duration) error
	// ShouldStop decides whether the loop ends after a pass. Nil means
	// defaultShouldStop.
	ShouldStop func(prev, cur Iteration) (stop bool, reason string)
	// OnIteration observes each pass. It runs after the pass completed and
	// before the loop decides whether to continue.
	OnIteration func(Iteration)
}

// Run drives the loop until it stops, the context is cancelled, or the
// iteration ceiling is reached.
func (l *Loop) Run(ctx context.Context) (Report, error) {
	var rep Report
	if l == nil {
		return rep, errors.New("loop: no loop")
	}
	if l.Runner == nil {
		return rep, errors.New("loop: no runner")
	}
	if strings.TrimSpace(l.Task) == "" {
		return rep, errors.New("loop: no task")
	}
	h := l.Hub
	if h == nil {
		h = hub.NewHub()
		l.Hub = h
	}
	interval := l.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	max := l.MaxIterations
	if max <= 0 {
		max = DefaultMaxIterations
	}
	wait := l.Wait
	if wait == nil {
		wait = SleepContext
	}

	var prev Iteration
	havePrev := false
	for i := 0; i < max; i++ {
		if err := ctx.Err(); err != nil {
			rep.Stopped = "cancelled"
			return rep, err
		}
		it := l.iteration(ctx, h, i)
		rep.Iterations = append(rep.Iterations, it)
		if l.OnIteration != nil {
			l.OnIteration(it)
		}
		stop, reason := l.shouldStop(havePrev, prev, it)
		if stop {
			rep.Iterations[len(rep.Iterations)-1].Reason = reason
			rep.Stopped = reason
			return rep, nil
		}
		prev, havePrev = it, true
		if i+1 >= max {
			break
		}
		if err := wait(ctx, interval); err != nil {
			rep.Stopped = "cancelled"
			return rep, err
		}
	}
	rep.Stopped = fmt.Sprintf("reached the iteration ceiling (%d)", max)
	return rep, nil
}

// iteration runs one pass as a hub run and folds its outcome into an Iteration.
func (l *Loop) iteration(ctx context.Context, h *hub.Hub, idx int) Iteration {
	it := Iteration{Index: idx, Started: l.now()}
	title := l.Title
	if strings.TrimSpace(title) == "" {
		title = l.Task
	}
	title = fmt.Sprintf("%s · check %d", clipLine(title, loopTitleMax), idx+1)

	task, runner := l.Task, l.Runner
	id := h.Spawn(title, l.Role, l.Model, func(runCtx context.Context) (*hub.Result, int, error) {
		return runner(runCtx, task)
	})
	it.RunID = id

	// The hub's own context outlives its caller by design (a detached run must
	// survive one turn's deadline). A loop is the exception: cancelling the loop
	// must cancel the pass it is waiting on, or Ctrl-C would leave a model
	// request running for the rest of the cadence.
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
		it.Err = fmt.Errorf("run %s disappeared", id)
	case run.State == hub.StateFailed:
		it.Err = errors.New(run.Error)
	case run.State == hub.StateKilled && ctx.Err() == nil:
		it.Err = errors.New("the pass was killed")
	case waitErr != nil:
		it.Err = waitErr
	}
	if ok {
		it.Tokens = run.Tokens
		if run.Result != nil {
			it.Output = run.Result.Content
		}
	}
	return it
}

// loopTitleMax bounds the hub title derived from the task text. The roster is
// a table, not a document.
const loopTitleMax = 40

// shouldStop applies the caller's rule, or the default one. KeepGoingOnError is
// honoured here rather than inside the default rule, so it also softens a custom
// rule: "keep going after a failure" is the loop's own promise, not a detail of
// one stop policy.
func (l *Loop) shouldStop(havePrev bool, prev, cur Iteration) (bool, string) {
	if l.KeepGoingOnError && cur.Err != nil {
		return false, ""
	}
	if l.ShouldStop != nil {
		return l.ShouldStop(prev, cur)
	}
	return defaultShouldStop(havePrev, prev, cur)
}

// defaultShouldStop ends the loop when another check cannot help:
//
//   - the pass failed, and the caller did not ask to keep going — the same
//     wiring will fail the same way;
//   - the pass returned the same answer as the previous one — a check that
//     reports nothing new has nothing left to report.
//
// Everything else continues: a loop is a cadence, not a retry.
func defaultShouldStop(havePrev bool, prev, cur Iteration) (bool, string) {
	if cur.Err != nil {
		return true, fmt.Sprintf("the check failed (%v) · another pass cannot help", cur.Err)
	}
	if havePrev && prev.Err == nil && sameOutput(prev.Output, cur.Output) {
		return true, "the check returned the same answer twice · another pass cannot help"
	}
	return false, ""
}

// sameOutput compares two answers after whitespace normalization, so a
// difference in trailing blank lines is not mistaken for progress.
func sameOutput(a, b string) bool {
	return strings.Join(strings.Fields(a), " ") == strings.Join(strings.Fields(b), " ")
}

// now reads the clock, defaulting to the wall clock.
func (l *Loop) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// SleepContext waits for d or until ctx is cancelled.
func SleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
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
