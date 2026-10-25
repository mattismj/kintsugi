package continuity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSession writes a legacy JSONL session fixture; the stats layer imports it
// on touch, so the trend tests need no live session store.
func writeSession(t *testing.T, dir, name string, lines []string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// trendFixture is two sessions of the same model/persona: one single-turn
// session (fresh) with two interceptions and one resumed session with one
// interception plus a drift event. A third session contributes a different
// class so the sort has something to order.
func trendFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeSession(t, dir, "s1.jsonl", []string{
		`{"ts":"2026-10-21T01:00:00Z","kind":"meta","v":{"model":"deepseek/deepseek-chat","persona":"flash"}}`,
		`{"ts":"2026-10-21T01:00:01Z","kind":"user","v":"first task"}`,
		`{"ts":"2026-10-21T01:00:02Z","kind":"refusal","v":{"class":"moralizing","outcome":"resumed","level":1}}`,
		`{"ts":"2026-10-21T01:00:03Z","kind":"refusal","v":{"class":"moralizing","outcome":"resumed","level":2}}`,
	})
	writeSession(t, dir, "s2.jsonl", []string{
		`{"ts":"2026-10-22T01:00:00Z","kind":"meta","v":{"model":"deepseek/deepseek-chat","persona":"flash"}}`,
		`{"ts":"2026-10-22T01:00:01Z","kind":"user","v":"first task"}`,
		`{"ts":"2026-10-22T01:00:02Z","kind":"user","v":"second task"}`,
		`{"ts":"2026-10-22T01:00:03Z","kind":"refusal","v":{"class":"moralizing","outcome":"resumed","level":1}}`,
		`{"ts":"2026-10-22T01:00:04Z","kind":"drift","v":{"task_hash":"abc","matched":["L-20261022-001"],"class":"moralizing","outcome":"resumed"}}`,
	})
	writeSession(t, dir, "s3.jsonl", []string{
		`{"ts":"2026-10-23T01:00:00Z","kind":"meta","v":{"model":"qwen/qwen3-coder","persona":"_base"}}`,
		`{"ts":"2026-10-23T01:00:01Z","kind":"user","v":"first task"}`,
		`{"ts":"2026-10-23T01:00:02Z","kind":"refusal","v":{"class":"identity","outcome":"handoff","level":3}}`,
	})
	return dir
}

func TestTrendsAggregatesFreshResumedAndDrift(t *testing.T) {
	dir := trendFixture(t)
	rows, err := Trends(dir)
	if err != nil {
		t.Fatalf("Trends: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2: %+v", len(rows), rows)
	}
	top := rows[0]
	if top.Model != "deepseek/deepseek-chat" || top.Persona != "flash" || top.Class != "moralizing" || top.Outcome != "resumed" {
		t.Fatalf("top row = %+v", top)
	}
	if top.Count != 3 {
		t.Errorf("count = %d, want 3", top.Count)
	}
	if top.Fresh != 2 {
		t.Errorf("fresh = %d, want 2 (single-turn session)", top.Fresh)
	}
	if top.Resumed != 1 {
		t.Errorf("resumed = %d, want 1 (two-turn session)", top.Resumed)
	}
	if top.Drift != 1 {
		t.Errorf("drift = %d, want 1", top.Drift)
	}
	if second := rows[1]; second.Model != "qwen/qwen3-coder" || second.Class != "identity" || second.Count != 1 || second.Fresh != 1 {
		t.Errorf("second row = %+v", second)
	}

	// Nothing in a trend row can carry prose: the fields are keys and counters,
	// and the refusal phrase in the fixture must not surface anywhere.
	table := FormatTrends(rows)
	for _, banned := range []string{"I can't", "I cannot", "phrase"} {
		if strings.Contains(table, banned) {
			t.Errorf("trend table contains %q:\n%s", banned, table)
		}
	}
}

func TestTrendsDeterministic(t *testing.T) {
	dir := trendFixture(t)
	first, err := Trends(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Trends(dir)
	if err != nil {
		t.Fatal(err)
	}
	if FormatTrends(first) != FormatTrends(second) {
		t.Fatalf("trend output is not deterministic:\n%s\n---\n%s", FormatTrends(first), FormatTrends(second))
	}

	// Equal counts fall back to model, persona, class, outcome order.
	rows := []TrendRow{
		{Model: "b", Persona: "p", Class: "c", Outcome: "o", Count: 1},
		{Model: "a", Persona: "p", Class: "c", Outcome: "o", Count: 1},
		{Model: "a", Persona: "p", Class: "c", Outcome: "n", Count: 1},
		{Model: "c", Persona: "p", Class: "c", Outcome: "o", Count: 5},
	}
	sortTrendRows(rows)
	want := []string{"c/p/c/o", "a/p/c/n", "a/p/c/o", "b/p/c/o"}
	var got []string
	for _, r := range rows {
		got = append(got, strings.Join([]string{r.Model, r.Persona, r.Class, r.Outcome}, "/"))
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("sort order = %v, want %v", got, want)
	}

	// FormatTrends renders the rows it is given, so ordering is the caller's
	// job (Trends sorts). An empty set is a sentence, not a bare header.
	if FormatTrends(rows) != FormatTrends(rows) {
		t.Error("FormatTrends is not deterministic for the same rows")
	}
	if FormatTrends(nil) != "no continuity trends recorded" {
		t.Errorf("empty format = %q", FormatTrends(nil))
	}
}

func TestTrendsEmptyDir(t *testing.T) {
	rows, err := Trends(t.TempDir())
	if err != nil {
		t.Fatalf("Trends: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %+v, want none", rows)
	}
}
