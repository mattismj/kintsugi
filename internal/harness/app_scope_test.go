package harness

// Declared scope at the application level: the [scope] config block and the
// CONTEXT.md "Scope" section reach the compiled prompt through
// scope.FromWorkspace — the same call tools.Executor.ApplySecurity makes — so
// the boundary the model reads is the boundary the tool layer enforces.

import (
	"path/filepath"
	"strings"
	"testing"

	"kintsugi/internal/adapters"
	"kintsugi/internal/config"
	"kintsugi/internal/persona"
	"kintsugi/internal/prompt"
	"kintsugi/internal/scope"
)

// scopeHarness is the minimum App Compile needs, with a workspace the test can
// write a CONTEXT.md into.
type scopeHarness struct {
	app *App
	res *Resolved
	ws  string
}

func newScopeHarness(t *testing.T) *scopeHarness {
	t.Helper()

	ws := t.TempDir()
	packDir := t.TempDir()
	write(t, filepath.Join(packDir, "_base.md"), "---\nname: _base\n---\nBASE DOCTRINE\n")
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
	return &scopeHarness{app: app, res: res, ws: ws}
}

func scopeLayerOf(t *testing.T, comp *prompt.Compiled) prompt.Layer {
	t.Helper()
	for _, l := range comp.Layers {
		if l.ID == "scope" {
			return l
		}
	}
	t.Fatalf("compiled prompt has no scope layer: %v", layerIDsOf(comp))
	return prompt.Layer{}
}

func layerIDsOf(comp *prompt.Compiled) []string {
	var ids []string
	for _, l := range comp.Layers {
		ids = append(ids, l.ID)
	}
	return ids
}

// A workspace with no [scope] and no CONTEXT.md scope section gains no layer:
// an absent scope is an absence of a decision, not a boundary.
func TestCompileAddsNoScopeLayerWhenNoneIsDeclared(t *testing.T) {
	h := newScopeHarness(t)
	comp, err := h.app.Compile(h.res, CompileOpts{})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	for _, l := range comp.Layers {
		if l.ID == "scope" {
			t.Fatalf("a workspace with no declared scope got a scope layer:\n%s", l.Text)
		}
	}
	if strings.Contains(comp.Text, "## Declared scope") {
		t.Fatal("the compiled prompt claims a scope the workspace never declared")
	}
}

// The [scope] block reaches the prompt, with the config file as the manifest
// path, and the boundary it states is the one the executor enforces.
func TestCompileInjectsConfigScopeAndEnforcementAgrees(t *testing.T) {
	h := newScopeHarness(t)
	h.app.ConfigPaths = []string{filepath.Join(h.ws, "kintsugi.toml")}
	h.app.Cfg.Scope = config.Scope{
		Paths:      []string{"src/**"},
		Commands:   []string{"go test"},
		Hosts:      []string{"example.com"},
		OutOfScope: []string{"production"},
	}

	comp, err := h.app.Compile(h.res, CompileOpts{})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	layer := scopeLayerOf(t, comp)
	if !strings.Contains(layer.Text, "## Declared scope") {
		t.Fatalf("the scope block is not the rendered scope:\n%s", layer.Text)
	}
	if !strings.Contains(layer.Path, "kintsugi.toml") || !strings.Contains(layer.Path, "[scope]") {
		t.Fatalf("scope layer path = %q, want the config file that declared it", layer.Path)
	}

	// The manifest carries the layer too, so /prompt shows it and /prompt diff
	// catches a rule change.
	var found bool
	for _, l := range comp.Manifest.Layers {
		if l.ID != "scope" {
			continue
		}
		found = true
		if l.Hash == "" || l.Tokens == 0 || l.Path == "" {
			t.Fatalf("manifest scope layer is incomplete: %+v", l)
		}
	}
	if !found {
		t.Fatal("the manifest has no scope layer")
	}

	// Agreement with enforcement: the executor builds its rules with
	// scope.FromWorkspace(cfg, workspace), so the prompt's statement and that
	// rule set must give the same verdict on every sample.
	enforced := scope.FromWorkspace(h.app.Cfg, h.app.Workspace)
	for _, s := range []struct {
		kind, value string
		want        bool
	}{
		{"path", "src/main.go", true},
		{"path", "src/production/main.go", false}, // deny wins over the path rule
		{"path", "README.md", false},
		{"command", "go test ./...", true},
		{"command", "curl example.com", false},
		{"host", "example.com", true},
		{"host", "other.example", false},
	} {
		var got bool
		switch s.kind {
		case "path":
			got = enforced.AllowsPath(s.value)
		case "command":
			got = enforced.AllowsCommand(s.value)
		case "host":
			got = enforced.AllowsHost(s.value)
		}
		if got != s.want {
			t.Fatalf("%s %q: enforcement allows=%v, want %v", s.kind, s.value, got, s.want)
		}
	}

	// And the prompt text names every rule enforcement honours.
	for _, want := range []string{"src/**", "go test", "example.com", "production"} {
		if !strings.Contains(layer.Text, want) {
			t.Fatalf("the prompt does not state the enforced rule %q:\n%s", want, layer.Text)
		}
	}
}

