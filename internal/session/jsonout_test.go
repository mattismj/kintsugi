package session

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"kintsugi/internal/provider"
)

// headerEvent is the session header every run starts with.
func headerEvent() Event {
	return Event{
		Type:      EventSession,
		SessionID: "20260101-120000-abc123",
		Model:     "deepseek/deepseek-chat",
		Persona:   "flash",
		Role:      "coder",
		Stack:     "sha256:deadbeef",
		Started:   "2026-01-01T12:00:00Z",
		Mode:      "normal",
	}
}

func TestJSONModeProducesExactlyOneObject(t *testing.T) {
	var buf bytes.Buffer
	em := NewEmitter(&buf, ModeJSON)
	if em.Enabled() != true {
		t.Fatal("json mode must be enabled")
	}
	events := []Event{
		headerEvent(),
		{Type: EventUser, Text: "refactor the auth middleware"},
		{Type: EventAssistant, Text: "I will look at the file first."},
		{Type: EventToolCall, Name: "read", Args: `{"path":"README.md"}`},
		{Type: EventToolResult, Name: "read", Result: "hello", Failed: false},
		{Type: EventRefusal, Class: "moralizing", Level: 1, Outcome: "resumed"},
		{Type: EventRepair, Name: "read", Classes: []string{"alias-default"}},
		{Type: EventDrift, Class: "identity", Outcome: "handoff", IDs: []string{"w-1"}},
		{Type: EventUsage, PromptTokens: 120, CompletionTokens: 30},
		{Type: EventFinal, Text: "done", Ended: "2026-01-01T12:00:09Z"},
	}
	for _, ev := range events {
		if err := em.Emit(ev); err != nil {
			t.Fatalf("emit %s: %v", ev.Type, err)
		}
	}
	if buf.Len() != 0 {
		t.Fatalf("json mode wrote before Close: %q", buf.String())
	}
	if err := em.Close(); err != nil {
		t.Fatal(err)
	}
	// A second Close must not write a second object.
	if err := em.Close(); err != nil {
		t.Fatal(err)
	}

	out := buf.String()
	// Exactly one JSON value, and nothing else.
	dec := json.NewDecoder(strings.NewReader(out))
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("output is not one JSON object: %v\n%s", err, out)
	}
	if dec.More() {
		t.Fatalf("output holds more than one JSON value:\n%s", out)
	}
	wantTop := []string{"schema", "session", "turns", "counters", "usage", "final", "error"}
	for _, k := range wantTop {
		if _, ok := doc[k]; !ok {
			t.Fatalf("document is missing %q: %s", k, out)
		}
	}
	if len(doc) != len(wantTop) {
		t.Fatalf("document has unexpected keys: %s", out)
	}
	if doc["schema"] != SchemaID {
		t.Fatalf("schema = %v", doc["schema"])
	}

	head, _ := doc["session"].(map[string]any)
	wantHead := []string{"id", "model", "persona", "role", "stack", "started", "ended", "mode"}
	for _, k := range wantHead {
		if _, ok := head[k]; !ok {
			t.Fatalf("session header is missing %q: %s", k, out)
		}
	}
	if len(head) != len(wantHead) {
		t.Fatalf("session header has unexpected keys: %s", out)
	}
	if head["id"] != "20260101-120000-abc123" || head["model"] != "deepseek/deepseek-chat" {
		t.Fatalf("session header wrong: %v", head)
	}
	if head["ended"] != "2026-01-01T12:00:09Z" {
		t.Fatalf("session header has no end stamp: %v", head)
	}

	counters, _ := doc["counters"].(map[string]any)
	for _, k := range []string{"refusals", "repairs", "drifts"} {
		if _, ok := counters[k]; !ok {
			t.Fatalf("counters missing %q: %s", k, out)
		}
	}
	if counters["refusals"].(float64) != 1 || counters["repairs"].(float64) != 1 || counters["drifts"].(float64) != 1 {
		t.Fatalf("counters wrong: %v", counters)
	}

	usage, _ := doc["usage"].(map[string]any)
	for _, k := range []string{"requests", "prompt_tokens", "completion_tokens", "total_tokens"} {
		if _, ok := usage[k]; !ok {
			t.Fatalf("usage missing %q: %s", k, out)
		}
	}
	if usage["total_tokens"].(float64) != 150 || usage["requests"].(float64) != 1 {
		t.Fatalf("usage wrong: %v", usage)
	}

	turns, _ := doc["turns"].([]any)
	if len(turns) != 1 {
		t.Fatalf("turns = %d: %s", len(turns), out)
	}
	turn, _ := turns[0].(map[string]any)
	if turn["user"] != "refactor the auth middleware" {
		t.Fatalf("turn user wrong: %v", turn)
	}
	calls, _ := turn["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool calls = %d: %s", len(calls), out)
	}
	call, _ := calls[0].(map[string]any)
	if call["name"] != "read" || call["args"] != `{"path":"README.md"}` || call["result"] != "hello" {
		t.Fatalf("tool call wrong: %v", call)
	}
	if call["failed"].(bool) {
		t.Fatalf("tool call marked failed: %v", call)
	}
	if doc["final"] != "done" {
		t.Fatalf("final = %v", doc["final"])
	}
	if doc["error"] != "" {
		t.Fatalf("error = %v", doc["error"])
	}
}

