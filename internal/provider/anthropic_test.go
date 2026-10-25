package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kintsugi/internal/config"
)

func TestAnthropicStreamRequestAndToolUse(t *testing.T) {
	var got anthropicRequest
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if r.Header.Get("x-api-key") != "sk-test" {
			t.Errorf("x-api-key = %q", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Error("missing anthropic-version")
		}
		if r.Header.Get("Authorization") != "" {
			t.Errorf("anthropic wire must not send a bearer token, got %q", r.Header.Get("Authorization"))
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n")
		fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"HI\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_2\",\"name\":\"read\",\"input\":{}}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"pa\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"th\\\":\\\"b.md\\\"}\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_stop\",\"index\":1}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":7}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	c, err := New(config.Provider{BaseURL: srv.URL, Wire: "anthropic-messages", Offline: true}, "claude-test", "normal")
	if err != nil {
		t.Fatal(err)
	}
	c.APIKey = "sk-test"

	msgs := []Message{
		{Role: "system", Content: "sys one"},
		{Role: "system", Content: "sys two"},
		{Role: "user", Content: "hello"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "toolu_1", Type: "function", Function: Function{Name: "read", Arguments: `{"path":"a.md"}`}}}},
		{Role: "tool", ToolCallID: "toolu_1", Content: "file body"},
	}
	tools := []Tool{{
		Type: "function",
		Function: ToolFunction{
			Name: "read", Description: "read a file",
			Parameters: ToolParameters{Type: "object", Required: []string{"path"}, Properties: map[string]ToolProp{
				"path": {Type: "string", Description: "path"},
			}},
		},
	}}
	var streamed string
	msg, err := c.Stream(context.Background(), msgs, tools, StreamCallbacks{OnDelta: func(s string) { streamed += s }})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/messages" {
		t.Fatalf("path = %q", path)
	}
	if got := systemText(got.System); got != "sys one\n\nsys two" {
		t.Fatalf("system = %q", got)
	}
	if len(got.System) != 1 || got.System[0].Type != "text" {
		t.Fatalf("system blocks = %+v", got.System)
	}
	if got.MaxTokens != 4096 || !got.Stream {
		t.Fatalf("max_tokens=%d stream=%v", got.MaxTokens, got.Stream)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("messages = %d: %+v", len(got.Messages), got.Messages)
	}
	if got.Messages[0].Role != "user" || got.Messages[1].Role != "assistant" || got.Messages[2].Role != "user" {
		t.Fatalf("roles = %s/%s/%s", got.Messages[0].Role, got.Messages[1].Role, got.Messages[2].Role)
	}
	if got.Messages[1].Content[0].Type != "tool_use" || got.Messages[1].Content[0].ID != "toolu_1" {
		t.Fatalf("assistant blocks = %+v", got.Messages[1].Content)
	}
	if got.Messages[2].Content[0].Type != "tool_result" || got.Messages[2].Content[0].ToolUseID != "toolu_1" {
		t.Fatalf("tool result blocks = %+v", got.Messages[2].Content)
	}
	if len(got.Tools) != 1 || got.Tools[0].InputSchema["type"] != "object" {
		t.Fatalf("tools = %+v", got.Tools)
	}

	if msg.Content != "HI" || streamed != "HI" {
		t.Fatalf("content=%q streamed=%q", msg.Content, streamed)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].ID != "toolu_2" || msg.ToolCalls[0].Function.Name != "read" {
		t.Fatalf("tool calls = %+v", msg.ToolCalls)
	}
	if msg.ToolCalls[0].Function.Arguments != `{"path":"b.md"}` {
		t.Fatalf("arguments = %q", msg.ToolCalls[0].Function.Arguments)
	}
	if msg.Usage == nil || msg.Usage.PromptTokens != 10 || msg.Usage.CompletionTokens != 7 {
		t.Fatalf("usage = %+v", msg.Usage)
	}
}

func TestAnthropicStreamErrorEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"try later\"}}\n\n")
	}))
	defer srv.Close()
	c, _ := New(config.Provider{BaseURL: srv.URL, Wire: "anthropic-messages", Offline: true}, "m", "normal")
	_, err := c.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, StreamCallbacks{})
	if err == nil || !strings.Contains(err.Error(), "overloaded_error") {
		t.Fatalf("err = %v", err)
	}
}

func TestAnthropicMessageTranslationMergesAndNormalizes(t *testing.T) {
	system, out, err := toAnthropicMessages([]Message{
		{Role: "system", Content: "s"},
		{Role: "user", Content: "one"},
		{Role: "note", Content: "two"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "t", Function: Function{Name: "read", Arguments: "not-json"}}}},
		{Role: "tool", ToolCallID: "t", Content: "ok"},
		{Role: "tool", ToolCallID: "t2", Content: "ok2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if system != "s" || len(out) != 3 {
		t.Fatalf("system=%q messages=%+v", system, out)
	}
	if len(out[0].Content) != 2 {
		t.Fatalf("consecutive user messages were not merged: %+v", out[0].Content)
	}
	if string(out[1].Content[0].Input) != "{}" {
		t.Fatalf("invalid args should become {}, got %s", out[1].Content[0].Input)
	}
	if len(out[2].Content) != 2 {
		t.Fatalf("consecutive tool results were not merged: %+v", out[2].Content)
	}
}

