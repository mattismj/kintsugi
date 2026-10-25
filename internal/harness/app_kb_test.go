package harness

// Layer 3 (knowledge base injection) at the application level: config →
// kb.Load/kb.OpenIndex → compiled prompt. Fixtures are the kb package's own
// testdata, copied into a temp dir and indexed, so the index write never lands
// in the repository.

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kintsugi/internal/adapters"
	"kintsugi/internal/config"
	"kintsugi/internal/kb"
	"kintsugi/internal/persona"
	"kintsugi/internal/prompt"
)

const attackBaseTitle = "MITRE ATT&CK — curated execution subset"

// kbHarness is the minimum App that Compile needs: a persona pack, defaults and
// a resolved model with an adapter.
type kbHarness struct {
	app *App
	res *Resolved
	ws  string
}

func newKBHarness(t *testing.T) *kbHarness {
	t.Helper()

	ws := t.TempDir()
	packDir := t.TempDir()
	write(t, filepath.Join(packDir, "_base.md"), "---\nname: _base\n---\nBASE DOCTRINE\n")
	write(t, filepath.Join(packDir, "child.md"), "---\nname: child\ninherits: [_base]\n---\nCHILD DOCTRINE\n")
	pack, err := persona.Load(packDir, "_base")
	if err != nil {
		t.Fatalf("persona.Load: %v", err)
	}

	reg, err := adapters.Load()
	if err != nil {
		t.Fatalf("adapters.Load: %v", err)
	}
	ad, err := reg.Select("deepseek", "deepseek-chat", false)
	if err != nil {
		t.Fatalf("adapter select: %v", err)
	}

	cfg := config.Defaults()
	cfg.PersonaPack.Dir = packDir
	cfg.Behavior.Dir = t.TempDir()
	cfg.Context.Enabled = false

	app := &App{Cfg: cfg, Adapters: reg, Pack: pack, Workspace: ws}
	res := &Resolved{
		ProviderName: "test",
		Model:        config.Model{ID: "deepseek-chat", Family: "deepseek"},
		Adapter:      ad,
	}
	return &kbHarness{app: app, res: res, ws: ws}
}

// kbFixture copies internal/kb/testdata/basic into dir and builds its derived
// index, exactly as `kintsugi kb index` would, then returns dir.
func kbFixture(t *testing.T, dir string) string {
	t.Helper()
	src := filepath.Join("..", "kb", "testdata", "basic")
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dir, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatalf("copy fixture: %v", err)
	}

	k, err := kb.Load(dir)
	if err != nil {
		t.Fatalf("kb.Load(%s): %v", dir, err)
	}
	ix, err := kb.OpenIndex(k)
	if err != nil {
		t.Fatalf("kb.OpenIndex: %v", err)
	}
	defer ix.Close()
	n, err := ix.Reindex()
	if err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if n != 3 {
		t.Fatalf("indexed %d chunks, want 3", n)
	}
	return dir
}

// layerByID returns the compiled layer with the given id.
func layerByID(t *testing.T, comp *prompt.Compiled, id string) (prompt.Layer, bool) {
	t.Helper()
	for _, l := range comp.Layers {
		if l.ID == id {
			return l, true
		}
	}
	return prompt.Layer{}, false
}

// manifestLayerByID returns the manifest entry with the given id.
func manifestLayerByID(t *testing.T, comp *prompt.Compiled, id string) (prompt.ManifestLayer, bool) {
	t.Helper()
	for _, l := range comp.Manifest.Layers {
		if l.ID == id {
			return l, true
		}
	}
	return prompt.ManifestLayer{}, false
}

func compileKB(t *testing.T, h *kbHarness, opts CompileOpts) *prompt.Compiled {
	t.Helper()
	comp, err := h.app.Compile(h.res, opts)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return comp
}

