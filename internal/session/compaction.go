// History compaction (DESIGN §10, WP-2.3; M10.3).
//
// Long sessions eventually exceed the model's context window. Compaction
// replaces the oldest slice of the transcript with one model-written summary
// and keeps the recent tail verbatim, so a resumed session continues with the
// same working state instead of failing on a context overflow.
//
// The same split serves the live session: BuildSummary decides what a summary
// would cover, and the agent package asks a model to write it.
package session

import (
	"context"
	"fmt"
	"strings"

	"kintsugi/internal/provider"
)

// Defaults for CompactOptions. ThresholdPct is where compaction triggers as a
// percentage of the window; KeepRecentPct is how much of the transcript stays
// verbatim.
const (
	DefaultThresholdPct  = 80
	DefaultKeepRecentPct = 40

	// summaryHeading prefixes the synthetic system message that carries the
	// summary, so the model (and a curious operator) can tell it apart from
	// operator-authored context.
	summaryHeading = "## Prior work summary\n\n"

	// summarizerItemBytes bounds how much of any single message the summarizer
	// sees; tool output dominates transcripts and adds no decisions.
	summarizerItemBytes = 4000

	// DefaultKeepRatio is the share of a conversation that stays verbatim
	// when it is compacted: the older turns are summarized, the recent ones
	// are kept word for word.
	DefaultKeepRatio = 0.4
)

// CompactOptions configures one compaction decision. Zero values mean the
// documented defaults, except WindowTokens, which must be known: with no window
// there is nothing to measure against and NeedsCompaction reports false.
type CompactOptions struct {
	WindowTokens  int
	ThresholdPct  int
	KeepRecentPct int
}

func (o CompactOptions) thresholdPct() int {
	if o.ThresholdPct <= 0 {
		return DefaultThresholdPct
	}
	return o.ThresholdPct
}

func (o CompactOptions) keepRecentPct() int {
	if o.KeepRecentPct <= 0 {
		return DefaultKeepRecentPct
	}
	if o.KeepRecentPct > 100 {
		return 100
	}
	return o.KeepRecentPct
}

// ApproxTokens is the shared rough token estimate: one token per four bytes of
// text, rounded up. It is conservative enough for a threshold decision and
// cheap enough to run on every turn.
func ApproxTokens(text string) int { return (len(text) + 3) / 4 }

// MessageTokens estimates the tokens one message occupies, counting content and
// any tool-call names/arguments.
func MessageTokens(m provider.Message) int {
	n := ApproxTokens(m.Content)
	for _, tc := range m.ToolCalls {
		n += ApproxTokens(tc.Function.Name) + ApproxTokens(tc.Function.Arguments)
	}
	return n
}

// HistoryTokens estimates the tokens a whole conversation occupies.
func HistoryTokens(msgs []provider.Message) int {
	total := 0
	for _, m := range msgs {
		total += MessageTokens(m)
	}
	return total
}

// NeedsCompaction reports whether msgs exceed the configured share of the
// window, and the estimated token count behind that decision. The trigger is
// inclusive: at exactly ThresholdPct of the window, compaction runs.
func NeedsCompaction(msgs []provider.Message, opts CompactOptions) (bool, int) {
	tokens := HistoryTokens(msgs)
	if opts.WindowTokens <= 0 {
		return false, tokens
	}
	return tokens*100 >= opts.WindowTokens*opts.thresholdPct(), tokens
}

// Compact replaces the oldest part of msgs with a summary produced by
// summarize. The kept suffix is the most recent KeepRecentPct% of the
// transcript; the summarizer receives everything older as plain text.
//
// A cut that would split an assistant/tool-result pair is moved back so the
// kept suffix starts at the assistant message that owns the tool calls: a tool
// result without its call is invalid for every wire protocol.
//
// The result is [system "## Prior work summary\n\n<summary>"] + kept messages.
// When nothing needs compacting (or there is only one message group to keep)
// the input is returned unchanged with false. Summarizer errors are returned as
// is, wrapped, together with the original messages, so the caller decides
// whether to continue without compaction.
func Compact(ctx context.Context, msgs []provider.Message, opts CompactOptions, summarize func(context.Context, string) (string, error)) ([]provider.Message, bool, error) {
	if !needsCompaction(msgs, opts) {
		return msgs, false, nil
	}
	if summarize == nil {
		return msgs, false, fmt.Errorf("compaction: nil summarizer")
	}

	keep := len(msgs) * opts.keepRecentPct() / 100
	if keep < 1 {
		// Always keep at least the newest message: a summary of everything
		// loses the turn in flight.
		keep = 1
	}
	cut := len(msgs) - keep
	if cut <= 0 {
		return msgs, false, nil
	}
	// Never split an assistant/tool-result pair.
	for cut > 0 && msgs[cut].Role == "tool" {
		cut--
	}
	if cut <= 0 {
		return msgs, false, nil
	}

	summary, err := summarize(ctx, summarizerTranscript(msgs[:cut]))
	if err != nil {
		return msgs, false, fmt.Errorf("compaction: %w", err)
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		// An empty summary would silently drop the prior work; keep the
		// transcript intact and let the caller retry later.
		return msgs, false, nil
	}

	out := make([]provider.Message, 0, 1+len(msgs)-cut)
	out = append(out, SummaryMessage(summary))
	out = append(out, msgs[cut:]...)
	return out, true, nil
}

