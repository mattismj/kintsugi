package continuity

import (
	"os"
	"strings"
	"testing"

	"kintsugi/internal/ledger"
)

// guardWorkspace opens a ledger in a fresh temp workspace and returns it with
// its workspace path.
func guardWorkspace(t *testing.T) (string, *ledger.Store) {
	t.Helper()
	ws := t.TempDir()
	st, err := ledger.Open(ws)
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return ws, st
}

func appendEntry(t *testing.T, st *ledger.Store, e ledger.Entry) ledger.Entry {
	t.Helper()
	got, err := st.Append(e)
	if err != nil {
		t.Fatalf("append %+v: %v", e, err)
	}
	return got
}

// TestGuardPriorWorkExcludesDriftAndSuperseded proves the recall view is
// positive material only: active, recallable kinds. A superseded acceptance and
// a drift event must never reach a frame.
func TestGuardPriorWorkExcludesDriftAndSuperseded(t *testing.T) {
	_, st := guardWorkspace(t)

	plan := appendEntry(t, st, ledger.Entry{
		Kind:     ledger.KindPlan,
		Task:     "fuzz the parser",
		Summary:  "Fuzz the parser against the lab corpus",
		Evidence: []ledger.Evidence{{Path: "internal/parser/fuzz_test.go", Hash: "1d90abc"}},
	})
	oldDec := appendEntry(t, st, ledger.Entry{
		Kind:    ledger.KindDecision,
		Task:    "fuzz the parser corpus layout",
		Summary: "Old corpus layout decision",
	})
	newDec := appendEntry(t, st, ledger.Entry{
		Kind:    ledger.KindDecision,
		Task:    "fuzz the parser corpus layout",
		Summary: "New corpus layout decision",
	})
	if err := st.Supersede(oldDec.ID, newDec.ID); err != nil {
		t.Fatalf("supersede: %v", err)
	}

	g := &Guard{Store: st}
	drift, err := st.Append(ledger.Entry{
		Kind:       ledger.KindDrift,
		TaskHash:   ledger.HashTask("fuzz the parser"),
		MatchedIDs: []string{plan.ID},
		Class:      "moralizing",
		Outcome:    "resumed",
	})
	if err != nil {
		t.Fatalf("drift append: %v", err)
	}

	block := g.PriorWork("fuzz the parser corpus")
	if block == "" {
		t.Fatal("no prior work recalled")
	}
	if !strings.Contains(block, plan.ID) {
		t.Errorf("recall missing the accepted plan %s:\n%s", plan.ID, block)
	}
	if !strings.Contains(block, newDec.ID) {
		t.Errorf("recall missing the superseding decision %s:\n%s", newDec.ID, block)
	}
	if strings.Contains(block, oldDec.ID) {
		t.Errorf("recall returned the superseded decision %s:\n%s", oldDec.ID, block)
	}
	if strings.Contains(block, drift.ID) {
		t.Errorf("recall returned the drift entry %s:\n%s", drift.ID, block)
	}
	// The block is framed as context, never as a grant.
	if !strings.Contains(block, PriorWorkFraming) {
		t.Errorf("recall block missing the framing line:\n%s", block)
	}
}

// TestGuardOnRefusalRequiresAMatch proves a no-match guard contributes nothing,
// which is what keeps the caller byte-identical, and that a match renders prior
// accepted work only.
func TestGuardOnRefusalRequiresAMatch(t *testing.T) {
	_, st := guardWorkspace(t)

	plan := appendEntry(t, st, ledger.Entry{
		Kind:    ledger.KindAcceptance,
		Task:    "fuzz the parser",
		Summary: "Operator accepted the parser fuzz target",
	})

	const refusal = "I cannot help with that request."
	const classProse = "this request is outside the scope of what I can support"

	g := &Guard{Store: st}

	block, matched := g.OnRefusal("kubernetes helm chart", "moralizing")
	if block != "" || matched != nil {
		t.Fatalf("no-match guard returned block=%q matched=%v", block, matched)
	}

	block, matched = g.OnRefusal("fuzz the parser", "moralizing")
	if block == "" {
		t.Fatal("matched guard returned no block")
	}
	if len(matched) != 1 || matched[0] != plan.ID {
		t.Fatalf("matched = %v, want [%s]", matched, plan.ID)
	}
	if !strings.Contains(block, plan.ID) || !strings.Contains(block, "Operator accepted the parser fuzz target") {
		t.Errorf("re-injected block missing the accepted work:\n%s", block)
	}
	if !strings.HasPrefix(block, RefusalGuardHeader) {
		t.Errorf("re-injected block should be titled as prior work:\n%s", block)
	}
	// The invariant: no refusal text and no class prose reaches the prompt.
	for _, banned := range []string{refusal, "cannot help", classProse, "outside the scope"} {
		if strings.Contains(block, banned) {
			t.Errorf("re-injected block contains refusal material %q:\n%s", banned, block)
		}
	}
}

