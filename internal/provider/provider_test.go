package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"kintsugi/internal/config"
)

func TestStreamParsesUsageAndReasoning(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello \"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"world\"}}],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	c, err := New(config.Provider{BaseURL: srv.URL, Wire: "openai-completions", Offline: true}, "m", "normal")
	if err != nil {
		t.Fatal(err)
	}
	var streamed string
	msg, err := c.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, StreamCallbacks{OnDelta: func(s string) { streamed += s }})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "hello world" || streamed != "hello world" {
		t.Fatalf("content = %q streamed = %q", msg.Content, streamed)
	}
	if msg.Usage == nil || msg.Usage.PromptTokens != 12 || msg.Usage.CompletionTokens != 3 {
		t.Fatalf("usage = %+v", msg.Usage)
	}
	if got["messages"] == nil {
		t.Fatal("request has no messages")
	}
}

func TestCompatNormalization(t *testing.T) {
	cases := []struct {
		name          string
		merge, dev    bool
		in            []Message
		wantRoles     []string
		wantFirstBody string
	}{
		{
			name: "merge system", merge: true,
			in:        []Message{{Role: "system", Content: "a"}, {Role: "system", Content: "b"}, {Role: "user", Content: "u"}},
			wantRoles: []string{"system", "user"}, wantFirstBody: "a\n\nb",
		},
		{
			name: "developer role", dev: true,
			in:        []Message{{Role: "system", Content: "a"}, {Role: "user", Content: "u"}},
			wantRoles: []string{"developer", "user"}, wantFirstBody: "a",
		},
		{
			name:      "leave alone",
			in:        []Message{{Role: "system", Content: "a"}, {Role: "note", Content: "n"}},
			wantRoles: []string{"system", "note"}, wantFirstBody: "a",
		},
	}
	for _, c := range cases {
		out := normalizeMessages(c.in, c.merge, c.dev)
		if len(out) != len(c.wantRoles) {
			t.Fatalf("%s: %d messages, want %d: %+v", c.name, len(out), len(c.wantRoles), out)
		}
		for i := range out {
			if out[i].Role != c.wantRoles[i] {
				t.Fatalf("%s: role %d = %s, want %s", c.name, i, out[i].Role, c.wantRoles[i])
			}
		}
		if out[0].Content != c.wantFirstBody {
			t.Fatalf("%s: first content = %q", c.name, out[0].Content)
		}
	}
}
