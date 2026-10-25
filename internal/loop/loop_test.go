package loop

// Loop tests. The clock and the wait are both injected, so a loop of any length
// runs in microseconds and the test asserts the *cadence* rather than sleeping
// through it.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"kintsugi/internal/hub"
)

// fakeClock advances only when the loop waits, which is exactly the property
// under test: a loop that slept anywhere else would not move this clock.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	waits []time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Wait is the loop's sleep seam: it records the duration and advances the clock.
func (c *fakeClock) Wait(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.waits = append(c.waits, d)
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return nil
}

func (c *fakeClock) Waits() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]time.Duration, len(c.waits))
	copy(out, c.waits)
	return out
}

// fakeRunner answers each pass from a script, recording the task it was given.
type fakeRunner struct {
	mu     sync.Mutex
	calls  int
	tasks  []string
	answer func(call int) (string, error)
}

func (r *fakeRunner) Run(_ context.Context, task string) (*hub.Result, int, error) {
	r.mu.Lock()
	r.calls++
	call := r.calls
	r.tasks = append(r.tasks, task)
	fn := r.answer
	r.mu.Unlock()
	out, err := fn(call)
	if err != nil {
		return nil, 0, err
	}
	return &hub.Result{Content: out}, 10, nil
}

func (r *fakeRunner) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// TestLoopRunsOnceThenReRuns is the core I-57 property: a loop runs the task
// immediately, then again on every cadence tick, and the clock only moves when
// it waits.
func TestLoopRunsOnceThenReRuns(t *testing.T) {
	clock := newFakeClock()
	runs := 0
	runner := &fakeRunner{answer: func(call int) (string, error) {
		// A different answer each pass, so the "same answer twice" stop rule
		// never fires and the ceiling is what ends the loop.
		return fmt.Sprintf("check %d: all quiet", call), nil
	}}
	lp := &Loop{
		Task:          "watch the deploy",
		Interval:      2 * time.Minute,
		MaxIterations: 4,
		Runner:        runner.Run,
		Now:           clock.Now,
		Wait:          clock.Wait,
		OnIteration:   func(Iteration) { runs++ },
	}

	rep, err := lp.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := runner.Calls(); got != 4 {
		t.Fatalf("the loop ran %d checks, want 4", got)
	}
	if len(rep.Iterations) != 4 {
		t.Fatalf("report carries %d iterations, want 4", len(rep.Iterations))
	}
	if runs != 4 {
		t.Fatalf("OnIteration fired %d times, want 4", runs)
	}

	// The first check ran immediately: the first iteration's start time is the
	// clock's initial reading, not a value after a wait.
	start := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	if !rep.Iterations[0].Started.Equal(start) {
		t.Errorf("the first check started at %s, want %s (it must run before any wait)",
			rep.Iterations[0].Started, start)
	}
	// Checks 2..4 are one interval apart, and there are exactly three waits.
	want := []time.Time{
		start.Add(2 * time.Minute),
		start.Add(4 * time.Minute),
		start.Add(6 * time.Minute),
	}
	for i, w := range want {
		if got := rep.Iterations[i+1].Started; !got.Equal(w) {
			t.Errorf("check %d started at %s, want %s", i+2, got, w)
		}
	}
	waits := clock.Waits()
	if len(waits) != 3 {
		t.Fatalf("the loop waited %d times, want 3 (no wait after the final check): %v", len(waits), waits)
	}
	for i, d := range waits {
		if d != 2*time.Minute {
			t.Errorf("wait %d = %s, want 2m", i, d)
		}
	}
	if !strings.Contains(rep.Stopped, "ceiling") {
		t.Errorf("the report does not explain the stop: %q", rep.Stopped)
	}

	// Every pass got the loop's task text, unmodified.
	for i, task := range runner.tasks {
		if task != "watch the deploy" {
			t.Errorf("check %d ran %q, want the loop's task", i, task)
		}
	}
}

// TestLoopStopsWhenTheAnswerRepeats is the "ending when another check cannot
// help" half: two identical answers end the loop.
func TestLoopStopsWhenTheAnswerRepeats(t *testing.T) {
	clock := newFakeClock()
	runner := &fakeRunner{answer: func(int) (string, error) { return "nothing to report", nil }}
	lp := &Loop{
		Task: "poll", Interval: time.Minute, MaxIterations: 10,
		Runner: runner.Run, Now: clock.Now, Wait: clock.Wait,
	}

	rep, err := lp.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := runner.Calls(); got != 2 {
		t.Fatalf("the loop ran %d checks, want 2 (the second repeated the first)", got)
	}
	if !strings.Contains(rep.Stopped, "same answer") {
		t.Errorf("the stop reason does not name the repeat: %q", rep.Stopped)
	}
	last, ok := rep.Last()
	if !ok || !strings.Contains(last.Reason, "same answer") {
		t.Errorf("the final iteration does not carry the stop reason: %+v", last)
	}
}

// TestLoopContinuesWhileTheAnswerChanges proves the repeat rule is not just
// "stop after two passes".
func TestLoopContinuesWhileTheAnswerChanges(t *testing.T) {
	clock := newFakeClock()
	runner := &fakeRunner{answer: func(call int) (string, error) {
		return fmt.Sprintf("queue depth %d", call), nil
	}}
	lp := &Loop{
		Task: "poll", Interval: time.Minute, MaxIterations: 3,
		Runner: runner.Run, Now: clock.Now, Wait: clock.Wait,
	}
	if _, err := lp.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := runner.Calls(); got != 3 {
		t.Fatalf("the loop ran %d checks, want 3", got)
	}
}

