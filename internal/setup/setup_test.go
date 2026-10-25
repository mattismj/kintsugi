package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaterializeWritesPackAndSkipsExisting(t *testing.T) {
	root := t.TempDir()
	written, skipped, err := Materialize(root, false)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("first run skipped %v", skipped)
	}
	for _, rel := range []string{"agents/global/_base.md", "agents/global/local-small.md", "agents/behavior/coder-pipeline.md"} {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Errorf("missing %s: %v", rel, err)
		}
	}
	if len(written) == 0 {
		t.Fatal("expected written files")
	}
	written2, skipped2, err := Materialize(root, false)
	if err != nil {
		t.Fatalf("second materialize: %v", err)
	}
	if len(written2) != 0 || len(skipped2) == 0 {
		t.Fatalf("second run should skip everything, wrote=%d skipped=%d", len(written2), len(skipped2))
	}
	// force overwrites
	written3, _, err := Materialize(root, true)
	if err != nil || len(written3) == 0 {
		t.Fatalf("force should rewrite, wrote=%d err=%v", len(written3), err)
	}
}

func TestMaterializeDoesNotFollowSymlink(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "pack")
	if err := os.MkdirAll(filepath.Join(real, "global"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "global", "_base.md"), []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(root, "agents")); err != nil {
		t.Fatal(err)
	}
	// Without force: skipped, link untouched, target unchanged.
	_, skipped, err := Materialize(root, false)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if len(skipped) == 0 {
		t.Fatal("expected a symlink skip note")
	}
	if b, _ := os.ReadFile(filepath.Join(real, "global", "_base.md")); string(b) != "original" {
		t.Fatal("symlink target was modified")
	}
	// With force: the link becomes a real directory copy.
	if _, _, err := Materialize(root, true); err != nil {
		t.Fatalf("force materialize: %v", err)
	}
	fi, err := os.Lstat(filepath.Join(root, "agents"))
	if err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("agents is still a symlink (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(root, "agents", "global", "_base.md")); err != nil {
		t.Fatalf("materialized copy missing: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(real, "global", "_base.md")); string(b) != "original" {
		t.Fatal("force materialize modified the symlink target")
	}
}

func TestConfigAndAgentsText(t *testing.T) {
	cfg := ConfigText("ollama/qwen3.5:9b")
	for _, want := range []string{`mode = "normal"`, `default_model = "ollama/qwen3.5:9b"`, "persona_pack", "coder-pipeline", `reasoning_effort = "none"`} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config missing %q", want)
		}
	}
	cloud := ConfigText("deepseek/deepseek-chat")
	if strings.Contains(cloud, "reasoning_effort") {
		t.Error("cloud config should not force reasoning_effort")
	}
	agents := AgentsTemplate()
	for _, want := range []string{"Operator AGENTS.md", "Workspace rules", "Model-specific personas", "~/.config/kintsugi/agents/global"} {
		if !strings.Contains(agents, want) {
			t.Errorf("agents template missing %q", want)
		}
	}
}

func TestRunWritesAndRespectsNoClobber(t *testing.T) {
	root := t.TempDir()
	res, err := Run(root, "ollama/qwen3.5:9b", false)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := os.Stat(res.ConfigPath); err != nil {
		t.Fatalf("config not written: %v", err)
	}
	if _, err := os.Stat(res.AgentsPath); err != nil {
		t.Fatalf("agents.md not written: %v", err)
	}
	// Operator edit must survive a second non-force run.
	if err := os.WriteFile(res.AgentsPath, []byte("# my rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(root, "ollama/other:7b", false); err != nil {
		t.Fatalf("second run: %v", err)
	}
	b, _ := os.ReadFile(res.AgentsPath)
	if string(b) != "# my rules\n" {
		t.Fatalf("AGENTS.md was overwritten: %q", string(b))
	}
	if _, err := Run(root, "ollama/other:7b", true); err != nil {
		t.Fatalf("force run: %v", err)
	}
	b, _ = os.ReadFile(res.ConfigPath)
	if !strings.Contains(string(b), "ollama/other:7b") {
		t.Fatalf("force did not update config: %s", string(b))
	}
}

func TestModelRef(t *testing.T) {
	if got := ModelRef("huihui_ai/qwen3.5:9b"); got != "ollama/huihui_ai/qwen3.5:9b" {
		t.Fatalf("got %q", got)
	}
	if ModelRef("  ") != "" {
		t.Fatal("blank should be empty")
	}
}
