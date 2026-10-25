package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"kintsugi/internal/hub"
	"kintsugi/internal/provider"
)

// gateTasks is a TaskRunner the test releases by hand, so a background run can
// be observed while it is still going. The got slice is guarded: the foreground
// RunTasks and a background spawn's copy both append to it, so an unsynchronized
// append is a data race under heavy scheduling.
type gateTasks struct {
	mu      sync.Mutex
	got     []TaskRequest
	release chan (struct{})
	out     string
}

func (g *gateTasks) RunTasks(ctx context.Context, tasks []TaskRequest) (string, error) {
	g.mu.Lock()
	g.got = append(g.got, tasks...)
	g.mu.Unlock()
	if g.release != nil {
		select {
		case <-g.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if g.out != "" {
		return g.out, nil
	}
	return `{"results":[]}`, nil
}

func callTask(t *testing.T, e *Executor, args string) Result {
	t.Helper()
	return e.Dispatch(provider.ToolCall{Function: provider.Function{Name: "task", Arguments: args}})
}

func callAgentOutput(t *testing.T, e *Executor, args string) Result {
	t.Helper()
	return e.Dispatch(provider.ToolCall{Function: provider.Function{Name: "agent_output", Arguments: args}})
}

func decodeResult(t *testing.T, res Result) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatalf("result is not JSON: %q (%v)", res.Content, err)
	}
	return out
}

// ---------- detach ----------

func TestTaskBackgroundReturnsIDImmediately(t *testing.T) {
	g := &gateTasks{release: make(chan struct{})}
	h := hub.NewHub()
	e := &Executor{Workspace: t.TempDir(), Tasks: g, Hub: h, Policy: Policy{Yolo: true}}

	res := callTask(t, e, `{"tasks":[{"title":"slow","prompt":"take your time","background":true}]}`)
	if res.Failed {
		t.Fatalf("dispatch failed: %s", res.Content)
	}
	payload := decodeResult(t, res)
	ids, ok := payload["background"].([]any)
	if !ok || len(ids) != 1 {
		t.Fatalf("payload = %+v, want one background id", payload)
	}
	id, _ := ids[0].(string)
	run, ok := h.Status(id)
	if !ok {
		t.Fatalf("hub has no run %q", id)
	}
	if run.State != hub.StateRunning {
		t.Fatalf("State = %q, want running before the runner is released", run.State)
	}
	// The call returned while the work had not finished: that is the whole
	// point of detaching.
	close(g.release)
	waitForRun(t, h, id, hub.StateDone)
}

func TestTaskBackgroundWithoutHubSaysSo(t *testing.T) {
	e := &Executor{Workspace: t.TempDir(), Tasks: &gateTasks{}, Policy: Policy{Yolo: true}}
	res := callTask(t, e, `{"tasks":[{"title":"a","prompt":"b","background":true}]}`)
	if !res.Failed {
		t.Fatal("a background task with no hub was accepted")
	}
	if !strings.Contains(res.Content, "hub") {
		t.Fatalf("the error does not name the missing piece: %s", res.Content)
	}
	// And it must not have quietly run in the foreground instead.
	if len(e.Tasks.(*gateTasks).got) != 0 {
		t.Fatal("the task ran despite the refusal")
	}
}

func TestTaskForegroundStillWaitsWhenBackgroundIsUnset(t *testing.T) {
	g := &gateTasks{out: `{"results":[{"title":"a"}]}`}
	e := &Executor{Workspace: t.TempDir(), Tasks: g, Hub: hub.NewHub(), Policy: Policy{Yolo: true}}
	res := callTask(t, e, `{"tasks":[{"title":"a","prompt":"b"}]}`)
	if res.Failed {
		t.Fatalf("dispatch failed: %s", res.Content)
	}
	if res.Content != `{"results":[{"title":"a"}]}` {
		t.Fatalf("foreground result changed shape: %s", res.Content)
	}
	if strings.Contains(res.Content, "background") {
		t.Fatalf("an awaited call reported background ids: %s", res.Content)
	}
}

