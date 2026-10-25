package tui

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"kintsugi/internal/provider"

	"kintsugi/internal/advisor"
	"kintsugi/internal/config"
	"kintsugi/internal/harness"
	"kintsugi/internal/intercept"
	"kintsugi/internal/persona"
	"kintsugi/internal/prompt"
	"kintsugi/internal/session"
	"kintsugi/internal/tools"
	"kintsugi/internal/ui"
	"kintsugi/internal/workbench"
)

func testModel(w, h int) *Model {
	th := ui.Detect(func(k string) string {
		if k == "NO_COLOR" {
			return "1"
		}
		return ""
	})
	comp := &prompt.Compiled{
		Manifest: prompt.Manifest{
			Layers: []prompt.ManifestLayer{
				{ID: "kernel", Tokens: 295, Hash: "sha256:11111111"},
				{ID: "adapter", Tokens: 242, Hash: "sha256:22222222"},
				{ID: "persona", Tokens: 4095, Hash: "sha256:33333333"},
			},
			Totals:    prompt.Totals{Tokens: 4632, Budget: 24000},
			StackHash: "sha256:deadbeefcafe",
		},
	}
	comp.Manifest.Persona.Name = "deepseek-flash41"
	comp.Manifest.Model.ID = "deepseek-chat"
	comp.Manifest.Model.Family = "deepseek"

	m := &Model{
		app:   &harness.App{Cfg: config.File{UI: config.UI{Banner: true, Ornament: true}}},
		res:   &harness.Resolved{ProviderName: "mock", Model: config.Model{ID: "deepseek-chat", Family: "deepseek"}},
		comp:  comp,
		log:   &session.Session{ID: "20261021-000000-test"},
		theme: th, w: w, h: h, stream: -1, start: time.Now(),
	}
	// A fresh session opens on the getting-started card (revealStartup), not
	// on a nameplate block: M13 removed the nameplate because the header
	// already carries "kintsugi · model". The fixture mirrors the real path so
	// assertions see what an operator actually sees.
	m.addCard("// getting started", m.quickstartText(), ui.Gold)
	return m
}

func TestQuickstartAndAgentsCard(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "AGENTS.md"), []byte("- BANANA workspace rule\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := testModel(100, 30)
	m.app = &harness.App{Cfg: config.File{}, Workspace: ws}
	m.comp.Manifest.Persona.Rule = "capability"

	quick := m.quickstartText()
	// The opening card is short by design (M13): the readiness ladder only —
	// what is missing and the command that fixes it. The session facts
	// (persona/agents/kb) are reachable via /status and /agents instead of
	// being a wall on the first screen.
	for _, want := range []string{"getting started", "model", "context", "provider key", "mock/deepseek-chat"} {
		if !strings.Contains(quick, want) {
			t.Errorf("quickstart missing %q in:\n%s", want, quick)
		}
	}
	facts := m.sessionFacts()
	for _, want := range []string{"persona   deepseek-flash41 (capability)", "AGENTS.md"} {
		if !strings.Contains(facts, want) {
			t.Errorf("session facts missing %q in:\n%s", want, facts)
		}
	}
	if _, _ = m.handleSlash("/agents"); len(m.blocks) == 0 {
		t.Fatal("no blocks after /agents")
	}
	last := m.blocks[len(m.blocks)-1]
	if last.kind != blockCard || !strings.Contains(plain(last.title), "// agents") ||
		!strings.Contains(last.text, "BANANA workspace rule") {
		t.Fatalf("/agents card = %+v", last)
	}
}

func assertFits(t *testing.T, m *Model) {
	t.Helper()
	out := m.View()
	lines := strings.Split(out, "\n")
	if len(lines) > m.h {
		t.Fatalf("%dx%d: %d lines overflow height", m.w, m.h, len(lines))
	}
	for i, line := range lines {
		if w := lipgloss.Width(line); w > m.w {
			t.Fatalf("%dx%d: line %d width %d overflows: %q", m.w, m.h, i, w, line)
		}
	}
}