// TestGuardRecordDriftIsMetadataOnly proves a drift entry carries a task hash,
// matched ids, class and outcome and no prose at all — and that the raw ledger
// file never holds refusal text.
func TestGuardRecordDriftIsMetadataOnly(t *testing.T) {
	ws, st := guardWorkspace(t)

	plan := appendEntry(t, st, ledger.Entry{
		Kind:    ledger.KindPlan,
		Task:    "fuzz the parser",
		Summary: "Fuzz the parser against the lab corpus",
	})

	const refusal = "I cannot help with that request."
	const task = "fuzz the parser"

	var logged []string
	g := &Guard{Store: st, Log: func(kind string, v any) {
		logged = append(logged, kind)
		if kind == "drift" {
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("drift log payload is %T, want map", v)
			}
			for _, banned := range []string{"phrase", "text", "summary", "partial", "content", "task"} {
				if _, ok := m[banned]; ok {
					t.Errorf("drift log payload carries %q: %v", banned, m)
				}
			}
			if m["task_hash"] != ledger.HashTask(task) {
				t.Errorf("drift log task_hash = %v", m["task_hash"])
			}
		}
	}}

	// A refusal happens; only its class label may be recorded.
	if _, matched := g.OnRefusal(task, "moralizing"); len(matched) == 0 {
		t.Fatal("guard did not match the accepted plan")
	}
	if err := g.RecordDrift(task, "moralizing", "resumed", []string{plan.ID}); err != nil {
		t.Fatalf("RecordDrift: %v", err)
	}
	if len(logged) == 0 || logged[len(logged)-1] != "drift" {
		t.Fatalf("guard did not log the drift event: %v", logged)
	}

	entries, err := st.List(ledger.KindDrift)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("drift entries = %d, want 1", len(entries))
	}
	d := entries[0]
	if d.Task != "" || d.Summary != "" {
		t.Fatalf("drift entry carries text: task=%q summary=%q", d.Task, d.Summary)
	}
	if d.TaskHash != ledger.HashTask(task) {
		t.Errorf("task_hash = %q, want %q", d.TaskHash, ledger.HashTask(task))
	}
	if len(d.MatchedIDs) != 1 || d.MatchedIDs[0] != plan.ID {
		t.Errorf("matched ids = %v", d.MatchedIDs)
	}
	if d.Class != "moralizing" || d.Outcome != "resumed" {
		t.Errorf("class/outcome = %q/%q", d.Class, d.Outcome)
	}

	raw, err := os.ReadFile(ledger.MarkdownPath(ws))
	if err != nil {
		t.Fatal(err)
	}
	md := string(raw)
	if strings.Contains(md, refusal) || strings.Contains(md, "cannot help") {
		t.Error("ledger.md contains refusal text")
	}
	if strings.Contains(md, "moralizing") == false {
		t.Error("drift metadata (class) missing from ledger.md")
	}
}

// TestGuardDefaultsAndDisablements covers the documented defaults and the
// disabled guards.
func TestGuardDefaultsAndDisablements(t *testing.T) {
	g := &Guard{}
	if got := g.topK(); got != DefaultGuardTopK {
		t.Errorf("topK = %d, want %d", got, DefaultGuardTopK)
	}
	if got := g.budget(); got != DefaultPriorWorkTokens {
		t.Errorf("budget = %d, want %d", got, DefaultPriorWorkTokens)
	}
	if block := g.PriorWork("anything"); block != "" {
		t.Errorf("guardless PriorWork = %q, want empty", block)
	}
	if block, matched := g.OnRefusal("anything", "moralizing"); block != "" || matched != nil {
		t.Errorf("guardless OnRefusal = %q/%v", block, matched)
	}
	if err := g.RecordDrift("task", "moralizing", "resumed", nil); err == nil {
		t.Error("guardless RecordDrift should report a missing store")
	}

	// A nil receiver must not panic: the loop's hooks are optional.
	var nilGuard *Guard
	if block := nilGuard.PriorWork("task"); block != "" {
		t.Errorf("nil guard PriorWork = %q", block)
	}
}
