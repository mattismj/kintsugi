package audit

import (
	"os"
	"strings"
	"testing"
)

func TestEventsAreAppendedInOrder(t *testing.T) {
	ws := t.TempDir()
	l, err := Open(ws)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	for _, tool := range []string{"read", "edit", "bash"} {
		if err := l.Record(Event{Tool: tool, Decision: DecisionAllow}); err != nil {
			t.Fatalf("Record(%s): %v", tool, err)
		}
	}

	events, err := Read(ws)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	for i, want := range []string{"read", "edit", "bash"} {
		if events[i].Tool != want {
			t.Fatalf("event %d is %q, want %q", i, events[i].Tool, want)
		}
		if events[i].TS == "" {
			t.Fatalf("event %d has no timestamp", i)
		}
	}
}

func TestReadRoundTripsEveryField(t *testing.T) {
	ws := t.TempDir()
	l, err := Open(ws)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	want := Event{
		TS:        "2026-01-02T15:04:05.123456789Z",
		SessionID: "20260102-150405-abc",
		Tool:      "bash",
		Detail:    "git status --short",
		Decision:  DecisionDeny,
		Reason:    "denied by rule bash(rm )",
		Rule:      "bash(rm )",
	}
	if err := l.Record(want); err != nil {
		t.Fatalf("Record: %v", err)
	}
	events, err := Read(ws)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	got := events[0]
	if got != want {
		t.Fatalf("event = %+v, want %+v", got, want)
	}
}

func TestSecretsAreRedactedOnDisk(t *testing.T) {
	ws := t.TempDir()
	l, err := Open(ws)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	const secret = "sk-abcdef0123456789abcdef"
	if err := l.Record(Event{
		Tool:     "bash",
		Detail:   "curl -H 'Authorization: Bearer " + secret + "' https://example.com",
		Decision: DecisionAllow,
		Reason:   "allowed by rule bash(curl )",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	raw, err := os.ReadFile(Path(ws))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("the secret reached the disk:\n%s", raw)
	}
	if !strings.Contains(string(raw), "[REDACTED]") {
		t.Fatalf("the redaction placeholder is missing:\n%s", raw)
	}
	// The redacted form is what a reader gets back, not just what is stored.
	events, err := Read(ws)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if strings.Contains(events[0].Detail, secret) {
		t.Fatalf("the secret survived the round trip: %q", events[0].Detail)
	}
}

func TestTheLogFileIsPrivate(t *testing.T) {
	ws := t.TempDir()
	l, err := Open(ws)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := l.Record(Event{Tool: "read", Decision: DecisionAllow}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	info, err := os.Stat(Path(ws))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("the audit log mode is %o, want 600", perm)
	}
}

func TestAnExistingLogKeepsItsPrivateMode(t *testing.T) {
	ws := t.TempDir()
	path := Path(ws)
	if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := Open(ws)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("the audit log mode is %o, want 600", perm)
	}
}

func TestClearEmptiesTheLog(t *testing.T) {
	ws := t.TempDir()
	l, err := Open(ws)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := l.Record(Event{Tool: "read", Decision: DecisionAllow}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := Clear(ws); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	events, err := Read(ws)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("the log still holds %d events after Clear", len(events))
	}
	// The file is still there, still private: clearing empties it, it does
	// not remove it.
	info, err := os.Stat(Path(ws))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("the audit log mode is %o, want 600", perm)
	}
}

func TestClearOnAMissingLogIsNotAnError(t *testing.T) {
	if err := Clear(t.TempDir()); err != nil {
		t.Fatalf("Clear: %v", err)
	}
}

func TestReadOnAMissingLogIsNotAnError(t *testing.T) {
	events, err := Read(t.TempDir())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("got %d events, want 0", len(events))
	}
}

func TestRecordAppendsOneEvent(t *testing.T) {
	ws := t.TempDir()
	if err := Record(ws, Event{Tool: "mode", Decision: DecisionSwitch, Reason: "default → plan"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	events, err := Read(ws)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Tool != "mode" || events[0].Decision != DecisionSwitch {
		t.Fatalf("event = %+v, want the mode switch", events[0])
	}
}

func TestAMalformedLineIsSkipped(t *testing.T) {
	ws := t.TempDir()
	l, err := Open(ws)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := l.Record(Event{Tool: "read", Decision: DecisionAllow}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	l.Close()
	// A truncated write: the last line is half an event.
	f, err := os.OpenFile(Path(ws), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"tool\":\"edit\",\"decis"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	events, err := Read(ws)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Tool != "read" {
		t.Fatalf("event = %+v, want the read", events[0])
	}
}

func TestWrapLoggerRecordsHookDenials(t *testing.T) {
	ws := t.TempDir()
	l, err := Open(ws)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	var seen []string
	inner := loggerFunc(func(kind string, v any) {
		seen = append(seen, kind)
	})
	wrapped := WrapLogger(inner, l)
	wrapped.Log("tool_call", map[string]any{"tool": "edit"})
	wrapped.Log(KindHookBlock, map[string]any{
		"event": "PreToolUse", "tool": "write", "reason": "writes are frozen",
	})

	if len(seen) != 2 {
		t.Fatalf("the inner logger saw %d events, want 2", len(seen))
	}
	events, err := Read(ws)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d audit events, want 1", len(events))
	}
	ev := events[0]
	if ev.Tool != "write" {
		t.Fatalf("event tool = %q, want write", ev.Tool)
	}
	if ev.Decision != DecisionDeny {
		t.Fatalf("event decision = %q, want deny", ev.Decision)
	}
	if ev.Rule != RuleHook {
		t.Fatalf("event rule = %q, want %q", ev.Rule, RuleHook)
	}
	if ev.Reason != "writes are frozen" {
		t.Fatalf("event reason = %q, want the hook's reason", ev.Reason)
	}
}

// dirOf is the directory part of a path.
func dirOf(path string) string {
	i := strings.LastIndex(path, string(os.PathSeparator))
	if i < 0 {
		return "."
	}
	return path[:i]
}