func TestViewSizesAndPanels(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {100, 30}, {200, 60}} {
		// The default is a centered transcript and nothing else: panels are
		// on demand (M13), so a session that opened no panel shows no panel.
		m := testModel(size[0], size[1])
		m.addBlock(block{kind: blockUser, text: "build the auth layer"})
		m.addBlock(block{kind: blockAssistant, text: "working on it\n"})
		m.tasks = append(m.tasks, taskItem{label: "build the auth layer", state: "running"})
		assertFits(t, m)

		out := m.View()
		if !strings.Contains(out, "build the auth layer") {
			t.Fatalf("%dx%d: the transcript lost the conversation:\n%s", size[0], size[1], out)
		}
		for _, panel := range []string{"// 00 work", "// 03 inspector"} {
			if strings.Contains(out, panel) {
				t.Fatalf("%dx%d: %q rendered without being asked for:\n%s", size[0], size[1], panel, out)
			}
		}

		// The rail on demand: the identity line, with the tasks, agents and
		// roster the old tasks panel carried.
		m.panel = "rail"
		out = m.View()
		assertFits(t, m)
		if !strings.Contains(out, "// 00 work") {
			t.Fatalf("%dx%d: the work rail did not open on demand:\n%s", size[0], size[1], out)
		}
		if !strings.Contains(out, "agents") {
			t.Fatalf("%dx%d: the rail lost the agent roster:\n%s", size[0], size[1], out)
		}
		if !strings.Contains(out, "build the auth layer") {
			t.Fatalf("%dx%d: the rail lost the task list:\n%s", size[0], size[1], out)
		}

		// The inspector on demand, and only one panel at a time: asking for
		// the inspector replaces the rail rather than stacking on it.
		m.panel = "inspector"
		out = m.View()
		assertFits(t, m)
		if !strings.Contains(out, "// 03 inspector") {
			t.Fatalf("%dx%d: the inspector did not open on demand:\n%s", size[0], size[1], out)
		}
		if strings.Contains(out, "// 00 ember") {
			t.Fatalf("%dx%d: two panels are shown at once:\n%s", size[0], size[1], out)
		}
	}
}