// The CONTEXT.md "Scope" section is honoured exactly as enforcement honours it:
// merged with [scope] by FromWorkspace, deny list included.
func TestCompileInjectsContextScopeSection(t *testing.T) {
	h := newScopeHarness(t)
	h.app.Cfg.Context = config.Context{Enabled: true, Path: "CONTEXT.md"}
	write(t, filepath.Join(h.ws, "CONTEXT.md"), `# Project: demo

## Scope

paths = ["docs/**"]
out_of_scope = ["**/drafts/**"]
`)

	comp, err := h.app.Compile(h.res, CompileOpts{})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	layer := scopeLayerOf(t, comp)
	if !strings.Contains(layer.Text, "docs/**") || !strings.Contains(layer.Text, "**/drafts/**") {
		t.Fatalf("the CONTEXT.md scope section did not reach the prompt:\n%s", layer.Text)
	}
	if !strings.Contains(layer.Path, "CONTEXT.md") {
		t.Fatalf("scope layer path = %q, want the CONTEXT.md it came from", layer.Path)
	}

	enforced := scope.FromWorkspace(h.app.Cfg, h.app.Workspace)
	if !enforced.AllowsPath("docs/intro.md") {
		t.Fatal("the prompt says docs/** is in scope but enforcement denies it")
	}
	if enforced.AllowsPath("docs/drafts/wip.md") {
		t.Fatal("the prompt says the drafts deny list applies but enforcement allows it")
	}
	if !strings.Contains(layer.Text, "**/drafts/**") {
		t.Fatalf("the deny entry is not stated in the prompt:\n%s", layer.Text)
	}
}

// Cache stability at the application level: recompiling the same workspace is
// byte-identical, and a scope edit is the only thing that moves the hash.
func TestCompileScopeLayerIsCacheStable(t *testing.T) {
	h := newScopeHarness(t)
	h.app.Cfg.Scope = config.Scope{Paths: []string{"src/**"}}

	base, err := h.app.Compile(h.res, CompileOpts{Task: "read a file"})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	again, err := h.app.Compile(h.res, CompileOpts{Task: "read a different file"})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	// The stack hash covers the task layer, so a new task moves it; the cached
	// prefix must not.
	if scopeLayerOf(t, base).Hash != scopeLayerOf(t, again).Hash {
		t.Fatal("the scope layer moved on a task-only turn")
	}
	if got := prompt.PrefixChanged(base.Manifest, again.Manifest); len(got) != 0 {
		t.Fatalf("a task change invalidated the cache prefix: %v", got)
	}

	h.app.Cfg.Scope = config.Scope{Paths: []string{"src/**", "docs/**"}}
	edited, err := h.app.Compile(h.res, CompileOpts{Task: "read a file"})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if edited.Manifest.StackHash == base.Manifest.StackHash {
		t.Fatal("a [scope] edit did not change the stack hash")
	}
	got := prompt.PrefixChanged(base.Manifest, edited.Manifest)
	if len(got) != 1 || got[0] != "scope" {
		t.Fatalf("scope edit reported as %v, want exactly [scope]", got)
	}
	diff := strings.Join(prompt.Diff(base.Manifest, edited.Manifest), "\n")
	if !strings.Contains(diff, "~ scope (") {
		t.Fatalf("/prompt diff does not catch the scope change:\n%s", diff)
	}
}