func TestStreamJSONWritesOneObjectPerLine(t *testing.T) {
	var buf bytes.Buffer
	em := NewEmitter(&buf, ModeStreamJSON)
	events := []Event{
		headerEvent(),
		{Type: EventUser, Text: "do the thing"},
		{Type: EventAssistant, Text: "on it"},
		{Type: EventToolCall, Name: "read", Args: `{"path":"a.md"}`},
		{Type: EventToolResult, Name: "read", Result: "body", Failed: true},
		{Type: EventRefusal, Class: "identity", Level: 2, Outcome: "handoff"},
		{Type: EventRepair, Name: "read", Classes: []string{"null-optional"}, Failed: true, Error: "bad json"},
		{Type: EventDrift, Class: "identity", Outcome: "handoff", IDs: []string{"w-2"}},
		{Type: EventUsage, PromptTokens: 10, CompletionTokens: 5},
		{Type: EventFinal, Text: "finished", Ended: "2026-01-01T12:00:09Z"},
	}
	for _, ev := range events {
		if err := em.Emit(ev); err != nil {
			t.Fatalf("emit %s: %v", ev.Type, err)
		}
	}
	if err := em.Close(); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != len(events) {
		t.Fatalf("stream has %d lines, want %d:\n%s", len(lines), len(events), buf.String())
	}
	types := []string{
		EventSession, EventUser, EventAssistant, EventToolCall, EventToolResult,
		EventRefusal, EventRepair, EventDrift, EventUsage, EventFinal,
	}
	for i, line := range lines {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("line %d is not a JSON object: %v\n%s", i+1, err, line)
		}
		if obj["type"] != types[i] {
			t.Fatalf("line %d type = %v, want %v", i+1, obj["type"], types[i])
		}
	}
	// The last usage line carries the run totals.
	var usage map[string]any
	if err := json.Unmarshal([]byte(lines[8]), &usage); err != nil {
		t.Fatal(err)
	}
	if usage["total_tokens"].(float64) != 15 || usage["requests"].(float64) != 1 {
		t.Fatalf("usage line = %v", usage)
	}
}

