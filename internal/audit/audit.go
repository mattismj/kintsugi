// Package audit is the workspace's local security record (M11.3).
//
// The session log keeps the conversation; the audit log keeps what was
// allowed or refused. Every permission decision the executor makes lands
// here: the mode baseline, the rule that decided it, the operator's answer,
// a scope refusal, a sandbox refusal, a hook denial.
//
// Three properties hold, and they are the point of the package:
//
//  1. Local. The file lives in the workspace and never leaves the machine.
//     Nothing uploads it, and no prompt ever carries it.
//  2. Append-only. There is no API that rewrites or removes an event. The
//     only way to empty the log is the operator's explicit command
//     (`kintsugi audit --clear`).
//  3. Redacted. A detail may quote command output, so every string is
//     scrubbed through internal/redact before it touches the disk.
//
// The file is JSON Lines: one event per line, so a truncated write costs one
// event and never the whole record.
package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"kintsugi/internal/redact"
)

// Event is one permission decision.
type Event struct {
	// TS is the decision's timestamp, RFC 3339 with nanoseconds in UTC.
	// An empty TS is filled in when the event is recorded.
	TS string `json:"ts"`
	// SessionID names the session the decision was made in. It is empty
	// for a decision that belongs to the workspace rather than a session.
	SessionID string `json:"session_id,omitempty"`
	// Tool is the tool the decision was about ("edit", "bash", "mode").
	Tool string `json:"tool"`
	// Detail is the argument the decision was about — a path, a command,
	// a one-line preview. It may quote command output, so it is redacted.
	Detail string `json:"detail,omitempty"`
	// Decision is the outcome: allow, ask, deny or switch.
	Decision string `json:"decision"`
	// Reason explains the decision in one human-readable line.
	Reason string `json:"reason,omitempty"`
	// Rule is the rule that decided, formatted as "tool" or "tool(spec)".
	// It is empty when the mode baseline decided.
	Rule string `json:"rule,omitempty"`
}

// Decision values.
const (
	// DecisionAllow: the call was allowed.
	DecisionAllow = "allow"
	// DecisionAsk: the call was put to the operator.
	DecisionAsk = "ask"
	// DecisionDeny: the call was refused.
	DecisionDeny = "deny"
	// DecisionSwitch: the session's permission mode changed.
	DecisionSwitch = "switch"
)

// Rule labels for the floors that are not matrix rules.
const (
	// RuleScope marks a refusal by the declared engagement scope.
	RuleScope = "scope"
	// RuleSandbox marks a refusal because the sandbox could not be enforced.
	RuleSandbox = "sandbox"
	// RuleHook marks a refusal by a lifecycle hook.
	RuleHook = "hook"
	// RuleDenyPaths marks a refusal by the built-in path deny list.
	RuleDenyPaths = "deny_paths"
	// RuleDenyCommands marks a refusal by the configured command deny list.
	RuleDenyCommands = "deny_commands"
)

// KindHookBlock is the session-log kind the agent loop writes when a
// PreToolUse hook denies a tool call. The hook gate lives in the agent loop,
// not in the executor, so this is how a hook denial reaches the audit.
const KindHookBlock = "hook_block"

// dirName is the workspace directory the harness owns.
const dirName = ".kintsugi"

// fileName is the audit log's name inside that directory.
const fileName = "audit.jsonl"

// Path is the audit log's path for a workspace.
func Path(workspace string) string {
	return filepath.Join(workspace, dirName, fileName)
}

// Log is an open audit log. It is append-only: there is no method that
// rewrites or removes an event.
type Log struct {
	path string
	f    *os.File
	mu   sync.Mutex
}

// Open opens (or creates) the workspace's audit log for appending.
//
// The file is 0600: the record is the operator's, not the workspace's. An
// existing file keeps its old mode otherwise, so the mode is set explicitly.
func Open(workspace string) (*Log, error) {
	if strings.TrimSpace(workspace) == "" {
		return nil, fmt.Errorf("audit: no workspace")
	}
	path := Path(workspace)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return nil, fmt.Errorf("audit: %w", err)
	}
	return &Log{path: path, f: f}, nil
}

// Record appends one event to the log.
//
// The timestamp is filled in when the caller left it empty, and the detail
// and the reason are redacted before they reach the disk: a detail may quote
// command output, and command output may carry a credential.
func (l *Log) Record(ev Event) error {
	if l == nil || l.f == nil {
		return fmt.Errorf("audit: the log is not open")
	}
	if strings.TrimSpace(ev.TS) == "" {
		ev.TS = time.Now().UTC().Format(time.RFC3339Nano)
	}
	ev.Detail = redact.Redact(ev.Detail)
	ev.Reason = redact.Redact(ev.Reason)
	line, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// One write per event: the line lands whole or not at all.
	if _, err := l.f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("audit: %s: %w", l.path, err)
	}
	return nil
}

// Close closes the log. The writes are unbuffered, so everything recorded so
// far is already on disk.
func (l *Log) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	err := l.f.Close()
	l.f = nil
	return err
}

// Read returns the workspace's audit events, oldest first.
//
// A missing log is not an error: it simply has no events. A malformed line
// is skipped rather than fatal, so one truncated write never hides the rest
// of the record.
func Read(workspace string) ([]Event, error) {
	data, err := os.ReadFile(Path(workspace))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("audit: %w", err)
	}
	var out []Event
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		out = append(out, ev)
	}
	return out, nil
}

// Clear truncates the audit log. It is the operator's explicit command, never
// a side effect: nothing else in the harness empties the file. A missing log
// is not an error.
func Clear(workspace string) error {
	f, err := os.OpenFile(Path(workspace), os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("audit: %w", err)
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	return nil
}

// Record appends one event to the workspace's audit log, opening and closing
// it for the call.
//
// It is for the rare events that have no long-lived handle — a permission
// mode switch, say. The executor keeps an open log instead, so a tool call
// costs one line and nothing else.
func Record(workspace string, ev Event) error {
	l, err := Open(workspace)
	if err != nil {
		return err
	}
	defer l.Close()
	return l.Record(ev)
}

// Logger is the session-log sink an agent loop writes to. It is the same
// shape as agent.Logger, so any of them fits.
type Logger interface {
	Log(kind string, v any)
}

// WrapLogger returns a logger that writes every event to inner and also
// records the hook denials in the audit log.
//
// The hook gate runs in the agent loop, before the executor, so a PreToolUse
// denial never reaches the executor's own recording. Wrapping the loop's
// logger is where that denial becomes part of the security record.
func WrapLogger(inner Logger, l *Log) Logger {
	return loggerFunc(func(kind string, v any) {
		if inner != nil {
			inner.Log(kind, v)
		}
		if l == nil || kind != KindHookBlock {
			return
		}
		ev := Event{Decision: DecisionDeny, Rule: RuleHook, Detail: "pre-tool-use hook"}
		if m, ok := v.(map[string]any); ok {
			ev.Tool, _ = m["tool"].(string)
			ev.Reason, _ = m["reason"].(string)
		}
		// A failed write is telemetry: the turn must not fail because the
		// audit log could not be written.
		_ = l.Record(ev)
	})
}

// loggerFunc adapts a function to the Logger interface.
type loggerFunc func(kind string, v any)

func (f loggerFunc) Log(kind string, v any) { f(kind, v) }
