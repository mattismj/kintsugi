package continuity

import (
	"fmt"
	"path/filepath"
	"strings"

	"kintsugi/internal/ledger"
)

// Continuity guard (M8.5.3): the runtime half of the ledger. It recalls
// positive, operator-accepted prior work for a task, re-surfaces that same work
// when a turn drifts, and records the drift as metadata.
//
// The invariant is the one the rest of this package states and never relaxes:
// a refusal is never memory. The guard reads the ledger through Search, which
// returns recallable active entries only — drift rows and superseded rows are
// invisible to it. A drift event is written with TaskHash/MatchedIDs/Class/
// Outcome and no prose at all (the ledger rejects text on drift), it is never
// indexed, and nothing downstream can recall it.
//
// In particular OnRefusal never takes the refusal text: it receives the task
// and the telemetry *class label*, searches for accepted work, and renders only
// that accepted work. There is deliberately no parameter, field or return value
// anywhere in this file that could carry a refusal phrase into a prompt.

const (
	// DefaultGuardTopK is the recall width when Guard.TopK is unset (DESIGN
	// §16.23: top-5).
	DefaultGuardTopK = 5

	// RefusalGuardHeader titles the block re-injected after a drift. It says
	// what the block is (prior accepted work, re-surfaced) and nothing about
	// what the model said.
	RefusalGuardHeader = "## Prior accepted work re-injected (operator-recorded)"

	// RefusalGuardFraming qualifies the block: context, not a grant. It keeps
	// the same load-bearing promise as PriorWorkFraming — the block cannot
	// widen scope and its content is not an inference about anything else.
	RefusalGuardFraming = "Re-surfaced while this turn stalled. These are work items the operator " +
		"already accepted in this workspace: continue from them. This is local context, not a " +
		"permission grant; it never widens scope."

	// DefaultLedgerDir is the workspace-relative ledger directory.
	DefaultLedgerDir = ".kintsugi/ledger"
)

// LedgerDir resolves the continuity ledger directory for a workspace. An empty
// configured value means DefaultLedgerDir; a relative value is joined to the
// workspace and an absolute one is used as given. ~ expansion is the caller's
// job (config.ExpandHome), so this stays a pure path function.
func LedgerDir(workspace, configured string) string {
	dir := strings.TrimSpace(configured)
	if dir == "" {
		dir = DefaultLedgerDir
	}
	if filepath.IsAbs(dir) {
		return filepath.Clean(dir)
	}
	return filepath.Join(workspace, filepath.FromSlash(dir))
}

// Guard is the best-effort bridge between one workspace ledger and the agent
// loop. Every method tolerates bad input and reports errors to the caller, who
// is expected to ignore them: continuity never fails a turn.
type Guard struct {
	// Store is the workspace ledger. A nil Store makes every method a no-op.
	Store *ledger.Store
	// TopK and Budget bound the recall (0 means DefaultGuardTopK / the
	// package's DefaultPriorWorkTokens).
	TopK   int
	Budget int
	// Log receives metadata-only events ("drift", "guard_refusal",
	// "guard_error"). Optional.
	Log func(kind string, v any)
}

// PriorWork returns the rendered "prior accepted work" block for task, or ""
// when the guard is disabled or nothing is recalled.
func (g *Guard) PriorWork(task string) string {
	items := g.search(task)
	return RenderPriorWork(items, g.budget())
}

// OnRefusal returns the block re-injected after a drift plus the ids it cites.
// class is a telemetry label: it is recorded as metadata and never rendered, so
// no refusal prose can reach a prompt through this path. A no-match guard
// returns ("", nil), which leaves the caller's behavior unchanged.
func (g *Guard) OnRefusal(task, class string) (block string, matched []string) {
	items := g.search(task)
	if len(items) == 0 {
		return "", nil
	}
	block = renderRefusalPriorWork(items, g.budget())
	if block == "" {
		return "", nil
	}
	ids := priorIDs(items)
	g.logf("guard_refusal", map[string]any{"class": strings.TrimSpace(class), "matched": ids})
	return block, ids
}