// systemText joins the system block array back into the prompt string, the way
// toAnthropicMessages produced it before the block-array form landed.
func systemText(blocks []anthropicBlock) string {
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n\n")
}

// countCacheMarkers returns the total number of cache_control markers on a
// request: the system blocks plus every message block.
func countCacheMarkers(req anthropicRequest) int {
	n := 0
	for _, b := range req.System {
		if b.CacheControl != nil {
			n++
		}
	}
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.CacheControl != nil {
				n++
			}
		}
	}
	return n
}

// anthropicEcho captures the request body so a test can assert on the exact
// markers the wire sends, then streams a minimal completion.
func anthropicEcho(t *testing.T, got *anthropicRequest) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(got)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
}

func anthropicClient(t *testing.T, url string) *Client {
	t.Helper()
	c, err := New(config.Provider{BaseURL: url, Wire: "anthropic-messages", Offline: true}, "claude-test", "normal")
	if err != nil {
		t.Fatal(err)
	}
	c.APIKey = "sk-test"
	return c
}

// TestAnthropicCacheBreakpoints pins the M10-A rule: exactly two markers — the
// last system block and the message immediately before the final user turn —
// and never one on the task layer itself.
func TestAnthropicCacheBreakpoints(t *testing.T) {
	var got anthropicRequest
	srv := anthropicEcho(t, &got)
	defer srv.Close()
	c := anthropicClient(t, srv.URL)

	msgs := []Message{
		{Role: "system", Content: "layer 0-5 stack"},
		{Role: "user", Content: "earlier task"},
		{Role: "assistant", Content: "earlier answer"},
		{Role: "user", Content: "the current task"},
	}
	if _, err := c.Stream(context.Background(), msgs, nil, StreamCallbacks{}); err != nil {
		t.Fatal(err)
	}

	if n := countCacheMarkers(got); n != 2 {
		t.Fatalf("cache markers = %d, want exactly 2: %+v", n, got)
	}
	// Marker 1: the last (only) system block.
	if len(got.System) != 1 || got.System[0].CacheControl == nil {
		t.Fatalf("last system block is not marked: %+v", got.System)
	}
	if got.System[0].CacheControl.Type != "ephemeral" {
		t.Fatalf("system cache_control type = %q", got.System[0].CacheControl.Type)
	}
	// The converted messages are user / assistant / user — three turns. The
	// trailing user turn is the task layer and must stay unmarked; the marker
	// lands on the assistant turn immediately before it.
	if len(got.Messages) != 3 {
		t.Fatalf("messages = %d: %+v", len(got.Messages), got.Messages)
	}
	last := got.Messages[len(got.Messages)-1]
	for _, b := range last.Content {
		if b.CacheControl != nil {
			t.Fatalf("the final user turn must never carry a marker: %+v", last.Content)
		}
	}
	prev := got.Messages[len(got.Messages)-2]
	n := len(prev.Content)
	if n == 0 || prev.Content[n-1].CacheControl == nil {
		t.Fatalf("the turn before the task is not marked: %+v", prev.Content)
	}
}

// TestAnthropicCacheBreakpointsShortHistory proves the second marker is skipped
// when there is nothing stable before the task: system + a single user turn has
// no "message immediately before the final turn" to cache.
func TestAnthropicCacheBreakpointsShortHistory(t *testing.T) {
	var got anthropicRequest
	srv := anthropicEcho(t, &got)
	defer srv.Close()
	c := anthropicClient(t, srv.URL)

	msgs := []Message{
		{Role: "system", Content: "stack"},
		{Role: "user", Content: "only task"},
	}
	if _, err := c.Stream(context.Background(), msgs, nil, StreamCallbacks{}); err != nil {
		t.Fatal(err)
	}
	// Exactly one marker: the system block. The lone user turn is the task and
	// must stay unmarked.
	if n := countCacheMarkers(got); n != 1 {
		t.Fatalf("cache markers = %d, want 1 (system only): %+v", n, got)
	}
	if len(got.System) != 1 || got.System[0].CacheControl == nil {
		t.Fatalf("system block not marked: %+v", got.System)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("messages = %d: %+v", len(got.Messages), got.Messages)
	}
	for _, b := range got.Messages[0].Content {
		if b.CacheControl != nil {
			t.Fatalf("the task turn carries a marker: %+v", got.Messages[0].Content)
		}
	}
}

