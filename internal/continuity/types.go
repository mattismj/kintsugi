// Package continuity implements the local continuity layer (M8.5, DESIGN
// §16.23): the static session frame composed of an operator attestation and a
// bounded "prior accepted work" block, plus a local preflight risk scorer.
//
// Invariant — a refusal is never memory. Nothing in this package stores, reads,
// or renders refusal text or refusal outcomes. The types below carry only
// positive, operator-recorded fields: what was accepted, by whom, when, with
// which evidence. There is deliberately no field for an outcome, a rejection, a
// transcript, or a model message, so a refusal cannot be carried into a frame
// even by mistake; it exists only as a class counter that the preflight scorer
// reads as a number.
//
// Trust boundary: continuity is context, never policy. Rendering a frame never
// widens scope and never changes permissions — every rendered block says so in
// as many words.
package continuity

// PriorWork is one accepted entry recalled from the local context ledger. It is
// what the operator already approved for this workspace: a plan, an artifact, a
// decision or an acceptance.
//
// Evidence holds content hashes or workspace-relative paths; at most one is
// rendered (bounded recall, DESIGN §16.23: ≤5 entries, ≤600 tokens).
type PriorWork struct {
	ID       string   `json:"id"`                 // stable ledger id, e.g. "L0007"
	Date     string   `json:"date"`               // YYYY-MM-DD or RFC3339, as recorded
	Kind     string   `json:"kind"`               // plan | artifact | decision | acceptance
	Summary  string   `json:"summary"`            // what was accepted, operator-visible
	Evidence []string `json:"evidence,omitempty"` // hashes or workspace-relative paths
}

// Frame is the continuity material for one session. Attestation is the already
// rendered operator attestation block (see context.RenderAttestation); this
// package does not interpret it, it only composes it with prior work so the
// output stays deterministic.
//
// Per DESIGN §3.6 the static part (the attestation) may live in the cached
// prefix; the prior-work block is conversation-level material and is rendered
// per task.
type Frame struct {
	Attestation  string      `json:"attestation,omitempty"`
	PriorWork    []PriorWork `json:"prior_work,omitempty"`
	BudgetTokens int         `json:"budget_tokens,omitempty"` // 0 = DefaultPriorWorkTokens
}

// Render renders the frame: attestation first, then prior work.
func (f Frame) Render() string {
	return RenderFrame(f.Attestation, f.PriorWork, f.BudgetTokens)
}
