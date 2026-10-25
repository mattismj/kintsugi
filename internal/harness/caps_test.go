package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSmallModelDetection(t *testing.T) {
	small := []string{
		"huihui_ai/qwen3.5-abliterated:9b",
		"qwen3.5-abliterated:9b",
		"llama3-8b",
		"phi:3.8b",
		"tinyllama:1.1b",
	}
	large := []string{
		"qwen3-coder-plus",
		"deepseek-chat",
		"llama3-70b",
		"qwen2.5-32b-instruct",
		"glm-4.6",
	}
	for _, id := range small {
		if !smallModel(id) {
			t.Errorf("smallModel(%q) = false, want true", id)
		}
	}
	for _, id := range large {
		if smallModel(id) {
			t.Errorf("smallModel(%q) = true, want false", id)
		}
	}
}

func TestSmallModelSelectsLocalSmallPersonaAndAgentsMD(t *testing.T) {
	// Hermetic: without this the test reads the operator's real
	// ~/.config/kintsugi (a fetched models catalog changes family
	// resolution), so it passed on the dev machine and failed in CI on the
	// same code. Isolated, both environments see the same defaults.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ws := t.TempDir()
	app, err := New(ws)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	r, err := app.Resolve("ollama/huihui_ai/qwen3.5-abliterated:9b")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	comp, err := app.Compile(r, CompileOpts{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if got := comp.Manifest.Persona.Name; got != "local-small" {
		t.Fatalf("persona = %q, want local-small", got)
	}

	// A workspace AGENTS.md is an operator layer and must reach the prompt.
	if err := os.WriteFile(filepath.Join(ws, "AGENTS.md"), []byte("- BANANA operator rule\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	comp, err = app.Compile(r, CompileOpts{})
	if err != nil {
		t.Fatalf("compile with AGENTS.md: %v", err)
	}
	if !strings.Contains(comp.Text, "BANANA operator rule") {
		t.Fatal("workspace AGENTS.md was not injected")
	}
	if p := app.AgentsMDPath(); !strings.Contains(p, "AGENTS.md") {
		t.Fatalf("AgentsMDPath = %q", p)
	}
	// The local-small persona is compact; the full _base doctrine must not be
	// pulled in (that is what makes 9B models ignore tools).
	if strings.Contains(comp.Text, "## 9. Communication and tone") {
		t.Fatal("local-small unexpectedly inherited the full _base doctrine")
	}
}