func TestTaskMixedCallReportsBoth(t *testing.T) {
	g := &gateTasks{out: `{"results":[]}`}
	h := hub.NewHub()
	e := &Executor{Workspace: t.TempDir(), Tasks: g, Hub: h, Policy: Policy{Yolo: true}}

	res := callTask(t, e, `{"tasks":[
		{"title":"detached","prompt":"go slowly","background":true},
		{"title":"awaited","prompt":"finish now"}
	]}`)
	if res.Failed {
		t.Fatalf("dispatch failed: %s", res.Content)
	}
	payload := decodeResult(t, res)
	ids, _ := payload["background"].([]any)
	if len(ids) != 1 {
		t.Fatalf("payload = %+v, want exactly one background id", payload)
	}
	if payload["foreground"] == nil {
		t.Fatalf("a mixed call dropped the awaited result: %+v", payload)
	}
}

func TestTaskBackgroundSurvivesCallerCancel(t *testing.T) {
	// A detached run is not the turn's child: the turn ending must not kill it.
	g := &gateTasks{release: make(chan struct{})}
	h := hub.NewHub()
	e := &Executor{Workspace: t.TempDir(), Tasks: g, Hub: h, Policy: Policy{Yolo: true}}

	ctx, cancel := context.WithCancel(context.Background())
	res := e.DispatchContext(ctx, provider.ToolCall{Function: provider.Function{
		Name:      "task",
		Arguments: `{"tasks":[{"title":"outlives","prompt":"keep going","background":true}]}`,
	}})
	if res.Failed {
		t.Fatalf("dispatch failed: %s", res.Content)
	}
	id := decodeResult(t, res)["background"].([]any)[0].(string)
	cancel() // the caller's context dies

	close(g.release)
	waitForRun(t, h, id, hub.StateDone) // the run still finishes
}

// ---------- collect ----------

func TestAgentOutputRegisteredOnlyWithHub(t *testing.T) {
	e := &Executor{Workspace: t.TempDir(), Tasks: &gateTasks{}}
	if hasTool(e.Definitions(), "agent_output") {
		t.Fatal("agent_output registered without a hub")
	}
	e.Hub = hub.NewHub()
	if !hasTool(e.Definitions(), "agent_output") {
		t.Fatal("agent_output not registered with a hub")
	}
}

func TestAgentOutputStatusWaitsAndKill(t *testing.T) {
	g := &gateTasks{release: make(chan struct{}), out: `{"results":[{"summary":"done deed"}]}`}
	h := hub.NewHub()
	e := &Executor{Workspace: t.TempDir(), Tasks: g, Hub: h, Policy: Policy{Yolo: true}}

	spawned := decodeResult(t, callTask(t, e,
		`{"tasks":[{"title":"job","prompt":"work","background":true}]}`))
	id := spawned["background"].([]any)[0].(string)

	// status on a live run: reported, not waited on.
	st := decodeResult(t, callAgentOutput(t, e, `{"id":"`+id+`"}`))
	if st["state"] != string(hub.StateRunning) {
		t.Fatalf("status = %+v, want running", st)
	}

	close(g.release)
	waitForRun(t, h, id, hub.StateDone)

	// wait returns the stored result.
	res := decodeResult(t, callAgentOutput(t, e, `{"id":"`+id+`","action":"wait"}`))
	if res["state"] != string(hub.StateDone) {
		t.Fatalf("wait = %+v, want done", res)
	}
	if got, ok := res["result"].(string); !ok || !strings.Contains(got, "done deed") {
		t.Fatalf("wait did not carry the worker output: %+v", res)
	}

	// kill after completion is refused, not silently accepted.
	if out := callAgentOutput(t, e, `{"id":"`+id+`","action":"kill"}`); !out.Failed {
		t.Fatalf("killing a finished run succeeded: %s", out.Content)
	}
}