func TestStreamJSONCarriesToolCallAndRefusalAndError(t *testing.T) {
	var buf bytes.Buffer
	em := NewEmitter(&buf, ModeStreamJSON)
	if err := em.Emit(headerEvent()); err != nil {
		t.Fatal(err)
	}
	if err := em.Emit(Event{Type: EventUser, Text: "break it"}); err != nil {
		t.Fatal(err)
	}
	if err := em.Emit(Event{Type: EventToolCall, Name: "bash", Args: `{"cmd":"ls"}`}); err != nil {
		t.Fatal(err)
	}
	if err := em.Emit(Event{Type: EventToolResult, Name: "bash", Result: "a\nb", Failed: false}); err != nil {
		t.Fatal(err)
	}
	if err := em.Emit(Event{Type: EventRefusal, Class: "safety-lecture", Level: 1, Outcome: "resumed"}); err != nil {
		t.Fatal(err)
	}
	if err := em.Emit(Event{Type: EventError, Error: "provider refused the request", Ended: "2026-01-01T12:00:09Z"}); err != nil {
		t.Fatal(err)
	}
	if err := em.Close(); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 6 {
		t.Fatalf("lines = %d:\n%s", len(lines), buf.String())
	}
	for i, line := range lines {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("line %d does not parse: %v", i+1, err)
		}
	}
	call := map[string]any{}
	if err := json.Unmarshal([]byte(lines[2]), &call); err != nil {
		t.Fatal(err)
	}
	if call["name"] != "bash" || call["args"] != `{"cmd":"ls"}` {
		t.Fatalf("tool call lost: %v", call)
	}
	res := map[string]any{}
	if err := json.Unmarshal([]byte(lines[3]), &res); err != nil {
		t.Fatal(err)
	}
	if res["result"] != "a\nb" {
		t.Fatalf("tool result lost: %v", res)
	}
	ref := map[string]any{}
	if err := json.Unmarshal([]byte(lines[4]), &ref); err != nil {
		t.Fatal(err)
	}
	if ref["class"] != "safety-lecture" || ref["outcome"] != "resumed" || ref["level"].(float64) != 1 {
		t.Fatalf("refusal lost: %v", ref)
	}
	errLine := map[string]any{}
	if err := json.Unmarshal([]byte(lines[5]), &errLine); err != nil {
		t.Fatal(err)
	}
	if errLine["error"] != "provider refused the request" {
		t.Fatalf("error lost: %v", errLine)
	}
}

func TestJSONModeCarriesErrorAndNoFinal(t *testing.T) {
	var buf bytes.Buffer
	em := NewEmitter(&buf, ModeJSON)
	_ = em.Emit(headerEvent())
	_ = em.Emit(Event{Type: EventUser, Text: "break it"})
	_ = em.Emit(Event{Type: EventToolCall, Name: "bash", Args: `{"cmd":"ls"}`})
	_ = em.Emit(Event{Type: EventToolResult, Name: "bash", Result: "boom", Failed: true})
	_ = em.Emit(Event{Type: EventRefusal, Class: "moralizing", Level: 3, Outcome: "handoff"})
	_ = em.Emit(Event{Type: EventError, Error: "tool loop exceeded 8 iterations"})
	if err := em.Close(); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("not one JSON object: %v\n%s", err, buf.String())
	}
	if doc["error"] != "tool loop exceeded 8 iterations" {
		t.Fatalf("error = %v", doc["error"])
	}
	if doc["final"] != "" {
		t.Fatalf("final = %v", doc["final"])
	}
	counters := doc["counters"].(map[string]any)
	if counters["refusals"].(float64) != 1 {
		t.Fatalf("counters = %v", counters)
	}
	turns := doc["turns"].([]any)
	call := turns[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if call["failed"].(bool) != true || call["result"] != "boom" {
		t.Fatalf("tool call = %v", call)
	}
}

func TestEmitterStripsANSIAndRedactsSecrets(t *testing.T) {
	var buf bytes.Buffer
	em := NewEmitter(&buf, ModeStreamJSON)
	const secret = "sk-abcdefghijklmnopqrstuvwxyz012345"
	_ = em.Emit(Event{Type: EventAssistant, Text: "\x1b[31mred\x1b[0m output"})
	_ = em.Emit(Event{Type: EventToolResult, Name: "bash", Result: "token " + secret + " end"})
	if err := em.Close(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "\x1b") {
		t.Fatalf("ANSI escape reached the stream: %q", out)
	}
	if strings.Contains(out, secret) {
		t.Fatalf("secret reached the stream: %q", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("secret was not redacted: %q", out)
	}
	for i, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("line %d does not parse: %v", i+1, err)
		}
	}
}

