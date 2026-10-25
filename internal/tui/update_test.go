package tui

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbletea"

	"kintsugi/internal/agent"
	"kintsugi/internal/audit"
	"kintsugi/internal/config"
	"kintsugi/internal/harness"
	"kintsugi/internal/plan"
	"kintsugi/internal/provider"
	"kintsugi/internal/session"
	"kintsugi/internal/tools"
	"kintsugi/internal/workbench"
)

// wheelMsg is a wheel press — the only mouse event the TUI acts on.
func wheelMsg(btn tea.MouseButton) tea.MouseMsg {
	return tea.MouseMsg{Action: tea.MouseActionPress, Button: btn}
}

// addLines grows the transcript with n single-line notes so there is something
// to scroll.
func addLines(m *Model, n int) {
	for i := 0; i < n; i++ {
		m.addNote(fmt.Sprintf("line %02d", i))
	}
}

func scrollableModel(w, h int) *Model {
	m := testModel(w, h)
	addLines(m, 40)
	return m
}

func TestWheelScrollsTranscriptAndClamps(t *testing.T) {
	m := scrollableModel(80, 24)
	limit := m.maxScroll()
	if limit <= 3 {
		t.Fatalf("test needs a scrollable transcript, maxScroll = %d", limit)
	}

	// At the bottom, wheel down is a no-op and wheel up moves three lines back.
	m.Update(wheelMsg(tea.MouseButtonWheelDown))
	if m.scroll != 0 {
		t.Fatalf("wheel down at the bottom scrolled to %d, want 0", m.scroll)
	}
	m.Update(wheelMsg(tea.MouseButtonWheelUp))
	if m.scroll != 3 {
		t.Fatalf("wheel up = %d, want 3", m.scroll)
	}

	for i := 0; i < 40; i++ {
		m.Update(wheelMsg(tea.MouseButtonWheelUp))
	}
	if m.scroll != limit {
		t.Fatalf("wheel up clamped at %d, want maxScroll %d", m.scroll, limit)
	}

	for i := 0; i < 40; i++ {
		m.Update(wheelMsg(tea.MouseButtonWheelDown))
	}
	if m.scroll != 0 {
		t.Fatalf("wheel down clamped at %d, want 0", m.scroll)
	}
}

func TestMouseIgnoresClicksAndHorizontalWheels(t *testing.T) {
	m := scrollableModel(80, 24)
	m.scroll = 4
	for _, msg := range []tea.MouseMsg{
		{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft},
		{Action: tea.MouseActionPress, Button: tea.MouseButtonRight},
		{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelLeft},
		{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelRight},
		{Action: tea.MouseActionRelease, Button: tea.MouseButtonWheelUp},
		{Action: tea.MouseActionMotion, Button: tea.MouseButtonWheelDown},
	} {
		m.Update(msg)
	}
	if m.scroll != 4 {
		t.Fatalf("a non-wheel mouse event moved the transcript to %d, want 4", m.scroll)
	}
}

func wbModel() *Model {
	m := testModel(120, 30)
	sections := make([]workbench.ContextSection, 0, 40)
	for i := 0; i < 40; i++ {
		sections = append(sections, workbench.ContextSection{
			Title: fmt.Sprintf("Section %02d", i), Class: "state", Tokens: 12, Gate: "auto", Injected: true,
		})
	}
	m.wb = &workbenchView{kind: "context", ctx: &workbench.ContextSnapshot{
		Path: "/ws/CONTEXT.md", Exists: true, Project: "lab", Hash: "sha256:abc123",
		Tokens: 480, Budget: 1200, Sections: sections,
	}}
	return m
}

func TestWheelScrollsWorkbenchAndClamps(t *testing.T) {
	m := wbModel()
	limit := m.wbMaxScroll()
	if limit <= 3 {
		t.Fatalf("test needs a scrollable workbench, max = %d", limit)
	}

	m.Update(wheelMsg(tea.MouseButtonWheelDown))
	if m.wb.scroll != 3 {
		t.Fatalf("workbench wheel down = %d, want 3", m.wb.scroll)
	}
	m.Update(wheelMsg(tea.MouseButtonWheelUp))
	if m.wb.scroll != 0 {
		t.Fatalf("workbench wheel up = %d, want 0", m.wb.scroll)
	}

	for i := 0; i < 40; i++ {
		m.Update(wheelMsg(tea.MouseButtonWheelDown))
	}
	if m.wb.scroll != limit {
		t.Fatalf("workbench wheel down clamped at %d, want %d", m.wb.scroll, limit)
	}
	m.Update(wheelMsg(tea.MouseButtonWheelUp))
	if m.wb.scroll != limit-3 {
		t.Fatalf("workbench wheel up = %d, want %d", m.wb.scroll, limit-3)
	}
	if m.scroll != 0 {
		t.Fatalf("workbench wheel leaked into the transcript scroll: %d", m.scroll)
	}
}

