package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTier drops one document into the workspace's model-tier directory.
func writeTier(t *testing.T, ws, name, content string) {
	t.Helper()
	dir := filepath.Join(ws, ".kintsugi", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestModelKeyedAgentsTierReachesThePrompt is the I-80 contract: a document
// keyed to the resolved model's family is part of the compiled prompt, labelled
// as the model tier, and sits between the global and the project tier.
func TestModelKeyedAgentsTierReachesThePrompt(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ws := t.TempDir()
	app, err := New(ws)
	if err != nil {
		t.Fatal(err)
	}
	res, err := app.Resolve("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// Key the document to whatever family this workspace resolves to, so the
	// test is about the mechanism and not about a particular provider.
	writeTier(t, ws, "matched.md",
		"---\nnote: model tier\nmatch:\n  family: ["+res.Model.Family+"]\n---\nMODEL-TIER-BODY\n")
	if err := os.WriteFile(filepath.Join(ws, "AGENTS.md"), []byte("PROJECT-TIER-BODY\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	comp, err := app.Compile(res, CompileOpts{})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !strings.Contains(comp.Text, "MODEL-TIER-BODY") {
		t.Fatalf("the model-keyed tier did not reach the compiled prompt:\n%s", comp.Text)
	}
	if !strings.Contains(comp.Text, "PROJECT-TIER-BODY") {
		t.Fatal("the project tier stopped reaching the compiled prompt")
	}
	// Most specific last: the model tier must come before the project tier.
	if mi, pi := strings.Index(comp.Text, "MODEL-TIER-BODY"), strings.Index(comp.Text, "PROJECT-TIER-BODY"); mi > pi {
		t.Fatalf("tier order is wrong: model at %d, project at %d", mi, pi)
	}
	if !strings.Contains(comp.Text, "AGENTS.md (model)") {
		t.Fatal("the model tier is not labelled in the prompt")
	}
}

// A document whose match rules do not name this model must not be injected: a
// per-model instruction applied to the wrong model is worse than none.
func TestNonMatchingModelTierIsNotInjected(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ws := t.TempDir()
	app, err := New(ws)
	if err != nil {
		t.Fatal(err)
	}
	res, err := app.Resolve("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	writeTier(t, ws, "other.md",
		"---\nmatch:\n  id_glob: [\"definitely-not-this-model-*\"]\n---\nWRONG-MODEL-BODY\n")

	comp, err := app.Compile(res, CompileOpts{})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if strings.Contains(comp.Text, "WRONG-MODEL-BODY") {
		t.Fatal("a document keyed to another model was injected")
	}
}

// The explicit override is the TUI's "use this document" action and must win
// over the match rules.
func TestModelDocOverrideForcesADocument(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ws := t.TempDir()
	app, err := New(ws)
	if err != nil {
		t.Fatal(err)
	}
	res, err := app.Resolve("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	writeTier(t, ws, "forced.md", "---\nmatch:\n  id_glob: [\"never-matches-*\"]\n---\nFORCED-BODY\n")

	app.SetModelDocOverride("forced")
	comp, err := app.Compile(res, CompileOpts{})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !strings.Contains(comp.Text, "FORCED-BODY") {
		t.Fatal("the override did not force the document into the prompt")
	}

	app.SetModelDocOverride("")
	if _, err := app.Compile(res, CompileOpts{}); err != nil {
		t.Fatalf("Compile after clearing the override: %v", err)
	}
}

// ModelDocs backs the TUI list: it must report the documents, the one selected,
// and the warnings, without re-reading the directory per render.
func TestModelDocsReportsSelectionAndWarnings(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ws := t.TempDir()
	app, err := New(ws)
	if err != nil {
		t.Fatal(err)
	}
	res, err := app.Resolve("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	writeTier(t, ws, "good.md",
		"---\nmatch:\n  family: ["+res.Model.Family+"]\n---\nBODY\n")
	writeTier(t, ws, "bad.md", "---\nnote: [unclosed\n---\nBODY\n")
	if _, err := app.Compile(res, CompileOpts{}); err != nil {
		t.Fatalf("Compile: %v", err)
	}

	docs, warns, sel := app.ModelDocs()
	if len(docs) != 1 || docs[0].Name != "good" {
		t.Fatalf("docs = %+v", docs)
	}
	if len(warns) == 0 {
		t.Fatal("the malformed document was not reported")
	}
	if sel.Absent || sel.Doc == nil || sel.Rule != "family" {
		t.Fatalf("selection = %+v", sel)
	}
}

func TestModelDocPathLivesBesideTheWorkspace(t *testing.T) {
	ws := t.TempDir()
	app, err := New(ws)
	if err != nil {
		t.Fatal(err)
	}
	got := app.ModelDocPath("qwen")
	want := filepath.Join(ws, ".kintsugi", "agents", "qwen.md")
	if got != want {
		t.Fatalf("ModelDocPath = %q, want %q", got, want)
	}
	// A name that already carries the suffix is not doubled.
	if again := app.ModelDocPath("qwen.md"); again != want {
		t.Fatalf("ModelDocPath doubled the suffix: %q", again)
	}
}
