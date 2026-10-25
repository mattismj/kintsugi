package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kintsugi/internal/config"
)

func TestReasoningEffortForwarded(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c, err := New(config.Provider{BaseURL: srv.URL, Wire: "openai-completions", ReasoningEffort: "none"}, "m", "normal")
	if err != nil {
		t.Fatal(err)
	}
	msg, err := c.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, StreamCallbacks{})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if msg.Content != "ok" {
		t.Fatalf("content = %q", msg.Content)
	}
	if got["reasoning_effort"] != "none" {
		t.Fatalf("reasoning_effort not forwarded: %v", got["reasoning_effort"])
	}
}

func TestReasoningOnlyStreamGivesActionableError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"\",\"reasoning\":\"thinking only\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c, _ := New(config.Provider{BaseURL: srv.URL, Wire: "openai-completions"}, "m", "normal")
	_, err := c.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, StreamCallbacks{})
	if err == nil {
		t.Fatal("expected an error for a reasoning-only stream")
	}
	if !strings.Contains(err.Error(), "reasoning_effort") {
		t.Fatalf("error should suggest reasoning_effort: %v", err)
	}
}
