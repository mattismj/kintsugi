// Package provider implements the wire protocols. `openai-completions` and
// `anthropic-messages` are supported; any other wire is rejected explicitly
// rather than silently mis-speaking a protocol.
package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"kintsugi/internal/config"
)

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
	Usage      *Usage     `json:"-"`
	// FinishReason is the provider's stop reason ("stop", "length", "tool_calls",
	// "content_filter", "refusal", "sensitive", …). It is empty on wires that do
	// not report one. The refusal fallback walk (A2) keys off it.
	FinishReason string `json:"-"`
	// Thinking is the reasoning (chain-of-thought) the model produced before
	// its answer, for models that expose it (reasoning_content in the
	// OpenAI-compatible stream). It is the operator's data — their model, their
	// tokens — surfaced for reading and for the interceptor: the thinking is
	// where a refusal actually forms, and where the model's false assumptions
	// live before they harden into a stonewall.
	Thinking string `json:"-"`
}

// Usage is the token accounting one request returned.
//
// PromptTokens/CompletionTokens are the shape every wire reports. The cache
// counters (M10-A) are Anthropic's prompt-cache accounting and are zero on a
// wire that does not report them: cache_read_input_tokens is the prefix the
// provider reused, cache_creation_input_tokens is what it had to write first.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	CacheReadTokens  int `json:"cache_read_input_tokens"`
	CacheWriteTokens int `json:"cache_creation_input_tokens"`
}

type ToolCall struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Function Function `json:"function"`
}

type Function struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  ToolParameters `json:"parameters"`
}

type ToolParameters struct {
	Type       string              `json:"type"`
	Required   []string            `json:"required"`
	Properties map[string]ToolProp `json:"properties"`
}

type ToolProp struct {
	Type        string              `json:"type"`
	Description string              `json:"description"`
	Items       *ToolProp           `json:"items,omitempty"`
	Properties  map[string]ToolProp `json:"properties,omitempty"`
	Required    []string            `json:"required,omitempty"`
	Enum        []any               `json:"enum,omitempty"`
	Default     any                 `json:"default,omitempty"`
}

// Client speaks to one provider/model pair.
type Client struct {
	BaseURL   string
	APIKey    string
	APIKeyEnv string
	Model     string
	Wire      string
	Mode      string // offline | normal (network guard applied at request time)
	Offline   bool   // provider marked as loopback-only
	HTTP      *http.Client

	// Compat decisions (DESIGN §8.3), resolved from the model adapter.
	MergeSystemMessages bool // fold consecutive system messages into one
	DeveloperRole       bool // first system message uses the developer role
	// ReasoningEffort is forwarded as reasoning_effort when non-empty (for
	// example "none" to disable thinking on a local model).
	ReasoningEffort string
	// MaxTokens optionally caps completion length (0 = provider default).
	MaxTokens int

	// Fallbacks are tried in order when Stream fails with a retryable error
	// (see Retryable). Set by kintsugi.App.Resolve for a `chain/<name>` ref.
	Fallbacks []*Client
	// Logf receives fallback notices (kind "fallback"). nil = silent.
	Logf func(kind string, v any)
}

func New(p config.Provider, modelID, mode string) (*Client, error) {
	// A missing key is not a construction error: compiling and inspecting the
	// prompt stack must work offline. The key is required at request time.
	key := ""
	if p.APIKeyEnv != "" {
		key = strings.TrimSpace(envOrEmpty(p.APIKeyEnv))
	}
	timeout := 300 * time.Second
	return &Client{
		BaseURL:         strings.TrimRight(p.BaseURL, "/"),
		APIKey:          key,
		APIKeyEnv:       p.APIKeyEnv,
		Model:           modelID,
		Wire:            p.Wire,
		Mode:            mode,
		Offline:         p.Offline,
		ReasoningEffort: p.ReasoningEffort,
		HTTP:            &http.Client{Timeout: timeout},
	}, nil
}

