package harness

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"kintsugi/internal/adapters"
	"kintsugi/internal/config"
	"kintsugi/internal/session"
)

// curatorApp builds an App whose curator model answers with the given content.
// The returned counter reports how many requests actually reached a provider.
func curatorApp(t *testing.T, content string) (*App, *int32) {
	t.Helper()
	ws := t.TempDir()
	write(t, filepath.Join(ws, "CONTEXT.md"), `# Project: lab

## Authorization & scope

- AUTH: authorized local sandbox work only.

## Current state

- stale bullet
`)

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		b, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": content}}},
		})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
	}))
	t.Cleanup(srv.Close)

	reg, err := adapters.Load()
	if err != nil {
		t.Fatal(err)
	}
	app := &App{Workspace: ws, Adapters: reg}
	app.Cfg.Core.Mode = "normal"
	app.Cfg.Context.Enabled = true
	app.Cfg.Context.Path = "CONTEXT.md"
	app.Cfg.Context.AutoAcceptClasses = []string{"state"}
	app.Cfg.Context.CuratorModel = "mock/curator"
	app.Cfg.Providers = map[string]config.Provider{
		"mock": {
			BaseURL: srv.URL, Wire: "openai-completions", Family: "qwen", Offline: true,
			Models: []config.Model{{ID: "curator", Family: "qwen", Wire: "openai-completions"}},
		},
	}
	return app, &calls
}

// testSession creates (and returns) a session with one operator turn.
func testSession(t *testing.T) string {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	s, err := session.New()
	if err != nil {
		t.Fatal(err)
	}
	s.Log("user", "task: build the fixture")
	s.Log("assistant", "wrote the fixture")
	s.Close()
	return s.ID
}

func TestMaybeCurateDisabledDoesNothing(t *testing.T) {
	app, calls := curatorApp(t, `[{"section":"state","op":"replace","content":"- x","confidence":"high"}]`)
	app.Cfg.Context.CuratorOnSessionEnd = false

	if res := MaybeCurate(app, "20260101-000000-aaaaaa"); res != nil {
		t.Fatalf("disabled curation returned a result: %+v", res)
	}
	if n := atomic.LoadInt32(calls); n != 0 {
		t.Fatalf("disabled curation called the provider %d times", n)
	}
	if _, err := os.Stat(filepath.Join(app.Workspace, ".kintsugi")); !os.IsNotExist(err) {
		t.Fatal("disabled curation wrote workspace state")
	}
}

func TestMaybeCurateWithoutModelDoesNothing(t *testing.T) {
	app, calls := curatorApp(t, `[]`)
	app.Cfg.Context.CuratorOnSessionEnd = true
	app.Cfg.Context.CuratorModel = ""

	if res := MaybeCurate(app, "20260101-000000-aaaaaa"); res != nil {
		t.Fatalf("unconfigured curation returned a result: %+v", res)
	}
	if n := atomic.LoadInt32(calls); n != 0 {
		t.Fatalf("unconfigured curation called the provider %d times", n)
	}
}

func TestMaybeCurateWritesProposalAndAppliesState(t *testing.T) {
	app, calls := curatorApp(t, `[{"section":"state","op":"replace","content":"- M7 refresh wiring landed","reason":"session","confidence":"high"}]`)
	app.Cfg.Context.CuratorOnSessionEnd = true
	sid := testSession(t)

	res := MaybeCurate(app, sid)
	if res == nil {
		t.Fatal("curation returned nil for an enabled, configured app")
	}
	if n := atomic.LoadInt32(calls); n != 1 {
		t.Fatalf("provider calls = %d, want 1", n)
	}
	if res.Proposals != 1 {
		t.Fatalf("proposals = %d, want 1", res.Proposals)
	}
	if _, err := os.Stat(res.ProposalPath); err != nil {
		t.Fatalf("proposal file missing: %v", err)
	}
	if len(res.Apply.Applied) != 1 {
		t.Fatalf("state proposal was not auto-applied: %+v", res.Apply)
	}

	raw, err := os.ReadFile(filepath.Join(app.Workspace, "CONTEXT.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(raw), "M7 refresh wiring landed") {
		t.Fatalf("state section not updated:\n%s", raw)
	}
	// Authorization stays operator-authored even when a session ends.
	if !contains(string(raw), "AUTH: authorized local sandbox work only.") {
		t.Fatalf("authorization body changed:\n%s", raw)
	}
}

func TestMaybeCurateSwallowsProviderErrors(t *testing.T) {
	app, calls := curatorApp(t, "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		http.Error(w, "no curator today", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	app.Cfg.Providers = map[string]config.Provider{
		"mock": {
			BaseURL: srv.URL, Wire: "openai-completions", Family: "qwen", Offline: true,
			Models: []config.Model{{ID: "curator", Family: "qwen", Wire: "openai-completions"}},
		},
	}
	app.Cfg.Context.CuratorOnSessionEnd = true
	sid := testSession(t)

	if res := MaybeCurate(app, sid); res != nil {
		t.Fatalf("a failing curator must return nil, got %+v", res)
	}
	if n := atomic.LoadInt32(calls); n != 1 {
		t.Fatalf("provider calls = %d, want 1", n)
	}
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
