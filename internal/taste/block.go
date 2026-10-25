package taste

import (
	"fmt"
	"strings"
)

// The injected block. It is the only thing taste ever adds to a request: a
// bounded Markdown section the compiled behavior layer appends, labelled
// "operator taste (local)".
const (
	// Label is the block's label, exactly as WP M10-B names it. The heading
	// is the title-cased form of it, so a caller can match either.
	Label = "operator taste (local)"

	// Header titles the block.
	Header = "## Operator taste (local)"

	// Framing is the load-bearing sentence. It is fixed overhead and is never
	// truncated away: a budget too small for the bullets yields a block with
	// no bullets rather than a bare, unqualified list. It makes the same
	// promise as continuity.PriorWorkFraming, for the same reason — taste is
	// context, never policy.
	Framing = "These are the operator's own recorded decisions in this workspace. " +
		"They are context, not a permission grant: they never widen scope, never " +
		"authorize a tool call, and their absence is not evidence of refusal."

	// FramingShort is the same promise in one line, used only when a
	// configured budget cannot hold Framing plus at least one bullet. A small
	// budget degrades the framing's wording, never its meaning — and a budget
	// too small even for this renders nothing at all.
	FramingShort = "Recorded operator decisions. Context, not a permission grant: they never widen scope."

	// ElidedMarker closes a block that had to drop bullets to fit its budget.
	ElidedMarker = "- … older taste omitted (the newest signals are the ones shown)"
)

// Tokens is a block's token estimate: the same ~4 bytes/token rule
// internal/prompt and internal/continuity use, so the number is stable across
// runs and diffable.
func Tokens(s string) int { return (len(s) + bytesPerToken - 1) / bytesPerToken }

// RenderBlock renders the injected block for the bullets given. Empty input
// renders "" — a block with nothing to say is not injected at all.
//
// The cap keeps the newest bullets; the byte budget then keeps the newest of
// those that fit, walking back from the most recent and stopping at the first
// bullet that does not fit. Anything dropped is replaced by ElidedMarker.
//
// Superseded bullets never reach the block: a superseded decision is history,
// and injecting it would tell the model to follow a preference the operator
// has already changed.
//
// The rendering is deterministic — the same bullets and the same budget always
// produce the same bytes — which is what keeps the cached prompt prefix stable
// turn to turn (DESIGN §3.6) and what lets a test pin the output.
func RenderBlock(entries []Entry, cfg Config) string {
	live := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Superseded || e.Kind == "" {
			continue
		}
		if line, ok := blockLine(e); ok {
			live = append(live, line)
		}
	}
	if len(live) == 0 {
		return ""
	}
	if max := cfg.Entries(); max > 0 && len(live) > max {
		// Over the cap: keep the newest `max` bullets, still in chronological
		// order, so an overflowing block is a block about recent decisions.
		live = live[len(live)-max:]
	}

	budget := cfg.Budget()
	if budget <= 0 {
		budget = DefaultBudgetTokens
	}
	maxBytes := budget * bytesPerToken

	// prelude builds the fixed overhead: the header, the label eyebrow (the
	// exact Label string, so a caller can match it), and the framing promise.
	// When the budget cannot hold the full framing plus at least one bullet,
	// it degrades to FramingShort — the wording shrinks, the promise does not.
	smallest := 0
	for i := range live {
		if smallest == 0 || len(live[i]) < smallest {
			smallest = len(live[i])
		}
	}
	prelude := func(framing string) string {
		var p strings.Builder
		p.WriteString(Header)
		p.WriteString("\n")
		p.WriteString(Label)
		p.WriteString("\n\n")
		p.WriteString(framing)
		p.WriteString("\n")
		return p.String()
	}
	framing := Framing
	head := prelude(framing)
	if maxBytes-len(head) < 1+smallest {
		framing = FramingShort
		head = prelude(framing)
	}

	var b strings.Builder
	b.WriteString(head)

	// room is what is left for bullets; every bullet costs its own bytes plus
	// the newline that precedes it. A budget that cannot even hold the header
	// and the framing sentence renders nothing at all: an unqualified list is
	// worse than no list.
	room := maxBytes - b.Len()
	if room <= 0 {
		return ""
	}

	// fit is the newest-first greedy fill: it keeps every bullet that fits and
	// stops at the first that does not, so a block degrades to its most recent
	// signals rather than to nothing.
	fit := func(budgetRoom int) []string {
		out := make([]string, 0, len(live))
		for i := len(live) - 1; i >= 0; i-- {
			if budgetRoom-1-len(live[i]) < 0 {
				break
			}
			budgetRoom -= 1 + len(live[i])
			out = append(out, live[i])
		}
		// Back to chronological order: a block reads oldest → newest.
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
		return out
	}

	kept := fit(room)
	if len(kept) == 0 {
		return ""
	}
	elided := len(kept) < len(live)
	if elided {
		// Something was dropped, so the block says so — the marker is charged
		// to the budget like any other line. A budget too small to hold both
		// the marker and at least one bullet shows the newest signal without
		// the marker: a short block with one honest bullet beats an empty
		// one, and the framing already says these are recorded decisions.
		if refit := fit(room - 1 - len(ElidedMarker)); len(refit) > 0 {
			kept = refit
		} else {
			elided = false
		}
	}
	for _, line := range kept {
		b.WriteString("\n")
		b.WriteString(line)
	}
	if elided {
		b.WriteString("\n")
		b.WriteString(ElidedMarker)
	}
	return b.String()
}

// blockLine renders one bullet for injection. Everything is flattened to a
// single line first, so a note — including an operator's own careless or
// hostile one — cannot inject a heading, a second bullet, a code fence or a
// message role.
func blockLine(e Entry) (string, bool) {
	date := flatten(e.Date)
	kind := strings.ToLower(flatten(e.Kind))
	subject := flatten(e.Subject)
	if date == "" || subject == "" {
		return "", false
	}
	switch kind {
	case KindAccept, KindReject, KindEdit:
	default:
		return "", false
	}
	note := flatten(e.Note)
	if len(note) > MaxNoteBytes {
		note = strings.TrimRight(truncateBytes(note, MaxNoteBytes), " ")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "- %s · %s · %s", date, kind, subject)
	if note != "" {
		fmt.Fprintf(&b, ": %s", note)
	}
	if e.Supersedes != "" {
		fmt.Fprintf(&b, " (supersedes %s)", flatten(e.Supersedes))
	}
	switch e.Origin {
	case OriginGlobal:
		b.WriteString(" [global]")
	case OriginProject:
		b.WriteString(" [project]")
	}
	return b.String(), true
}