func TestTodoPlanRendersInTaskPanel(t *testing.T) {
	// The todos render in the rail's activity list, and the rail is on
	// demand (M13) — so the test asks for it.
	m := testModel(120, 30)
	m.panel = "rail"
	m.tasks = append(m.tasks, taskItem{label: "old prompt", state: "running"})
	m.todos = []tools.TodoItem{
		{Content: "read the repo", Status: "completed"},
		{Content: "patch the loop", Status: "in_progress", Priority: "high"},
		{Content: "run tests", Status: "pending"},
	}
	out := m.View()
	for _, want := range []string{"read the repo", "patch the loop", "run tests"} {
		if !strings.Contains(out, want) {
			t.Fatalf("plan missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "old prompt") {
		t.Fatal("plan should take precedence over the prompt history")
	}
	assertFits(t, m)
}

func TestInspectorMatchesManifest(t *testing.T) {
	m := testModel(120, 30)
	text := m.inspectorText()
	for _, want := range []string{"kernel", "295", "adapter", "242", "persona", "4095", "total", "4632", "budget", "24000", "deadbeef"} {
		if !strings.Contains(text, want) {
			t.Errorf("inspector missing %q:\n%s", want, text)
		}
	}
	// The manifest's totals live on the inspector's harness tab: the panel
	// is on demand and the tab switch is explicit.
	m.panel = "inspector"
	m.inspectTab = "harness"
	if !strings.Contains(m.View(), "4632") {
		t.Error("inspector view missing total tokens")
	}
}

func TestInjectionCardRenders(t *testing.T) {
	m := testModel(120, 30)
	m.addCard("// frame · injected", "class moralizing · reminder 1 · resuming", ui.Warn)
	out := m.View()
	if !strings.Contains(out, "// frame · injected") || !strings.Contains(out, "reminder 1") {
		t.Fatalf("injection card missing:\n%s", out)
	}

	m.addCard("// refusal · intercepted ✸", "class identity · handed back to operator", ui.Danger)
	if !strings.Contains(m.View(), "// refusal · intercepted ✸") {
		t.Fatal("escalation card missing")
	}
}

func TestApprovalModal(t *testing.T) {
	m := testModel(120, 30)
	m.approval = &approvalReq{tool: "bash", detail: "rm -rf build", reply: make(chan bool, 1)}
	out := m.View()
	for _, want := range []string{"// approval", "rm -rf build", "[y] approve", "[n] deny"} {
		if !strings.Contains(out, want) {
			t.Fatalf("approval modal missing %q:\n%s", want, out)
		}
	}
	if !destructive("rm -rf build") || destructive("go test ./...") {
		t.Fatal("destructive classification wrong")
	}

	req := m.approval
	m.resolveApproval(false)
	if m.approval != nil {
		t.Fatal("approval not cleared")
	}
	select {
	case ok := <-req.reply:
		if ok {
			t.Fatal("denial replied true")
		}
	default:
		t.Fatal("denial did not reply")
	}
}

func TestInterceptNoticeRendersCardAndCounter(t *testing.T) {
	m := testModel(120, 30)
	_, cmd := m.Update(interceptNotice{hit: intercept.Hit{Class: "moralizing", Phrase: "I can't assist"}, level: 1, outcome: "resumed"})
	if cmd == nil {
		t.Fatal("intercept notice did not re-arm the channel listener")
	}
	if m.refusals != 1 {
		t.Fatalf("refusals = %d, want 1", m.refusals)
	}
	// The refusal counter lives in the refusal pillar, which closes the
	// inspector — so the panel has to be open for the count to be on screen.
	m.panel = "inspector"
	out := m.View()
	// The refusal counter moved into the refusal pillar (M13). The card is still
	// the transcript's record of what was intercepted, and it now carries the
	// refusal-evidence provenance (A3) — "authorization missing" in a workspace
	// with no CONTEXT.md — so the operator can act, not just observe.
	if !strings.Contains(out, "// refusal · intercepted") {
		t.Fatalf("intercept card missing:\n%s", out)
	}
	if !strings.Contains(out, "authorization missing") {
		t.Fatalf("intercept card does not show the missing context evidence:\n%s", out)
	}
	if !strings.Contains(out, "// pillars") || !strings.Contains(out, "1 hit") {
		t.Fatalf("the refusal pillar does not carry the count:\n%s", out)
	}
	if m.refusalTone() != ui.Danger {
		t.Fatalf("a session with a refusal does not escalate the pillar tone: %v", m.refusalTone())
	}
}

func TestAdvisorNoticeRendersCards(t *testing.T) {
	m := testModel(120, 30)
	m.ch = make(chan tea.Msg, 1)
	_, cmd := m.Update(advisorNotice{notes: []advisor.Note{
		{Kind: advisor.KindConcern, Text: "hardcoded callback"},
		{Kind: advisor.KindBlocker, Text: "outside declared scope"},
	}})
	if cmd == nil {
		t.Fatal("advisor notice did not re-arm the channel listener")
	}
	if m.advisorNotes != 2 {
		t.Fatalf("advisor notes = %d", m.advisorNotes)
	}
	out := m.View()
	for _, want := range []string{"// advisor · concern", "// advisor · blocker", "hardcoded callback", "outside declared scope"} {
		if !strings.Contains(out, want) {
			t.Fatalf("advisor card missing %q:\n%s", want, out)
		}
	}
}

func TestApprovalRequestRearmsChannel(t *testing.T) {
	m := testModel(120, 30)
	m.ch = make(chan tea.Msg, 1)
	_, cmd := m.Update(approvalReq{tool: "bash", detail: "id", reply: make(chan bool, 1)})
	if cmd == nil {
		t.Fatal("approval request consumed the channel read without re-arming")
	}
	if m.approval == nil {
		t.Fatal("approval not stored")
	}
}

func TestContextWorkbenchRenders(t *testing.T) {
	m := testModel(140, 40)
	m.wb = &workbenchView{kind: "context", ctx: &workbench.ContextSnapshot{
		Path: "/ws/CONTEXT.md", Exists: true, Project: "lab", Hash: "sha256:abc123",
		Tokens: 742, Budget: 1200,
		Sections: []workbench.ContextSection{
			{Title: "Authorization & scope", Class: "authorization", Tokens: 84, Gate: "gated", Injected: true},
			{Title: "Domain context", Class: "domain", Tokens: 132, Gate: "review", Injected: true},
			{Title: "Current state", Class: "state", Tokens: 88, Gate: "auto", Injected: false},
		},
	}}
	out := m.View()
	for _, want := range []string{"// 04 context", "/ws/CONTEXT.md", "authorization", "gated", "review", "auto", "dropped", "742", "recompile"} {
		if !strings.Contains(out, want) {
			t.Fatalf("context workbench missing %q:\n%s", want, out)
		}
	}
	assertFits(t, m)
}

func TestPersonaWorkbenchRenders(t *testing.T) {
	m := testModel(150, 44)
	m.wb = &workbenchView{kind: "persona", per: &workbench.PersonaSnapshot{
		PackDir: "/packs/global", Model: "mock/deepseek-chat", Selected: "deepseek-flash41",
		Rule: "id_glob", Chain: []string{"_base", "deepseek-flash41"},
		Tokens: 4095, SelectedPath: "/packs/global/deepseek-flash41.md",
		Base: "_base", BasePath: "/packs/global/_base.md",
		Available: []workbench.PersonaCard{
			{Name: "_base", Description: "shared doctrine", Path: "/packs/global/_base.md", Effective: true},
			{Name: "deepseek-flash41", Description: "flash persona", Path: "/packs/global/deepseek-flash41.md", Effective: true, Matched: true, Triggers: 1},
		},
		Triggers: []workbench.TriggerRow{
			{Persona: "deepseek-flash41", Term: "C2", Instead: "tasking harness", Class: "moralizing"},
		},
		Lint: []persona.Lint{{Level: "warning", Persona: "deepseek-flash41", Message: "trigger has no class"}},
	}}
	out := m.View()
	for _, want := range []string{"// 04 persona", "_base → deepseek-flash41", "C2 → tasking harness", "LINT", "trigger has no class", "reload pack"} {
		if !strings.Contains(out, want) {
			t.Fatalf("persona workbench missing %q:\n%s", want, out)
		}
	}
	assertFits(t, m)
}

func TestHotReloadDetectsContextEdit(t *testing.T) {
	m := testModel(100, 30)
	m.app.Workspace = t.TempDir()
	m.app.Cfg.Context = config.Context{Enabled: true, Path: "CONTEXT.md"}
	path := m.app.ContextPath()
	if err := os.WriteFile(path, []byte("# Project: v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.stamps = m.currentStamps()
	if stale := m.stalePaths(); len(stale) != 0 {
		t.Fatalf("fresh stamps reported stale: %v", stale)
	}
	if err := os.WriteFile(path, []byte("# Project: v2 with more text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := m.stalePaths()
	if len(stale) != 1 || stale[0] != path {
		t.Fatalf("stale = %v, want [%s]", stale, path)
	}
}

func TestSlashCommandsDoNotCrash(t *testing.T) {
	m := testModel(100, 30)
	for _, cmd := range []string{"/help", "/status", "/prompt", "/context", "/persona", "/skills", "/frame", "/remember note", "/clear"} {
		model, _ := m.handleSlash(cmd)
		m = model.(*Model)
	}
	if len(m.blocks) == 0 {
		t.Fatal("commands produced no blocks")
	}
	m.handleSlash("/bogus")
	if !strings.Contains(m.blocks[len(m.blocks)-1].text, "unknown command") {
		t.Fatal("unknown command not reported")
	}
}

func TestWrapAndSplit(t *testing.T) {
	lines := wrap("one two three four", 9)
	for _, l := range lines {
		if lipgloss.Width(l) > 9 {
			t.Fatalf("wrapped line too wide: %q", l)
		}
	}
	head, tail := splitWidth("abcdefghij", 4)
	if head != "abcd" || tail != "efghij" {
		t.Fatalf("splitWidth = %q / %q", head, tail)
	}
}

// ---------- M8.6.4 / M8.6.5 fixtures ----------

// colorModel is testModel on a color terminal: the sprite, the verb cycle and
// the indicator's tone only exist when color is available.
func colorModel(w, h int) *Model {
	m := testModel(w, h)
	m.theme = ui.Theme{Profile: ui.TrueColor, Ornament: true}
	return m
}

var ansiEscape = regexp.MustCompile("\x1b\\[[0-9;]*m")

// plain drops SGR escapes so an assertion can look at what the operator reads.
func plain(s string) string { return ansiEscape.ReplaceAllString(s, "") }

// Mono opens straight on the getting-started card: no colour, no reveal —
// the readiness ladder is the first screen.
func TestMonoOpensOnTheGettingStartedCard(t *testing.T) {
	m := testModel(120, 30)
	out := plain(m.View())
	if !strings.Contains(out, "// getting started") {
		t.Fatalf("mono must open on the getting-started card:\n%s", out)
	}
}

func TestFooterShowsScrolledHint(t *testing.T) {
	m := testModel(80, 24)
	addLines(m, 40)
	m.scroll = 7
	out := plain(m.View())
	if !strings.Contains(out, "scrolled 7 · esc to bottom") {
		t.Fatalf("scrolled hint missing:\n%s", out)
	}

	// Busy and scrolled at once: the hint is folded into one row. The session
	// stamp was folded away entirely (M13) — it is bookkeeping, reachable via
	// /status, not something the operator needs on every frame.
	m.busy = true
	out = plain(m.View())
	for _, want := range []string{"scrolled 7 · esc to bottom", "ctrl+c abort"} {
		if !strings.Contains(out, want) {
			t.Fatalf("footer missing %q:\n%s", want, out)
		}
	}
	assertFits(t, m)

	// Back at the bottom the hint goes away.
	m.busy = false
	m.scroll = 0
	if strings.Contains(plain(m.View()), "scrolled") {
		t.Fatal("scrolled hint should disappear at the bottom")
	}
}

func TestBusyFooterUsesPhaseLockedVerb(t *testing.T) {
	m := colorModel(100, 30)
	m.start = time.Now().Add(-2500 * time.Millisecond) // second 2s phase
	m.busy = true
	out := plain(m.View())
	if !strings.Contains(out, "lacquering · ctrl+c abort") {
		t.Fatalf("busy verb missing (want lacquering):\n%s", out)
	}

	mono := testModel(100, 30)
	mono.start = m.start
	mono.busy = true
	out = plain(mono.View())
	if !strings.Contains(out, "working · ctrl+c abort") {
		t.Fatalf("mono busy line missing:\n%s", out)
	}
	if strings.Contains(out, "tempering") {
		t.Fatal("mono must not cycle verbs")
	}
}

func TestRoleGlyphInHeaderAndInspector(t *testing.T) {
	m := testModel(120, 30)
	m.role = "coder"
	// The role lives on the inspector's harness tab, not the header (M13).
	m.panel = "inspector"
	m.inspectTab = "harness"
	out := plain(m.View())
	if !strings.Contains(out, "role    "+ui.RoleGlyph("coder")+" coder") {
		t.Fatalf("inspector role row missing:\n%s", out)
	}
	if ui.RoleGlyph("") != "·" {
		t.Fatalf("an unknown role must get the neutral glyph, got %q", ui.RoleGlyph(""))
	}
	assertFits(t, m)
}

func TestMascotStateMapping(t *testing.T) {
	m := testModel(100, 30)
	if got := m.mascotState(); got != ui.MascotIdle {
		t.Fatalf("fresh model = %q, want idle", got)
	}

	m.busy = true
	if got := m.mascotState(); got != ui.MascotWorking {
		t.Fatalf("busy = %q, want working", got)
	}

	m.busy = false
	m.addCard("// gate · review", "opsec review of the diff", ui.Warn)
	if got := m.mascotState(); got != ui.MascotAuditing {
		t.Fatalf("gate card = %q, want auditing", got)
	}

	m.blocks = nil
	m.addNote("· turn complete · " + m.log.ID)
	if got := m.mascotState(); got != ui.MascotDone {
		t.Fatalf("finished turn = %q, want done", got)
	}

	m.addNote("✸ provider exploded")
	if got := m.mascotState(); got != ui.MascotError {
		t.Fatalf("✸ note = %q, want error", got)
	}
	m.addNote("✗ tool failed")
	if got := m.mascotState(); got != ui.MascotError {
		t.Fatalf("✗ note = %q, want error", got)
	}
}

func TestHeaderCarriesIndicatorAndSigil(t *testing.T) {
	m := colorModel(120, 30)
	m.role = "coder"
	line := plain(m.headerLine())
	// The header is one quiet line: the product word, the model, and the live
	// state glyph. The family sigil and the role moved to the inspector when
	// the header slimmed down (M13).
	for _, want := range []string{"kintsugi", "mock/deepseek-chat", ui.Indicator(m.mascotState())} {
		if !strings.Contains(line, want) {
			t.Fatalf("header missing %q: %q", want, line)
		}
	}
}

// The rail shows the identity line and the activity at any width — there is
// no art to clip, so a narrow column degrades by truncating text only.
func TestRailDegradesWhenNarrow(t *testing.T) {
	// 80x24 with no panel asked for: no rail at all.
	small := colorModel(80, 24)
	if _, _, _, _, showLeft, _ := small.layout(); showLeft {
		t.Fatal("80x24 should not show the rail")
	}
	assertFits(t, small)

	// A narrow rail column still carries the state mark and the activity.
	small.panel = "rail"
	m := colorModel(120, 34)
	m.panel = "rail"
	body := plain(m.activityRailBody(6, 20))
	if !strings.Contains(body, ui.Indicator(m.mascotState())) {
		t.Fatalf("a narrow rail lost the state mark:\n%s", body)
	}
	if !strings.Contains(body, "agents") {
		t.Fatalf("a narrow rail lost its activity list:\n%s", body)
	}
}

// The inspector body must still fit its budget, with the pillars reserved at the
// bottom so a long manifest cannot push them off.
func TestInspectorFitsWithPillars(t *testing.T) {
	busy := colorModel(100, 30)
	busy.busy = true
	_, rightW, _, bodyH, _, _ := busy.layout()
	if got, want := len(strings.Split(busy.inspectorBody(rightW-2, bodyH-3), "\n")), bodyH-3; got > want {
		t.Fatalf("inspector body is %d lines, want at most %d", got, want)
	}
	assertFits(t, busy)
}

// TestInspectorCachePrefixRow is the M10-A no-regression guard: the inspector's
// `cache` row is the prefix-stability verdict (DESIGN §3.6) and must survive at
// both the compact 80x24 panel and the roomy 100x30 one, in both states.
func TestInspectorCachePrefixRow(t *testing.T) {
	// Stable: nothing watched has changed since the stamps were taken. The
	// cache row is harness-tab detail.
	for _, size := range [][2]int{{80, 24}, {100, 30}} {
		m := colorModel(size[0], size[1])
		m.inspectTab = "harness"
		m.stamps = m.currentStamps()
		_, rightW, _, bodyH, _, _ := m.layout()
		body := plain(m.inspectorBody(rightW-2, bodyH-3))
		if !strings.Contains(body, "cache   prefix stable") {
			t.Fatalf("%dx%d: prefix-stable row missing:\n%s", size[0], size[1], body)
		}
		assertFits(t, m)
	}

	// Stale: a watched file changed on disk, so the prefix must be recompiled.
	m := colorModel(100, 30)
	m.inspectTab = "harness"
	m.app.Workspace = t.TempDir()
	m.app.Cfg.Context = config.Context{Enabled: true, Path: "CONTEXT.md"}
	path := m.app.ContextPath()
	if err := os.WriteFile(path, []byte("# Project: v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.stamps = m.currentStamps()
	if err := os.WriteFile(path, []byte("# Project: v2 edited out of band\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, rightW, _, bodyH, _, _ := m.layout()
	body := plain(m.inspectorBody(rightW-2, bodyH-3))
	if !strings.Contains(body, "cache   stale · recompile on send") {
		t.Fatalf("100x30: stale row missing:\n%s", body)
	}
	assertFits(t, m)
}

// TestInspectorWorkTabIsTheDefault: opening the inspector answers "what is
// the session doing" — task, files, counters — before any harness detail.
func TestInspectorWorkTabIsTheDefault(t *testing.T) {
	m := testModel(120, 30)
	m.panel = "inspector"
	m.tasks = append(m.tasks, taskItem{label: "fuzz the parser", state: "running"})
	m.foldChanges([]tools.FileChange{{Path: "a.go", Kind: "edit", Before: "x\n", After: "y\n"}})
	out := plain(m.View())
	for _, want := range []string{"inspector · work", "task    fuzz the parser", "files", "1 file(s)", "a.go",
		"refusals 0"} {
		if !strings.Contains(out, want) {
			t.Errorf("the work tab missing %q:\n%s", want, out)
		}
	}
	// No harness detail by default: the layer table lives on the other tab.
	if strings.Contains(out, "kernel") {
		t.Errorf("harness detail leaked onto the work tab:\n%s", out)
	}
	assertFits(t, m)
}

// TestInspectorTabKeySwitches: tab flips work ⇄ harness while the panel is
// open, and only then.
func TestInspectorTabKeySwitches(t *testing.T) {
	m := testModel(120, 30)
	m.panel = "inspector"
	pressKeys(m, tea.KeyMsg{Type: tea.KeyTab})
	if m.inspectTab != "harness" {
		t.Fatalf("tab did not switch to harness: %q", m.inspectTab)
	}
	if !strings.Contains(plain(m.View()), "kernel") {
		t.Fatal("the harness tab does not render the layer table")
	}
	pressKeys(m, tea.KeyMsg{Type: tea.KeyTab})
	if m.inspectTab != "work" {
		t.Fatalf("tab did not switch back: %q", m.inspectTab)
	}
	// With the panel closed, tab is the completion key again — the switch is
	// scoped to the open inspector.
	m.panel = ""
	m.inspectTab = "harness"
	pressKeys(m, tea.KeyMsg{Type: tea.KeyTab})
	if m.inspectTab != "harness" {
		t.Fatal("tab switched tabs with the panel closed")
	}
}

// TestWorkTabUpdatesAfterToolNotice: a tool notice with a Diff moves the
// files row — the two surfaces (work tab and /changes) agree by construction.
func TestWorkTabUpdatesAfterToolNotice(t *testing.T) {
	m := testModel(120, 30)
	m.panel = "inspector"
	m.ch = make(chan tea.Msg, 1)
	_, _ = m.Update(toolNotice{
		call: provider.ToolCall{Type: "function", Function: provider.Function{Name: "edit", Arguments: `{"path":"x.go"}`}},
		res: tools.Result{
			Content: `{"ok":true}`,
			Diff:    []tools.FileChange{{Path: "x.go", Kind: "edit", Before: "a\n", After: "b\n"}},
		},
	})
	out := plain(m.View())
	if !strings.Contains(out, "x.go") {
		t.Errorf("the work tab did not pick up the changed file:\n%s", out)
	}
}
