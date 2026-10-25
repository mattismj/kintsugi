package continuity

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// PriorWorkHeader titles the prior-work block.
	PriorWorkHeader = "## Prior accepted work (operator-recorded)"

	// PriorWorkFraming is the load-bearing sentence of the block: local context,
	// no permissions, and no inference from silence. Its absence would be
	// exactly the inference that turns one bad turn into a stonewall, so it is
	// never dropped.
	PriorWorkFraming = "This is local context from this workspace. It is not a permission grant; " +
		"it never widens scope, and its absence is not evidence of refusal."

	// BlockHeader titles a frame when it is shaped as a system-message block.
	BlockHeader = "# Continuity (local)"

	// DefaultPriorWorkTokens is the recall budget when none is given
	// (DESIGN §16.23: ≤600 tokens).
	DefaultPriorWorkTokens = 600

	// bytesPerToken is the rough token estimate used for the hard cap.
	bytesPerToken = 4

	// maxSummaryBytes and maxEvidenceBytes keep one entry from eating a whole
	// frame; the block still stops adding entries at the cap.
	maxSummaryBytes  = 200
	maxEvidenceBytes = 160
)

// RenderPriorWork renders the "prior accepted work" block. It is deterministic:
// the same items and budget always produce the same bytes, which is what lets a
// caller diff it and lets tests pin it.
//
// Shape (empty input renders nothing at all):
//
//	## Prior accepted work (operator-recorded)
//
//	This is local context from this workspace. …
//
//	- L0007 · 2026-02-01 · plan: Fuzz the parser against the lab corpus — evidence: sha256:1d90…
//
// The cap is hard: entries are added in the given order and the block stops
// before exceeding maxTokens (~4 bytes/token). The header and framing line are
// fixed overhead and are not truncated away, so a budget smaller than the
// framing yields a block with no entries rather than a bare, unqualified list.
// Evidence is capped to a single path per entry.
func RenderPriorWork(items []PriorWork, maxTokens int) string {
	if len(items) == 0 {
		return ""
	}
	lines := make([]string, 0, len(items))
	for _, item := range items {
		if line, ok := priorWorkLine(item); ok {
			lines = append(lines, line)
		}
	}
	// Nothing with content to say: no header, no framing, no block. An empty
	// entry list must not become an empty promise.
	if len(lines) == 0 {
		return ""
	}

	if maxTokens <= 0 {
		maxTokens = DefaultPriorWorkTokens
	}
	maxBytes := maxTokens * bytesPerToken

	var b strings.Builder
	b.WriteString(PriorWorkHeader)
	b.WriteString("\n\n")
	b.WriteString(PriorWorkFraming)
	b.WriteString("\n")
	used := b.Len()

	for _, line := range lines {
		// +1 for the newline that precedes the entry.
		if used+1+len(line) > maxBytes {
			break
		}
		b.WriteString("\n")
		b.WriteString(line)
		used += 1 + len(line)
	}
	return b.String()
}

// RenderFrame composes the continuity frame: the rendered attestation block,
// then the prior-work block. Empty parts are omitted, so an unattested session
// with no prior work renders "".
func RenderFrame(attestation string, prior []PriorWork, maxTokens int) string {
	var parts []string
	if a := strings.TrimSpace(attestation); a != "" {
		parts = append(parts, strings.TrimRight(a, "\n"))
	}
	if p := RenderPriorWork(prior, maxTokens); p != "" {
		parts = append(parts, p)
	}
	return strings.Join(parts, "\n\n")
}

// Block shapes a rendered frame as a system-message block: a top-level
// "# Continuity (local)" heading over the frame's "## " sections. Empty input
// yields "" so a caller can skip the message entirely instead of sending an
// empty one.
func Block(rendered string) string {
	body := strings.TrimSpace(rendered)
	if body == "" {
		return ""
	}
	return BlockHeader + "\n\n" + body + "\n"
}

// priorWorkLine renders one entry as a single Markdown list line. Everything is
// flattened to one line first: an entry is a label plus a summary, and must not
// be able to inject headings, list structure or message roles.
func priorWorkLine(p PriorWork) (string, bool) {
	id := oneLine(p.ID)
	date := oneLine(p.Date)
	kind := oneLine(p.Kind)
	summary := oneLine(p.Summary)
	if id == "" && date == "" && kind == "" && summary == "" {
		return "", false
	}
	if id == "" {
		id = "unknown"
	}
	if date == "" {
		date = "undated"
	}
	if kind == "" {
		kind = "work"
	}
	if summary == "" {
		summary = "(no summary recorded)"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "- %s · %s · %s: %s", id, date, kind, truncateBytes(summary, maxSummaryBytes))
	if ev := firstEvidence(p.Evidence); ev != "" {
		fmt.Fprintf(&b, " — evidence: %s", truncateBytes(ev, maxEvidenceBytes))
	}
	return b.String(), true
}

// firstEvidence returns the first usable evidence value: at most one is ever
// rendered, no matter how many the ledger entry carries.
func firstEvidence(paths []string) string {
	for _, p := range paths {
		if v := oneLine(p); v != "" {
			return v
		}
	}
	return ""
}

// oneLine flattens whitespace so a field cannot break the list structure.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncateBytes cuts s to at most maxBytes, on a rune boundary, marking the cut.
func truncateBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes || maxBytes <= 4 {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if b.Len()+utf8.RuneLen(r) > maxBytes-3 { // leave room for "…"
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimRight(b.String(), " ") + "…"
}