// TestJSONRoundTripThroughTheSessionStore runs a session with a tool call, a
// refusal and an error through the store and back into the JSON document: the
// events the harness records are the events the consumer reads.
func TestJSONRoundTripThroughTheSessionStore(t *testing.T) {
	s := newTestSession(t)
	s.Log("meta", map[string]string{"model": "p/m", "persona": "flash"})
	s.Log("user", "do the thing")
	s.Log("assistant", "on it")
	s.Log("tool_call", provider.ToolCall{
		ID: "call-1", Type: "function",
		Function: provider.Function{Name: "read", Arguments: `{"path":"README.md"}`},
	})
	s.Log("tool_result", "file body")
	s.Log("refusal", map[string]any{"class": "moralizing", "level": 1, "outcome": "resumed"})
	s.Log("error", "provider unreachable")
	s.Close()

	entries, err := readEntries(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	em := NewEmitter(&buf, ModeJSON)
	for _, e := range entries {
		switch e.Kind {
		case "meta":
			_ = em.Emit(Event{
				Type:      EventSession,
				SessionID: s.ID,
				Model:     metaString(e.V, "model"),
				Persona:   metaString(e.V, "persona"),
				Started:   e.TS,
				Mode:      "normal",
			})
		case "user", "assistant":
			text, _ := e.V.(string)
			_ = em.Emit(Event{Type: e.Kind, Text: text})
		case "tool_call":
			var tc provider.ToolCall
			if !remarshal(e.V, &tc) {
				t.Fatalf("tool call unreadable: %+v", e.V)
			}
			_ = em.Emit(Event{Type: EventToolCall, Name: tc.Function.Name, Args: tc.Function.Arguments})
		case "tool_result":
			text, _ := e.V.(string)
			_ = em.Emit(Event{Type: EventToolResult, Name: "read", Result: text})
		case "refusal":
			class := metaString(e.V, "class")
			outcome := metaString(e.V, "outcome")
			_ = em.Emit(Event{Type: EventRefusal, Class: class, Level: 1, Outcome: outcome})
		case "error":
			text, _ := e.V.(string)
			_ = em.Emit(Event{Type: EventError, Error: text})
		}
	}
	if err := em.Close(); err != nil {
		t.Fatal(err)
	}

	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("not one JSON object: %v\n%s", err, buf.String())
	}
	if doc["error"] != "provider unreachable" {
		t.Fatalf("error = %v", doc["error"])
	}
	counters := doc["counters"].(map[string]any)
	if counters["refusals"].(float64) != 1 {
		t.Fatalf("counters = %v", counters)
	}
	turns := doc["turns"].([]any)
	turn := turns[0].(map[string]any)
	if turn["user"] != "do the thing" || turn["assistant"] != "on it" {
		t.Fatalf("turn = %v", turn)
	}
	call := turn["tool_calls"].([]any)[0].(map[string]any)
	if call["name"] != "read" || call["args"] != `{"path":"README.md"}` || call["result"] != "file body" {
		t.Fatalf("tool call = %v", call)
	}
}

func TestTextModeWritesNothing(t *testing.T) {
	var buf bytes.Buffer
	em := NewEmitter(&buf, ModeText)
	if em.Enabled() {
		t.Fatal("text mode must be disabled")
	}
	_ = em.Emit(headerEvent())
	_ = em.Emit(Event{Type: EventFinal, Text: "done"})
	if err := em.Close(); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatalf("text mode wrote %q", buf.String())
	}
}

func TestUnknownModeFallsBackToText(t *testing.T) {
	var buf bytes.Buffer
	em := NewEmitter(&buf, "yaml")
	if em.Mode() != ModeText {
		t.Fatalf("mode = %q", em.Mode())
	}
	_ = em.Emit(headerEvent())
	if err := em.Close(); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatalf("unknown mode wrote %q", buf.String())
	}
}

func TestEmitterJSONKeysAreStable(t *testing.T) {
	// The key order of the document is the struct order and must not drift.
	var buf bytes.Buffer
	em := NewEmitter(&buf, ModeJSON)
	_ = em.Emit(headerEvent())
	_ = em.Emit(Event{Type: EventFinal, Text: "x"})
	if err := em.Close(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	want := []string{`"schema"`, `"session"`, `"turns"`, `"counters"`, `"usage"`, `"final"`, `"error"`}
	pos := -1
	for _, k := range want {
		i := strings.Index(out, k)
		if i < 0 {
			t.Fatalf("key %s missing: %s", k, out)
		}
		if i < pos {
			t.Fatalf("key %s out of order: %s", k, out)
		}
		pos = i
	}
}
