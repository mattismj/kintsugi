package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"kintsugi/internal/config"
	"kintsugi/internal/harness"
	"kintsugi/internal/memory"
	"kintsugi/internal/ui"
)

func TestZZScratchRender(t *testing.T) {
	m := testModel(120, 34)
	m.theme = ui.Theme{Profile: ui.TrueColor, Ornament: true}
	m.app = &harness.App{Cfg: config.File{}, Workspace: t.TempDir()}
	m.res = &harness.Resolved{ProviderName: "deepseek", Model: config.Model{ID: "deepseek-chat", Family: "deepseek"}}
	m.blocks = nil
	m.revealStartup()
	m.addNote("· turn complete · 20261021-000000-test")
	m.tasks = append(m.tasks, taskItem{label: "build the authentication layer for the kintsugi service", state: "running"})
	fmt.Fprintln(os.Stderr, "---- 120x34 ----")
	fmt.Fprintln(os.Stderr, m.View())
}

func TestZZScratchKB(t *testing.T) {
	ws := t.TempDir()
	// copy the kb fixture into the workspace
	src := filepath.Join("..", "kb", "testdata", "basic")
	dst := filepath.Join(ws, "kb", "attack")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(src, "kb.yaml"))
	os.WriteFile(filepath.Join(dst, "kb.yaml"), raw, 0o644)
	chunks, _ := os.ReadDir(filepath.Join(src, "chunks"))
	os.MkdirAll(filepath.Join(dst, "chunks"), 0o755)
	for _, c := range chunks {
		b, _ := os.ReadFile(filepath.Join(src, "chunks", c.Name()))
		os.WriteFile(filepath.Join(dst, "chunks", c.Name()), b, 0o644)
	}

	m := testModel(120, 34)
	m.theme = ui.Theme{Profile: ui.TrueColor, Ornament: true}
	m.app = &harness.App{Cfg: config.File{
		KBs: map[string]config.KB{
			"attack": {Path: "kb/attack", Strategy: "retrieval", TopK: 3},
		},
	}, Workspace: ws}
	m.blocks = nil
	m.handleSlash("/kb")
	m.handleSlash("/kb powershell encoded command")
	for _, b := range m.blocks {
		fmt.Fprintln(os.Stderr, "---- card ----")
		fmt.Fprintln(os.Stderr, b.title)
		fmt.Fprintln(os.Stderr, b.text)
	}

	// memory
	store, err := memory.Open(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.AddLesson("never trust a config file that was written by a tired human", "deepseek", "session test"); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(memory.SkillsDir(ws), 0o755)
	os.WriteFile(filepath.Join(memory.SkillsDir(ws), "review.md"), []byte("# review\ncheck the diff before it ships\n"), 0o644)
	m.mem = store
	m.blocks = nil
	m.handleSlash("/memory")
	m.handleSlash("/memory diff")
	for _, b := range m.blocks {
		fmt.Fprintln(os.Stderr, "---- memory ----")
		fmt.Fprintln(os.Stderr, b.title)
		fmt.Fprintln(os.Stderr, b.text)
	}
}