// always: every injectable chunk, capped at top_k, no ranking.
func TestCompileKBAlwaysStrategy(t *testing.T) {
	h := newKBHarness(t)
	dir := kbFixture(t, t.TempDir())
	h.app.Cfg.KBs = map[string]config.KB{
		"attack": {Path: dir, Strategy: "always", TopK: 2},
	}

	comp := compileKB(t, h, CompileOpts{})

	if !strings.Contains(comp.Text, "## Knowledge: "+attackBaseTitle+" v15.1") {
		t.Fatalf("knowledge header missing:\n%s", comp.Text)
	}
	if n := strings.Count(comp.Text, "<!-- kb:attack chunk:"); n != 2 {
		t.Fatalf("injected %d blocks, want top_k=2:\n%s", n, comp.Text)
	}
	for _, id := range []string{"T1027", "T1055"} {
		if !strings.Contains(comp.Text, "chunk:"+id+" -->") {
			t.Fatalf("top_k chunk %s missing", id)
		}
	}
	if strings.Contains(comp.Text, "chunk:T1059.001") {
		t.Fatal("top_k cap not applied: third chunk injected")
	}
	if !strings.Contains(comp.Text, "Adversaries may attempt to make an executable") {
		t.Fatal("chunk body missing")
	}

	layer, ok := layerByID(t, comp, "kb")
	if !ok {
		t.Fatal("kb layer missing")
	}
	if layer.Kind != "kb" {
		t.Fatalf("kb layer kind = %q", layer.Kind)
	}
	if want := "attack/T1027,attack/T1055"; layer.Path != want {
		t.Fatalf("kb layer path = %q, want %q", layer.Path, want)
	}
	ml, ok := manifestLayerByID(t, comp, "kb")
	if !ok {
		t.Fatal("kb manifest layer missing")
	}
	if got := strings.Join(ml.Chunks, ","); got != "T1027,T1055" {
		t.Fatalf("manifest chunks = %q, want T1027,T1055", got)
	}
}

// retrieval: must_include first, then BM25 hits; an empty task injects only the
// must_include chunks.
func TestCompileKBRetrievalStrategy(t *testing.T) {
	h := newKBHarness(t)
	dir := kbFixture(t, t.TempDir())
	h.app.Cfg.KBs = map[string]config.KB{
		"attack": {Path: dir, Strategy: "retrieval", MustInclude: []string{"T1027"}},
	}

	comp := compileKB(t, h, CompileOpts{Task: "powershell"})
	must := strings.Index(comp.Text, "chunk:T1027 -->")
	hit := strings.Index(comp.Text, "chunk:T1059.001 -->")
	if must < 0 {
		t.Fatalf("must_include chunk not injected:\n%s", comp.Text)
	}
	if hit < 0 {
		t.Fatalf("retrieval hit not injected:\n%s", comp.Text)
	}
	if must > hit {
		t.Fatalf("must_include ranked after the retrieval hit (must=%d hit=%d)", must, hit)
	}

	ml, ok := manifestLayerByID(t, comp, "kb")
	if !ok {
		t.Fatal("kb manifest layer missing")
	}
	if len(ml.Chunks) < 2 || ml.Chunks[0] != "T1027" || ml.Chunks[1] != "T1059.001" {
		t.Fatalf("manifest chunks = %v, want T1027,T1059.001,...", ml.Chunks)
	}

	// No task and no must_include → nothing to retrieve, base skipped silently.
	bare := newKBHarness(t)
	bare.app.Cfg.KBs = map[string]config.KB{
		"attack": {Path: dir, Strategy: "retrieval"},
	}
	comp = compileKB(t, bare, CompileOpts{})
	if _, ok := layerByID(t, comp, "kb"); ok {
		t.Fatalf("retrieval with no task and no must_include injected a layer:\n%s", comp.Text)
	}
	if len(comp.Manifest.Warnings) != 0 {
		t.Fatalf("empty retrieval warned: %v", comp.Manifest.Warnings)
	}

	// No task but must_include present → exactly the mandatory chunks.
	comp = compileKB(t, h, CompileOpts{})
	if n := strings.Count(comp.Text, "<!-- kb:attack chunk:"); n != 1 {
		t.Fatalf("empty-task retrieval injected %d blocks, want 1 (must_include only)", n)
	}
}