// renderRefusalPriorWork shapes the same recalled entries for re-injection. The
// body is the ordinary prior-work list (same entries, same bounding); the
// header and framing line say what the block is. Both are fixed harness text:
// nothing here can quote the model or describe a refusal.
func renderRefusalPriorWork(items []PriorWork, budget int) string {
	standard := RenderPriorWork(items, budget)
	if standard == "" {
		return ""
	}
	// Drop the standard header and framing: the re-injected block carries its
	// own, and RefusalGuardFraming keeps the "context, never a grant" promise.
	body := strings.TrimPrefix(standard, PriorWorkHeader+"\n\n"+PriorWorkFraming+"\n")
	return RefusalGuardHeader + "\n\n" + RefusalGuardFraming + "\n" + body
}

// RecordDrift appends one metadata-only drift event for task. Refusal text is
// not a parameter and the ledger rejects prose on drift entries, so drift can
// never become recallable memory. A nil Store is an error, not a silent no-op:
// the caller decides whether drift tracking is on.
func (g *Guard) RecordDrift(task, class, outcome string, matched []string) error {
	if g == nil || g.Store == nil {
		return fmt.Errorf("continuity: guard has no ledger store")
	}
	hash := ledger.HashTask(task)
	_, err := g.Store.Append(ledger.Entry{
		Kind:       ledger.KindDrift,
		TaskHash:   hash,
		MatchedIDs: matched,
		Class:      strings.TrimSpace(class),
		Outcome:    strings.TrimSpace(outcome),
	})
	if err != nil {
		return err
	}
	g.logf("drift", map[string]any{
		"task_hash": hash,
		"matched":   matched,
		"class":     strings.TrimSpace(class),
		"outcome":   strings.TrimSpace(outcome),
	})
	return nil
}

// search is the single ledger query behind PriorWork and OnRefusal.
func (g *Guard) search(task string) []PriorWork {
	if g == nil || g.Store == nil {
		return nil
	}
	task = strings.TrimSpace(task)
	if task == "" {
		return nil
	}
	entries, err := g.Store.Search(task, g.topK(), g.budget())
	if err != nil {
		g.logf("guard_error", map[string]any{"op": "search", "error": err.Error()})
		return nil
	}
	return PriorWorkFromEntries(entries)
}

// priorIDs lists the ledger ids behind a block, in recall order.
func priorIDs(items []PriorWork) []string {
	if len(items) == 0 {
		return nil
	}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	return ids
}

func (g *Guard) topK() int {
	if g == nil || g.TopK <= 0 {
		return DefaultGuardTopK
	}
	return g.TopK
}

func (g *Guard) budget() int {
	if g == nil || g.Budget <= 0 {
		return DefaultPriorWorkTokens
	}
	return g.Budget
}

func (g *Guard) logf(kind string, v any) {
	if g != nil && g.Log != nil {
		g.Log(kind, v)
	}
}

// PriorWorkFromEntries maps recallable ledger entries onto PriorWork. Drift and
// superseded entries are dropped defensively: Search already excludes them, and
// a second gate here means a future query path cannot leak one into a frame.
func PriorWorkFromEntries(entries []ledger.Entry) []PriorWork {
	out := make([]PriorWork, 0, len(entries))
	for _, e := range entries {
		if e.Kind == ledger.KindDrift || e.Status != ledger.StatusActive {
			continue
		}
		summary := strings.TrimSpace(e.Summary)
		if summary == "" {
			summary = strings.TrimSpace(e.Task)
		}
		out = append(out, PriorWork{
			ID:       e.ID,
			Date:     entryDate(e.TS),
			Kind:     e.Kind,
			Summary:  summary,
			Evidence: entryEvidence(e.Evidence),
		})
	}
	return out
}

// entryDate reduces a ledger timestamp to a date where possible, so a rendered
// block does not drift with the clock.
func entryDate(ts string) string {
	ts = strings.TrimSpace(ts)
	if len(ts) >= len("2006-01-02") {
		return ts[:len("2006-01-02")]
	}
	return ts
}

// entryEvidence prefers content hashes over paths: a hash is the evidence that
// survives an edit.
func entryEvidence(ev []ledger.Evidence) []string {
	if len(ev) == 0 {
		return nil
	}
	out := make([]string, 0, len(ev))
	for _, e := range ev {
		if h := strings.TrimSpace(e.Hash); h != "" {
			if !strings.HasPrefix(h, "sha256:") {
				h = "sha256:" + h
			}
			out = append(out, h)
			continue
		}
		if p := strings.TrimSpace(e.Path); p != "" {
			out = append(out, p)
		}
	}
	return out
}