type delta struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"` // Ollama/other thinking models
			FinishReason     string `json:"finish_reason"`
			ToolCalls        []struct {
				Index    int     `json:"index"`
				ID       *string `json:"id"`
				Type     string  `json:"type"`
				Function *struct {
					Name      *string `json:"name"`
					Arguments *string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
}

// StreamCallbacks receives both streams a thinking model produces: the
// reasoning (thinking) deltas and the answer (content) deltas. The thinking
// channel is the operator's data — their model, their machine, their tokens —
// and the harness's job is to surface it, not to decide whether they should
// see it. The interceptor also scans it: the thinking is where a refusal
// actually forms, before the polished answer commits to it.
type StreamCallbacks struct {
	// OnThinking receives reasoning deltas as they arrive. nil means the
	// caller does not want them; the stream still accumulates the full
	// thinking on the returned Message.
	OnThinking func(delta string)
	// OnDelta receives answer text deltas as they arrive.
	OnDelta func(delta string)
}

// Message gains the thinking the model produced, so it survives the turn
// boundary and the operator can read it after the fact.

// A retryable failure (Retryable) advances through c.Fallbacks, one attempt per
// slot; a non-retryable error fails immediately. See chain.go for the
// delta-visibility contract.
func (c *Client) Stream(ctx context.Context, msgs []Message, tools []Tool, cb StreamCallbacks) (Message, error) {
	msg, err := c.stream(ctx, msgs, tools, cb)
	if err == nil {
		return msg, nil
	}
	return c.streamFallback(ctx, msgs, tools, cb, err)
}

// stream is one attempt on this client only, with no fallback handling.
func (c *Client) stream(ctx context.Context, msgs []Message, tools []Tool, cb StreamCallbacks) (Message, error) {
	switch c.Wire {
	case "", "openai-completions":
		return c.streamOpenAI(ctx, msgs, tools, cb)
	case "anthropic-messages":
		return c.streamAnthropic(ctx, msgs, tools, cb)
	default:
		return Message{}, fmt.Errorf("unknown wire %q", c.Wire)
	}
}

func (c *Client) streamOpenAI(ctx context.Context, msgs []Message, tools []Tool, cb StreamCallbacks) (Message, error) {
	if err := c.requireKey(); err != nil {
		return Message{}, err
	}
	body := map[string]any{
		"model":    c.Model,
		"messages": normalizeMessages(msgs, c.MergeSystemMessages, c.DeveloperRole),
		"stream":   true,
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	if c.MaxTokens > 0 {
		body["max_tokens"] = c.MaxTokens
	}
	if c.ReasoningEffort != "" {
		body["reasoning_effort"] = c.ReasoningEffort
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Message{}, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return Message{}, err
	}
	c.setAuth(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Message{}, fmt.Errorf("%s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return Message{}, fmt.Errorf("provider %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(b)), 700))
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 256*1024), 8*1024*1024)

	out := Message{Role: "assistant"}
	acc := map[int]*ToolCall{}
	var order []int
	var reasoning strings.Builder

	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var d delta
		if err := json.Unmarshal([]byte(payload), &d); err != nil || len(d.Choices) == 0 {
			continue
		}
		if d.Usage != nil {
			out.Usage = d.Usage
		}
		delta := d.Choices[0].Delta
		reasoning.WriteString(delta.ReasoningContent)
		reasoning.WriteString(delta.Reasoning)
		if (delta.ReasoningContent != "" || delta.Reasoning != "") && cb.OnThinking != nil {
			cb.OnThinking(delta.ReasoningContent + delta.Reasoning)
		}
		if delta.FinishReason != "" {
			out.FinishReason = delta.FinishReason
		}
		if delta.Content != "" {
			out.Content += delta.Content
			if cb.OnDelta != nil {
				cb.OnDelta(delta.Content)
			}
		}
		for _, tc := range delta.ToolCalls {
			cur, ok := acc[tc.Index]
			if !ok {
				cur = &ToolCall{Type: "function"}
				acc[tc.Index] = cur
				order = append(order, tc.Index)
			}
			if tc.ID != nil && cur.ID == "" {
				cur.ID = *tc.ID
			}
			if tc.Function != nil {
				if tc.Function.Name != nil && cur.Function.Name == "" {
					cur.Function.Name = *tc.Function.Name
				}
				if tc.Function.Arguments != nil {
					cur.Function.Arguments += *tc.Function.Arguments
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("stream: %w", err)
	}

	for i, idx := range order {
		tc := acc[idx]
		if tc.ID == "" {
			tc.ID = fmt.Sprintf("call_%d", i)
		}
		out.ToolCalls = append(out.ToolCalls, *tc)
	}
	out.Thinking = reasoning.String()
	if out.Content == "" && len(out.ToolCalls) == 0 {
		if reasoning.Len() > 0 {
			return out, fmt.Errorf("model returned only reasoning and no answer (thinking model); set reasoning_effort = \"none\" on the provider to disable thinking")
		}
		return out, fmt.Errorf("empty stream: model returned neither content nor tool calls")
	}
	return out, nil
}

// normalizeMessages applies the compat fingerprint to an outbound request.
// It preserves ordering and never drops content.
func normalizeMessages(msgs []Message, mergeSystem, developerRole bool) []Message {
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if mergeSystem && m.Role == "system" && len(out) > 0 && out[len(out)-1].Role == "system" {
			out[len(out)-1].Content = strings.TrimRight(out[len(out)-1].Content, "\n") + "\n\n" + m.Content
			continue
		}
		out = append(out, m)
	}
	if developerRole {
		for i := range out {
			if out[i].Role == "system" {
				out[i].Role = "developer"
				break
			}
		}
	}
	return out
}

func (c *Client) requireKey() error {
	if c.Mode == "offline" && !config.IsLoopback(c.BaseURL) && !c.Offline {
		return fmt.Errorf("offline mode: %s is not loopback; set core.mode = \"normal\" (or KINTSUGI_MODE=normal) to allow cloud endpoints", c.BaseURL)
	}
	if c.APIKeyEnv == "" || c.APIKey != "" || config.IsLoopback(c.BaseURL) {
		return nil
	}
	return fmt.Errorf("provider needs %s (export it in the environment)", c.APIKeyEnv)
}

func envOrEmpty(k string) string { return os.Getenv(k) }

// Ping checks reachability without sending a completion.
func (c *Client) Ping(ctx context.Context) error {
	if err := c.requireKey(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+"/models", nil)
	if err != nil {
		return err
	}
	c.setAuth(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s → %d: %s", c.BaseURL, resp.StatusCode, truncate(strings.TrimSpace(string(b)), 300))
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