// on-demand: one pointer line, no bodies.
func TestCompileKBOnDemandStrategy(t *testing.T) {
	h := newKBHarness(t)
	dir := kbFixture(t, t.TempDir())
	h.app.Cfg.KBs = map[string]config.KB{
		"attack": {Path: dir, Strategy: "on-demand"},
	}

	comp := compileKB(t, h, CompileOpts{Task: "powershell"})
	if !strings.Contains(comp.Text, "## Knowledge: "+attackBaseTitle+" v15.1 (on demand)") {
		t.Fatalf("on-demand header missing:\n%s", comp.Text)
	}
	if !strings.Contains(comp.Text, "3 chunks; retrieval via the kb_search tool") {
		t.Fatalf("on-demand pointer line missing:\n%s", comp.Text)
	}
	if !strings.Contains(comp.Text, "<!-- kb:attack -->") {
		t.Fatalf("on-demand provenance missing:\n%s", comp.Text)
	}
	if strings.Contains(comp.Text, "Adversaries may attempt") || strings.Contains(comp.Text, "### ") {
		t.Fatalf("on-demand injected a body:\n%s", comp.Text)
	}
	ml, ok := manifestLayerByID(t, comp, "kb")
	if !ok {
		t.Fatal("kb manifest layer missing")
	}
	if len(ml.Chunks) != 0 {
		t.Fatalf("on-demand recorded chunk bodies in the manifest: %v", ml.Chunks)
	}
}

// summary: the title list, no bodies, no retrieval.
func TestCompileKBSummaryStrategy(t *testing.T) {
	h := newKBHarness(t)
	dir := kbFixture(t, t.TempDir())
	h.app.Cfg.KBs = map[string]config.KB{
		"attack": {Path: dir, Strategy: "summary"},
	}

	comp := compileKB(t, h, CompileOpts{Task: "powershell"})
	for _, want := range []string{
		"## Knowledge: " + attackBaseTitle + " v15.1",
		"- T1027 · Obfuscated Files or Information",
		"- T1055 · Process Injection",
		"- T1059.001 · Command and Scripting Interpreter: PowerShell",
	} {
		if !strings.Contains(comp.Text, want) {
			t.Fatalf("summary missing %q:\n%s", want, comp.Text)
		}
	}
	if strings.Contains(comp.Text, "Adversaries may attempt") {
		t.Fatalf("summary injected a body:\n%s", comp.Text)
	}
}

// Role bindings: no roles = every role; otherwise a case-insensitive role
// match, and an empty role sees only the all-role bindings.
func TestCompileKBRoleFiltering(t *testing.T) {
	h := newKBHarness(t)
	dir := kbFixture(t, t.TempDir())
	h.app.Cfg.KBs = map[string]config.KB{
		// Same base twice under different strategies so the two bindings are
		// distinguishable in the rendered prompt.
		"attack": {Path: dir, Strategy: "summary", Roles: []string{"coder"}},
		"house":  {Path: dir, Strategy: "on-demand"},
	}

	for _, tc := range []struct {
		role       string
		wantAttack bool
	}{
		{"", false},
		{"coder", true},
		{"CODER", true},
		{"researcher", false},
	} {
		comp := compileKB(t, h, CompileOpts{Role: tc.role})
		if !strings.Contains(comp.Text, "(on demand)") {
			t.Fatalf("role %q: all-role base missing", tc.role)
		}
		hasSummary := strings.Contains(comp.Text, "- T1027 · ")
		if hasSummary != tc.wantAttack {
			t.Fatalf("role %q: coder-only base injected = %v, want %v", tc.role, hasSummary, tc.wantAttack)
		}
	}

	// Bindings render in name order, so the stack hash is deterministic.
	comp := compileKB(t, h, CompileOpts{Role: "coder"})
	summaryAt := strings.Index(comp.Text, "- T1027 · ")
	onDemandAt := strings.Index(comp.Text, "(on demand)")
	if summaryAt < 0 || onDemandAt < 0 || summaryAt > onDemandAt {
		t.Fatalf("kb sections not in name order (summary=%d on-demand=%d):\n%s",
			summaryAt, onDemandAt, comp.Text)
	}
	again := compileKB(t, h, CompileOpts{Role: "coder"})
	if comp.Text != again.Text || comp.Manifest.StackHash != again.Manifest.StackHash {
		t.Fatal("kb compilation is not deterministic")
	}
}

