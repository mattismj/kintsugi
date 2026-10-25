package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kintsugi/internal/audit"
	"kintsugi/internal/config"
	"kintsugi/internal/provider"
	"kintsugi/internal/tools"
)

// auditWorkspace is a workspace with one file to read.
func auditWorkspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws
}

// seedAudit records a few events in the workspace's audit log.
func seedAudit(t *testing.T, ws string, evs ...audit.Event) {
	t.Helper()
	l, err := audit.Open(ws)
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	defer l.Close()
	for _, ev := range evs {
		if err := l.Record(ev); err != nil {
			t.Fatalf("audit.Record: %v", err)
		}
	}
}

func TestAuditTablePrintsTheLastEvents(t *testing.T) {
	ws := auditWorkspace(t)
	seedAudit(t, ws,
		audit.Event{TS: "2026-01-02T15:04:05Z", Tool: "read", Decision: "allow", Reason: "default mode allows read-only tools"},
		audit.Event{TS: "2026-01-02T15:04:06Z", Tool: "bash", Decision: "deny", Reason: "denied by rule bash(rm)", Rule: "bash(rm)"},
	)

	var out bytes.Buffer
	if err := auditCommand(ws, nil, &out, strings.NewReader("")); err != nil {
		t.Fatalf("auditCommand: %v", err)
	}
	text := out.String()
	for _, want := range []string{"TIME", "TOOL", "DECISION", "REASON", "read", "allow", "bash", "deny", "denied by rule bash(rm)", "rule bash(rm)"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the table is missing %q:\n%s", want, text)
		}
	}
}

func TestAuditTableKeepsOnlyTheLastN(t *testing.T) {
	ws := auditWorkspace(t)
	seedAudit(t, ws,
		audit.Event{TS: "2026-01-02T15:04:05Z", Tool: "old", Decision: "allow"},
		audit.Event{TS: "2026-01-02T15:04:06Z", Tool: "new", Decision: "allow"},
	)

	var out bytes.Buffer
	if err := auditCommand(ws, []string{"-n", "1"}, &out, strings.NewReader("")); err != nil {
		t.Fatalf("auditCommand: %v", err)
	}
	text := out.String()
	if strings.Contains(text, "old") {
		t.Fatalf("the table shows an event past the limit:\n%s", text)
	}
	if !strings.Contains(text, "new") {
		t.Fatalf("the table does not show the newest event:\n%s", text)
	}
}

