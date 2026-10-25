// Mid-session compaction (M10.3, DESIGN §16.8).
//
// Compaction used to run only when a session was resumed. This file is the
// in-session half: when the conversation grows past a threshold, the older
// turns are summarized and the recent ones are kept verbatim, so a long
// session stays inside the context window without losing the thread.
//
// Two rules govern the placement:
//
//   - The summary replaces the *oldest* turns. The compiled system prompt —
//     the stable, cacheable prefix — is never rewritten, so the bytes the
//     provider caches stay the bytes it cached.
//   - A summary that failed is not a summary. The turn continues on the
//     history it had: the thread is never lost to a compaction that did not
//     work.
//
// The compaction runs around the ordinary agent loop (CompactLoop), so
// everything the loop does — interception, continuity, hooks, permissions —
// still happens. Only what the model sees changes.
package agent

import (
	"context"
	"fmt"
	"strings"

	"kintsugi/internal/hooks"
	"kintsugi/internal/provider"
	"kintsugi/internal/session"
)

// Defaults for the in-session compaction trigger.
const (
	// DefaultCompactAfter is how many messages a conversation may hold before
	// the older ones are summarized. It is a turn budget, not a token budget:
	// the token-based compaction (session.Compact) still guards the window.
	DefaultCompactAfter = 24
	// DefaultCompactKeepRatio is the share of the conversation that stays
	// verbatim after a compaction.
	DefaultCompactKeepRatio = 0.4
)

// CompactOptions configures one compaction decision.
type CompactOptions struct {
	// After is the threshold in messages: a conversation longer than this is
	// compacted. Zero means DefaultCompactAfter; a negative value disables
	// compaction entirely.
	After int
	// KeepRatio is the share of the conversation that stays verbatim. Zero
	// means DefaultCompactKeepRatio.
	KeepRatio float64
}

// after returns the effective threshold. Zero is the default; a negative
// threshold means "never compact" and resolves to zero.
func (o CompactOptions) after() int {
	if o.After < 0 {
		return 0
	}
	if o.After == 0 {
		return DefaultCompactAfter
	}
	return o.After
}

// keepRatio returns the effective share of the conversation kept verbatim.
func (o CompactOptions) keepRatio() float64 {
	if o.KeepRatio <= 0 {
		return DefaultCompactKeepRatio
	}
	if o.KeepRatio > 1 {
		return 1
	}
	return o.KeepRatio
}

// Summarizer asks a model for a working summary of a transcript. It is a
// seam: the harness injects one that asks the session's own model, and the
// tests inject one that counts calls.
type Summarizer func(ctx context.Context, transcript string) (string, error)

// summaryPrompt is the instruction the summarizer reads. The shape is
// deliberate: a structured summary preserves the things a prose summary
// loses — decisions, file states, and the immediate next action — and the
// sections map to what a continuing agent needs to pick the work up cold.
// It is fed back as a system message, so it is dense, factual and free of
// ceremony.
const summaryPrompt = `You compress a long agent session into a working summary for the same agent, which is about to continue the task.

Write exactly these sections, in this order, with nothing outside them:

## Asked
What the operator asked for, in one or two lines — the task, not the conversation.

## Decided
Every decision that still constrains the work: settled approach, rejected options and why, constraints discovered. One bullet each.

## Files touched
Every file the work changed or depends on, with its CURRENT state (what it now contains that matters — a function signature, a config value, a schema), one line each. Mark files still in flight.

## Outcomes
What ran and what happened: commands, tests, failures with their error text, and anything that must not be re-run. Keep exact exit codes, error messages, paths and numbers.

## Open
What is unfinished and the immediate next action, named precisely enough to start without asking.

Rules:
- Keep every fact the agent still needs; drop pleasantries, repetition, and raw tool output that produced no decision.
- Preserve exact identifiers, paths, error messages and numbers.
- No preamble, no closing offer, no meta-commentary about summarising.`

// summaryUserHeader frames the transcript for the summarizer.
const summaryUserHeader = "Transcript to compress (oldest first):\n\n"

// NewSummarizer returns a summarizer that asks the given client for a plain
// working summary of the transcript. The client is the session's own model:
// summarizing is ordinary work and must not need a second budget.
//
// The summarizer asks for a plain completion (no tools), so the summary
// cannot itself call a tool or change the workspace.
func NewSummarizer(c *provider.Client) Summarizer {
	return func(ctx context.Context, transcript string) (string, error) {
		if c == nil {
			return "", fmt.Errorf("compaction: no client")
		}
		resp, err := c.Stream(ctx, []provider.Message{
			{Role: "system", Content: summaryPrompt},
			{Role: "user", Content: summaryUserHeader + transcript},
		}, nil, provider.StreamCallbacks{})
		if err != nil {
			return "", err
		}
		return resp.Content, nil
	}
}