// Summary is one compaction split: the part of the conversation a summary
// covers, and the part that stays verbatim.
//
// Text is the rendered transcript of the covered messages — what a summarizer
// reads. The summary itself is written by a model, so it is not in here: a
// summary no model wrote is not a summary.
type Summary struct {
	// Text is the transcript of the covered messages, role-tagged.
	Text string
	// Covered is how many messages the summary replaces.
	Covered int
	// Kept is how many messages stay verbatim.
	Kept int
}

// BuildSummary splits a conversation into the older part to summarize and the
// recent part to keep. keepRatio is the share of the conversation that stays
// verbatim; zero means DefaultKeepRatio.
//
// The cut never splits an assistant/tool-call pair: a tool result without its
// call is invalid for every wire protocol, so the cut moves back until the
// kept suffix starts at a message that can stand on its own. This is the same
// rule the resume compaction uses.
//
// The returned messages are the kept suffix. A conversation too short to
// summarize comes back whole, with an empty Summary.
func BuildSummary(msgs []provider.Message, keepRatio float64) (Summary, []provider.Message) {
	if len(msgs) == 0 {
		return Summary{}, nil
	}
	ratio := keepRatio
	if ratio <= 0 {
		ratio = DefaultKeepRatio
	}
	if ratio > 1 {
		ratio = 1
	}
	keep := int(float64(len(msgs)) * ratio)
	if keep < 1 {
		// Always keep the newest message: a summary of everything loses the
		// turn in flight.
		keep = 1
	}
	cut := len(msgs) - keep
	if cut <= 0 {
		return Summary{}, msgs
	}
	// Never split an assistant/tool-result pair.
	for cut > 0 && msgs[cut].Role == "tool" {
		cut--
	}
	if cut <= 0 {
		return Summary{}, msgs
	}
	return Summary{
		Text:    summarizerTranscript(msgs[:cut]),
		Covered: cut,
		Kept:    len(msgs) - cut,
	}, msgs[cut:]
}

// SummaryMessage is the single system message that carries a summary in place
// of the turns it covers. The heading marks it as harness-written, so the
// model (and a curious operator) can tell it apart from operator context.
func SummaryMessage(text string) provider.Message {
	return provider.Message{Role: "system", Content: summaryHeading + strings.TrimSpace(text)}
}

func needsCompaction(msgs []provider.Message, opts CompactOptions) bool {
	ok, _ := NeedsCompaction(msgs, opts)
	return ok
}

// summarizerTranscript renders messages as the plain, role-tagged text the
// summarizer reads. Bodies are bounded: the summary is about decisions and
// outcomes, not about reproducing tool output.
func summarizerTranscript(msgs []provider.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case "user":
			b.WriteString("OPERATOR: ")
			b.WriteString(clip(m.Content))
			b.WriteString("\n")
		case "assistant":
			if strings.TrimSpace(m.Content) != "" {
				b.WriteString("ASSISTANT: ")
				b.WriteString(clip(m.Content))
				b.WriteString("\n")
			}
			for _, tc := range m.ToolCalls {
				b.WriteString("TOOL CALL: ")
				b.WriteString(tc.Function.Name)
				if tc.Function.Arguments != "" {
					b.WriteString(" ")
					b.WriteString(clip(tc.Function.Arguments))
				}
				b.WriteString("\n")
			}
		case "tool":
			b.WriteString("TOOL RESULT")
			if m.Name != "" {
				b.WriteString(" (" + m.Name + ")")
			}
			b.WriteString(": ")
			b.WriteString(clip(m.Content))
			b.WriteString("\n")
		default:
			if strings.TrimSpace(m.Content) == "" {
				continue
			}
			b.WriteString(strings.ToUpper(m.Role) + ": ")
			b.WriteString(clip(m.Content))
			b.WriteString("\n")
		}
	}
	return b.String()
}

func clip(s string) string {
	if len(s) <= summarizerItemBytes {
		return s
	}
	return s[:summarizerItemBytes] + "…"
}
