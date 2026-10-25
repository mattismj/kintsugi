package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BurntSushi/toml"

	"kintsugi/internal/config"
)

// sseHandler streams a one-chunk completion.
func sseHandler(answer string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", answer)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{}}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

func statusHandler(code int, body string) (http.HandlerFunc, *int32) {
	var hits int32
	return func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(code)
		fmt.Fprint(w, body)
	}, &hits
}

func testClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	c, err := New(config.Provider{BaseURL: baseURL, Wire: "openai-completions", Offline: true}, "m", "normal")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRetryable(t *testing.T) {
	retry := []error{
		fmt.Errorf("provider 429: rate limited"),
		fmt.Errorf("provider 500: boom"),
		fmt.Errorf("provider 503: overloaded"),
		fmt.Errorf("provider 599: weird"),
		context.DeadlineExceeded,
		fmt.Errorf("stream: %w", context.DeadlineExceeded),
		fmt.Errorf("dial tcp: service unavailable"),
		fmt.Errorf("provider quota exceeded for today"),
		fmt.Errorf("upstream rate limit hit"),
	}
	for _, err := range retry {
		if !Retryable(err) {
			t.Errorf("Retryable(%v) = false, want true", err)
		}
	}
	fatal := []error{
		nil,
		fmt.Errorf("provider 400: bad request"),
		fmt.Errorf("provider 401: unauthorized"),
		fmt.Errorf("provider 403: forbidden"),
		fmt.Errorf("provider 404: no such model"),
		fmt.Errorf("unknown wire %q", "grpc"),
		fmt.Errorf("provider needs DEEPSEEK_API_KEY (export it in the environment)"),
		errors.New("empty stream: model returned neither content nor tool calls"),
	}
	for _, err := range fatal {
		if Retryable(err) {
			t.Errorf("Retryable(%v) = true, want false", err)
		}
	}
}

func TestStreamFallsBackOn429(t *testing.T) {
	bad, badHits := statusHandler(429, `{"error":"rate limit exceeded"}`)
	srv1 := httptest.NewServer(bad)
	defer srv1.Close()
	srv2 := httptest.NewServer(sseHandler("answer from slot 2"))
	defer srv2.Close()

	primary := testClient(t, srv1.URL)
	secondary := testClient(t, srv2.URL)
	primary.SetFallbacks([]*Client{secondary})

	var logs []map[string]string
	primary.Logf = func(kind string, v any) {
		if kind != "fallback" {
			t.Errorf("log kind = %q, want fallback", kind)
			return
		}
		m, ok := v.(map[string]string)
		if !ok {
			t.Fatalf("log value type = %T", v)
		}
		logs = append(logs, m)
	}

	var streamed strings.Builder
	msg, err := primary.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil,
		StreamCallbacks{OnDelta: func(s string) { streamed.WriteString(s) }})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "answer from slot 2" {
		t.Fatalf("content = %q", msg.Content)
	}
	if streamed.String() != "answer from slot 2" {
		t.Fatalf("streamed = %q (failed attempt text must not leak)", streamed.String())
	}
	if n := atomic.LoadInt32(badHits); n != 1 {
		t.Fatalf("primary hits = %d, want 1", n)
	}
	if len(logs) != 1 {
		t.Fatalf("fallback logs = %d, want 1: %+v", len(logs), logs)
	}
	if !strings.Contains(logs[0]["reason"], "429") {
		t.Fatalf("reason = %q, want the 429", logs[0]["reason"])
	}
	if !strings.Contains(logs[0]["from"], srv1.URL) || !strings.Contains(logs[0]["to"], srv2.URL) {
		t.Fatalf("from/to = %+v", logs[0])
	}
	if msg.Usage == nil || msg.Usage.PromptTokens != 3 {
		t.Fatalf("usage = %+v", msg.Usage)
	}
}

func TestStreamFailsFastOnNonRetryable(t *testing.T) {
	bad, badHits := statusHandler(401, `{"error":"bad key"}`)
	srv1 := httptest.NewServer(bad)
	defer srv1.Close()
	srv2Hits := new(int32)
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(srv2Hits, 1)
		sseHandler("should not happen")(w, r)
	}))
	defer srv2.Close()

	primary := testClient(t, srv1.URL)
	primary.SetFallbacks([]*Client{testClient(t, srv2.URL)})
	var logged int
	primary.Logf = func(kind string, v any) { logged++ }

	_, err := primary.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, StreamCallbacks{})
	if err == nil {
		t.Fatal("401 should fail")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v", err)
	}
	if n := atomic.LoadInt32(badHits); n != 1 {
		t.Fatalf("primary hits = %d", n)
	}
	if n := atomic.LoadInt32(srv2Hits); n != 0 {
		t.Fatalf("fallback hit %d times on a non-retryable error", n)
	}
	if logged != 0 {
		t.Fatalf("logged %d fallbacks on a non-retryable error", logged)
	}
}

