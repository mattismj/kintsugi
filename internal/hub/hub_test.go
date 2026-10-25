package hub

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestHubSpawnAndCollect(t *testing.T) {
	h := NewHub()

	id := h.Spawn("test task", "coder", "mock/model", func(ctx context.Context) (*Result, int, error) {
		time.Sleep(50 * time.Millisecond)
		return &Result{Content: "completed successfully"}, 100, nil
	})
	if id == "" {
		t.Fatal("Spawn returned empty ID")
	}

	run, ok := h.Status(id)
	if !ok {
		t.Fatal("Status returned false for spawned run")
	}
	if run.State != StateRunning {
		t.Errorf("State = %v, want running", run.State)
	}
	if run.Title != "test task" || run.Role != "coder" || run.Model != "mock/model" {
		t.Errorf("run metadata = %+v", run)
	}

	res, err := h.Wait(id)
	if err != nil {
		t.Fatalf("Wait error: %v", err)
	}
	if res == nil || res.Content != "completed successfully" {
		t.Errorf("Wait result = %+v", res)
	}

	run, ok = h.Status(id)
	if !ok {
		t.Fatal("Status returned false after completion")
	}
	if run.State != StateDone {
		t.Errorf("State = %v, want done", run.State)
	}
	if run.Tokens != 100 {
		t.Errorf("Tokens = %d, want 100", run.Tokens)
	}
	if run.EndedAt.IsZero() {
		t.Error("EndedAt is zero after completion")
	}
	if run.Elapsed() < 50*time.Millisecond {
		t.Errorf("Elapsed = %v, want >= 50ms", run.Elapsed())
	}
}

func TestHubSpawnFailure(t *testing.T) {
	h := NewHub()

	id := h.Spawn("failing task", "researcher", "mock/model", func(ctx context.Context) (*Result, int, error) {
		time.Sleep(20 * time.Millisecond)
		return nil, 50, errors.New("task failed")
	})

	if _, err := h.Wait(id); err == nil {
		t.Fatal("Wait should return error for failed task")
	}

	run, ok := h.Status(id)
	if !ok {
		t.Fatal("Status returned false for failed run")
	}
	if run.State != StateFailed {
		t.Errorf("State = %v, want failed", run.State)
	}
	if run.Error != "task failed" {
		t.Errorf("Error = %q, want %q", run.Error, "task failed")
	}
}

func TestHubSpawnNoResult(t *testing.T) {
	h := NewHub()

	id := h.Spawn("empty task", "coder", "mock/model", func(ctx context.Context) (*Result, int, error) {
		return nil, 0, nil
	})
	if _, err := h.Wait(id); err == nil {
		t.Fatal("Wait should return error for a run that produced no result")
	}
}

func TestHubKill(t *testing.T) {
	h := NewHub()

	id := h.Spawn("long task", "coder", "mock/model", func(ctx context.Context) (*Result, int, error) {
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-time.After(10 * time.Second):
			return &Result{Content: "done"}, 100, nil
		}
	})

	time.Sleep(20 * time.Millisecond)

	if err := h.Kill(id); err != nil {
		t.Fatalf("Kill error: %v", err)
	}

	run, ok := h.Status(id)
	if !ok {
		t.Fatal("Status returned false after kill")
	}
	if run.State != StateKilled {
		t.Errorf("State = %v, want killed", run.State)
	}
	if run.EndedAt.IsZero() {
		t.Error("EndedAt is zero after kill")
	}
	if _, err := h.Wait(id); err == nil {
		t.Fatal("Wait should return error for killed task")
	}
}

func TestHubKillUnknown(t *testing.T) {
	h := NewHub()
	if err := h.Kill("unknown"); err == nil {
		t.Fatal("Kill should fail for unknown ID")
	}
}

