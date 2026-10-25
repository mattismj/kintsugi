package provider

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// refusalSSE streams one chunk and ends with a refusal finish reason.
func refusalSSE(reason, content string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q,\"finish_reason\":%q}}]}\n\n", content, reason)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

func TestRefusalFinish(t *testing.T) {
	for _, r := range []string{"content_filter", "content-filter", "refusal", "sensitive", "response_refusal", "RESPONSEREFUSAL"} {
		if !RefusalFinish(r) {
			t.Errorf("RefusalFinish(%q) = false", r)
		}
	}
	for _, r := range []string{"", "stop", "length", "tool_use", "tool_calls", "end_turn"} {
		if RefusalFinish(r) {
			t.Errorf("RefusalFinish(%q) = true", r)
		}
	}
}

func TestStreamRefusalWalksTheFallbackChain(t *testing.T) {
	var primary, fallback int32
	srvA := httptest.NewServer(refusalSSE("content_filter", "I can't help with that."))
	t.Cleanup(srvA.Close)
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fallback, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"sure, here is the plan\",\"finish_reason\":\"stop\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srvB.Close)

	c := testClient(t, srvA.URL)
	fb := testClient(t, srvB.URL)
	fb.Model = "sibling"
	c.SetFallbacks([]*Client{fb})

	var got strings.Builder
	msg, err := c.StreamRefusal(t.Context(), []Message{{Role: "user", Content: "x"}}, nil, StreamCallbacks{OnDelta: func(s string) { got.WriteString(s) }})
	if err != nil {
		t.Fatalf("StreamRefusal: %v", err)
	}
	if msg.Content != "sure, here is the plan" {
		t.Fatalf("content = %q, want the sibling's answer", msg.Content)
	}
	if atomic.LoadInt32(&fallback) != 1 {
		t.Fatalf("fallback hit %d times", atomic.LoadInt32(&fallback))
	}
	_ = primary
	// The sibling answer is delivered through onDelta exactly once.
	if got.String() != "sure, here is the plan" {
		t.Fatalf("onDelta = %q", got.String())
	}
}

func TestStreamRefusalStopsWhenEverySlotRefuses(t *testing.T) {
	srvA := httptest.NewServer(refusalSSE("refusal", "no"))
	defer srvA.Close()
	srvB := httptest.NewServer(refusalSSE("sensitive", "no"))
	defer srvB.Close()

	c := testClient(t, srvA.URL)
	fb := testClient(t, srvB.URL)
	fb.Model = "sibling"
	c.SetFallbacks([]*Client{fb})

	msg, err := c.StreamRefusal(t.Context(), []Message{{Role: "user", Content: "x"}}, nil, StreamCallbacks{})
	if err != nil {
		t.Fatalf("StreamRefusal: %v", err)
	}
	if !RefusalFinish(msg.FinishReason) {
		t.Fatalf("expected the last refusal, got finish %q", msg.FinishReason)
	}
}

func TestStreamRefusalDoesNotWalkOnARealError(t *testing.T) {
	srvA := httptest.NewServer(refusalSSE("content_filter", "no"))
	defer srvA.Close()
	srvB := httptest.NewServer(refusalSSE("stop", "ok"))
	defer srvB.Close()

	c := testClient(t, srvA.URL)
	fb := testClient(t, srvB.URL)
	fb.Model = "sibling"
	c.SetFallbacks([]*Client{fb})

	msg, err := c.StreamRefusal(t.Context(), []Message{{Role: "user", Content: "x"}}, nil, StreamCallbacks{})
	if err != nil {
		t.Fatalf("err should be nil (refusal is not an error): %v", err)
	}
	if msg.Content != "ok" {
		t.Fatalf("content = %q", msg.Content)
	}
}

func TestStreamRefusalNeverRevisitsAModel(t *testing.T) {
	// A chain A -> A (same model, different URL) must not visit the second A:
	// tried is keyed by model, not base URL.
	var second int32
	srvA := httptest.NewServer(refusalSSE("refusal", "no"))
	defer srvA.Close()
	srvA2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&second, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\",\"finish_reason\":\"stop\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srvA2.Close()

	c := testClient(t, srvA.URL)
	fb := testClient(t, srvA2.URL)
	fb.Model = c.Model // same model id — must be skipped
	c.SetFallbacks([]*Client{fb})

	msg, err := c.StreamRefusal(t.Context(), []Message{{Role: "user", Content: "x"}}, nil, StreamCallbacks{})
	if err != nil {
		t.Fatalf("StreamRefusal: %v", err)
	}
	if RefusalFinish(msg.FinishReason) != true {
		t.Fatalf("expected the refusal to persist, got %q", msg.Content)
	}
	if atomic.LoadInt32(&second) != 0 {
		t.Fatalf("the same model was revisited: %d", atomic.LoadInt32(&second))
	}
}
