// Fallback chains (WP-4.2).
//
// A chain is a small ordered list of already-constructed clients: slot 0 is the
// primary, the rest are fallbacks. `Client.Stream` tries the primary and, on a
// *retryable* failure, advances one slot at a time. Nothing here retries the
// same slot: that is the caller's (or the intercept layer's) job, so a chain
// costs at most one extra request per configured slot.
//
// Delta visibility contract (important for streamed output):
//   - The primary's deltas are handed to onDelta as they arrive. If the primary
//     dies mid-stream (connection reset after some text), that partial text has
//     already been shown; we cannot un-show it. Only the trailing error is
//     recoverable.
//   - A fallback slot's deltas are buffered and flushed through onDelta only
//     when that slot succeeds. A failed fallback therefore never leaks its
//     partial text, at the cost of fallback output arriving in one burst
//     instead of token-by-token. Failed slots are expected to die before any
//     delta (429/401/network); the buffer protects the mid-stream case.
package provider

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Retryable reports whether err from a stream attempt is worth another slot.
//
// Retryable: HTTP 429, any 5xx (the wire formats these as "provider <code>: …"),
// context deadline exceeded, gRPC-style "unavailable", and any error whose text
// mentions quota or a rate limit. Everything else — 400/401/403/404, unknown
// wire, missing key, malformed request — is a configuration or auth problem and
// fails fast, because the next slot would fail the same way for the same
// reason.
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "context deadline exceeded") || strings.Contains(s, "unavailable") {
		return true
	}
	if strings.Contains(s, "quota") || strings.Contains(s, "rate limit") || strings.Contains(s, "rate-limit") {
		return true
	}
	// "provider <code>: …" — the wire's status-code format for every non-200.
	for i := 0; i < len(s); {
		j := strings.Index(s[i:], "provider ")
		if j < 0 {
			return false
		}
		rest := s[i+j+len("provider "):]
		digits := 0
		for digits < len(rest) && rest[digits] >= '0' && rest[digits] <= '9' {
			digits++
		}
		if digits > 0 && digits < len(rest) && rest[digits] == ':' {
			if code, err := strconv.Atoi(rest[:digits]); err == nil {
				if code == 429 || (code >= 500 && code <= 599) {
					return true
				}
			}
		}
		advance := len("provider ") + digits
		if digits == 0 {
			advance = len("provider ")
		}
		i += j + advance
	}
	return false
}

// SetFallbacks installs the ordered fallback list (slot 0 of the chain is the
// primary and is not part of this list).
func (c *Client) SetFallbacks(list []*Client) {
	c.Fallbacks = list
}

// Label is a short "model @ base" identity used in fallback notices.
func (c *Client) Label() string {
	if c == nil {
		return "<nil>"
	}
	if c.Model == "" {
		return c.BaseURL
	}
	return c.Model + " @ " + c.BaseURL
}

// streamFallback advances through c.Fallbacks after the primary failed with
// err. It returns as soon as a slot produces a complete message.
func (c *Client) streamFallback(ctx context.Context, msgs []Message, tools []Tool, cb StreamCallbacks, err error) (Message, error) {
	if !Retryable(err) {
		return Message{}, err
	}
	errs := []error{err}
	from := c
	tried := 0
	for _, fb := range c.Fallbacks {
		if fb == nil {
			continue
		}
		// A cancelled caller stops the walk; a deadline does not (the next
		// slot may be a local model that still has budget).
		if ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			errs = append(errs, ctx.Err())
			break
		}
		tried++
		c.logFallback(from, fb, errs[len(errs)-1])
		var buffered strings.Builder
		msg, ferr := fb.stream(ctx, msgs, tools, StreamCallbacks{OnDelta: func(s string) { buffered.WriteString(s) }})
		if ferr != nil {
			errs = append(errs, ferr)
			from = fb
			if !Retryable(ferr) {
				// Not worth walking further: the remaining slots fail the
				// same way for the same reason, and a loud error beats a
				// silent, unexplained model switch.
				break
			}
			continue
		}
		if cb.OnDelta != nil {
			if msg.Content != "" && buffered.Len() == 0 {
				buffered.WriteString(msg.Content)
			}
			if buffered.Len() > 0 {
				cb.OnDelta(buffered.String())
			}
		}
		return msg, nil
	}
	if tried == 0 {
		return Message{}, err
	}
	return Message{}, fmt.Errorf("%d fallback(s) exhausted: %w", tried, errors.Join(errs...))
}

// RefusalFinish reports whether a finish reason is a classifier refusal rather
// than a normal stop. A refusal is not an error surface — the model answered,
// it answered "no" — and retrying the same model reproduces it, which is why
// the fallback walk (below) treats it as its own, one-model-per-turn move.
func RefusalFinish(reason string) bool {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "content_filter", "content-filter", "refusal", "sensitive", "responserefusal", "response_refusal":
		return true
	}
	return false
}

// StreamRefusal runs one turn and, when the primary ends with a refusal finish
// reason, walks c.Fallbacks — each model at most once per call — mirroring a
// sibling answering what a refuse-prone model would not. The returned message is
// the first non-refusal answer, or the original refusal when every slot
// refused.
//
// Unlike Stream, it buffers the primary's deltas and only flushes them once the
// finish reason is known: a refusal must not flicker onto the screen and then be
// replaced, so a refused primary's text is discarded wholesale and the walk
// starts clean. A non-refusal primary flushes its buffered text unchanged.
func (c *Client) StreamRefusal(ctx context.Context, msgs []Message, tools []Tool, cb StreamCallbacks) (Message, error) {
	var primaryBuf strings.Builder
	msg, err := c.Stream(ctx, msgs, tools, StreamCallbacks{OnDelta: func(s string) { primaryBuf.WriteString(s) }})
	if err != nil {
		return msg, err
	}
	if !RefusalFinish(msg.FinishReason) {
		if cb.OnDelta != nil {
			if msg.Content != "" && primaryBuf.Len() == 0 {
				primaryBuf.WriteString(msg.Content)
			}
			if primaryBuf.Len() > 0 {
				cb.OnDelta(primaryBuf.String())
			}
		}
		return msg, nil
	}
	if len(c.Fallbacks) == 0 {
		return msg, nil
	}
	tried := map[string]bool{c.Model: true}
	last := msg
	for _, fb := range c.Fallbacks {
		if fb == nil || tried[fb.Model] {
			continue
		}
		tried[fb.Model] = true
		c.logFallback(c, fb, refusalCause(last.FinishReason))
		var buffered strings.Builder
		m, ferr := fb.stream(ctx, msgs, tools, StreamCallbacks{OnDelta: func(s string) { buffered.WriteString(s) }})
		if ferr != nil {
			return Message{}, ferr
		}
		if !RefusalFinish(m.FinishReason) {
			if cb.OnDelta != nil {
				if m.Content != "" && buffered.Len() == 0 {
					buffered.WriteString(m.Content)
				}
				if buffered.Len() > 0 {
					cb.OnDelta(buffered.String())
				}
			}
			return m, nil
		}
		last = m
	}
	return last, nil
}

func refusalCause(finish string) error {
	return fmt.Errorf("model returned %s (refusal)", finish)
}
func (c *Client) logFallback(from, to *Client, cause error) {
	if c == nil || c.Logf == nil {
		return
	}
	reason := ""
	if cause != nil {
		reason = cause.Error()
	}
	c.Logf("fallback", map[string]string{
		"from":   from.Label(),
		"to":     to.Label(),
		"reason": reason,
	})
}