// A killed run stays killed after its work function returns. The function comes
// back with a cancellation error, and recording that as a failure would rewrite
// the operator's own decision into a crash.
func TestHubKillStateSurvivesWorkerReturn(t *testing.T) {
	h := NewHub()

	returned := make(chan struct{})
	id := h.Spawn("long task", "coder", "mock/model", func(ctx context.Context) (*Result, int, error) {
		defer close(returned)
		<-ctx.Done()
		return nil, 0, ctx.Err()
	})

	if err := h.Kill(id); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	<-returned // the goroutine has now run its state write

	run, ok := h.Status(id)
	if !ok {
		t.Fatal("run vanished")
	}
	if run.State != StateKilled {
		t.Fatalf("State = %q, want killed after the worker returned", run.State)
	}
}

func TestHubKillCompleted(t *testing.T) {
	h := NewHub()

	id := h.Spawn("quick task", "coder", "mock/model", func(ctx context.Context) (*Result, int, error) {
		return &Result{Content: "done"}, 10, nil
	})
	_, _ = h.Wait(id)

	if err := h.Kill(id); err == nil {
		t.Fatal("Kill should fail for completed task")
	}
}

func TestHubList(t *testing.T) {
	h := NewHub()

	var ids []string
	roles := []string{"coder", "researcher", "opsec"}
	for i, role := range roles {
		role, n := role, i+1
		ids = append(ids, h.Spawn("task", role, "mock/model", func(ctx context.Context) (*Result, int, error) {
			time.Sleep(time.Duration(n) * 30 * time.Millisecond)
			return &Result{Content: "done"}, n * 10, nil
		}))
	}

	if runs := h.List(); len(runs) != 3 {
		t.Fatalf("List returned %d runs, want 3", len(runs))
	}
	for _, id := range ids {
		_, _ = h.Wait(id)
	}
	runs := h.List()
	if len(runs) != 3 {
		t.Fatalf("List returned %d runs after completion, want 3", len(runs))
	}
	for _, run := range runs {
		if run.State != StateDone {
			t.Errorf("Run %s state = %v, want done", run.ID, run.State)
		}
	}
}

func TestHubCleanup(t *testing.T) {
	h := NewHub()

	id1 := h.Spawn("old task", "coder", "mock/model", func(ctx context.Context) (*Result, int, error) {
		return &Result{Content: "done"}, 10, nil
	})
	_, _ = h.Wait(id1)

	h.mu.Lock()
	h.runs[id1].EndedAt = time.Now().Add(-2 * time.Hour)
	h.mu.Unlock()

	id2 := h.Spawn("recent task", "coder", "mock/model", func(ctx context.Context) (*Result, int, error) {
		return &Result{Content: "done"}, 20, nil
	})
	_, _ = h.Wait(id2)

	if removed := h.Cleanup(1 * time.Hour); removed != 1 {
		t.Errorf("Cleanup removed %d tasks, want 1", removed)
	}

	runs := h.List()
	if len(runs) != 1 || runs[0].ID != id2 {
		t.Fatalf("after cleanup runs = %+v, want only %s", runs, id2)
	}
}

func TestHubWaitUnknown(t *testing.T) {
	h := NewHub()
	if _, err := h.Wait("unknown"); err == nil {
		t.Fatal("Wait should fail for unknown ID")
	}
}

func TestHubWaitContextCancel(t *testing.T) {
	h := NewHub()

	id := h.Spawn("long task", "coder", "mock/model", func(ctx context.Context) (*Result, int, error) {
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-time.After(10 * time.Second):
			return &Result{Content: "done"}, 100, nil
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := h.WaitContext(ctx, id); err == nil {
		t.Fatal("WaitContext should fail when its context expires")
	}
}

func TestRunElapsed(t *testing.T) {
	run := &Run{
		StartedAt: time.Now().Add(-5 * time.Second),
		State:     StateRunning,
	}
	if elapsed := run.Elapsed(); elapsed < 5*time.Second || elapsed > 6*time.Second {
		t.Errorf("Elapsed = %v, want ~5s", elapsed)
	}

	run.EndedAt = run.StartedAt.Add(3 * time.Second)
	if elapsed := run.Elapsed(); elapsed != 3*time.Second {
		t.Errorf("Elapsed = %v, want 3s", elapsed)
	}
}
