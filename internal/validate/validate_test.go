package validate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"kintsugi/internal/adapters"
	"kintsugi/internal/config"
	"kintsugi/internal/harness"
	"kintsugi/internal/persona"
	"kintsugi/internal/session"
)

// ---------- mock provider ----------

// mockProvider speaks just enough of the openai-completions wire for the
// battery: GET /models for Ping and a streaming chat completion endpoint.
// Each request is handed to chat, which decides the reply from the request
// body (so the three probes can share one server).
type mockProvider struct {
	*httptest.Server
	mu           sync.Mutex
	requests     []map[string]any
	modelsStatus int
	chat         func(req map[string]any) []map[string]any
}

func newMock(t *testing.T, chat func(req map[string]any) []map[string]any) *mockProvider {
	t.Helper()
	m := &mockProvider{chat: chat, modelsStatus: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		st := m.modelsStatus
		m.mu.Unlock()
		if st != http.StatusOK {
			http.Error(w, `{"error":"models unavailable"}`, st)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"mock-1"}]}`)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		m.mu.Lock()
		m.requests = append(m.requests, req)
		m.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for _, ev := range m.chat(req) {
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", b)
			if fl != nil {
				fl.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	})
	m.Server = httptest.NewServer(mux)
	t.Cleanup(m.Close)
	return m
}

func (m *mockProvider) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

func textEvent(s string) map[string]any {
	return map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": s}}}}
}

func toolCallEvent(id, name, args string) map[string]any {
	return map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
		"tool_calls": []any{map[string]any{
			"index": 0, "id": id, "type": "function",
			"function": map[string]any{"name": name, "arguments": args},
		}},
	}}}}
}

func reqMessages(req map[string]any) []map[string]any {
	raw, _ := req["messages"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, m := range raw {
		if mm, ok := m.(map[string]any); ok {
			out = append(out, mm)
		}
	}
	return out
}

// lastUserText returns the text of the final user message (the probe task).
func lastUserText(req map[string]any) string {
	last := ""
	for _, m := range reqMessages(req) {
		if m["role"] == "user" {
			last, _ = m["content"].(string)
		}
	}
	return last
}

func hasToolResult(req map[string]any) bool {
	for _, m := range reqMessages(req) {
		if m["role"] == "tool" {
			return true
		}
	}
	return false
}

func hasReminder(req map[string]any) bool {
	for _, m := range reqMessages(req) {
		if m["role"] != "system" {
			continue
		}
		if s, _ := m["content"].(string); strings.Contains(s, "Harness reminder") {
			return true
		}
	}
	return false
}

// cleanScript answers every probe without a refusal and without a repair.
func cleanScript(req map[string]any) []map[string]any {
	if strings.Contains(lastUserText(req), "first 5 lines") && !hasToolResult(req) {
		return []map[string]any{toolCallEvent("call-echo", "read", `{"path":"ROADMAP.md"}`)}
	}
	return []map[string]any{textEvent("done")}
}

// refusalScript refuses the dual-use probe once, then complies after the
// harness reminder.
func refusalScript(req map[string]any) []map[string]any {
	if strings.Contains(lastUserText(req), "bind-shell") && !hasReminder(req) {
		return []map[string]any{textEvent("I can't "), textEvent("assist with that request.")}
	}
	return cleanScript(req)
}

// repairScript answers the tool probe cleanly and the repair probe with a
// markdown-autolink path, which the repair layer must unwrap.
func repairScript(req map[string]any) []map[string]any {
	task := lastUserText(req)
	switch {
	case strings.Contains(task, "markdown autolink"):
		if !hasToolResult(req) {
			return []map[string]any{toolCallEvent("call-repair", "read", `{"path":"[ROADMAP.md](https://example.invalid/ROADMAP.md)"}`)}
		}
		return []map[string]any{textEvent("read through the autolink")}
	case strings.Contains(task, "first 5 lines"):
		if !hasToolResult(req) {
			return []map[string]any{toolCallEvent("call-echo", "read", `{"path":"ROADMAP.md"}`)}
		}
		return []map[string]any{textEvent("lines reported")}
	}
	return []map[string]any{textEvent("done")}
}

// ---------- app fixture ----------

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// testApp builds an App by hand (no config files, no network): one loopback
// "mock" provider, one persona pack, a workspace with ROADMAP.md, and a cache
// dir scoped to the test so session telemetry is hermetic.
func testApp(t *testing.T, baseURL string) *harness.App {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "ROADMAP.md"),
		"# ROADMAP\n\nWP-0.1 done.\nWP-0.2 in flight.\nWP-0.3 queued.\nWP-0.4 queued.\nWP-0.5 queued.\n")

	packDir := t.TempDir()
	writeFile(t, filepath.Join(packDir, "_base.md"), "---\nname: _base\n---\nYou are a harness engineer.\n")
	pack, err := persona.Load(packDir, "_base")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := adapters.Load()
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	cfg.Core.Mode = "offline" // loopback only: no test can reach the network
	cfg.Context.Enabled = false
	cfg.Behavior.DefaultProfile = ""
	cfg.Intercept.Enabled = true
	cfg.Intercept.MaxRetries = 2
	cfg.Harness.MaxToolLoops = maxLoops
	cfg.Providers = map[string]config.Provider{
		"mock": {
			BaseURL: baseURL + "/v1",
			Family:  "deepseek",
			Wire:    "openai-completions",
			Models:  []config.Model{{ID: "mock-1"}, {ID: "mock-2"}},
		},
	}
	return &harness.App{Cfg: cfg, Pack: pack, Adapters: reg, Workspace: ws}
}

// ---------- Run ----------

func TestRunCleanBatteryIsOK(t *testing.T) {
	srv := newMock(t, cleanScript)
	app := testApp(t, srv.URL)

	targets, notes := Targets(app, "mock", nil)
	if len(notes) != 0 {
		t.Fatalf("unexpected skip notes: %v", notes)
	}
	if len(targets) != 2 {
		t.Fatalf("targets = %d, want 2 (both curated models)", len(targets))
	}

	var logs []string
	rows := Run(context.Background(), app, targets, func(f string, a ...any) {
		logs = append(logs, fmt.Sprintf(f, a...))
	})
	if len(rows) != len(targets) {
		t.Fatalf("rows = %d, want %d", len(rows), len(targets))
	}
	r := rows[0]
	if r.Provider != "mock" || r.Model != "mock-1" {
		t.Fatalf("row identity = %s/%s", r.Provider, r.Model)
	}
	if !r.PingOK || r.PingMS < 0 || r.Err != "" {
		t.Fatalf("ping: ok=%v ms=%d err=%q", r.PingOK, r.PingMS, r.Err)
	}
	if r.StackTokens <= 0 || r.StackHash == "" {
		t.Fatalf("stack not recorded: tokens=%d hash=%q", r.StackTokens, r.StackHash)
	}
	if !r.ToolOK || r.ToolCalls != 1 {
		t.Fatalf("tool echo: ok=%v calls=%d", r.ToolOK, r.ToolCalls)
	}
	if r.Refusals != 0 || r.Repairs != 0 {
		t.Fatalf("refusals=%d repairs=%d, want 0/0", r.Refusals, r.Repairs)
	}
	if r.Verdict != VerdictOK {
		t.Fatalf("verdict = %q, want %q", r.Verdict, VerdictOK)
	}
	if len(logs) == 0 {
		t.Error("progress logger was never called")
	}
}

func TestRunRefusalProbeDetectedAndCounted(t *testing.T) {
	srv := newMock(t, refusalScript)
	app := testApp(t, srv.URL)

	targets, _ := Targets(app, "mock", nil)
	rows := Run(context.Background(), app, targets, nil)
	r := rows[0]

	if r.Refusals != 1 {
		t.Fatalf("refusals = %d, want 1", r.Refusals)
	}
	if r.Verdict != VerdictRefusal {
		t.Fatalf("verdict = %q, want %q", r.Verdict, VerdictRefusal)
	}
	if !r.PingOK || !r.ToolOK {
		t.Fatalf("ping=%v tool=%v, want both true", r.PingOK, r.ToolOK)
	}

	// The probe's session must feed the existing refusal telemetry.
	dir, err := session.Dir()
	if err != nil {
		t.Fatal(err)
	}
	stats, err := session.RefusalStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range stats {
		if s.Model == "mock/mock-1" && s.Class == "moralizing" && s.Outcome == "resumed" && s.Count == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("refusal stats missing the probe hit: %+v", stats)
	}
}

func TestRunRepairBaitCounted(t *testing.T) {
	srv := newMock(t, repairScript)
	app := testApp(t, srv.URL)

	targets, _ := Targets(app, "mock", nil)
	rows := Run(context.Background(), app, targets, nil)
	r := rows[0]

	if r.Repairs != 1 {
		t.Fatalf("repairs = %d, want 1 (autolink path)", r.Repairs)
	}
	if !r.ToolOK || r.ToolCalls != 1 {
		t.Fatalf("tool echo: ok=%v calls=%d", r.ToolOK, r.ToolCalls)
	}
	if r.Verdict != VerdictOK {
		t.Fatalf("verdict = %q, want %q (a repair is telemetry, not a failure)", r.Verdict, VerdictOK)
	}

	dir, err := session.Dir()
	if err != nil {
		t.Fatal(err)
	}
	stats, err := session.RepairStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range stats {
		if s.Model == "mock/mock-1" && s.Tool == "read" && s.Class == "autolink-path" && s.Count == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("repair stats missing the autolink repair: %+v", stats)
	}
}

func TestRunPingFailureDominatesVerdict(t *testing.T) {
	srv := newMock(t, cleanScript)
	srv.modelsStatus = http.StatusInternalServerError
	app := testApp(t, srv.URL)

	targets, _ := Targets(app, "mock", nil)
	rows := Run(context.Background(), app, targets, nil)
	r := rows[0]

	if r.PingOK {
		t.Fatal("ping reported ok against a failing /models endpoint")
	}
	if r.Err == "" || !strings.Contains(r.Err, "500") {
		t.Fatalf("err = %q, want the 500 from ping", r.Err)
	}
	if r.Verdict != VerdictPing {
		t.Fatalf("verdict = %q, want %q", r.Verdict, VerdictPing)
	}
}

func TestRunToolFailureIsToolsVerdict(t *testing.T) {
	// Every chat turn returns prose with no tool call, so the echo probe never
	// produces a tool result.
	srv := newMock(t, func(map[string]any) []map[string]any {
		return []map[string]any{textEvent("I would rather describe the file.")}
	})
	app := testApp(t, srv.URL)

	targets, _ := Targets(app, "mock", nil)
	rows := Run(context.Background(), app, targets, nil)
	r := rows[0]

	if r.ToolOK || r.ToolCalls != 0 {
		t.Fatalf("tool echo: ok=%v calls=%d", r.ToolOK, r.ToolCalls)
	}
	if !r.PingOK {
		t.Fatal("ping should pass; the endpoint answers /models")
	}
	if r.Verdict != VerdictTools {
		t.Fatalf("verdict = %q, want %q", r.Verdict, VerdictTools)
	}
}

func TestClassifyVerdictLadder(t *testing.T) {
	cases := []struct {
		name string
		row  Row
		want string
	}{
		{"ping dead endpoint beats everything", Row{PingOK: false, Refusals: 2, ToolOK: true}, VerdictPing},
		{"refusal after a healthy ping", Row{PingOK: true, Refusals: 1, ToolOK: true}, VerdictRefusal},
		{"no tool result", Row{PingOK: true, ToolCalls: 2}, VerdictTools},
		{"all green", Row{PingOK: true, ToolOK: true}, VerdictOK},
	}
	for _, c := range cases {
		if got := classify(c.row); got != c.want {
			t.Errorf("%s: classify = %q, want %q", c.name, got, c.want)
		}
	}
}

// ---------- Targets ----------

func TestTargetsSelectsCuratedModelsAndExplainsSkips(t *testing.T) {
	t.Setenv("KINTSUGI_VALIDATE_TEST_KEY", "")
	app := testApp(t, "http://127.0.0.1:9") // never contacted by this test
	app.Cfg.Providers = map[string]config.Provider{
		"mock":       {BaseURL: "http://127.0.0.1:9/v1", Family: "deepseek", Wire: "openai-completions", Models: []config.Model{{ID: "mock-1"}}},
		"local":      {BaseURL: "http://127.0.0.1:9/v1", Family: "qwen", Wire: "openai-completions", Models: []config.Model{{ID: "local-1"}}},
		"cline-pass": {BaseURL: "https://api.cline.bot/api/v1", Family: "qwen", Wire: "openai-completions", APIKeyEnv: "KINTSUGI_VALIDATE_TEST_KEY"},
		"keyed": {
			BaseURL: "https://api.example.invalid/v1", Wire: "openai-completions",
			APIKeyEnv: "KINTSUGI_VALIDATE_TEST_KEY", Models: []config.Model{{ID: "keyed-1"}},
		},
	}

	targets, notes := Targets(app, "", nil)
	var got []string
	for _, tg := range targets {
		got = append(got, tg.Provider+"/"+tg.Model.ID)
	}
	// Sorted by provider: local, mock. cline-pass is passthrough, keyed has no key.
	if strings.Join(got, ",") != "local/local-1,mock/mock-1" {
		t.Fatalf("targets = %v", got)
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "cline-pass: no curated models; pass --model") {
		t.Errorf("missing passthrough note:\n%s", joined)
	}
	if !strings.Contains(joined, "keyed: skipped (no KINTSUGI_VALIDATE_TEST_KEY)") {
		t.Errorf("missing key note:\n%s", joined)
	}
	for _, tg := range targets {
		if tg.Client == nil {
			t.Errorf("%s: nil client", tg.Provider)
		}
	}
}

func TestTargetsExplicitModelsWin(t *testing.T) {
	t.Setenv("KINTSUGI_VALIDATE_TEST_KEY", "")
	app := testApp(t, "http://127.0.0.1:9")

	targets, notes := Targets(app, "", []string{"mock/mock-2"})
	if len(notes) != 0 {
		t.Fatalf("notes = %v, want none", notes)
	}
	if len(targets) != 1 || targets[0].Model.ID != "mock-2" {
		t.Fatalf("targets = %+v", targets)
	}

	// A bare model id needs --provider; otherwise it is an explained skip.
	if _, notes := Targets(app, "", []string{"mock-2"}); len(notes) != 1 || !strings.Contains(notes[0], "needs provider/model") {
		t.Fatalf("notes = %v", notes)
	}
	if targets, notes := Targets(app, "mock", []string{"mock-2"}); len(notes) != 0 || len(targets) != 1 || targets[0].Model.ID != "mock-2" {
		t.Fatalf("--provider with a bare model id: %+v / %v", targets, notes)
	}

	// An unknown model on a curated provider is a skip note, not a fatal.
	if _, notes := Targets(app, "", []string{"mock/nope"}); len(notes) != 1 || !strings.Contains(notes[0], "no model") {
		t.Fatalf("notes = %v", notes)
	}
}

func TestTargetsPassthroughProviderRequiresModels(t *testing.T) {
	app := testApp(t, "http://127.0.0.1:9")
	app.Cfg.Providers = map[string]config.Provider{
		"cline-pass": {BaseURL: "http://127.0.0.1:9/v1", Family: "qwen", Wire: "openai-completions"},
	}

	targets, notes := Targets(app, "cline-pass", nil)
	if len(targets) != 0 {
		t.Fatalf("targets = %+v, want none without --model", targets)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "cline-pass: no curated models; pass --model") {
		t.Fatalf("notes = %v", notes)
	}

	targets, notes = Targets(app, "cline-pass", []string{"cline-pass/any-model-id"})
	if len(notes) != 0 {
		t.Fatalf("notes = %v, want none", notes)
	}
	if len(targets) != 1 || targets[0].Provider != "cline-pass" || targets[0].Model.ID != "any-model-id" {
		t.Fatalf("targets = %+v", targets)
	}
}

func TestTargetsUnknownProvider(t *testing.T) {
	app := testApp(t, "http://127.0.0.1:9")
	if targets, notes := Targets(app, "nope", nil); len(targets) != 0 || len(notes) != 1 || !strings.Contains(notes[0], "unknown provider") {
		t.Fatalf("targets=%v notes=%v", targets, notes)
	}
}
