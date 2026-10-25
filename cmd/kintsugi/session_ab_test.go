package main

// Tests for `kintsugi session ab`: a hermetic replay against a mock SSE
// provider registered in a temp workspace config, so the two models answer
// differently and the report shows both.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kintsugi/internal/provider"
	"kintsugi/internal/session"
)

// abMock streams a canned answer chosen by the request's model field.
func abMock(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		answer := "model A answer"
		if strings.HasSuffix(req.Model, "-b") {
			answer = "model B answer"
		}
		b, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": answer}}},
		})
		fmt.Fprintf(w, "data: %s\n\n", b)
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// abWorkspace is a temp workspace whose config names the mock provider.
func abWorkspace(t *testing.T, url string) string {
	t.Helper()
	ws := t.TempDir()
	body := "[providers.mock]\nbase_url = \"" + url + "/v1\"\nwire = \"openai-completions\"\noffline = true\n"
	if err := os.WriteFile(filepath.Join(ws, "kintsugi.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestSessionABReplaysTwoModels(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	srv := abMock(t)
	ws := abWorkspace(t, srv.URL)

	// A session with a checkpoint holding a user turn.
	s, err := session.New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.SaveCheckpoint(s.ID, "before", []provider.Message{
		{Role: "user", Content: "write the scanner"},
	}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	out := captureStdout(t, func() {
		cmdSessionAB(ws, []string{s.ID, "--model-a", "mock/qwen-a", "--model-b", "mock/qwen-b"})
	})

	// Both models ran and answered; the report carries the table and both
	// outputs with the differ verdict.
	for _, want := range []string{
		"mock/qwen-a", "mock/qwen-b",
		"tokens in/out", "model A answer", "model B answer",
		"verdict: outputs differ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the A/B report missing %q:\n%s", want, out)
		}
	}
	// The original session is untouched: one checkpoint, no extra turns.
	cps, err := session.ListCheckpoints(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cps) != 1 {
		t.Fatalf("the original session gained checkpoints: %d", len(cps))
	}
}
