package tools

import (
	"strings"
	"sync"
	"testing"
	"time"

	"kintsugi/internal/redact"
)

// streamCollector records streamed lines with their arrival times, so a test
// can prove lines arrive WHILE the tool runs, not only after it returns.
type streamCollector struct {
	mu     sync.Mutex
	events []ToolOutputEvent
	times  []time.Time
}

func (c *streamCollector) on(ev ToolOutputEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
	c.times = append(c.times, time.Now())
}

func (c *streamCollector) lines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.events))
	for i, ev := range c.events {
		out[i] = ev.Line
	}
	return out
}

func (c *streamCollector) firstArrival() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.times) == 0 {
		return time.Time{}
	}
	return c.times[0]
}

// TestBashStreamsLinesWhileRunning pins the streaming contract: the first
// line of a command's output is observed while the command is still running
// (here: visibly before a 600ms sleep completes), and the event carries the
// tool and the command label.
func TestBashStreamsLinesWhileRunning(t *testing.T) {
	ws := testWorkspace(t)
	c := &streamCollector{}
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}, OnOutput: c.on}

	start := time.Now()
	res := dispatch(t, ex, "bash", `{"command":"echo first; sleep 0.6; echo second"}`)
	if res.Failed {
		t.Fatalf("bash failed: %+v", res)
	}
	// The first line must have arrived before the sleep finished — i.e. well
	// before dispatch returned.
	if c.firstArrival().Sub(start) > 400*time.Millisecond {
		t.Fatalf("the first streamed line arrived at %v, want while the command was still running (started %v)", c.firstArrival(), start)
	}
	got := c.lines()
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("streamed lines = %v, want [first second]", got)
	}
	if c.events[0].Tool != "bash" || c.events[0].Label != "echo first; sleep 0.6; echo second" {
		t.Fatalf("the event is mislabelled: %+v", c.events[0])
	}
	// The final result carries the same output: the stream is a view, the
	// result is the record.
	if !strings.Contains(res.Content, "first") || !strings.Contains(res.Content, "second") ||
		!strings.Contains(res.Content, "[exit 0]") {
		t.Fatalf("the final result lost the output:\n%s", res.Content)
	}
}

// TestBashStreamLinesAreRedacted: a secret printed by a command is redacted
// in the STREAM, not only in the final result — a key must never flash
// unredacted in a front end, even transiently.
func TestBashStreamLinesAreRedacted(t *testing.T) {
	ws := testWorkspace(t)
	c := &streamCollector{}
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}, OnOutput: c.on}

	_ = dispatch(t, ex, "bash", `{"command":"cat .env"}`)
	for _, l := range c.lines() {
		if strings.Contains(l, "super-secret-value") {
			t.Fatalf("a secret streamed unredacted: %q", l)
		}
	}
	// The .env pattern redacts to KEY=[REDACTED], so the streamed line still
	// names the key but not the value.
	if len(c.lines()) == 0 || !strings.Contains(c.lines()[0], "[REDACTED]") {
		t.Fatalf("the streamed .env line was not redacted in place: %v", c.lines())
	}
}

// TestBashStreamFailurePath: a failing command streams its output and the
// result still carries the exit code.
func TestBashStreamFailurePath(t *testing.T) {
	ws := testWorkspace(t)
	c := &streamCollector{}
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}, OnOutput: c.on}

	res := dispatch(t, ex, "bash", `{"command":"echo boom >&2; exit 3"}`)
	if !res.Failed {
		t.Fatal("exit 3 reported success")
	}
	if !strings.Contains(res.Content, "boom") || !strings.Contains(res.Content, "[exit 3]") {
		t.Fatalf("the failure result is wrong:\n%s", res.Content)
	}
	if len(c.lines()) != 1 || c.lines()[0] != "boom" {
		t.Fatalf("stderr did not stream: %v", c.lines())
	}
}

// TestBashWithoutOnOutputIsQuiet: the default executor never emits — the
// unstreamed path is byte-identical to the pre-streaming behaviour.
func TestBashWithoutOnOutputIsQuiet(t *testing.T) {
	ws := testWorkspace(t)
	c := &streamCollector{}
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}} // OnOutput nil

	res := dispatch(t, ex, "bash", `{"command":"echo quiet"}`)
	if res.Failed {
		t.Fatalf("bash failed: %+v", res)
	}
	if len(c.events) != 0 {
		t.Fatalf("output streamed without an observer: %v", c.events)
	}
	if !strings.Contains(res.Content, "quiet") {
		t.Fatalf("the result lost the output:\n%s", res.Content)
	}
}

var _ = redact.Placeholder
