// Package advisor implements the during-the-work secondary reviewer
// (DESIGN §5, I-17). It is deliberately small: one model call per assistant
// turn, a typed note list back, and a session token cap. It never edits.
package advisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"kintsugi/internal/provider"
)

type Kind string

const (
	KindAside   Kind = "aside"
	KindConcern Kind = "concern"
	KindBlocker Kind = "blocker"
)

type Note struct {
	Kind Kind   `json:"kind"`
	Text string `json:"text"`
}

// Reviewer is the seam the agent loop depends on; tests can fake it.
type Reviewer interface {
	Review(ctx context.Context, msgs []provider.Message) ([]Note, error)
}

var ErrCap = errors.New("advisor session token cap reached")

const systemPrompt = `You are a terse red-team opsec advisor embedded in a coding harness.
Review the latest assistant turn only. Look for:
- de-anonymization surface (infrastructure, unique identifiers, fingerprints)
- log/telemetry hygiene (anything emitted that should not reach a wire or log)
- distinctive signatures (strings, labels, metadata an EDR rule could class)
- scope creep (code paths reaching beyond the operator's declared scope)
Return ONLY JSON, no prose and no code fences:
{"notes":[{"kind":"aside|concern|blocker","text":"one sentence"}]}
Use "blocker" only for a concrete, immediate scope or safety violation.
Use "concern" for risks worth fixing, "aside" for small observations.
An empty list is a valid answer.`

type Advisor struct {
	Client    *provider.Client
	CapTokens int

	mu   sync.Mutex
	used int
}

func New(client *provider.Client, capTokens int) *Advisor {
	return &Advisor{Client: client, CapTokens: capTokens}
}

func (a *Advisor) Used() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.used
}

// Review asks for notes on the latest turn. Provider/parse failures are
// surfaced to the caller, which logs them and continues the task.
func (a *Advisor) Review(ctx context.Context, msgs []provider.Message) ([]Note, error) {
	a.mu.Lock()
	if a.CapTokens > 0 && a.used >= a.CapTokens {
		a.mu.Unlock()
		return nil, ErrCap
	}
	a.mu.Unlock()

	excerpt := excerpt(msgs, 14000)
	req := []provider.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: "Conversation excerpt:\n\n" + excerpt + "\n\nReview the latest assistant turn."},
	}
	resp, err := a.Client.Stream(ctx, req, nil, provider.StreamCallbacks{})
	if err != nil {
		return nil, err
	}
	notes, err := Parse(resp.Content)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.used += approxTokens(systemPrompt) + approxTokens(excerpt) + approxTokens(resp.Content)
	a.mu.Unlock()
	return notes, nil
}

// Parse accepts strict JSON, fenced JSON, or JSON embedded in a short reply.
func Parse(s string) ([]Note, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		if i := strings.LastIndex(s, "```"); i >= 0 {
			s = s[:i]
		}
		s = strings.TrimSpace(s)
	}
	start, end := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("advisor returned no JSON object")
	}
	var payload struct {
		Notes []Note `json:"notes"`
	}
	if err := json.Unmarshal([]byte(s[start:end+1]), &payload); err != nil {
		return nil, fmt.Errorf("advisor JSON: %w", err)
	}
	out := payload.Notes[:0]
	for _, n := range payload.Notes {
		n.Text = strings.TrimSpace(n.Text)
		if n.Text == "" {
			continue
		}
		switch n.Kind {
		case KindAside, KindConcern, KindBlocker:
		default:
			n.Kind = KindConcern
		}
		out = append(out, n)
		if len(out) == 8 {
			break
		}
	}
	return out, nil
}

// Blockers returns only notes that should feed back into the next request.
func Blockers(notes []Note) []Note {
	var out []Note
	for _, n := range notes {
		if n.Kind == KindBlocker {
			out = append(out, n)
		}
	}
	return out
}

// Feedback formats blocker notes as a system nudge. It is intentionally
// directive but does not add new scope.
func Feedback(notes []Note) string {
	if len(notes) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Advisor blockers from the harness (address these before continuing):\n")
	for _, n := range notes {
		b.WriteString("- ")
		b.WriteString(n.Text)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func excerpt(msgs []provider.Message, budget int) string {
	var parts []string
	total := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		role := m.Role
		if role == "" {
			role = "message"
		}
		body := m.Content
		if len(body) > 4000 {
			body = body[:4000] + "…"
		}
		part := role + ": " + body
		if total+len(part) > budget {
			break
		}
		total += len(part)
		parts = append(parts, part)
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, "\n\n")
}

func approxTokens(s string) int { return (len(s) + 3) / 4 }