func TestAgentOutputKillStopsARun(t *testing.T) {
	g := &gateTasks{release: make(chan struct{})}
	h := hub.NewHub()
	e := &Executor{Workspace: t.TempDir(), Tasks: g, Hub: h, Policy: Policy{Yolo: true}}

	id := decodeResult(t, callTask(t, e,
		`{"tasks":[{"title":"doomed","prompt":"never finishes","background":true}]}`),
	)["background"].([]any)[0].(string)

	out := callAgentOutput(t, e, `{"id":"`+id+`","action":"kill"}`)
	if out.Failed {
		t.Fatalf("kill failed: %s", out.Content)
	}
	if state := decodeResult(t, out)["state"]; state != string(hub.StateKilled) {
		t.Fatalf("kill payload = %v, want killed", state)
	}
	// The gate is never released: only a real cancellation ends the run.
	waitForRun(t, h, id, hub.StateKilled)
}

func TestAgentOutputWaitTimeoutReportsPending(t *testing.T) {
	g := &gateTasks{release: make(chan struct{})}
	h := hub.NewHub()
	e := &Executor{Workspace: t.TempDir(), Tasks: g, Hub: h, Policy: Policy{Yolo: true}}

	id := decodeResult(t, callTask(t, e,
		`{"tasks":[{"title":"slow","prompt":"very slow","background":true}]}`),
	)["background"].([]any)[0].(string)

	// A wait that expires is not a failure: the run is alive and worth waiting
	// on again. timeout is in seconds and the floor is 1.
	start := time.Now()
	out := callAgentOutput(t, e, `{"id":"`+id+`","action":"wait","timeout":1}`)
	if out.Failed {
		t.Fatalf("an expired wait failed the call: %s", out.Content)
	}
	payload := decodeResult(t, out)
	if payload["pending"] != true {
		t.Fatalf("payload = %+v, want pending after the timeout", payload)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("the wait ignored its timeout (%v)", time.Since(start))
	}
	close(g.release)
}

func TestAgentOutputRejectsUnknownAndBadAction(t *testing.T) {
	h := hub.NewHub()
	e := &Executor{Workspace: t.TempDir(), Tasks: &gateTasks{}, Hub: h, Policy: Policy{Yolo: true}}
	if out := callAgentOutput(t, e, `{"id":"nope"}`); !out.Failed {
		t.Fatalf("an unknown run id was accepted: %s", out.Content)
	}
	if out := callAgentOutput(t, e, `{"id":"nope","action":"delete"}`); !out.Failed {
		t.Fatalf("an unknown action was accepted: %s", out.Content)
	}
	if out := callAgentOutput(t, e, `{}`); !out.Failed {
		t.Fatal("a missing id was accepted")
	}
}

// ---------- schema ----------

func TestTaskBackgroundInSchema(t *testing.T) {
	defs := (&Executor{Workspace: t.TempDir(), Tasks: &gateTasks{}, Hub: hub.NewHub()}).Definitions()
	for _, d := range defs {
		if d.Function.Name != "task" {
			continue
		}
		items := d.Function.Parameters.Properties["tasks"].Items
		bg, ok := items.Properties["background"]
		if !ok {
			t.Fatal("task item schema has no background property")
		}
		if bg.Type != "boolean" {
			t.Fatalf("background schema = %+v, want boolean", bg)
		}
		for _, req := range items.Required {
			if req == "background" {
				t.Fatal("background must be optional")
			}
		}
		return
	}
	t.Fatal("task tool not registered")
}

// waitForRun polls one hub run until it reaches want or the test gives up.
func waitForRun(t *testing.T, h *hub.Hub, id string, want hub.RunState) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		run, ok := h.Status(id)
		if ok && run.State == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	state := "<gone>"
	if run, ok := h.Status(id); ok {
		state = string(run.State)
	}
	t.Fatalf("run %q never reached %q (state %q)", id, want, state)
}
