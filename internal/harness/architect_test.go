package harness

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
	"kintsugi/internal/agent"
	"kintsugi/internal/config"
	"kintsugi/internal/persona"
	"kintsugi/internal/prompt"
	"kintsugi/internal/tools"
)

// mockModel is a fake provider: it streams the frames it was given and
// records every request it was sent.
type mockModel struct {
	*httptest.Server
	mu       sync.Mutex
	requests []map[string]any
	respond  func(n int) []string
	label    string
	order    *[]string
}

func newMockModel(t *testing.T, respond func(n int) []string) *mockModel {
	t.Helper()
	m := &mockModel{respond: respond}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		m.mu.Lock()
		m.requests = append(m.requests, req)
		n := len(m.requests)
		m.mu.Unlock()
		if m.order != nil {
			*m.order = append(*m.order, m.label)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for _, frame := range m.respond(n) {
			fmt.Fprintf(w, "data: %s\n\n", frame)
			if fl != nil {
				fl.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	t.Cleanup(m.Close)
	return m
}

// textReply is the frame of one assistant message.
func textReply(text string) []string {
	if text == "" {
		return nil
	}
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{"content": text}}},
	})
	return []string{string(b)}
}

// toolCallReply is the frame of one assistant message that calls a tool.
func toolCallReply(id, name, args string) []string {
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{
			"tool_calls": []any{map[string]any{
				"index": 0, "id": id, "type": "function",
				"function": map[string]any{"name": name, "arguments": args},
			}},
		}}},
	})
	return []string{string(b)}
}

// always answers every request with the same frames.
func always(frames []string) func(int) []string {
	return func(int) []string { return frames }
}

// calls reports how many requests the model received.
func (m *mockModel) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

// messages returns the message array of the n-th request.
func (m *mockModel) messages(t *testing.T, n int) []map[string]any {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.requests) <= n {
		t.Fatalf("model %q got %d requests, want more than %d", m.label, len(m.requests), n)
	}
	raw, _ := m.requests[n]["messages"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		mm, _ := r.(map[string]any)
		out = append(out, mm)
	}
	return out
}

// architectApp builds an App with two mock providers: the architect and the
// coder. The opsec role is bound to the architect model, so the role
// resolution has something to resolve.
func architectApp(t *testing.T, arch, coder *mockModel) *App {
	t.Helper()
	reg, err := adapters.Load()
	if err != nil {
		t.Fatal(err)
	}
	packDir := t.TempDir()
	write(t, filepath.Join(packDir, "_base.md"), "---\nname: _base\n---\nBASE\n")
	pk, err := persona.Load(packDir, "_base")
	if err != nil {
		t.Fatal(err)
	}
	app := &App{Adapters: reg, Workspace: t.TempDir(), Pack: pk}
	app.Cfg.Providers = map[string]config.Provider{
		"arch": {
			BaseURL: arch.URL,
			Wire:    "openai-completions",
			Models: []config.Model{
				{ID: "arch-model", Family: "deepseek", Context: 32000},
				{ID: "alt-model", Family: "deepseek", Context: 32000},
			},
		},
		"coder": {
			BaseURL: coder.URL,
			Wire:    "openai-completions",
			Models:  []config.Model{{ID: "coder-model", Family: "qwen", Context: 32000}},
		},
	}
	app.Cfg.Core.DefaultModel = "coder/coder-model"
	app.Cfg.Roles.Opsec = "arch/arch-model"
	return app
}

// coderLoop builds the loop the edit phase runs on, plus the coder's resolved
// model and compiled stack.
func coderLoop(t *testing.T, app *App) (*agent.Loop, *Resolved, *prompt.Compiled) {
	t.Helper()
	coder, err := app.Resolve("coder/coder-model")
	if err != nil {
		t.Fatal(err)
	}
	comp, err := app.Compile(coder, CompileOpts{})
	if err != nil {
		t.Fatal(err)
	}
	loop := &agent.Loop{
		Client:   coder.Client,
		Tools:    &tools.Executor{Workspace: app.Workspace, Policy: tools.Policy{Yolo: true}},
		MaxLoops: 2,
	}
	return loop, coder, comp
}

