package session

import (
	"strings"
	"testing"
)

// TestModelReliabilityAggregates pins the per-model row: turns, refusals,
// rate, hard count, top class, repairs, tokens — grouped by model+persona.
func TestModelReliabilityAggregates(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	s.Log("meta", map[string]string{"model": "deepseek/deepseek-chat", "persona": "deepseek-flash41"})
	s.Log("user", "task one")
	s.Log("assistant", "answer")
	s.Log("refusal", map[string]any{"class": "moralizing", "outcome": "resumed"})
	s.Log("refusal", map[string]any{"class": "safety", "outcome": "handoff"})
	s.Log("user", "task two")
	s.Log("assistant", "answer")
	s.Log("refusal", map[string]any{"class": "moralizing", "outcome": "resumed"})
	s.Log("tool_repair", map[string]any{"tool": "bash", "classes": []string{"arg_quotes"}})
	s.Log("usage", map[string]any{"prompt_tokens": 100, "completion_tokens": 50})
	s.Close()

	dir, _ := Dir()
	rows, err := ModelReliabilityStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	r := rows[0]
	if r.Model != "deepseek/deepseek-chat" || r.Persona != "deepseek-flash41" {
		t.Errorf("row identity = %+v", r)
	}
	if r.Turns != 2 || r.Refusals != 3 {
		t.Errorf("turns/refusals = %d/%d, want 2/3", r.Turns, r.Refusals)
	}
	if r.RefusalRate != 150 {
		t.Errorf("rate = %.1f, want 150 (3 refusals over 2 turns is honest — the row carries the denominator)", r.RefusalRate)
	}
	if r.HardRefusals != 1 {
		t.Errorf("hard = %d, want 1", r.HardRefusals)
	}
	if r.TopClass != "moralizing" {
		t.Errorf("top class = %q, want moralizing", r.TopClass)
	}
	if r.Repairs != 1 || r.PromptTokens != 100 || r.CompletionTokens != 50 {
		t.Errorf("repairs/tokens = %+v", r)
	}
}

// TestModelReliabilityFiltered: the windowed report counts only kept sessions.
func TestModelReliabilityFiltered(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	a, _ := New()
	a.Log("meta", map[string]string{"model": "m/a"})
	a.Log("user", "t")
	a.Close()
	b, _ := New()
	b.Log("meta", map[string]string{"model": "m/b"})
	b.Log("user", "t")
	b.Close()

	dir, _ := Dir()
	rows, err := ModelReliabilityStatsFiltered(dir, func(id string) bool { return id == a.ID })
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Model != "m/a" {
		t.Fatalf("filtered rows = %+v", rows)
	}
}

// TestFormatModelReliabilityStatesDenominators: the table carries the warning
// that a low-turn model is not a ranking.
func TestFormatModelReliabilityStatesDenominators(t *testing.T) {
	out := FormatModelReliability([]ModelReliability{{Model: "m", Turns: 2, Refusals: 1, RefusalRate: 50}})
	if !strings.Contains(out, "not a ranking") {
		t.Errorf("the header does not state the denominator caveat:\n%s", out)
	}
	if !strings.Contains(out, "50.0%") {
		t.Errorf("the rate is missing:\n%s", out)
	}
	if FormatModelReliability(nil) != "no model telemetry yet — run a task first" {
		t.Error("the empty state is wrong")
	}
}