// TestLoopStopsOnAFailedCheck proves a failure ends the loop by default: the
// same wiring will fail the same way.
func TestLoopStopsOnAFailedCheck(t *testing.T) {
	clock := newFakeClock()
	runner := &fakeRunner{answer: func(int) (string, error) {
		return "", errors.New("provider unreachable")
	}}
	lp := &Loop{
		Task: "poll", Interval: time.Minute, MaxIterations: 5,
		Runner: runner.Run, Now: clock.Now, Wait: clock.Wait,
	}
	rep, err := lp.Run(context.Background())
	if err != nil {
		t.Fatalf("a failed check must not fail the loop: %v", err)
	}
	if got := runner.Calls(); got != 1 {
		t.Fatalf("the loop ran %d checks after a failure, want 1", got)
	}
	if !strings.Contains(rep.Stopped, "failed") {
		t.Errorf("the stop reason does not name the failure: %q", rep.Stopped)
	}
}

// TestLoopKeepGoing proves the operator can override the failure stop.
func TestLoopKeepGoing(t *testing.T) {
	clock := newFakeClock()
	runner := &fakeRunner{answer: func(call int) (string, error) {
		if call == 1 {
			return "", errors.New("first pass trips")
		}
		return fmt.Sprintf("pass %d fine", call), nil
	}}
	lp := &Loop{
		Task: "poll", Interval: time.Minute, MaxIterations: 3, KeepGoingOnError: true,
		Runner: runner.Run, Now: clock.Now, Wait: clock.Wait,
	}
	rep, err := lp.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := runner.Calls(); got != 3 {
		t.Fatalf("keep-going ran %d checks, want 3", got)
	}
	if len(rep.Iterations) != 3 || rep.Iterations[0].Err == nil {
		t.Fatalf("the first iteration does not carry its failure: %+v", rep.Iterations)
	}
	if !strings.Contains(rep.Stopped, "ceiling") {
		t.Errorf("the loop stopped for the wrong reason: %q", rep.Stopped)
	}
}

// TestLoopCancelsMidWait proves an interrupt ends the loop cleanly and that no
// further check runs.
func TestLoopCancelsMidWait(t *testing.T) {
	runner := &fakeRunner{answer: func(call int) (string, error) {
		return fmt.Sprintf("pass %d", call), nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	lp := &Loop{
		Task: "poll", Interval: time.Hour, MaxIterations: 10,
		Runner: runner.Run,
		Wait: func(ctx context.Context, _ time.Duration) error {
			cancel() // the operator hits ctrl-c during the wait
			return ctx.Err()
		},
		OnIteration: func(Iteration) { cancel() },
	}
	rep, err := lp.Run(ctx)
	if err == nil {
		t.Fatal("a cancelled loop must return the cancellation, not a clean stop")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
	if got := runner.Calls(); got != 1 {
		t.Fatalf("the loop ran %d checks after cancellation, want 1", got)
	}
	if rep.Stopped != "cancelled" {
		t.Errorf("report says %q, want \"cancelled\"", rep.Stopped)
	}
}

// TestLoopEachPassIsAHubRun proves the pass really is a hub run: the report
// carries the id, and the hub knows the run's state.
func TestLoopEachPassIsAHubRun(t *testing.T) {
	clock := newFakeClock()
	h := hub.NewHub()
	runner := &fakeRunner{answer: func(call int) (string, error) {
		return fmt.Sprintf("pass %d", call), nil
	}}
	lp := &Loop{
		Task: "poll", Interval: time.Minute, MaxIterations: 2,
		Runner: runner.Run, Hub: h, Now: clock.Now, Wait: clock.Wait,
	}
	rep, err := lp.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, it := range rep.Iterations {
		run, ok := h.Status(it.RunID)
		if !ok {
			t.Fatalf("pass %d reported run %q, which the hub does not know", it.Index, it.RunID)
		}
		if run.State != hub.StateDone {
			t.Errorf("run %s state = %s, want done", it.RunID, run.State)
		}
		if !strings.Contains(run.Title, "check") {
			t.Errorf("run title %q does not say which check it was", run.Title)
		}
	}
	if len(h.List()) != 2 {
		t.Fatalf("the hub holds %d runs, want 2", len(h.List()))
	}
}

// TestLoopRequiresATaskAndRunner pins the two preconditions.
func TestLoopRequiresATaskAndRunner(t *testing.T) {
	if _, err := (&Loop{}).Run(context.Background()); err == nil {
		t.Error("a loop with no runner was accepted")
	}
	if _, err := (&Loop{Runner: func(context.Context, string) (*hub.Result, int, error) {
		return nil, 0, nil
	}}).Run(context.Background()); err == nil {
		t.Error("a loop with no task was accepted")
	}
}

// TestSleepContextReturnsOnCancel keeps the real wait honest.
func TestSleepContextReturnsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := SleepContext(ctx, time.Hour); err == nil {
		t.Fatal("SleepContext ignored a cancelled context")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("SleepContext slept %s after cancellation", d)
	}
}