// findPlanMessage returns the message that carries the architect's plan and
// its index, or (-1) when there is none.
func findPlanMessage(msgs []map[string]any) (map[string]any, int) {
	for i, m := range msgs {
		if strings.Contains(fmt.Sprint(m["content"]), "## Approved plan (from the architect)") {
			return m, i
		}
	}
	return nil, -1
}

// TestRunArchitectPlansThenEdits proves the split: the architect model is
// called first, the coder second, and the coder's prompt carries the plan the
// architect produced. The plan is a real artifact on disk.
func TestRunArchitectPlansThenEdits(t *testing.T) {
	var order []string
	arch := newMockModel(t, always(textReply("# Add a health check\n\n1. add internal/health.go\n")))
	arch.label = "architect"
	arch.order = &order
	coder := newMockModel(t, always(textReply("wrote the health check")))
	coder.label = "coder"
	coder.order = &order

	app := architectApp(t, arch, coder)
	loop, coderRes, comp := coderLoop(t, app)

	var notes []string
	out, err := app.RunArchitect(context.Background(), ArchitectOptions{
		Task:      "add a health check",
		Coder:     coderRes,
		CoderLoop: loop,
		Compiled:  comp,
		Note:      func(format string, a ...any) { notes = append(notes, fmt.Sprintf(format, a...)) },
	})
	if err != nil {
		t.Fatal(err)
	}

	// The architect is called first, the coder second.
	if len(order) != 2 || order[0] != "architect" || order[1] != "coder" {
		t.Fatalf("call order = %v, want [architect coder]", order)
	}
	if arch.calls() != 1 || coder.calls() != 1 {
		t.Fatalf("architect calls = %d, coder calls = %d, want 1 and 1", arch.calls(), coder.calls())
	}
	if out.ArchitectModel != "arch/arch-model" {
		t.Fatalf("architect model = %q", out.ArchitectModel)
	}
	if out.CoderModel != "coder/coder-model" {
		t.Fatalf("coder model = %q", out.CoderModel)
	}

	// The plan phase runs under the architect's brief and carries the task.
	archMsgs := arch.messages(t, 0)
	if len(archMsgs) < 2 {
		t.Fatalf("architect prompt = %d messages", len(archMsgs))
	}
	last := archMsgs[len(archMsgs)-1]
	if !strings.Contains(fmt.Sprint(last["content"]), "You are the architect") ||
		!strings.Contains(fmt.Sprint(last["content"]), "add a health check") {
		t.Fatalf("architect prompt does not carry the brief and the task: %v", last)
	}

	// The coder's prompt carries the plan.
	coderMsgs := coder.messages(t, 0)
	planMsg, at := findPlanMessage(coderMsgs)
	if at < 0 {
		t.Fatalf("the coder's prompt does not carry the plan:\n%v", coderMsgs)
	}
	if planMsg["role"] != "system" {
		t.Fatalf("the plan is not a system message: %v", planMsg)
	}
	body := fmt.Sprint(planMsg["content"])
	if !strings.HasPrefix(body, "## Approved plan (from the architect)\n") {
		t.Fatalf("plan message = %q", body)
	}
	if !strings.Contains(body, "# Add a health check") {
		t.Fatalf("plan message does not carry the plan:\n%s", body)
	}
	if !strings.Contains(body, out.PlanPath) {
		t.Fatalf("plan message does not reference the artifact %q:\n%s", out.PlanPath, body)
	}
	// The compiled system prompt is the first message and is not rewritten.
	if at == 0 {
		t.Fatal("the plan replaced the compiled system prompt")
	}
	if !strings.HasPrefix(fmt.Sprint(coderMsgs[0]["content"]), comp.Text[:40]) {
		t.Fatal("the compiled system prompt was rewritten")
	}
	if out.Final != "wrote the health check" {
		t.Fatalf("final = %q", out.Final)
	}

	// The plan is an artifact under .kintsugi/plans/.
	want := filepath.Join(app.Workspace, ".kintsugi", "plans", "add-a-health-check.md")
	if out.PlanPath != want {
		t.Fatalf("plan path = %q, want %q", out.PlanPath, want)
	}
	raw, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	if !strings.Contains(string(raw), "# Add a health check") {
		t.Fatalf("plan artifact does not carry the plan:\n%s", raw)
	}

	// The phase boundaries are printed plainly.
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "· architect: arch/arch-model") {
		t.Fatalf("notes do not name the architect:\n%s", joined)
	}
	if !strings.Contains(joined, "· coder: coder/coder-model") {
		t.Fatalf("notes do not name the coder:\n%s", joined)
	}
}