func TestAuditJSONPrintsTheEvents(t *testing.T) {
	ws := auditWorkspace(t)
	seedAudit(t, ws, audit.Event{
		TS: "2026-01-02T15:04:05Z", SessionID: "sess-1", Tool: "edit",
		Decision: "deny", Reason: "denied by rule edit(secrets/)", Rule: "edit(secrets/)",
	})

	var out bytes.Buffer
	if err := auditCommand(ws, []string{"--json"}, &out, strings.NewReader("")); err != nil {
		t.Fatalf("auditCommand: %v", err)
	}
	var events []audit.Event
	if err := json.Unmarshal(out.Bytes(), &events); err != nil {
		t.Fatalf("the JSON output does not parse: %v\n%s", err, out.String())
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.Tool != "edit" || ev.Decision != "deny" || ev.Rule != "edit(secrets/)" {
		t.Fatalf("event = %+v, want the recorded denial", ev)
	}
	if ev.SessionID != "sess-1" {
		t.Fatalf("session id = %q, want sess-1", ev.SessionID)
	}
}

func TestAuditJSONOnAnEmptyLogIsAnEmptyArray(t *testing.T) {
	var out bytes.Buffer
	if err := auditCommand(t.TempDir(), []string{"--json"}, &out, strings.NewReader("")); err != nil {
		t.Fatalf("auditCommand: %v", err)
	}
	var events []audit.Event
	if err := json.Unmarshal(out.Bytes(), &events); err != nil {
		t.Fatalf("the JSON output does not parse: %v\n%s", err, out.String())
	}
	if len(events) != 0 {
		t.Fatalf("got %d events, want 0", len(events))
	}
}

func TestAuditClearAsksFirst(t *testing.T) {
	ws := auditWorkspace(t)
	seedAudit(t, ws, audit.Event{TS: "2026-01-02T15:04:05Z", Tool: "read", Decision: "allow"})

	// A refusal leaves the record alone.
	var out bytes.Buffer
	if err := auditCommand(ws, []string{"--clear"}, &out, strings.NewReader("n\n")); err != nil {
		t.Fatalf("auditCommand: %v", err)
	}
	if !strings.Contains(out.String(), "not cleared") {
		t.Fatalf("the refusal was not reported:\n%s", out.String())
	}
	events, err := audit.Read(ws)
	if err != nil {
		t.Fatalf("audit.Read: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("the log holds %d events after the refusal, want 1", len(events))
	}

	// A confirmation empties it.
	out.Reset()
	if err := auditCommand(ws, []string{"--clear"}, &out, strings.NewReader("y\n")); err != nil {
		t.Fatalf("auditCommand: %v", err)
	}
	if !strings.Contains(out.String(), "cleared") {
		t.Fatalf("the clear was not reported:\n%s", out.String())
	}
	events, err = audit.Read(ws)
	if err != nil {
		t.Fatalf("audit.Read: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("the log holds %d events after the clear, want 0", len(events))
	}
}

// TestExecutorRecordsADeniedCallWithTheRule is the wiring test: a denied
// tool call leaves one event in the audit log, and the event names the rule
// that decided it.
func TestExecutorRecordsADeniedCallWithTheRule(t *testing.T) {
	ws := auditWorkspace(t)
	l, err := audit.Open(ws)
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	defer l.Close()

	ex := &tools.Executor{
		Workspace: ws,
		Policy: tools.Policy{Rules: []config.PermRule{
			{Tool: "bash", Spec: "rm ", Action: "deny"},
		}},
		Audit:     l,
		SessionID: "sess-1",
	}

	res := ex.Dispatch(provider.ToolCall{
		Type:     "function",
		Function: provider.Function{Name: "bash", Arguments: `{"command":"rm -rf notes"}`},
	})
	if !res.Failed {
		t.Fatalf("the denied command ran: %+v", res)
	}

	events, err := audit.Read(ws)
	if err != nil {
		t.Fatalf("audit.Read: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("the audit log holds %d events, want 1: %+v", len(events), events)
	}
	ev := events[0]
	if ev.Decision != "deny" {
		t.Fatalf("decision = %q, want deny", ev.Decision)
	}
	if ev.Rule != "bash(rm)" {
		t.Fatalf("rule = %q, want the rule that decided", ev.Rule)
	}
	if !strings.Contains(ev.Reason, "denied by rule") {
		t.Fatalf("reason = %q, want the rule's reason", ev.Reason)
	}
	if ev.Tool != "bash" {
		t.Fatalf("tool = %q, want bash", ev.Tool)
	}
	if ev.SessionID != "sess-1" {
		t.Fatalf("session id = %q, want sess-1", ev.SessionID)
	}
	if !strings.Contains(ev.Detail, "rm -rf notes") {
		t.Fatalf("detail = %q, want the command", ev.Detail)
	}
}

// TestExecutorRecordsAnAllowedCall: a read the mode allows leaves one event
// saying so.
func TestExecutorRecordsAnAllowedCall(t *testing.T) {
	ws := auditWorkspace(t)
	l, err := audit.Open(ws)
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	defer l.Close()

	ex := &tools.Executor{Workspace: ws, Policy: tools.Policy{}, Audit: l, SessionID: "sess-1"}
	res := ex.Dispatch(provider.ToolCall{
		Type:     "function",
		Function: provider.Function{Name: "read", Arguments: `{"path":"README.md"}`},
	})
	if res.Failed {
		t.Fatalf("the read failed: %+v", res)
	}
	events, err := audit.Read(ws)
	if err != nil {
		t.Fatalf("audit.Read: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("the audit log holds %d events, want 1: %+v", len(events), events)
	}
	if events[0].Decision != "allow" || events[0].Tool != "read" {
		t.Fatalf("event = %+v, want the allowed read", events[0])
	}
}

// TestExecutorRecordsTheOperatorsRefusal: a call the operator refused leaves
// one event, and the reason says the operator refused it.
func TestExecutorRecordsTheOperatorsRefusal(t *testing.T) {
	ws := auditWorkspace(t)
	l, err := audit.Open(ws)
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	defer l.Close()

	ex := &tools.Executor{
		Workspace: ws,
		Policy:    tools.Policy{},
		Audit:     l,
		SessionID: "sess-1",
		Approver:  tools.ApproverFunc(func(ar tools.ApprovalRequest) bool { return false }),
	}
	res := ex.Dispatch(provider.ToolCall{
		Type:     "function",
		Function: provider.Function{Name: "edit", Arguments: `{"path":"README.md","old":"hello","new":"bye"}`},
	})
	if !res.Failed {
		t.Fatalf("the refused edit ran: %+v", res)
	}
	events, err := audit.Read(ws)
	if err != nil {
		t.Fatalf("audit.Read: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("the audit log holds %d events, want 1: %+v", len(events), events)
	}
	ev := events[0]
	if ev.Decision != "deny" {
		t.Fatalf("decision = %q, want deny", ev.Decision)
	}
	if !strings.Contains(ev.Reason, "denied by the operator") {
		t.Fatalf("reason = %q, want the operator's refusal", ev.Reason)
	}
}
