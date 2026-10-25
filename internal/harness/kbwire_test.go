package harness

import (
	"os"
	"path/filepath"
	"testing"

	"kintsugi/internal/config"
	"kintsugi/internal/tools"
)

// AttachTools must hand the executor the knowledge bases the operator
// configured: the kb_search tool only searches bases it is told about, so an
// unwired executor would silently answer "no knowledge base is configured".
func TestAttachToolsWiresConfiguredKnowledgeBases(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if err := os.MkdirAll(filepath.Join(ws, "knowledgebases", "attack"), 0o755); err != nil {
		t.Fatal(err)
	}
	app, err := New(ws)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	app.Cfg.KBs = map[string]config.KB{
		"attack": {Path: "knowledgebases/attack", Strategy: "retrieval", TopK: 3},
		"broken": {Path: "", Strategy: "always"},
	}

	ex := &tools.Executor{Workspace: ws}
	closer := app.AttachTools(ex, nil)
	defer closer()

	if len(ex.KBs) != 1 {
		t.Fatalf("executor knowledge bases = %+v, want exactly the one with a path", ex.KBs)
	}
	if ex.KBs[0].ID != "attack" {
		t.Fatalf("knowledge base id = %q, want attack", ex.KBs[0].ID)
	}
	want := filepath.Join(ws, "knowledgebases", "attack")
	if ex.KBs[0].Dir != want {
		t.Fatalf("knowledge base dir = %q, want %q", ex.KBs[0].Dir, want)
	}
}

// The kb_search tool must be registered for every executor, so the model can
// reach an on-demand knowledge base the prompt announced.
func TestExecutorExposesKBSearchTool(t *testing.T) {
	ex := &tools.Executor{Workspace: t.TempDir()}
	found := false
	for _, d := range ex.Definitions() {
		if d.Function.Name == "kb_search" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("kb_search is not in the tool definitions")
	}
}
