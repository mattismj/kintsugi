// Package hub is the background run registry for detached subagent work
// (M10-D). It is deliberately independent of the tool and subagent packages:
// a hub stores the lifecycle of a run and its text result, and the task tool
// decides how to produce that result. That boundary is what keeps
// tools → subagent → tools from becoming a cycle.
//
// One level of delegation stays enforced elsewhere (the subagent manager, not
// the hub): a worker cannot spawn its own workers.
package hub

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// RunState is the lifecycle state of one background run.
type RunState string

const (
	StateRunning RunState = "running"
	StateDone    RunState = "done"
	StateFailed  RunState = "failed"
	StateKilled  RunState = "killed"
)

// Result is the outcome of one background run. The hub stays independent of
// the tool layer, so the payload is plain text: the task tool folds a worker's
// result into Content (JSON when the worker returns a structured result) when
// it spawns a background run. Failures are carried on the Run, not here.
type Result struct {
	Content string
}

// Run is one background subagent run.
type Run struct {
	ID        string
	Title     string
	Role      string
	Model     string
	State     RunState
	StartedAt time.Time
	EndedAt   time.Time
	Tokens    int
	Result    *Result
	Error     string
	cancel    context.CancelFunc
}

// Elapsed returns the duration since the run started (or until it ended).
func (r *Run) Elapsed() time.Duration {
	if r.EndedAt.IsZero() {
		return time.Since(r.StartedAt)
	}
	return r.EndedAt.Sub(r.StartedAt)
}

// Hub manages background subagent runs.
type Hub struct {
	mu     sync.RWMutex
	runs   map[string]*Run
	nextID int
}

// NewHub creates a new hub.
func NewHub() *Hub {
	return &Hub{runs: make(map[string]*Run)}
}

// Spawn starts a background run and returns its ID immediately.
func (h *Hub) Spawn(title, role, model string, fn func(ctx context.Context) (*Result, int, error)) string {
	h.mu.Lock()
	h.nextID++
	id := fmt.Sprintf("run-%d", h.nextID)
	ctx, cancel := context.WithCancel(context.Background())
	run := &Run{
		ID:        id,
		Title:     title,
		Role:      role,
		Model:     model,
		State:     StateRunning,
		StartedAt: time.Now(),
		cancel:    cancel,
	}
	h.runs[id] = run
	h.mu.Unlock()

	go func() {
		res, tokens, err := fn(ctx)
		h.mu.Lock()
		// A run that Kill already moved to a terminal state stays there: its
		// work function returns with a cancellation error, and that is the
		// expected end of a killed run, not a failure to record over the kill.
		if run.State != StateRunning {
			h.mu.Unlock()
			return
		}
		run.EndedAt = time.Now()
		run.Tokens = tokens
		if err != nil {
			run.State = StateFailed
			run.Error = err.Error()
		} else if res != nil {
			run.State = StateDone
			run.Result = res
		} else {
			run.State = StateFailed
			run.Error = "no result"
		}
		h.mu.Unlock()
	}()

	return id
}

// Status returns a snapshot of one run's state.
func (h *Hub) Status(id string) (*Run, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	run, ok := h.runs[id]
	if !ok {
		return nil, false
	}
	// Return a copy to avoid races.
	cp := *run
	return &cp, true
}

// Wait blocks until the run completes and returns its result.
func (h *Hub) Wait(id string) (*Result, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		run, ok := h.Status(id)
		if !ok {
			return nil, fmt.Errorf("unknown run %q", id)
		}
		if run.State == StateDone {
			return run.Result, nil
		}
		if run.State == StateFailed || run.State == StateKilled {
			return nil, fmt.Errorf("run %s: %s", run.State, run.Error)
		}
	}
	return nil, fmt.Errorf("unreachable")
}

// WaitContext blocks until the run completes or context is cancelled.
func (h *Hub) WaitContext(ctx context.Context, id string) (*Result, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			run, ok := h.Status(id)
			if !ok {
				return nil, fmt.Errorf("unknown run %q", id)
			}
			if run.State == StateDone {
				return run.Result, nil
			}
			if run.State == StateFailed || run.State == StateKilled {
				return nil, fmt.Errorf("run %s: %s", run.State, run.Error)
			}
		}
	}
}

// Kill cancels a running background task.
func (h *Hub) Kill(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	run, ok := h.runs[id]
	if !ok {
		return fmt.Errorf("unknown run %q", id)
	}
	if run.State != StateRunning {
		return fmt.Errorf("run %q is not running (state: %s)", id, run.State)
	}
	run.cancel()
	run.State = StateKilled
	run.EndedAt = time.Now()
	return nil
}

// List returns all runs.
func (h *Hub) List() []*Run {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*Run, 0, len(h.runs))
	for _, run := range h.runs {
		cp := *run
		out = append(out, &cp)
	}
	return out
}

// Cleanup removes completed runs older than the given duration.
func (h *Hub) Cleanup(olderThan time.Duration) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	removed := 0
	for id, run := range h.runs {
		if run.State != StateRunning && !run.EndedAt.IsZero() {
			if now.Sub(run.EndedAt) > olderThan {
				delete(h.runs, id)
				removed++
			}
		}
	}
	return removed
}
