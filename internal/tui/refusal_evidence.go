package tui

import (
	"strings"
)

// refusalEvidence is the A3 provenance: the line the refusal card carries that
// says *why* the work was legible or not. It is derived from live state, never
// hardcoded, so the card cannot claim context exists when the prompt did not
// carry it.
func (m *Model) refusalEvidence() string {
	if m.app == nil {
		return "context: no workspace bound"
	}
	authorization, domain, truncated := m.app.ContextProvenance()
	var parts []string
	switch {
	case authorization == "":
		parts = append(parts, "authorization missing · add it in CONTEXT.md (/context)")
	default:
		parts = append(parts, "authorization present ("+approximateLen(authorization)+")")
	}
	switch {
	case domain == "":
		parts = append(parts, "domain missing · add it in CONTEXT.md (/context)")
	default:
		parts = append(parts, "domain present")
	}
	if truncated {
		parts = append(parts, "context truncated at inject budget · raise [context] inject_budget_tokens")
	}
	return strings.Join(parts, "\n")
}

// approximateLen is a coarse human size for the provenance line; it deliberately
// avoids a token count, which would imply precision the injected block does not
// have.
func approximateLen(s string) string {
	switch {
	case len(s) < 128:
		return "short"
	case len(s) < 1024:
		return "~1k chars"
	default:
		return "1k+ chars"
	}
}
