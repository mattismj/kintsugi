package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// anthropicRequest is the `anthropic-messages` request shape. The internal
// Message model stays OpenAI-shaped; this file owns the translation in both
// directions so the agent loop never learns a second protocol.
//
// System is the block-array form of the system prompt. The wire accepts either
// a plain string or an array of text blocks; only the array can carry
// `cache_control` (M10-A), so the array is what this client sends. The text is
// the same joined string it always was — one block, byte-identical content —
// so the only difference on the wire is the cache breakpoint.
type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    []anthropicBlock   `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
	Stream    bool               `json:"stream"`
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	// CacheControl marks this block as the end of a cached prefix. It is set by
	// applyCacheBreakpoints only, and only on the anthropic wire.
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

// anthropicCacheControl is the prompt-cache marker of the Messages API:
// `cache_control: {"type":"ephemeral"}`. A block that carries one ends a
// prefix the provider may reuse on the next request.
type anthropicCacheControl struct {
	Type string `json:"type"`
}

// cacheEphemeral is the only cache_control type the Messages API accepts today.
func cacheEphemeral() *anthropicCacheControl { return &anthropicCacheControl{Type: "ephemeral"} }

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

func (c *Client) streamAnthropic(ctx context.Context, msgs []Message, tools []Tool, cb StreamCallbacks) (Message, error) {
	if err := c.requireKey(); err != nil {
		return Message{}, err
	}
	system, converted, err := toAnthropicMessages(msgs)
	if err != nil {
		return Message{}, err
	}
	if len(converted) == 0 {
		return Message{}, fmt.Errorf("anthropic wire: no user/assistant messages")
	}
	systemBlocks := systemBlocks(system)
	applyCacheBreakpoints(systemBlocks, converted)

	reqBody := anthropicRequest{
		Model:     c.Model,
		MaxTokens: c.MaxTokens,
		System:    systemBlocks,
		Messages:  converted,
		Stream:    true,
	}
	if reqBody.MaxTokens <= 0 {
		reqBody.MaxTokens = 4096
	}
	for _, t := range tools {
		reqBody.Tools = append(reqBody.Tools, anthropicTool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: toolSchema(t.Function.Parameters),
		})
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return Message{}, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/messages", bytes.NewReader(raw))
	if err != nil {
		return Message{}, err
	}
	c.setAuth(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Message{}, fmt.Errorf("%s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return Message{}, fmt.Errorf("provider %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(b)), 700))
	}

	type toolAcc struct {
		call      ToolCall
		startArgs string
		args      strings.Builder
	}
	out := Message{Role: "assistant"}
	acc := map[int]*toolAcc{}
	var order []int
	usage := &Usage{}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 256*1024), 8*1024*1024)
scan:
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev anthropicEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				usage.PromptTokens = ev.Message.Usage.InputTokens
				usage.CompletionTokens = ev.Message.Usage.OutputTokens
				usage.CacheReadTokens = ev.Message.Usage.CacheReadInputTokens
				usage.CacheWriteTokens = ev.Message.Usage.CacheCreationInputTokens
			}
		case "content_block_start":
			if ev.ContentBlock == nil {
				continue
			}
			switch ev.ContentBlock.Type {
			case "text":
				if ev.ContentBlock.Text != "" {
					out.Content += ev.ContentBlock.Text
					if cb.OnDelta != nil {
						cb.OnDelta(ev.ContentBlock.Text)
					}
				}
			case "tool_use":
				tc := &toolAcc{call: ToolCall{
					ID:   ev.ContentBlock.ID,
					Type: "function",
					Function: Function{
						Name: ev.ContentBlock.Name,
					},
				}}
				if len(ev.ContentBlock.Input) > 0 && string(ev.ContentBlock.Input) != "{}" {
					tc.startArgs = string(ev.ContentBlock.Input)
				}
				acc[ev.Index] = tc
				order = append(order, ev.Index)
			}
		case "content_block_delta":
			if ev.Delta == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text != "" {
					out.Content += ev.Delta.Text
					if cb.OnDelta != nil {
						cb.OnDelta(ev.Delta.Text)
					}
				}
			case "input_json_delta":
				if tc, ok := acc[ev.Index]; ok {
					tc.args.WriteString(ev.Delta.PartialJSON)
				}
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				out.FinishReason = ev.Delta.StopReason
			}
			if ev.Usage != nil {
				usage.CompletionTokens = ev.Usage.OutputTokens
				if ev.Usage.InputTokens > 0 {
					usage.PromptTokens = ev.Usage.InputTokens
				}
				// Cache counters may arrive on either the message_start or the
				// message_delta frame depending on the endpoint; a non-zero
				// later frame wins, a zero one never erases a count.
				if ev.Usage.CacheReadInputTokens > 0 {
					usage.CacheReadTokens = ev.Usage.CacheReadInputTokens
				}
				if ev.Usage.CacheCreationInputTokens > 0 {
					usage.CacheWriteTokens = ev.Usage.CacheCreationInputTokens
				}
			}
		case "message_stop":
			break scan
		case "error":
			if ev.Error != nil {
				return out, fmt.Errorf("anthropic stream: %s: %s", ev.Error.Type, ev.Error.Message)
			}
			return out, fmt.Errorf("anthropic stream: %s", truncate(payload, 300))
		}
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("stream: %w", err)
	}

	for i, idx := range order {
		tc := acc[idx]
		args := tc.args.String()
		if args == "" {
			args = tc.startArgs
		}
		if args == "" {
			args = "{}"
		}
		tc.call.Function.Arguments = args
		if tc.call.ID == "" {
			tc.call.ID = fmt.Sprintf("toolu_%d", i)
		}
		out.ToolCalls = append(out.ToolCalls, tc.call)
	}
	if usage.PromptTokens > 0 || usage.CompletionTokens > 0 ||
		usage.CacheReadTokens > 0 || usage.CacheWriteTokens > 0 {
		out.Usage = usage
	}
	if out.Content == "" && len(out.ToolCalls) == 0 {
		return out, fmt.Errorf("empty stream: model returned neither content nor tool calls")
	}
	return out, nil
}

type anthropicEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// toAnthropicMessages translates the internal (OpenAI-shaped) transcript.
// Consecutive same-role messages are merged because the Messages API expects
// alternating turns and represents tool results as a user content block.
func toAnthropicMessages(msgs []Message) (system string, out []anthropicMessage, err error) {
	var sys []string
	for _, m := range msgs {
		switch strings.ToLower(strings.TrimSpace(m.Role)) {
		case "system":
			if strings.TrimSpace(m.Content) != "" {
				sys = append(sys, m.Content)
			}
			continue
		}

		var blocks []anthropicBlock
		role := "user"
		switch strings.ToLower(strings.TrimSpace(m.Role)) {
		case "assistant":
			role = "assistant"
			if strings.TrimSpace(m.Content) != "" {
				blocks = append(blocks, anthropicBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				input := json.RawMessage(tc.Function.Arguments)
				if len(input) == 0 || !json.Valid(input) {
					input = json.RawMessage("{}")
				}
				blocks = append(blocks, anthropicBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: input,
				})
			}
		case "tool":
			blocks = append(blocks, anthropicBlock{
				Type:      "tool_result",
				ToolUseID: m.ToolCallID,
				Content:   m.Content,
			})
		default: // user, note, anything else from an interceptor
			if strings.TrimSpace(m.Content) == "" {
				continue
			}
			blocks = append(blocks, anthropicBlock{Type: "text", Text: m.Content})
		}

		if len(blocks) == 0 {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			continue
		}
		out = append(out, anthropicMessage{Role: role, Content: blocks})
	}
	return strings.Join(sys, "\n\n"), out, nil
}

// systemBlocks wraps the joined system prompt in the block-array form the
// Messages API expects when a breakpoint is involved. The text is exactly what
// toAnthropicMessages produced — one block, byte-identical content — so the
// prompt prefix stays byte-stable (DESIGN §3.6); only the wrapper differs. An
// empty system prompt yields no blocks at all, exactly as `system` was omitted
// from the request before.
func systemBlocks(system string) []anthropicBlock {
	if strings.TrimSpace(system) == "" {
		return nil
	}
	return []anthropicBlock{{Type: "text", Text: system}}
}

// applyCacheBreakpoints places the explicit prompt-cache markers on an
// anthropic-messages request (M10-A). The harness keeps a byte-stable prompt
// prefix; explicit breakpoints let the provider reuse it instead of re-reading
// the whole stack on every turn.
//
// Exactly two markers are set, and only when there is something stable to mark:
//
//  1. the LAST system block — the compiled prompt stack (layers 0–5). It is the
//     part of the request that must not change between turns, so it is the
//     primary cache boundary;
//  2. the last content block of the message IMMEDIATELY BEFORE the final user
//     turn — the end of the conversation history that already happened, and
//     therefore also stable for the next request. It is only placed when there
//     are at least three messages: with fewer, everything before the task is the
//     task itself, and caching the volatile turn buys nothing.
//
// The final user turn (the task layer) is NEVER marked: it changes every turn,
// and a breakpoint there would pin a prefix that is already different on the
// next request. Markers are placed on the last block of the chosen message so a
// merged multi-block turn caches as a whole.
func applyCacheBreakpoints(system []anthropicBlock, msgs []anthropicMessage) {
	if n := len(system); n > 0 {
		system[n-1].CacheControl = cacheEphemeral()
	}
	// len(msgs) >= 3 keeps the marker off the task layer: index len(msgs)-2 is
	// the turn before the trailing one only when a turn before it exists.
	if len(msgs) >= 3 {
		prev := &msgs[len(msgs)-2]
		if n := len(prev.Content); n > 0 {
			prev.Content[n-1].CacheControl = cacheEphemeral()
		}
	}
}

func toolSchema(p ToolParameters) map[string]any {
	b, err := json.Marshal(p)
	if err != nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	if _, ok := out["type"]; !ok {
		out["type"] = "object"
	}
	return out
}

// setAuth applies the auth scheme implied by the wire. Anthropic uses
// x-api-key + a pinned API version; OpenAI-compatible endpoints use a bearer
// token. Both read the key from the same api_key_env.
func (c *Client) setAuth(req *http.Request) {
	if c.APIKey == "" {
		return
	}
	if c.Wire == "anthropic-messages" {
		req.Header.Set("x-api-key", c.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		return
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
}
