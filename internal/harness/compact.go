// Resume compaction (WP-2.3): the kintsugi-side half of session.Compact.
//
// session.Compact decides what to cut and keeps the assistant/tool-result pairs
// intact; this file supplies the summarizer — the configured curator model —
// and the window size to measure against.
package harness

import (
	"context"
	"fmt"
	"strings"

	"kintsugi/internal/provider"
	"kintsugi/internal/session"
)

// compactSystemPrompt turns the curator into a compressor: the output is fed
// back to the working model as a system message, so it must be dense, factual
// and free of ceremony.
const compactSystemPrompt = `You compress a long agent session into a working summary for the same agent, which is about to continue the task.

Rules:
- Keep every fact the agent still needs: what was asked, what was decided, files and commands touched, outcomes, failures, and what is still open.
- Drop pleasantries, repetition, and raw tool output that produced no decision.
- Preserve exact identifiers, paths, error messages and numbers.
- Write plain prose and short bullets. No preamble, no closing offer, no meta-commentary about summarising.
- If the work is unfinished, end with a "Next:" line naming the immediate next action.`

// compactUserHeader frames the transcript for the summarizer.
const compactUserHeader = "Transcript to compress (oldest first):\n\n"

// CompactHistory compacts msgs when the session is approaching the curator
// model's window. It is a no-op — original messages, false, nil — when no
// curator model is configured, when the model's window is unknown, or when the
// transcript is still under threshold.
//
// A summarizer failure is returned with the original messages: compaction is an
// optimisation, and the caller decides whether to continue on the full history.
func (a *App) CompactHistory(ctx context.Context, msgs []provider.Message) ([]provider.Message, bool, error) {
	if a == nil {
		return msgs, false, nil
	}
	ref := strings.TrimSpace(a.Cfg.Context.CuratorModel)
	if ref == "" {
		return msgs, false, nil
	}
	r, err := a.Resolve(ref)
	if err != nil {
		return msgs, false, fmt.Errorf("compaction: curator %q: %w", ref, err)
	}

	opts := session.CompactOptions{WindowTokens: r.Model.Context}
	if ok, _ := session.NeedsCompaction(msgs, opts); !ok {
		return msgs, false, nil
	}

	compact, changed, err := session.Compact(ctx, msgs, opts, func(ctx context.Context, transcript string) (string, error) {
		resp, err := r.Client.Stream(ctx, []provider.Message{
			{Role: "system", Content: compactSystemPrompt},
			{Role: "user", Content: compactUserHeader + transcript},
		}, nil, provider.StreamCallbacks{})
		if err != nil {
			return "", err
		}
		return resp.Content, nil
	})
	if err != nil {
		return msgs, false, err
	}
	return compact, changed, nil
}
