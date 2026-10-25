package harness

// WP-1.6 at the application level: the knowledge bases bound to the reviewing
// role become citable chunk ids in the gate reviewer's prompt, and a workspace
// with no binding produces the reviewer prompt it always produced.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"kintsugi/internal/adapters"
	"kintsugi/internal/config"
	"kintsugi/internal/prompt"
)

// gateKBApp builds an App whose gate model answers with content and whose
// knowledge bases are supplied by the caller.
func gateKBApp(t *testing.T, content string, kbs map[string]config.KB) (*App, *string) {
	t.Helper()
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		raw, _ := json.Marshal(req["messages"])
		seen = string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": content}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
	}))
	t.Cleanup(srv.Close)

	reg, err := adapters.Load()
	if err != nil {
		t.Fatal(err)
	}
	app := &App{Cfg: config.Defaults(), Adapters: reg, Workspace: t.TempDir()}
	app.Cfg.KBs = kbs
	app.Cfg.Gates.Model = "mock/gate"
	app.Cfg.Providers = map[string]config.Provider{
		"mock": {
			BaseURL: srv.URL, Wire: "openai-completions", Family: "qwen", Offline: true,
			Models: []config.Model{{ID: "gate", Family: "qwen", Wire: "openai-completions"}},
		},
	}
	return app, &seen
}

// TestGateReviewerCitesRoleBoundKBChunks proves the whole path: config binding →
// knowledge() → the reviewer prompt. The bound base's chunk ids appear as
// citable material and a citation round-trips into the finding.
func TestGateReviewerCitesRoleBoundKBChunks(t *testing.T) {
	dir := kbFixture(t, filepath.Join(t.TempDir(), "attack"))
	app, seen := gateKBApp(t, `{"findings":[{"severity":"medium","file":"agent.py","line":4,"consequence":"obfuscated loader matches a known technique","kb":["attack/T1027"]}],"verdict":"revise"}`,
		map[string]config.KB{"attack": {Path: dir, Strategy: "always", TopK: 2}})

	rev, err := app.GateReviewer()
	if err != nil {
		t.Fatal(err)
	}
	if rev.Knowledge == nil {
		t.Fatal("the gate reviewer has no knowledge hook")
	}
	out, err := rev.Review(context.Background(), "diff --git a/agent.py b/agent.py\n+obfuscated loader", "lab only")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"attack/T1027", "attack/T1055", "Citable knowledge-base chunks"} {
		if !strings.Contains(*seen, want) {
			t.Fatalf("reviewer prompt is missing the bound chunk %q:\n%s", want, *seen)
		}
	}
	// The chunk body itself is not smuggled in: only the ids are offered, so the
	// reviewer cites what it was shown without the gate re-injecting the corpus.
	if strings.Contains(*seen, "Adversaries may attempt") {
		t.Fatalf("the gate prompt carries chunk bodies:\n%s", *seen)
	}
	if len(out.Findings) != 1 || strings.Join(out.Findings[0].KB, ",") != "attack/T1027" {
		t.Fatalf("citation did not round-trip through the gate: %+v", out.Findings)
	}
}

// TestGateReviewerPromptUnchangedWithoutBinding proves the no-KB honesty clause
// at the app level: nothing bound, nothing changes.
func TestGateReviewerPromptUnchangedWithoutBinding(t *testing.T) {
	app, seen := gateKBApp(t, `{"findings":[],"verdict":"pass"}`, nil)
	rev, err := app.GateReviewer()
	if err != nil {
		t.Fatal(err)
	}
	if got := rev.Knowledge("any artifact"); len(got) != 0 {
		t.Fatalf("an unbound role offered citations: %v", got)
	}
	if _, err := rev.Review(context.Background(), "diff --git a/x b/x\n+y", "lab only"); err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"Knowledge bases", "Citable", "attack/"} {
		if strings.Contains(*seen, banned) {
			t.Fatalf("an unbound gate prompt carries %q:\n%s", banned, *seen)
		}
	}

	// A binding scoped to another role must not leak in either.
	dir := kbFixture(t, filepath.Join(t.TempDir(), "attack"))
	other, seenOther := gateKBApp(t, `{"findings":[],"verdict":"pass"}`,
		map[string]config.KB{"attack": {Path: dir, Strategy: "always", TopK: 1, Roles: []string{"builder"}}})
	revOther, err := other.GateReviewer()
	if err != nil {
		t.Fatal(err)
	}
	if got := revOther.Knowledge("diff"); len(got) != 0 {
		t.Fatalf("a base bound to another role was offered to the gate: %v", got)
	}
	if _, err := revOther.Review(context.Background(), "diff --git a/x b/x\n+y", "lab only"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(*seenOther, "attack/") {
		t.Fatalf("another role's base leaked into the gate prompt:\n%s", *seenOther)
	}
}

// TestGateKnowledgeCapsAndFormatsCitations pins the citation form and the order
// citations are offered in.
func TestGateKnowledgeCapsAndFormatsCitations(t *testing.T) {
	dir := kbFixture(t, filepath.Join(t.TempDir(), "attack"))
	app, _ := gateKBApp(t, `{"findings":[],"verdict":"pass"}`,
		map[string]config.KB{"attack": {Path: dir, Strategy: "always", TopK: 3}})

	got := app.GateKnowledge(gateReviewRole)("diff")
	want := []string{"attack/T1027", "attack/T1055", "attack/T1059.001"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("citations = %v, want %v", got, want)
	}
}

// TestKBCitationsCapsAndDegrades covers the flattening rule directly: base-prefixed
// ids, bare ids when a layer has no base id, and truncation at the cap.
func TestKBCitationsCapsAndDegrades(t *testing.T) {
	if got := kbCitations(nil, 16); got != nil {
		t.Fatalf("no meta produced %v", got)
	}
	got := kbCitations([]prompt.KBLayer{{ID: "attack", Chunks: []string{"T1027", "T1055", "T1059.001"}}}, 2)
	if strings.Join(got, ",") != "attack/T1027,attack/T1055" {
		t.Fatalf("cap not applied: %v", got)
	}
	bare := kbCitations([]prompt.KBLayer{{Chunks: []string{"T1027"}}, {ID: "cwe", Chunks: []string{"CWE-78"}}}, 16)
	if strings.Join(bare, ",") != "T1027,cwe/CWE-78" {
		t.Fatalf("a layer without a base id should yield bare chunk ids: %v", bare)
	}
	if empty := kbCitations([]prompt.KBLayer{{ID: "attack", Chunks: []string{"", "  "}}}, 16); len(empty) != 0 {
		t.Fatalf("blank chunk ids became citations: %v", empty)
	}
}