func TestScrollByClampsToBounds(t *testing.T) {
	m := scrollableModel(80, 24)
	limit := m.maxScroll()

	m.scrollBy(1000)
	if m.scroll != limit {
		t.Fatalf("scrollBy(+1000) = %d, want %d", m.scroll, limit)
	}
	m.scrollBy(-1000)
	if m.scroll != 0 {
		t.Fatalf("scrollBy(-1000) = %d, want 0", m.scroll)
	}

	// A transcript that fits on screen cannot scroll at all.
	short := testModel(80, 24)
	short.scrollBy(5)
	if short.scroll != 0 {
		t.Fatalf("short transcript scrolled to %d, want 0", short.scroll)
	}
}

func TestPageKeysClampAndEscReturnsToBottom(t *testing.T) {
	m := scrollableModel(80, 24)
	limit := m.maxScroll()

	m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	if m.scroll != 5 {
		t.Fatalf("PgUp = %d, want 5", m.scroll)
	}
	for i := 0; i < 20; i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	}
	if m.scroll != limit {
		t.Fatalf("PgUp clamped at %d, want %d", m.scroll, limit)
	}

	m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	if m.scroll != limit-5 {
		t.Fatalf("PgDn = %d, want %d", m.scroll, limit-5)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.scroll != 0 {
		t.Fatalf("Esc = %d, want 0", m.scroll)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	if m.scroll != 0 {
		t.Fatalf("PgDn below zero = %d, want 0", m.scroll)
	}
}

func TestArrowKeysStillWalkInputHistory(t *testing.T) {
	m := scrollableModel(80, 24)
	m.history = []string{"first task", "second task"}
	m.histPos = len(m.history)

	m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if m.input != "second task" {
		t.Fatalf("up = %q, want the last task", m.input)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if m.input != "first task" {
		t.Fatalf("up again = %q, want the first task", m.input)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m.input != "second task" {
		t.Fatalf("down = %q, want the last task", m.input)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m.input != "" {
		t.Fatalf("down past the end = %q, want an empty line", m.input)
	}
	if m.scroll != 0 {
		t.Fatalf("arrow keys scrolled the transcript to %d", m.scroll)
	}
}

func TestResizeReclampsScroll(t *testing.T) {
	m := scrollableModel(80, 24)
	m.scroll = m.maxScroll()
	_, cmd := m.Update(tea.WindowSizeMsg{Width: 200, Height: 60})
	if cmd != nil {
		t.Fatal("resize should not schedule work")
	}
	if m.scroll > m.maxScroll() {
		t.Fatalf("scroll %d exceeds the clamp %d after a resize", m.scroll, m.maxScroll())
	}
}

// ---------- plan mode and markdown agents (M9.2 / M9.3) ----------

// workspaceModel builds a model bound to a temporary workspace. The user
// tier is pointed at an empty directory so the test never reads the
// operator's own agent files.
func workspaceModel(t *testing.T) (*Model, string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ws := t.TempDir()
	m := testModel(100, 30)
	m.app = &harness.App{Cfg: config.File{}, Workspace: ws}
	return m, ws
}

// transcriptText is every block the transcript holds, in order.
func transcriptText(m *Model) string {
	var b strings.Builder
	for _, blk := range m.blocks {
		b.WriteString(blk.title + "\n")
		b.WriteString(blk.text + "\n")
	}
	return b.String()
}

// writeAgentFile drops a Markdown-defined agent into the workspace tier.
func writeAgentFile(t *testing.T, ws, name, body string) {
	t.Helper()
	dir := filepath.Join(ws, ".kintsugi", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPlanCommandSaysSoWhenThereAreNoPlans(t *testing.T) {
	m, _ := workspaceModel(t)
	m.handleSlash("/plan")

	out := transcriptText(m)
	if !strings.Contains(out, "no plans") {
		t.Fatalf("/plan with no plans did not say so:\n%s", out)
	}
}

// The plan surface changed shape in the review revamp: /plan now opens the
// newest plan in the review workbench instead of printing a listing card, so
// the plan's name lives in the workbench title. The listing is still reachable
// at /plan list, and the body is still rendered — both are asserted below.
func TestPlanCommandListsTheSavedPlans(t *testing.T) {
	m, ws := workspaceModel(t)
	art, err := plan.Save(ws, "Add OAuth login", "# Add OAuth login\n\nread the config first\n")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	m.handleSlash("/plan list")
	out := transcriptText(m)
	if !strings.Contains(out, art.Name) {
		t.Fatalf("/plan list did not list the plan:\n%s", out)
	}
	if !strings.Contains(out, art.Path) {
		t.Fatalf("/plan list did not show the newest plan's path:\n%s", out)
	}

	// /plan with no argument opens the newest plan for review.
	m.handleSlash("/plan")
	if m.wb == nil || m.wb.kind != "plan" {
		t.Fatalf("/plan did not open the plan workbench: %+v", m.wb)
	}
	if !strings.Contains(m.wb.plan.art.Name, art.Name) {
		t.Fatalf("/plan opened %q, want %q", m.wb.plan.art.Name, art.Name)
	}
}

func TestPlanCommandPrintsThePlanBody(t *testing.T) {
	m, ws := workspaceModel(t)
	if _, err := plan.Save(ws, "Add OAuth login", "# Add OAuth login\n\nread the config first\n"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	m.handleSlash("/plan add-oauth-login")
	if m.wb == nil || m.wb.kind != "plan" {
		t.Fatalf("/plan <name> did not open the workbench: %+v", m.wb)
	}
	body := strings.Join(m.planLines(100), "\n")
	if !strings.Contains(body, "read the config first") {
		t.Fatalf("/plan <name> did not render the body:\n%s", body)
	}
}

func TestPlanCommandReportsAnUnknownPlan(t *testing.T) {
	m, _ := workspaceModel(t)
	m.handleSlash("/plan nothing-here")
	out := transcriptText(m)
	if !strings.Contains(out, "nothing-here") {
		t.Fatalf("/plan <unknown> did not report the name:\n%s", out)
	}
}

func TestAgentsCommandListsMarkdownAgents(t *testing.T) {
	m, ws := workspaceModel(t)
	writeAgentFile(t, ws, "reviewer.md",
		"---\nname: reviewer\ndescription: reviews a diff\nmodel: researcher\ntools: read, list, glob, grep\n---\nYou are a reviewer.\n")

	m.handleSlash("/agents")
	out := transcriptText(m)
	for _, want := range []string{"reviewer", "reviews a diff", "researcher", "read"} {
		if !strings.Contains(out, want) {
			t.Fatalf("/agents does not mention %q:\n%s", want, out)
		}
	}
}

func TestUnknownMentionDoesNotStartATurn(t *testing.T) {
	m, ws := workspaceModel(t)
	writeAgentFile(t, ws, "reviewer.md", "---\nname: reviewer\n---\nbody\n")

	started := false
	m.StartAgent = func(AgentMention) tea.Cmd { started = true; return nil }

	m.input = "@nobody do the thing"
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if started {
		t.Fatal("an unknown agent started a turn")
	}
	if m.busy {
		t.Fatal("an unknown agent left the session busy")
	}
	if len(m.tasks) != 0 {
		t.Fatalf("an unknown agent queued a task: %+v", m.tasks)
	}
	out := transcriptText(m)
	if !strings.Contains(out, "nobody") {
		t.Fatalf("the unknown agent was not reported:\n%s", out)
	}
	if strings.Contains(out, "do the thing") {
		t.Fatalf("the line was consumed by an unknown agent:\n%s", out)
	}
}

func TestKnownMentionResolvesTheAgent(t *testing.T) {
	m, ws := workspaceModel(t)
	writeAgentFile(t, ws, "reviewer.md",
		"---\nname: reviewer\ndescription: reviews a diff\nmodel: researcher\ntools: read, list, glob, grep\n---\nYou are a reviewer.\n")

	var got AgentMention
	started := false
	m.StartAgent = func(am AgentMention) tea.Cmd { got = am; started = true; return nil }

	m.input = "@reviewer look at the auth code"
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if !started {
		t.Fatal("the mention did not start a turn")
	}
	if got.Agent.Name != "reviewer" {
		t.Fatalf("agent = %q, want reviewer", got.Agent.Name)
	}
	if got.Task != "look at the auth code" {
		t.Fatalf("task = %q, want the text after the name", got.Task)
	}
	if got.Role != "researcher" {
		t.Fatalf("role = %q, want researcher", got.Role)
	}
	if got.System != "You are a reviewer." {
		t.Fatalf("system prompt = %q, want the agent body", got.System)
	}
	want := []string{"read", "list", "glob", "grep"}
	if len(got.Tools) != len(want) {
		t.Fatalf("tools = %v, want %v", got.Tools, want)
	}
	for i := range want {
		if got.Tools[i] != want[i] {
			t.Fatalf("tools = %v, want %v", got.Tools, want)
		}
	}
}

func TestMentionWithoutATaskIsNotConsumed(t *testing.T) {
	m, ws := workspaceModel(t)
	writeAgentFile(t, ws, "reviewer.md", "---\nname: reviewer\n---\nbody\n")

	started := false
	m.StartAgent = func(AgentMention) tea.Cmd { started = true; return nil }

	m.input = "@reviewer"
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if started {
		t.Fatal("a mention without a task started a turn")
	}
	if m.busy {
		t.Fatal("a mention without a task left the session busy")
	}
}

func TestPlanModeAgentIsHeldToTheReadOnlyFloor(t *testing.T) {
	m, ws := workspaceModel(t)
	writeAgentFile(t, ws, "planner.md",
		"---\nname: planner\ntools: read, write\npermission_mode: plan\n---\nbody\n")

	am, err := m.resolveMention("@planner draft the plan")
	if err != nil {
		t.Fatalf("resolveMention: %v", err)
	}
	for _, tool := range am.Tools {
		if tool == "write" {
			t.Fatalf("a plan-mode agent kept the write tool: %v", am.Tools)
		}
	}
	if len(am.Tools) != 1 || am.Tools[0] != "read" {
		t.Fatalf("tools = %v, want [read]", am.Tools)
	}
}

// ---------- an @mention turn, end to end ----------

// mentionProvider is a fake OpenAI-compatible endpoint that answers one
// chat completion with the given text.
func mentionProvider(t *testing.T, answer string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := json.Marshal(answer)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%s}}]}\n\n", body)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

// mentionWorkspace writes a workspace config pointing at the fake provider.
func mentionWorkspace(t *testing.T, baseURL string) string {
	t.Helper()
	ws := t.TempDir()
	cfg := fmt.Sprintf(`[core]
mode = "normal"
default_model = "mock/mock-chat"

[providers.mock]
base_url = %q
api_key_env = "MOCK_API_KEY"
family = "qwen"
models = ["mock-chat"]
`, baseURL)
	if err := os.WriteFile(filepath.Join(ws, "kintsugi.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws
}

// ---------- custom commands and context references (M9.8) ----------

// recordingProvider is a fake OpenAI-compatible endpoint that records the
// request it was sent and answers one chat completion.
func recordingProvider(t *testing.T, seen *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*seen = string(body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

// writeCommandFile drops a custom command into the workspace tier.
func writeCommandFile(t *testing.T, ws, name, body string) {
	t.Helper()
	dir := filepath.Join(ws, ".kintsugi", "commands")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// commandWorkspace is a workspace with a custom command and a target file.
func commandWorkspace(t *testing.T, baseURL string) string {
	t.Helper()
	ws := mentionWorkspace(t, baseURL)
	writeCommandFile(t, ws, "review.md",
		"---\ndescription: review a file\nargument-hint: <path>\n---\nReview $1 carefully.\n")
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws
}

// commandModel builds a model bound to a workspace, with a loop that talks to
// the fake provider.
func commandModel(t *testing.T, ws string) (*Model, *agent.Loop) {
	t.Helper()
	app, err := harness.New(ws)
	if err != nil {
		t.Fatalf("harness.New: %v", err)
	}
	res, err := app.Resolve("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	comp, err := app.Compile(res, harness.CompileOpts{})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	log, err := session.New()
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	t.Cleanup(func() { log.Close() })

	m := testModel(100, 30)
	m.app = app
	m.res = res
	m.comp = comp
	m.log = log
	loop := &agent.Loop{Client: res.Client, Tools: &tools.Executor{Workspace: ws}, Log: log, MaxLoops: 4}
	m.loop = loop
	return m, loop
}

// pump runs the turn's messages until the session is idle again.
func pump(m *Model, cmd tea.Cmd) *Model {
	for i := 0; i < 100 && cmd != nil; i++ {
		msg := cmd()
		if msg == nil {
			break
		}
		next, c := m.Update(msg)
		m = next.(*Model)
		cmd = c
		if !m.busy {
			break
		}
	}
	return m
}

func TestCustomSlashCommandExpandsIntoATask(t *testing.T) {
	var seen string
	srv := recordingProvider(t, &seen)
	defer srv.Close()

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("MOCK_API_KEY", "test-key")
	ws := commandWorkspace(t, srv.URL+"/v1")
	m, _ := commandModel(t, ws)

	m.input = "/review main.go"
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(*Model)
	if cmd == nil {
		t.Fatal("the custom command did not start a turn")
	}
	m = pump(m, cmd)

	if !strings.Contains(seen, "Review main.go carefully.") {
		t.Fatalf("the model did not receive the expanded prompt:\n%s", seen)
	}
	out := transcriptText(m)
	if !strings.Contains(out, "/review main.go") {
		t.Fatalf("the transcript does not show the original line:\n%s", out)
	}
	if !strings.Contains(out, "Review main.go carefully.") {
		t.Fatalf("the transcript does not show the expanded prompt:\n%s", out)
	}
	// The input history keeps what the operator typed, not the expansion.
	if len(m.history) != 1 || m.history[0] != "/review main.go" {
		t.Fatalf("history = %v, want the typed line", m.history)
	}
}

func TestContextReferenceInjectsTheFileAndReportsIt(t *testing.T) {
	var seen string
	srv := recordingProvider(t, &seen)
	defer srv.Close()

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("MOCK_API_KEY", "test-key")
	ws := commandWorkspace(t, srv.URL+"/v1")
	m, _ := commandModel(t, ws)

	m.input = "@file main.go explain this"
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(*Model)
	if cmd == nil {
		t.Fatal("the reference did not start a turn")
	}
	m = pump(m, cmd)

	if !strings.Contains(seen, "package main") {
		t.Fatalf("the model did not receive the file contents:\n%s", seen)
	}
	if !strings.Contains(seen, "explain this") {
		t.Fatalf("the task text was lost:\n%s", seen)
	}
	out := transcriptText(m)
	if !strings.Contains(out, "@file main.go") {
		t.Fatalf("the transcript does not show the original line:\n%s", out)
	}
	if !strings.Contains(out, "injected 1 context reference") {
		t.Fatalf("the transcript does not report the injection:\n%s", out)
	}
	if !strings.Contains(out, "main.go") || !strings.Contains(out, "bytes") {
		t.Fatalf("the injection note does not name the path and its size:\n%s", out)
	}
}

func TestUnreadableReferenceStaysLiteralAndTheTurnStillRuns(t *testing.T) {
	var seen string
	srv := recordingProvider(t, &seen)
	defer srv.Close()

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("MOCK_API_KEY", "test-key")
	ws := commandWorkspace(t, srv.URL+"/v1")
	m, _ := commandModel(t, ws)

	m.input = "@file missing.go and @folder nowhere"
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(*Model)
	if cmd == nil {
		t.Fatal("the turn did not start")
	}
	m = pump(m, cmd)

	if !strings.Contains(seen, "@file missing.go") || !strings.Contains(seen, "@folder nowhere") {
		t.Fatalf("an unreadable path did not stay literal:\n%s", seen)
	}
	out := transcriptText(m)
	if !strings.Contains(out, "left literal") {
		t.Fatalf("the transcript does not report the unreadable path:\n%s", out)
	}
}

func TestCommandsCommandListsTheCustomCommands(t *testing.T) {
	m, ws := workspaceModel(t)
	writeCommandFile(t, ws, "review.md",
		"---\ndescription: review a diff\nargument-hint: <path>\n---\nReview $1.\n")

	m.handleSlash("/commands")
	out := transcriptText(m)
	for _, want := range []string{"/review", "review a diff", "<path>"} {
		if !strings.Contains(out, want) {
			t.Fatalf("/commands does not mention %q:\n%s", want, out)
		}
	}
}

func TestUnknownSlashCommandIsStillUnknown(t *testing.T) {
	m, _ := workspaceModel(t)
	m.handleSlash("/nothing-here")
	out := transcriptText(m)
	if !strings.Contains(out, "unknown command") {
		t.Fatalf("an unknown command was not reported:\n%s", out)
	}
	if m.busy {
		t.Fatal("an unknown command started a turn")
	}
}

func TestPlainREPLExpandsTheSameWay(t *testing.T) {
	var seen string
	srv := recordingProvider(t, &seen)
	defer srv.Close()

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("MOCK_API_KEY", "test-key")
	ws := commandWorkspace(t, srv.URL+"/v1")

	app, err := harness.New(ws)
	if err != nil {
		t.Fatalf("harness.New: %v", err)
	}
	res, err := app.Resolve("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	comp, err := app.Compile(res, harness.CompileOpts{})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	log, err := session.New()
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	defer log.Close()

	// Feed the plain REPL one line on stdin.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old }()
	if _, err := w.WriteString("/review main.go\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()

	if err := RunPlain(app, res, comp, log, nil, nil); err != nil {
		t.Fatalf("RunPlain: %v", err)
	}
	if !strings.Contains(seen, "Review main.go carefully.") {
		t.Fatalf("the plain REPL did not expand the command:\n%s", seen)
	}
}

// TestMentionTurnRunsOnTheAgentModel drives a whole @mention turn: the
// operator's line starts an agent turn, the agent answers and the answer
// lands in the transcript.
func TestMentionTurnRunsOnTheAgentModel(t *testing.T) {
	srv := mentionProvider(t, "the reviewer looked at the diff and found nothing")
	defer srv.Close()

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("MOCK_API_KEY", "test-key")
	ws := mentionWorkspace(t, srv.URL+"/v1")
	writeAgentFile(t, ws, "reviewer.md",
		"---\nname: reviewer\ndescription: reviews a diff\ntools: read, list, glob, grep\n---\nYou are a reviewer.\n")

	app, err := harness.New(ws)
	if err != nil {
		t.Fatalf("harness.New: %v", err)
	}
	res, err := app.Resolve("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	comp, err := app.Compile(res, harness.CompileOpts{})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	log, err := session.New()
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	defer log.Close()

	m := testModel(100, 30)
	m.app = app
	m.res = res
	m.comp = comp
	m.log = log
	m.loop = &agent.Loop{Client: res.Client, Tools: &tools.Executor{Workspace: ws}, Log: log, MaxLoops: 4}

	m.input = "@reviewer look at the diff"
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(*Model)
	if cmd == nil {
		t.Fatal("the mention did not start a turn")
	}
	// Pump the turn's messages until it is done.
	for i := 0; i < 100 && cmd != nil; i++ {
		msg := cmd()
		if msg == nil {
			break
		}
		next, c := m.Update(msg)
		m = next.(*Model)
		cmd = c
		if !m.busy {
			break
		}
	}
	out := transcriptText(m)
	if !strings.Contains(out, "the reviewer looked at the diff") {
		t.Fatalf("the agent's answer did not reach the transcript:\n%s", out)
	}
}

// ---------- runtime permission mode (M9) ----------

// modeModel builds a model with a live executor in a temporary workspace, so
// a mode switch can be seen in the next tool call. The approver says yes to
// everything: the mode, not the operator, is what the test varies.
func modeModel(t *testing.T) (*Model, *tools.Executor) {
	t.Helper()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := testModel(100, 30)
	m.app = &harness.App{Cfg: config.File{}, Workspace: ws}
	ex := &tools.Executor{
		Workspace: ws,
		Policy:    tools.Policy{},
		Approver:  tools.ApproverFunc(func(ar tools.ApprovalRequest) bool { return true }),
	}
	m.policy = ex.Policy
	m.loop = &agent.Loop{Tools: ex}
	return m, ex
}

// editCall is one edit of the workspace file.
func editCall() provider.ToolCall {
	return provider.ToolCall{
		Type:     "function",
		Function: provider.Function{Name: "edit", Arguments: `{"path":"README.md","old":"hello","new":"bye"}`},
	}
}

func TestModeWithoutAnArgumentReportsTheCurrentMode(t *testing.T) {
	m, _ := modeModel(t)

	m.handleSlash("/mode")
	out := transcriptText(m)
	if !strings.Contains(out, "default") {
		t.Fatalf("/mode does not report the current mode:\n%s", out)
	}
	// The card names the modes a session can be in.
	for _, want := range []string{"accept-edits", "plan", "yolo", "dont-ask"} {
		if !strings.Contains(out, want) {
			t.Fatalf("/mode does not list %q:\n%s", want, out)
		}
	}
}

func TestModeSwitchChangesTheNextToolCall(t *testing.T) {
	m, ex := modeModel(t)

	// Before the switch the default mode asks, and the operator approves.
	res := ex.Dispatch(editCall())
	if res.Failed {
		t.Fatalf("the approved edit failed: %+v", res)
	}

	m.handleSlash("/mode plan")
	if m.policy.Mode != string(tools.ModePlan) {
		t.Fatalf("the policy mode is %q, want plan", m.policy.Mode)
	}
	if ex.Policy.Mode != string(tools.ModePlan) {
		t.Fatalf("the executor mode is %q, want plan", ex.Policy.Mode)
	}

	// After the switch the plan baseline refuses the edit outright.
	res = ex.Dispatch(editCall())
	if !res.Failed {
		t.Fatalf("the plan baseline allowed the edit: %+v", res)
	}
	if !strings.Contains(res.Content, "plan mode is read-only") {
		t.Fatalf("the refusal does not name the plan baseline: %+v", res)
	}
}

func TestModeSwitchIsShownInTheTranscript(t *testing.T) {
	m, _ := modeModel(t)

	m.handleSlash("/mode accept-edits")
	out := transcriptText(m)
	if !strings.Contains(out, "permission mode default → accept-edits") {
		t.Fatalf("the transcript does not show the switch:\n%s", out)
	}
}

func TestAnUnknownModeIsRejectedWithTheValidList(t *testing.T) {
	m, ex := modeModel(t)

	m.handleSlash("/mode bogus")
	out := transcriptText(m)
	if !strings.Contains(out, "want default|accept-edits|plan|yolo|dont-ask") {
		t.Fatalf("the rejection does not name the valid modes:\n%s", out)
	}
	if m.policy.Mode != "" {
		t.Fatalf("the rejected mode was applied: %q", m.policy.Mode)
	}
	if ex.Policy.Mode != "" {
		t.Fatalf("the rejected mode reached the executor: %q", ex.Policy.Mode)
	}
}

func TestTheModeSwitchIsRecordedInTheAuditLog(t *testing.T) {
	m, _ := modeModel(t)

	m.handleSlash("/mode plan")
	events, err := audit.Read(m.app.Workspace)
	if err != nil {
		t.Fatalf("audit.Read: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("the audit log holds %d events, want 1: %+v", len(events), events)
	}
	ev := events[0]
	if ev.Tool != "mode" {
		t.Fatalf("the event names %q, want mode", ev.Tool)
	}
	if ev.Decision != audit.DecisionSwitch {
		t.Fatalf("the decision is %q, want %q", ev.Decision, audit.DecisionSwitch)
	}
	if ev.Reason != "default → plan" {
		t.Fatalf("the reason is %q, want the switch", ev.Reason)
	}
	if ev.Detail != "plan" {
		t.Fatalf("the detail is %q, want the new mode", ev.Detail)
	}
}

func TestTheModeIsVisibleInTheHeaderAndTheInspector(t *testing.T) {
	m, _ := modeModel(t)
	m.handleSlash("/mode plan")

	// The mode lives on the inspector's harness tab: the panel is on
	// demand and the tab switch is explicit, so the test asks for both.
	m.panel = "inspector"
	m.inspectTab = "harness"
	if !strings.Contains(m.inspectorBody(32, 30), "mode    plan") {
		t.Fatalf("the inspector does not show the mode:\n%s", m.inspectorBody(32, 30))
	}
}