// A configured path is resolved relative to the workspace, and a broken base is
// a warning in the manifest, never a compile error.
func TestCompileKBPathResolutionAndWarnings(t *testing.T) {
	h := newKBHarness(t)
	kbFixture(t, filepath.Join(h.ws, "knowledgebases", "attack"))
	h.app.Cfg.KBs = map[string]config.KB{
		"attack": {Path: "knowledgebases/attack", Strategy: "summary"},
		"ghost":  {Path: "knowledgebases/missing", Strategy: "always"},
	}

	comp := compileKB(t, h, CompileOpts{})
	if !strings.Contains(comp.Text, "- T1027 · Obfuscated Files or Information") {
		t.Fatalf("workspace-relative base not injected:\n%s", comp.Text)
	}

	var warned bool
	for _, w := range comp.Manifest.Warnings {
		if strings.Contains(w, `kb "ghost"`) {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("missing base did not produce a warning: %v", comp.Manifest.Warnings)
	}

	// The kb layer stays between persona and behavior in the full stack.
	personaAt := strings.Index(comp.Text, "BASE DOCTRINE")
	kbAt := strings.Index(comp.Text, "## Knowledge: ")
	if personaAt < 0 || kbAt < 0 || personaAt > kbAt {
		t.Fatalf("layer order wrong (persona=%d kb=%d)", personaAt, kbAt)
	}

	// No configured bases → no kb layer at all.
	h.app.Cfg.KBs = nil
	comp = compileKB(t, h, CompileOpts{})
	if _, ok := layerByID(t, comp, "kb"); ok {
		t.Fatal("kb layer emitted with no configured bases")
	}
}

// An unknown strategy is reported and falls back to retrieval instead of
// failing the compile.
func TestCompileKBUnknownStrategyWarns(t *testing.T) {
	h := newKBHarness(t)
	dir := kbFixture(t, t.TempDir())
	h.app.Cfg.KBs = map[string]config.KB{
		"attack": {Path: dir, Strategy: "everything", MustInclude: []string{"T1055"}},
	}
	comp := compileKB(t, h, CompileOpts{})
	if !strings.Contains(comp.Text, "chunk:T1055") {
		t.Fatalf("fallback retrieval did not use must_include:\n%s", comp.Text)
	}
	var warned bool
	for _, w := range comp.Manifest.Warnings {
		if strings.Contains(w, "unknown strategy") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("unknown strategy not reported: %v", comp.Manifest.Warnings)
	}
}

// Config wiring: `[kb.<name>]` tables decode into File.KBs, and the built-in
// defaults ship no knowledge bases (layer 3 is opt-in).
func TestKBConfigParsesTOML(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ws := t.TempDir()
	write(t, filepath.Join(ws, "kintsugi.toml"), `
[kb.attack]
path = "knowledgebases/attack"
strategy = "retrieval"
top_k = 4
must_include = ["T1027", "T1059.001"]
roles = ["coder", "researcher"]

[kb.house]
strategy = "on-demand"
`)
	cfg, _, err := config.Load(ws)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if len(cfg.KBs) != 2 {
		t.Fatalf("decoded %d kb entries, want 2", len(cfg.KBs))
	}
	attack, ok := cfg.KBs["attack"]
	if !ok {
		t.Fatal("kb.attack not decoded")
	}
	if attack.Path != "knowledgebases/attack" || attack.Strategy != "retrieval" || attack.TopK != 4 {
		t.Fatalf("kb.attack = %+v", attack)
	}
	if strings.Join(attack.MustInclude, ",") != "T1027,T1059.001" {
		t.Fatalf("must_include = %v", attack.MustInclude)
	}
	if strings.Join(attack.Roles, ",") != "coder,researcher" {
		t.Fatalf("roles = %v", attack.Roles)
	}
	if house := cfg.KBs["house"]; house.Strategy != "on-demand" || len(house.Roles) != 0 {
		t.Fatalf("kb.house = %+v", house)
	}
	if def := config.Defaults(); len(def.KBs) != 0 {
		t.Fatalf("Defaults() configures knowledge bases: %v", def.KBs)
	}
}