// A retryable failure on a middle slot advances to the next one, and only the
// winning slot's text reaches onDelta.
func TestStreamFallbackSkipsDeadSlots(t *testing.T) {
	h1, hits1 := statusHandler(429, "slow down")
	srv1 := httptest.NewServer(h1)
	defer srv1.Close()
	h2, hits2 := statusHandler(503, "overloaded")
	srv2 := httptest.NewServer(h2)
	defer srv2.Close()
	srv3 := httptest.NewServer(sseHandler("slot 3 wins"))
	defer srv3.Close()

	primary := testClient(t, srv1.URL)
	primary.SetFallbacks([]*Client{testClient(t, srv2.URL), nil, testClient(t, srv3.URL)})
	var logs int
	primary.Logf = func(kind string, v any) { logs++ }

	var streamed strings.Builder
	msg, err := primary.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil,
		StreamCallbacks{OnDelta: func(s string) { streamed.WriteString(s) }})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "slot 3 wins" || streamed.String() != "slot 3 wins" {
		t.Fatalf("content = %q streamed = %q", msg.Content, streamed.String())
	}
	if atomic.LoadInt32(hits1) != 1 || atomic.LoadInt32(hits2) != 1 {
		t.Fatalf("hits = %d/%d", atomic.LoadInt32(hits1), atomic.LoadInt32(hits2))
	}
	if logs != 2 {
		t.Fatalf("logs = %d, want 2 (one per slot change)", logs)
	}
}

func TestStreamAllSlotsFail(t *testing.T) {
	h1, _ := statusHandler(500, "boom")
	srv1 := httptest.NewServer(h1)
	defer srv1.Close()
	h2, _ := statusHandler(429, "later")
	srv2 := httptest.NewServer(h2)
	defer srv2.Close()

	primary := testClient(t, srv1.URL)
	primary.SetFallbacks([]*Client{testClient(t, srv2.URL)})
	_, err := primary.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, StreamCallbacks{})
	if err == nil {
		t.Fatal("expected failure")
	}
	for _, want := range []string{"1 fallback(s) exhausted", "500", "429"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want %q", err, want)
		}
	}
}

func TestStreamWithoutFallbacksReturnsPrimaryError(t *testing.T) {
	h, _ := statusHandler(503, "boom")
	srv := httptest.NewServer(h)
	defer srv.Close()
	c := testClient(t, srv.URL)
	if _, err := c.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, StreamCallbacks{}); err == nil {
		t.Fatal("expected error")
	} else if strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("a chain of one should return the plain error: %v", err)
	}
}

func TestStreamStopsWhenCallerCancels(t *testing.T) {
	h, _ := statusHandler(503, "boom")
	srv := httptest.NewServer(h)
	defer srv.Close()
	srv2Hits := new(int32)
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(srv2Hits, 1)
		sseHandler("nope")(w, r)
	}))
	defer srv2.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	primary := testClient(t, srv.URL)
	primary.SetFallbacks([]*Client{testClient(t, srv2.URL)})
	if _, err := primary.Stream(ctx, []Message{{Role: "user", Content: "hi"}}, nil, StreamCallbacks{}); err == nil {
		t.Fatal("expected error")
	}
	if n := atomic.LoadInt32(srv2Hits); n != 0 {
		t.Fatalf("cancelled caller still walked %d fallback(s)", n)
	}
}

// [chains] parses as an ordered slot list and defaults to nil.
func TestChainConfigParse(t *testing.T) {
	body := `
[chains.fast]
providers = ["deepseek/deepseek-chat", "groq/llama-3.3-70b", "ollama/qwen3:8b"]

[chains.empty]
providers = []
`
	var f config.File
	if _, err := toml.Decode(body, &f); err != nil {
		t.Fatal(err)
	}
	if got := f.Chains["fast"].Providers; len(got) != 3 || got[0] != "deepseek/deepseek-chat" || got[2] != "ollama/qwen3:8b" {
		t.Fatalf("chains = %+v", f.Chains)
	}
	if len(f.Chains["empty"].Providers) != 0 {
		t.Fatalf("empty chain = %+v", f.Chains["empty"])
	}
	def := config.Defaults()
	if def.Chains != nil {
		t.Fatalf("defaults should not invent chains: %+v", def.Chains)
	}
}

// A chain slot list resolves through the same provider/model parser the rest of
// the config uses, so a typo in a slot is caught before any request is made.
func TestChainSlotsResolveThroughConfig(t *testing.T) {
	f := config.Defaults()
	if _, err := toml.Decode(`
[chains.fast]
providers = ["deepseek/deepseek-chat", "groq/llama-3.3-70b"]
`, &f); err != nil {
		t.Fatal(err)
	}
	for _, slot := range f.Chains["fast"].Providers {
		if _, _, _, err := f.ResolveModel(slot); err != nil {
			t.Fatalf("slot %q: %v", slot, err)
		}
	}
	if _, _, _, err := f.ResolveModel("deepseek/nope"); err == nil {
		t.Fatal("curated provider typo should error")
	}
}