// TestAnthropicCacheUsageParsing proves the cache counters are read from the
// usage frames: cache_read/cache_creation on message_start, refined on
// message_delta.
func TestAnthropicCacheUsageParsing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":100,\"output_tokens\":0,\"cache_read_input_tokens\":80,\"cache_creation_input_tokens\":15}}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":9,\"cache_read_input_tokens\":90}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()
	c := anthropicClient(t, srv.URL)

	msg, err := c.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, StreamCallbacks{})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Usage == nil {
		t.Fatal("no usage parsed")
	}
	u := msg.Usage
	if u.PromptTokens != 100 || u.CompletionTokens != 9 {
		t.Fatalf("tokens = %d/%d", u.PromptTokens, u.CompletionTokens)
	}
	// message_start wrote 80/15; message_delta refined read to 90 and left the
	// write counter (absent → zero) alone rather than erasing it.
	if u.CacheReadTokens != 90 {
		t.Fatalf("cache read = %d, want 90 (message_delta refines)", u.CacheReadTokens)
	}
	if u.CacheWriteTokens != 15 {
		t.Fatalf("cache write = %d, want 15 (a zero delta must not erase)", u.CacheWriteTokens)
	}
}

// TestApplyCacheBreakpointsGate is the direct unit test of the breakpoint rule,
// independent of the wire: it pins the len>=3 gate and the "never the final
// user turn" invariant on the exact converted-message shapes the loop produces.
func TestApplyCacheBreakpointsGate(t *testing.T) {
	mk := func(roles ...string) []anthropicMessage {
		out := make([]anthropicMessage, 0, len(roles))
		for _, r := range roles {
			out = append(out, anthropicMessage{Role: r, Content: []anthropicBlock{{Type: "text", Text: r}}})
		}
		return out
	}
	sys := func() []anthropicBlock { return []anthropicBlock{{Type: "text", Text: "stack"}} }

	cases := []struct {
		name           string
		roles          []string
		wantMarkers    int // total markers expected (system + message)
		wantMsgMarkers int // markers expected on the message list alone
	}{
		{"one user turn (no history)", []string{"user"}, 1, 0},
		{"user+assistant (len 2, gate closed)", []string{"user", "assistant"}, 1, 0},
		{"user/assistant/user (len 3, gate open)", []string{"user", "assistant", "user"}, 2, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			system, msgs := sys(), mk(tc.roles...)
			applyCacheBreakpoints(system, msgs)
			// System block is always marked (there is one).
			if len(system) != 1 || system[0].CacheControl == nil {
				t.Fatalf("system block not marked: %+v", system)
			}
			// Count message markers and confirm none is on the final turn.
			msgMarkers, lastMarked := 0, false
			for i, m := range msgs {
				for _, b := range m.Content {
					if b.CacheControl != nil {
						msgMarkers++
						if i == len(msgs)-1 {
							lastMarked = true
						}
					}
				}
			}
			if lastMarked {
				t.Fatalf("the final user turn was marked: %+v", msgs)
			}
			if msgMarkers != tc.wantMsgMarkers {
				t.Fatalf("message markers = %d, want %d", msgMarkers, tc.wantMsgMarkers)
			}
			if got := 1 + msgMarkers; got != tc.wantMarkers {
				t.Fatalf("total markers = %d, want %d", got, tc.wantMarkers)
			}
		})
	}
}

// TestOpenAIWireCarriesNoCacheControl is the acceptance guard for M10-A: the
// openai-completions request is byte-identical to before — no `cache_control`
// anywhere, at any nesting level. The breakpoints live on the anthropic wire
// only, so a shared fixture that would leak a marker into the OpenAI body
// fails here.
func TestOpenAIWireCarriesNoCacheControl(t *testing.T) {
	var rawBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c, err := New(config.Provider{BaseURL: srv.URL, Wire: "openai-completions", Offline: true}, "m", "normal")
	if err != nil {
		t.Fatal(err)
	}
	msgs := []Message{
		{Role: "system", Content: "layer 0-5 stack"},
		{Role: "user", Content: "earlier task"},
		{Role: "assistant", Content: "earlier answer"},
		{Role: "user", Content: "the current task"},
	}
	if _, err := c.Stream(context.Background(), msgs, nil, StreamCallbacks{}); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(rawBody, []byte("cache_control")) {
		t.Fatalf("openai-completions body must not carry cache_control: %s", rawBody)
	}
}

// TestOpenAIUsageParsesCacheTokens proves the shared Usage tags cover the other
// wire too: an openai-completions provider that reports cache_read/cache_creation
// in its `usage` frame is parsed into the same fields, and one that omits them
// leaves the counters at zero. This is the "both shapes" half of M10-A req. 2.
func TestOpenAIUsageParsesCacheTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}],\"usage\":{\"prompt_tokens\":50,\"completion_tokens\":4,\"cache_read_input_tokens\":40,\"cache_creation_input_tokens\":6}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	c, err := New(config.Provider{BaseURL: srv.URL, Wire: "openai-completions", Offline: true}, "m", "normal")
	if err != nil {
		t.Fatal(err)
	}
	msg, err := c.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, StreamCallbacks{})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Usage == nil || msg.Usage.CacheReadTokens != 40 || msg.Usage.CacheWriteTokens != 6 {
		t.Fatalf("usage = %+v", msg.Usage)
	}
}
