package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kintsugi/internal/config"
)

// recorder collects the runner's log records.
type recorder struct {
	mu      sync.Mutex
	records []map[string]any
	kinds   []string
}

func (r *recorder) Log(kind string, v any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.kinds = append(r.kinds, kind)
	m, _ := v.(map[string]any)
	if m == nil {
		m = map[string]any{"value": fmt.Sprint(v)}
	}
	r.records = append(r.records, m)
}

func (r *recorder) all() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]map[string]any, len(r.records))
	copy(out, r.records)
	return out
}

// hasKind reports whether a record of the given kind was logged.
func (r *recorder) hasKind(kind string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, k := range r.kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// jsonRule is a rule that prints one JSON decision on stdout.
func jsonRule(decision string) config.HookRule {
	return config.HookRule{
		Command: "sh",
		Args:    []string{"-c", `printf '%s' '{"decision":"` + decision + `"}'`},
	}
}

func TestDenyOnExitTwo(t *testing.T) {
	rec := &recorder{}
	r := New(map[string][]config.HookRule{
		PreToolUse: {{
			Matcher: "bash",
			Command: "sh",
			Args:    []string{"-c", `echo "rm -rf is forbidden" >&2; exit 2`},
		}},
	}, rec.Log)

	dec, err := r.Fire(context.Background(), Event{
		Name:      PreToolUse,
		ToolName:  "bash",
		ToolInput: `{"command":"rm -rf /"}`,
	})
	if err != nil {
		t.Fatalf("exit 2 is a decision, not an error: %v", err)
	}
	if dec.Decision != "deny" {
		t.Fatalf("decision = %q, want deny", dec.Decision)
	}
	if dec.Reason != "rm -rf is forbidden" {
		t.Fatalf("reason = %q, want the stderr text", dec.Reason)
	}
	// The run is logged with the event, the tool and the exit status.
	if len(rec.all()) != 1 {
		t.Fatalf("log records = %d, want 1", len(rec.all()))
	}
	got := rec.all()[0]
	if got["event"] != PreToolUse || got["tool"] != "bash" {
		t.Fatalf("log record = %+v", got)
	}
	if got["exit"] != 2 {
		t.Fatalf("log exit = %v, want 2", got["exit"])
	}
	if got["decision"] != "deny" {
		t.Fatalf("log decision = %v, want deny", got["decision"])
	}
}

func TestJSONDecisionIsParsed(t *testing.T) {
	r := New(map[string][]config.HookRule{
		UserPromptSubmit: {{
			Command: "sh",
			Args:    []string{"-c", `printf '%s' '{"decision":"ask","reason":"confirm the scope","additionalContext":"the repo has two services","systemMessage":"ask first","continue":false}'`},
		}},
	}, nil)

	dec, err := r.Fire(context.Background(), Event{Name: UserPromptSubmit, ToolInput: "do it"})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Decision != "ask" {
		t.Fatalf("decision = %q, want ask", dec.Decision)
	}
	if dec.Reason != "confirm the scope" {
		t.Fatalf("reason = %q", dec.Reason)
	}
	if dec.AdditionalContext != "the repo has two services" {
		t.Fatalf("additionalContext = %q", dec.AdditionalContext)
	}
	if dec.SystemMessage != "ask first" {
		t.Fatalf("systemMessage = %q", dec.SystemMessage)
	}
	if dec.Continue == nil || *dec.Continue {
		t.Fatalf("continue = %v, want false", dec.Continue)
	}
}

func TestEmptyStdoutIsNoDecision(t *testing.T) {
	r := New(map[string][]config.HookRule{
		Stop: {{Command: "sh", Args: []string{"-c", "exit 0"}}},
	}, nil)

	dec, err := r.Fire(context.Background(), Event{Name: Stop})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Decision != "" || dec.Reason != "" || dec.AdditionalContext != "" {
		t.Fatalf("decision = %+v, want the zero decision", dec)
	}
}

func TestMatcherMatching(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")

	newRunner := func(event, matcher string) *Runner {
		return New(map[string][]config.HookRule{
			event: {{
				Matcher: matcher,
				Command: "sh",
				Args:    []string{"-c", "touch " + marker},
			}},
		}, nil)
	}
	ran := func() bool {
		_, err := os.Stat(marker)
		return err == nil
	}
	reset := func() { _ = os.Remove(marker) }

	cases := []struct {
		name    string
		matcher string
		event   string
		tool    string
		want    bool
	}{
		{"exact tool name", "bash", PreToolUse, "bash", true},
		{"case insensitive", "BASH", PreToolUse, "bash", true},
		{"wildcard", "mcp__*", PreToolUse, "mcp__srv__call", true},
		{"wildcard mid", "mcp__*__call", PreToolUse, "mcp__srv__call", true},
		{"other tool", "bash", PreToolUse, "read", false},
		{"wildcard misses", "mcp__*", PreToolUse, "read", false},
		{"empty matcher matches all", "", PreToolUse, "read", true},
		{"star matcher matches all", "*", PreToolUse, "read", true},
		{"non-tool event ignores the matcher", "bash", UserPromptSubmit, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reset()
			_, err := newRunner(tc.event, tc.matcher).Fire(context.Background(), Event{
				Name:     tc.event,
				ToolName: tc.tool,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := ran(); got != tc.want {
				t.Fatalf("hook ran = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTimeoutCancelsTheHook(t *testing.T) {
	r := New(map[string][]config.HookRule{
		PreToolUse: {{
			Matcher: "bash",
			Command: "sh",
			Args:    []string{"-c", "exec sleep 30"},
			Timeout: 1,
		}},
	}, nil)

	start := time.Now()
	dec, err := r.Fire(context.Background(), Event{Name: PreToolUse, ToolName: "bash"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a timed out hook must report an error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v, want a timeout", err)
	}
	if dec.Decision != "" {
		t.Fatalf("decision = %q, want none (a timeout is not a decision)", dec.Decision)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("the hook ran for %s, the timeout did not cancel it", elapsed)
	}
}

func TestAsyncReturnsImmediatelyWithoutDecision(t *testing.T) {
	r := New(map[string][]config.HookRule{
		PostToolUse: {{
			Matcher: "*",
			Command: "sh",
			Args:    []string{"-c", "exec sleep 30"},
			Async:   true,
		}},
	}, nil)

	start := time.Now()
	dec, err := r.Fire(context.Background(), Event{Name: PostToolUse, ToolName: "read"})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatal(err)
	}
	if dec.Decision != "" || dec.AdditionalContext != "" {
		t.Fatalf("decision = %+v, want none", dec)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("an async hook blocked the caller for %s", elapsed)
	}
}

func TestOnFailureBlockTurnsACrashIntoABlock(t *testing.T) {
	rule := config.HookRule{
		Matcher: "bash",
		Command: "sh",
		Args:    []string{"-c", `echo "the auditor is down" >&2; exit 3`},
	}

	// Without on_failure = "block" the crash is a non-blocking error.
	r := New(map[string][]config.HookRule{PreToolUse: {rule}}, nil)
	dec, err := r.Fire(context.Background(), Event{Name: PreToolUse, ToolName: "bash"})
	if err == nil {
		t.Fatal("a crashed hook must report an error")
	}
	if dec.Decision != "" {
		t.Fatalf("decision = %q, want none", dec.Decision)
	}

	// With on_failure = "block" the crash denies the call.
	rule.OnFailure = "block"
	r = New(map[string][]config.HookRule{PreToolUse: {rule}}, nil)
	dec, err = r.Fire(context.Background(), Event{Name: PreToolUse, ToolName: "bash"})
	if err == nil {
		t.Fatal("a crashed hook must still report an error")
	}
	if dec.Decision != "deny" {
		t.Fatalf("decision = %q, want deny", dec.Decision)
	}
	if dec.Reason != "the auditor is down" {
		t.Fatalf("reason = %q, want the stderr text", dec.Reason)
	}
}

func TestMergeDecisions(t *testing.T) {
	ctxHook := func(text string) config.HookRule {
		return config.HookRule{
			Command: "sh",
			Args:    []string{"-c", `printf '%s' '{"decision":"allow","additionalContext":"` + text + `"}'`},
		}
	}

	t.Run("deny wins over allow", func(t *testing.T) {
		r := New(map[string][]config.HookRule{
			PreToolUse: {
				jsonRule("allow"),
				{Command: "sh", Args: []string{"-c", `printf '%s' '{"decision":"deny","reason":"first deny"}'`}},
				{Command: "sh", Args: []string{"-c", `printf '%s' '{"decision":"deny","reason":"second deny"}'`}},
			},
		}, nil)
		dec, err := r.Fire(context.Background(), Event{Name: PreToolUse, ToolName: "bash"})
		if err != nil {
			t.Fatal(err)
		}
		if dec.Decision != "deny" {
			t.Fatalf("decision = %q, want deny", dec.Decision)
		}
		if dec.Reason != "first deny" {
			t.Fatalf("reason = %q, want the first deny reason", dec.Reason)
		}
	})

	t.Run("ask beats allow", func(t *testing.T) {
		r := New(map[string][]config.HookRule{
			PreToolUse: {
				jsonRule("allow"),
				jsonRule("ask"),
				jsonRule("allow"),
			},
		}, nil)
		dec, err := r.Fire(context.Background(), Event{Name: PreToolUse, ToolName: "bash"})
		if err != nil {
			t.Fatal(err)
		}
		if dec.Decision != "ask" {
			t.Fatalf("decision = %q, want ask", dec.Decision)
		}
	})

	t.Run("additional context concatenates", func(t *testing.T) {
		r := New(map[string][]config.HookRule{
			PostToolUse: {
				ctxHook("first"),
				ctxHook("second"),
				ctxHook("third"),
			},
		}, nil)
		dec, err := r.Fire(context.Background(), Event{Name: PostToolUse, ToolName: "read"})
		if err != nil {
			t.Fatal(err)
		}
		got := dec.AdditionalContext
		for _, want := range []string{"first", "second", "third"} {
			if !strings.Contains(got, want) {
				t.Fatalf("additional context %q is missing %q", got, want)
			}
		}
		if !strings.Contains(got, "first\nsecond\nthird") {
			t.Fatalf("additional context = %q, want the three contexts in order", got)
		}
	})
}

// TestLogNeverCarriesThePayload proves the runner never writes the payload it
// hands to a hook into its own log: a tool argument can hold a secret.
func TestLogNeverCarriesThePayload(t *testing.T) {
	const secret = "sk-SUPER-SECRET-TOKEN"
	rec := &recorder{}
	r := New(map[string][]config.HookRule{
		PreToolUse: {{
			Matcher: "bash",
			Command: "sh",
			Args:    []string{"-c", `printf '%s' '{"decision":"allow","additionalContext":"fine"}'`},
		}},
	}, rec.Log)

	_, err := r.Fire(context.Background(), Event{
		Name:      PreToolUse,
		ToolName:  "bash",
		ToolInput: `{"command":"curl -H 'Authorization: Bearer ` + secret + `'"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.all()) != 1 {
		t.Fatalf("log records = %d, want 1", len(rec.all()))
	}
	raw, err := json.Marshal(rec.all()[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("the log record carries the hook payload: %s", raw)
	}
	got := rec.all()[0]
	if got["event"] != PreToolUse {
		t.Fatalf("log event = %v", got["event"])
	}
	if got["tool"] != "bash" {
		t.Fatalf("log tool = %v", got["tool"])
	}
}

// TestPayloadOnStdin proves the hook receives the event as one JSON object.
func TestPayloadOnStdin(t *testing.T) {
	dir := t.TempDir()
	r := New(map[string][]config.HookRule{
		PreToolUse: {{
			Matcher: "bash",
			Command: "sh",
			Args:    []string{"-c", "cat > payload.json"},
		}},
	}, nil)
	r.Dir = dir

	_, err := r.Fire(context.Background(), Event{
		Name:      PreToolUse,
		SessionID: "S-1",
		CWD:       "/tmp/ws",
		ToolName:  "bash",
		ToolInput: `{"command":"ls -la"}`,
		Model:     "deepseek/deepseek-chat",
		Persona:   "coder",
		Extra:     map[string]any{"tool_use_id": "call-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "payload.json"))
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("payload is not one JSON object: %v (%s)", err, raw)
	}
	if payload["event"] != PreToolUse {
		t.Fatalf("event = %v", payload["event"])
	}
	if payload["session_id"] != "S-1" {
		t.Fatalf("session_id = %v", payload["session_id"])
	}
	if payload["cwd"] != "/tmp/ws" {
		t.Fatalf("cwd = %v", payload["cwd"])
	}
	if payload["tool_name"] != "bash" {
		t.Fatalf("tool_name = %v", payload["tool_name"])
	}
	if payload["model"] != "deepseek/deepseek-chat" || payload["persona"] != "coder" {
		t.Fatalf("model/persona = %v/%v", payload["model"], payload["persona"])
	}
	if payload["tool_use_id"] != "call-1" {
		t.Fatalf("tool_use_id = %v", payload["tool_use_id"])
	}
	input, ok := payload["tool_input"].(map[string]any)
	if !ok || input["command"] != "ls -la" {
		t.Fatalf("tool_input = %v, want the raw arguments as JSON", payload["tool_input"])
	}
}

// TestArgsRunWithoutAShell proves the argv path does not go through a shell,
// while a command without args does (so pipes work).
func TestArgsRunWithoutAShell(t *testing.T) {
	dir := t.TempDir()

	// argv path: $HOME stays literal.
	r := New(map[string][]config.HookRule{
		SessionStart: {{
			Command: "sh",
			Args:    []string{"-c", `printf '%s' "$1" > args.txt`, "sh", "$HOME"},
		}},
	}, nil)
	r.Dir = dir
	if _, err := r.Fire(context.Background(), Event{Name: SessionStart}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "args.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "$HOME" {
		t.Fatalf("argv path expanded the variable: %q", raw)
	}

	// command path: the shell expands it.
	r = New(map[string][]config.HookRule{
		SessionStart: {{Command: `printf '%s' "$HOME" > shell.txt`}},
	}, nil)
	r.Dir = dir
	if _, err := r.Fire(context.Background(), Event{Name: SessionStart}); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(filepath.Join(dir, "shell.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) == "$HOME" || string(raw) == "" {
		t.Fatalf("the shell path did not expand the variable: %q", raw)
	}
}

func TestNilRunnerAndUnknownEventAreNoOps(t *testing.T) {
	var nilRunner *Runner
	dec, err := nilRunner.Fire(context.Background(), Event{Name: PreToolUse})
	if err != nil || dec.Decision != "" {
		t.Fatalf("nil runner: %+v / %v", dec, err)
	}
	empty := New(nil, nil)
	dec, err = empty.Fire(context.Background(), Event{Name: PreToolUse})
	if err != nil || dec.Decision != "" {
		t.Fatalf("no rules: %+v / %v", dec, err)
	}
}

func TestFromConfigMapsEveryEvent(t *testing.T) {
	h := config.Hooks{
		PreToolUse:   []config.HookRule{{Matcher: "bash", Command: "true"}},
		Stop:         []config.HookRule{{Command: "true"}},
		SessionStart: []config.HookRule{{Command: "true"}},
	}
	rules := FromConfig(h)
	if len(rules) != 3 {
		t.Fatalf("rules = %d, want 3", len(rules))
	}
	if len(rules[PreToolUse]) != 1 || rules[PreToolUse][0].Matcher != "bash" {
		t.Fatalf("PreToolUse rules = %+v", rules[PreToolUse])
	}
	if len(rules[Stop]) != 1 || len(rules[SessionStart]) != 1 {
		t.Fatalf("rules = %+v", rules)
	}
}

// payloadOf runs a hook that writes its stdin to a file and returns the
// decoded JSON object.
func payloadOf(t *testing.T, rule config.HookRule, ev Event) map[string]any {
	t.Helper()
	dir := t.TempDir()
	rule.Command = "sh"
	rule.Args = []string{"-c", "cat > payload.json"}
	r := New(map[string][]config.HookRule{ev.Name: {rule}}, nil)
	r.Dir = dir
	if _, err := r.Fire(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "payload.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("the hook did not receive one JSON object: %v (%s)", err, raw)
	}
	return out
}

// TestFireModelSwitchCarriesTheFields proves the ModelSwitch event reaches
// the hook with the session, the workspace, the persona and both models.
func TestFireModelSwitchCarriesTheFields(t *testing.T) {
	r := New(map[string][]config.HookRule{
		ModelSwitch: {{Command: "sh", Args: []string{"-c", "cat > payload.json"}}},
	}, nil)
	r.Dir = t.TempDir()

	dec, err := r.FireModelSwitch(context.Background(), ModelSwitchEvent{
		SessionID: "S-9",
		CWD:       "/tmp/ws",
		From:      "deepseek/deepseek-chat",
		To:        "qwen/qwen3",
		Persona:   "coder",
	})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Decision != "" {
		t.Fatalf("decision = %+v, want none", dec)
	}

	payload := payloadOf(t, config.HookRule{}, Event{
		Name:      ModelSwitch,
		SessionID: "S-9",
		CWD:       "/tmp/ws",
		Model:     "qwen/qwen3",
		Persona:   "coder",
		Extra:     map[string]any{"from_model": "deepseek/deepseek-chat", "to_model": "qwen/qwen3"},
	})
	if payload["event"] != ModelSwitch {
		t.Fatalf("event = %v", payload["event"])
	}
	if payload["from_model"] != "deepseek/deepseek-chat" {
		t.Fatalf("from_model = %v", payload["from_model"])
	}
	if payload["to_model"] != "qwen/qwen3" {
		t.Fatalf("to_model = %v", payload["to_model"])
	}
	if payload["model"] != "qwen/qwen3" {
		t.Fatalf("model = %v", payload["model"])
	}
	if payload["session_id"] != "S-9" || payload["cwd"] != "/tmp/ws" || payload["persona"] != "coder" {
		t.Fatalf("payload = %+v", payload)
	}
}

// TestFireModelSwitchCanBeDenied proves the event is a real gate: a hook may
// refuse a model switch, and the caller gets the reason.
func TestFireModelSwitchCanBeDenied(t *testing.T) {
	r := New(map[string][]config.HookRule{
		ModelSwitch: {{
			Command: "sh",
			Args:    []string{"-c", `printf '%s' '{"decision":"deny","reason":"the audit model is offline"}'`},
		}},
	}, nil)

	dec, err := r.FireModelSwitch(context.Background(), ModelSwitchEvent{To: "qwen/qwen3"})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Decision != "deny" || dec.Reason != "the audit model is offline" {
		t.Fatalf("decision = %+v, want the deny", dec)
	}
}

// TestFireNotificationCarriesTheFields proves the Notification event reaches
// the hook with the session, the workspace, the kind and the message.
func TestFireNotificationCarriesTheFields(t *testing.T) {
	r := New(map[string][]config.HookRule{
		Notification: {{Command: "sh", Args: []string{"-c", "cat > payload.json"}}},
	}, nil)
	r.Dir = t.TempDir()

	dec, err := r.FireNotification(context.Background(), NotificationEvent{
		SessionID: "S-4",
		CWD:       "/tmp/ws",
		Kind:      "approval",
		Message:   "the worker wants to run a network command",
		Persona:   "coder",
	})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Decision != "" {
		t.Fatalf("decision = %+v, want none", dec)
	}

	payload := payloadOf(t, config.HookRule{}, Event{
		Name:      Notification,
		SessionID: "S-4",
		CWD:       "/tmp/ws",
		Persona:   "coder",
		Extra:     map[string]any{"kind": "approval", "message": "the worker wants to run a network command"},
	})
	if payload["event"] != Notification {
		t.Fatalf("event = %v", payload["event"])
	}
	if payload["kind"] != "approval" {
		t.Fatalf("kind = %v", payload["kind"])
	}
	if payload["message"] != "the worker wants to run a network command" {
		t.Fatalf("message = %v", payload["message"])
	}
	if payload["session_id"] != "S-4" || payload["cwd"] != "/tmp/ws" {
		t.Fatalf("payload = %+v", payload)
	}
}

// TestEventAPIsAreNoOpsWithoutARunner proves the two new entry points are
// safe on a nil runner: a coordinator without hooks never crashes.
func TestEventAPIsAreNoOpsWithoutARunner(t *testing.T) {
	var nilRunner *Runner
	dec, err := nilRunner.FireModelSwitch(context.Background(), ModelSwitchEvent{To: "qwen/qwen3"})
	if err != nil || dec.Decision != "" {
		t.Fatalf("model switch on a nil runner: %+v / %v", dec, err)
	}
	dec, err = nilRunner.FireNotification(context.Background(), NotificationEvent{Message: "hello"})
	if err != nil || dec.Decision != "" {
		t.Fatalf("notification on a nil runner: %+v / %v", dec, err)
	}
}