// Compaction is the outcome of one compaction decision.
type Compaction struct {
	// Messages is the conversation the turn should run on: the summary plus
	// the kept turns when the compaction worked, the original conversation
	// otherwise.
	Messages []provider.Message
	// Changed reports whether the conversation was compacted.
	Changed bool
	// Covered is how many turns the summary replaces; Kept is how many stay
	// verbatim.
	Covered int
	Kept    int
}

// Compact summarizes the older turns of a conversation that grew past the
// threshold. On success the covered turns are replaced by one system message
// ("## Prior work summary") and the recent turns are kept verbatim.
//
// A failure — a transport error, an empty summary — leaves the conversation
// exactly as it was and is reported to the caller: the thread is never lost
// to a compaction that did not work.
func Compact(ctx context.Context, msgs []provider.Message, opts CompactOptions, summarize Summarizer) (Compaction, error) {
	threshold := opts.after()
	if threshold <= 0 || len(msgs) <= threshold {
		return Compaction{Messages: msgs}, nil
	}
	if summarize == nil {
		return Compaction{Messages: msgs}, fmt.Errorf("compaction: nil summarizer")
	}
	sum, kept := session.BuildSummary(msgs, opts.keepRatio())
	if sum.Covered == 0 {
		return Compaction{Messages: msgs}, nil
	}
	text, err := summarize(ctx, sum.Text)
	if err != nil {
		return Compaction{Messages: msgs}, fmt.Errorf("compaction: %w", err)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		// An empty summary would silently drop the prior work: keep the
		// transcript intact and let the caller retry later.
		return Compaction{Messages: msgs}, nil
	}
	out := make([]provider.Message, 0, 1+len(kept))
	out = append(out, session.SummaryMessage(text))
	out = append(out, kept...)
	return Compaction{
		Messages: out,
		Changed:  true,
		Covered:  sum.Covered,
		Kept:     sum.Kept,
	}, nil
}

// CompactLoop is a turn runner that compacts the conversation before the turn
// when it has grown past the threshold. It wraps an ordinary agent loop, so
// everything the loop does still happens; the compaction only decides what
// the model sees.
//
// The threshold is checked once per turn. After a compaction the
// conversation is shorter than the threshold, so the same turn never
// compacts twice.
type CompactLoop struct {
	*Loop

	// CompactAfter is the threshold in messages. Zero is the default; a
	// negative value disables compaction.
	CompactAfter int
	// CompactKeepRatio is the share of the conversation kept verbatim. Zero
	// is the default.
	CompactKeepRatio float64
	// Summarize asks the model for the summary. Without it the conversation
	// is never compacted: an unsummarized conversation is still a
	// conversation.
	Summarize Summarizer
	// OnCompact reports a compaction: the number of covered turns and the
	// number of kept ones.
	OnCompact func(covered, kept int)
}

// Wiring returns the loop this runner is built on, so a caller can inherit
// its wiring (interception, continuity, hooks) for another phase.
func (c *CompactLoop) Wiring() *Loop {
	if c == nil {
		return nil
	}
	return c.Loop
}

// RunConversation compacts the conversation when it has grown past the
// threshold and then runs the turn.
//
// A failed compaction is not a failed turn: the error is logged and the turn
// continues on the full history.
func (c *CompactLoop) RunConversation(ctx context.Context, systemPrompt string, history []provider.Message, task string) (string, []provider.Message, error) {
	if c == nil || c.Loop == nil {
		return "", history, fmt.Errorf("compaction: no loop")
	}
	log := c.Log
	if log == nil {
		log = nopLogger{}
	}
	h := history
	if c.Summarize != nil {
		res, err := Compact(ctx, h, CompactOptions{
			After:     c.CompactAfter,
			KeepRatio: c.CompactKeepRatio,
		}, c.Summarize)
		switch {
		case err != nil:
			// The thread is never lost to a failed compaction.
			log.Log("compact_error", err.Error())
		case res.Changed:
			h = res.Messages
			log.Log("compact", map[string]any{"covered": res.Covered, "kept": res.Kept})
			if c.OnCompact != nil {
				c.OnCompact(res.Covered, res.Kept)
			}
		}
	}
	return c.Loop.RunConversation(ctx, systemPrompt, h, task)
}

// fireCompactHooks runs the compaction hooks (M9.1). A hook error is
// telemetry, never a failed turn.
func (l *Loop) fireCompactHooks(ctx context.Context, name string) {
	if l == nil || l.Hooks == nil {
		return
	}
	_, _ = l.Hooks.Fire(ctx, hooks.Event{
		Name:      name,
		SessionID: l.SessionID,
		CWD:       l.CWD,
	})
}
