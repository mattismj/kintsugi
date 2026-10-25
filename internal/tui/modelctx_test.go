package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"kintsugi/internal/config"
	"kintsugi/internal/harness"
)

// modelCtxModel binds a model to a workspace with the surface open.
func modelCtxModel(t *testing.T, w, h int) (*Model, string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ws := t.TempDir()
	app, err := harness.New(ws)
	if err != nil {
		t.Fatalf("harness.New: %v", err)
	}
	res, err := app.Resolve("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := app.Compile(res, harness.CompileOpts{}); err != nil {
		t.Fatalf("Compile: %v", err)
	}
	m := testModel(w, h)
	m.app = app
	m.openModelCtxWorkbench()
	if m.wb == nil || m.wb.kind != "agents" {
		t.Fatalf("the model-context surface did not open: %+v", m.wb)
	}
	return m, ws
}

func TestModelCtxSurfaceOpensAndRenders(t *testing.T) {
	m, _ := modelCtxModel(t, 120, 34)
	out := plain(m.View())
	for _, want := range []string{"// 04 model context", "tiers in this prompt", "model documents"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the surface is missing %q:\n%s", want, out)
		}
	}
	assertFits(t, m)
}

func TestModelCtxSlashCommandOpensIt(t *testing.T) {
	m, _ := modelCtxModel(t, 120, 34)
	m.wb = nil
	m.handleSlash("/modelctx")
	if m.wb == nil || m.wb.kind != "agents" {
		t.Fatalf("/modelctx did not open the surface: %+v", m.wb)
	}
}

func TestModelCtxListsDocumentsWithTheirMatchRules(t *testing.T) {
	m, ws := modelCtxModel(t, 140, 40)
	dir := filepath.Join(ws, ".kintsugi", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "qwen.md"),
		[]byte("---\nnote: qwen needs re-reads\nmatch:\n  family: [qwen]\n---\nBODY\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.openModelCtxWorkbench()

	out := plain(m.View())
	if !strings.Contains(out, "qwen") {
		t.Fatalf("the document is not listed:\n%s", out)
	}
	if !strings.Contains(out, "family qwen") {
		t.Fatalf("the match rule is not shown:\n%s", out)
	}
	if !strings.Contains(out, "qwen needs re-reads") {
		t.Fatalf("the note is not shown:\n%s", out)
	}
}

func TestModelCtxCreateWritesADocumentKeyedToThisModel(t *testing.T) {
	m, ws := modelCtxModel(t, 120, 34)

	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	m = mm.(*Model)
	if m.wb != nil {
		t.Fatal("creating a document left the surface open instead of going to the editor")
	}
	// The editor command is a tea.ExecProcess; the file must already exist.
	dir := filepath.Join(ws, ".kintsugi", "agents")
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) == 0 {
		t.Fatalf("no document was created: %v", err)
	}
	if cmd == nil {
		t.Fatal("no editor command was returned")
	}
	// The document is keyed to this model, so it applies immediately.
	if _, _, sel := m.app.ModelDocs(); sel.Doc == nil {
		t.Fatalf("the new document is not selected for this model: %+v", sel)
	}
}

func TestModelCtxUseForcesADocumentAndRSetsItBack(t *testing.T) {
	m, ws := modelCtxModel(t, 120, 34)
	dir := filepath.Join(ws, ".kintsugi", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A document that no model matches: only the explicit "use" can apply it.
	if err := os.WriteFile(filepath.Join(dir, "forced.md"),
		[]byte("---\nmatch:\n  id_glob: [\"never-*\"]\n---\nFORCED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.openModelCtxWorkbench()

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'u'}})
	text, path := m.app.AgentsMD()
	if !strings.Contains(text, "FORCED") {
		t.Fatalf("u did not force the document: %s", path)
	}

	// r returns to rule matching.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if text, _ := m.app.AgentsMD(); strings.Contains(text, "FORCED") {
		t.Fatal("r did not return to rule matching")
	}
}

func TestModelCtxEditOpensTheDocument(t *testing.T) {
	m, ws := modelCtxModel(t, 120, 34)
	dir := filepath.Join(ws, ".kintsugi", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("---\nmatch: {}\n---\nBODY\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.openModelCtxWorkbench()

	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	m = mm.(*Model)
	if cmd == nil {
		t.Fatal("e returned no editor command")
	}
	if m.wb != nil {
		t.Fatal("e left the surface open over the editor")
	}
}

func TestModelCtxCursorMovesAndClamps(t *testing.T) {
	m, ws := modelCtxModel(t, 120, 34)
	dir := filepath.Join(ws, ".kintsugi", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(dir, n+".md"), []byte("---\nmatch: {}\n---\nB\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m.openModelCtxWorkbench()

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlN})
	if got := m.wb.agents.cursor; got != 1 {
		t.Fatalf("cursor = %d after one down", got)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlN})
	if got := m.wb.agents.cursor; got != 1 {
		t.Fatalf("cursor clamped to %d, want 1", got)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlP})
	if got := m.wb.agents.cursor; got != 0 {
		t.Fatalf("cursor = %d after one up", got)
	}
}

func TestModelCtxFooterAdvertisesItsKeys(t *testing.T) {
	m, _ := modelCtxModel(t, 140, 40)
	footer := plain(m.workbenchFooter())
	for _, want := range []string{"edit", "new", "use this"} {
		if !strings.Contains(footer, want) {
			t.Fatalf("the footer is missing %q: %q", want, footer)
		}
	}
}

func TestModelCtxBodyMatchesTheScrollMirror(t *testing.T) {
	m, _ := modelCtxModel(t, 120, 34)
	width := m.w - 2
	lines := m.workbenchLines(width)
	body := strings.Split(m.workbenchBody(width, len(lines)), "\n")
	for i := range lines {
		if i >= len(body) {
			break
		}
		if body[i] != lines[i] {
			t.Fatalf("row %d differs between workbenchLines and workbenchBody:\n%q\n%q", i, lines[i], body[i])
		}
	}
}

func TestModelCtxFitsAtSmallSizes(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {120, 34}} {
		m, _ := modelCtxModel(t, size[0], size[1])
		assertFits(t, m)
	}
}

func TestSanitizeDocName(t *testing.T) {
	cases := map[string]string{
		"qwen3-coder-plus": "qwen3-coder-plus",
		"Qwen 3/Coder":     "qwen-3-coder",
		"":                 "model",
		"///":              "model",
	}
	for in, want := range cases {
		if got := sanitizeDocName(in); got != want {
			t.Fatalf("sanitizeDocName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCreateModelDocScaffoldsTheSections: the per-model doc the wizard writes
// opens on a scaffold, not a blank page — the four sections that move a
// model's refusal posture and reliability. The content is the operator's;
// the structure is the part worth not hand-writing.
func TestCreateModelDocScaffoldsTheSections(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ws := t.TempDir()
	m := testModel(100, 30)
	m.app = &harness.App{Cfg: config.File{}, Workspace: ws}
	body := m.modelDocTemplate(m.res)
	for _, want := range []string{
		"# Model context: deepseek-chat",
		"## Model behavior",
		"## Refusal posture",
		"## Output contract",
		"## Quirks",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the scaffold is missing %q:\n%s", want, body)
		}
	}
	// The scaffold points at the authorization reference: the per-model
	// doc is where the refusal ground gets laid, and it names where the
	// authorization lives.
	if !strings.Contains(body, "CONTEXT.md") {
		t.Errorf("the scaffold does not reference the workspace authorization:\n%s", body)
	}
}
