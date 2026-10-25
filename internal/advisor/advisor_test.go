package advisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kintsugi/internal/config"
	"kintsugi/internal/provider"
)

func testClient(t *testing.T, content string) *provider.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["tools"] != nil {
			t.Errorf("advisor must not send tools: %v", req["tools"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		b, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": content}}},
		})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
	}))
	t.Cleanup(srv.Close)
	c, err := provider.New(config.Provider{BaseURL: srv.URL, Wire: "openai-completions", Offline: true}, "advisor-test", "normal")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestReviewParsesNotes(t *testing.T) {
	c := testClient(t, `{"notes":[{"kind":"concern","text":"callback IP is hardcoded"},{"kind":"blocker","text":"target outside declared scope"},{"kind":"weird","text":"normalized"}]}`)
	a := New(c, 20000)
	notes, err := a.Review(context.Background(), []provider.Message{
		{Role: "user", Content: "build the fixture"},
		{Role: "assistant", Content: "here is the script"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 3 {
		t.Fatalf("notes = %d, want 3: %+v", len(notes), notes)
	}
	if notes[2].Kind != KindConcern {
		t.Fatalf("unknown kind not normalized: %+v", notes[2])
	}
	blockers := Blockers(notes)
	if len(blockers) != 1 || !strings.Contains(Feedback(blockers), "outside declared scope") {
		t.Fatalf("blockers/feedback wrong: %+v / %q", blockers, Feedback(blockers))
	}
	if a.Used() == 0 {
		t.Fatal("usage not accounted")
	}
}

func TestParseFencedAndEmpty(t *testing.T) {
	notes, err := Parse("```json\n{\"notes\":[{\"kind\":\"aside\",\"text\":\"ok\"}]}\n```")
	if err != nil || len(notes) != 1 || notes[0].Kind != KindAside {
		t.Fatalf("fenced parse: %v %+v", err, notes)
	}
	notes, err = Parse(`{"notes":[]}`)
	if err != nil || len(notes) != 0 {
		t.Fatalf("empty parse: %v %+v", err, notes)
	}
	if _, err := Parse("I think it is fine."); err == nil {
		t.Fatal("prose accepted as JSON")
	}
	if _, err := Parse(`{"notes":[`); err == nil {
		t.Fatal("truncated JSON accepted")
	}
}

func TestReviewCap(t *testing.T) {
	c := testClient(t, `{"notes":[{"kind":"aside","text":"one"}]}`)
	a := New(c, 1)
	_, err := a.Review(context.Background(), []provider.Message{{Role: "user", Content: strings.Repeat("x", 200)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Review(context.Background(), nil); !errors.Is(err, ErrCap) {
		t.Fatalf("second review err = %v, want ErrCap", err)
	}
}