// TestRunArchitectModelOverride proves the explicit architect model wins over
// the opsec role's model.
func TestRunArchitectModelOverride(t *testing.T) {
	arch := newMockModel(t, always(textReply("# Plan\n")))
	arch.label = "architect"
	coder := newMockModel(t, always(textReply("edited")))
	coder.label = "coder"

	app := architectApp(t, arch, coder)
	loop, coderRes, comp := coderLoop(t, app)

	out, err := app.RunArchitect(context.Background(), ArchitectOptions{
		Task:           "do the thing",
		ArchitectModel: "arch/alt-model",
		Coder:          coderRes,
		CoderLoop:      loop,
		Compiled:       comp,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.ArchitectModel != "arch/alt-model" {
		t.Fatalf("architect model = %q, want the override", out.ArchitectModel)
	}
	arch.mu.Lock()
	req := arch.requests[0]
	arch.mu.Unlock()
	if req["model"] != "alt-model" {
		t.Fatalf("the architect ran on model %v, want alt-model", req["model"])
	}
}

// TestRunArchitectWithoutAPlanStillEdits proves an empty plan is not a
// failure: the edit phase runs with an explicit note and nothing is invented.
func TestRunArchitectWithoutAPlanStillEdits(t *testing.T) {
	// A plan that is nothing but whitespace is nothing usable.
	arch := newMockModel(t, always(textReply("   \n")))
	arch.label = "architect"
	coder := newMockModel(t, always(textReply("edited without a plan")))
	coder.label = "coder"

	app := architectApp(t, arch, coder)
	loop, coderRes, comp := coderLoop(t, app)

	out, err := app.RunArchitect(context.Background(), ArchitectOptions{
		Task:      "do the thing",
		Coder:     coderRes,
		CoderLoop: loop,
		Compiled:  comp,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Plan != "" {
		t.Fatalf("plan = %q, want empty", out.Plan)
	}
	if out.PlanNote == "" || !strings.Contains(out.PlanNote, "no plan produced") {
		t.Fatalf("plan note = %q, want the explicit no-plan note", out.PlanNote)
	}
	if coder.calls() != 1 {
		t.Fatalf("the edit phase ran %d times, want 1", coder.calls())
	}
	coderMsgs := coder.messages(t, 0)
	planMsg, at := findPlanMessage(coderMsgs)
	if at < 0 {
		t.Fatalf("the coder was not told there is no plan:\n%v", coderMsgs)
	}
	if body := fmt.Sprint(planMsg["content"]); !strings.Contains(body, "no plan produced") {
		t.Fatalf("the coder was not told there is no plan:\n%s", body)
	}
	if out.PlanPath != "" {
		t.Fatalf("an empty plan wrote the artifact %q", out.PlanPath)
	}
	if _, err := os.Stat(filepath.Join(app.Workspace, ".kintsugi", "plans")); err == nil {
		entries, _ := os.ReadDir(filepath.Join(app.Workspace, ".kintsugi", "plans"))
		if len(entries) != 0 {
			t.Fatalf("an empty plan left %d artifacts behind", len(entries))
		}
	}
}

// TestArchitectPhaseCannotWrite proves the plan phase is read-only: a write
// the architect asks for is refused and the workspace does not change.
func TestArchitectPhaseCannotWrite(t *testing.T) {
	arch := newMockModel(t, func(n int) []string {
		if n == 1 {
			return toolCallReply("call-1", "write", `{"path":"hacked.txt","content":"nope"}`)
		}
		return textReply("# Plan\n")
	})
	arch.label = "architect"
	coder := newMockModel(t, always(textReply("edited")))
	coder.label = "coder"

	app := architectApp(t, arch, coder)
	loop, coderRes, comp := coderLoop(t, app)

	if _, err := app.RunArchitect(context.Background(), ArchitectOptions{
		Task:      "do the thing",
		Coder:     coderRes,
		CoderLoop: loop,
		Compiled:  comp,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(app.Workspace, "hacked.txt")); !os.IsNotExist(err) {
		t.Fatal("the architect wrote to the workspace")
	}
	// The refusal is reported back to the architect.
	msgs := arch.messages(t, 1)
	found := false
	for _, m := range msgs {
		if fmt.Sprint(m["role"]) == "tool" && strings.Contains(fmt.Sprint(m["content"]), "read-only") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the architect was not told the write was refused:\n%v", msgs)
	}
}

// TestRunArchitectWithAFailingPlannerStillEdits proves a planner that cannot
// answer is not a failed run: the edit phase runs with the explicit note.
func TestRunArchitectWithAFailingPlannerStillEdits(t *testing.T) {
	// The architect's provider answers with a server error.
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(failing.Close)
	arch := newMockModel(t, always(textReply("unused")))
	arch.label = "architect"
	coder := newMockModel(t, always(textReply("edited without a plan")))
	coder.label = "coder"

	app := architectApp(t, arch, coder)
	app.Cfg.Providers["arch"] = config.Provider{
		BaseURL: failing.URL,
		Wire:    "openai-completions",
		Models:  []config.Model{{ID: "arch-model", Family: "deepseek", Context: 32000}},
	}

	loop, coderRes, comp := coderLoop(t, app)

	out, err := app.RunArchitect(context.Background(), ArchitectOptions{
		Task:      "do the thing",
		Coder:     coderRes,
		CoderLoop: loop,
		Compiled:  comp,
	})
	if err != nil {
		t.Fatalf("a failing planner failed the run: %v", err)
	}
	if out.Plan != "" {
		t.Fatalf("plan = %q, want empty", out.Plan)
	}
	if out.PlanNote == "" || !strings.Contains(out.PlanNote, "no plan produced") {
		t.Fatalf("plan note = %q, want the explicit no-plan note", out.PlanNote)
	}
	if coder.calls() != 1 {
		t.Fatalf("the edit phase ran %d times, want 1", coder.calls())
	}
}

// TestCompactSettingsReadsTheHarnessKeys proves the two harness keys are
// honoured and that the documented defaults apply when they are absent.
func TestCompactSettingsReadsTheHarnessKeys(t *testing.T) {
	app := &App{}
	opts := app.CompactSettings()
	if opts.After != agent.DefaultCompactAfter || opts.KeepRatio != agent.DefaultCompactKeepRatio {
		t.Fatalf("defaults = %+v, want %d / %v", opts, agent.DefaultCompactAfter, agent.DefaultCompactKeepRatio)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "kintsugi.toml")
	write(t, path, "[harness]\ncompact_after = 7\ncompact_keep_ratio = 0.25\n")
	app.ConfigPaths = []string{path}
	opts = app.CompactSettings()
	if opts.After != 7 || opts.KeepRatio != 0.25 {
		t.Fatalf("settings = %+v, want 7 / 0.25", opts)
	}

	// A later file wins, exactly like the configuration loader.
	second := filepath.Join(dir, "second.toml")
	write(t, second, "[harness]\ncompact_after = 11\n")
	app.ConfigPaths = []string{path, second}
	opts = app.CompactSettings()
	if opts.After != 11 || opts.KeepRatio != 0.25 {
		t.Fatalf("settings = %+v, want 11 / 0.25", opts)
	}
}
