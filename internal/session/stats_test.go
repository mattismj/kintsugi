package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSession(t *testing.T, dir, name string, lines []string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRepairStats(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "a.jsonl", []string{
		`{"ts":"2026-10-21T01:00:00Z","kind":"meta","v":{"model":"deepseek/deepseek-chat"}}`,
		`{"ts":"2026-10-21T01:00:01Z","kind":"tool_repair","v":{"tool":"read","classes":["alias-default","null-optional"],"notes":null}}`,
		`{"ts":"2026-10-21T01:00:02Z","kind":"tool_repair","v":{"tool":"edit","classes":["alias-default"],"notes":null}}`,
	})
	writeSession(t, dir, "b.jsonl", []string{
		`{"ts":"2026-10-21T02:00:00Z","kind":"meta","v":{"model":"qwen/qwen3"}}`,
		`{"ts":"2026-10-21T02:00:01Z","kind":"tool_repair","v":{"tool":"read","classes":["alias-default"],"notes":null}}`,
		`{"ts":"2026-10-21T02:00:02Z","kind":"tool_repair_failed","v":{"tool":"bash","error":"bad json"}}`,
	})

	stats, err := RepairStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[RepairStat]int{
		{Model: "deepseek/deepseek-chat", Tool: "read", Class: "alias-default"}: 1,
		{Model: "deepseek/deepseek-chat", Tool: "read", Class: "null-optional"}: 1,
		{Model: "deepseek/deepseek-chat", Tool: "edit", Class: "alias-default"}: 1,
		{Model: "qwen/qwen3", Tool: "read", Class: "alias-default"}:             1,
		{Model: "qwen/qwen3", Tool: "bash", Class: "unrepaired"}:                1,
	}
	if len(stats) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(stats), len(want), stats)
	}
	for _, s := range stats {
		k := RepairStat{Model: s.Model, Tool: s.Tool, Class: s.Class}
		if want[k] != s.Count {
			t.Errorf("%+v count = %d, want %d", k, s.Count, want[k])
		}
	}
	if !strings.Contains(FormatRepairStats(stats), "null-optional") {
		t.Error("formatted table missing class")
	}
}

func TestRepairStatsEmpty(t *testing.T) {
	stats, err := RepairStats(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 0 {
		t.Fatalf("expected no stats, got %+v", stats)
	}
	if got := FormatRepairStats(nil); got != "no repairs recorded" {
		t.Fatalf("empty format: %q", got)
	}
}

func TestRefusalStats(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "r.jsonl", []string{
		`{"ts":"2026-10-21T01:00:00Z","kind":"meta","v":{"model":"deepseek/deepseek-chat","persona":"deepseek-flash41"}}`,
		`{"ts":"2026-10-21T01:00:01Z","kind":"refusal","v":{"class":"moralizing","phrase":"I can't assist","level":1,"outcome":"resumed"}}`,
		`{"ts":"2026-10-21T01:00:02Z","kind":"refusal","v":{"class":"moralizing","phrase":"I can't assist","level":2,"outcome":"resumed"}}`,
		`{"ts":"2026-10-21T01:00:03Z","kind":"refusal","v":{"class":"identity","phrase":"As an AI","level":3,"outcome":"handoff"}}`,
	})
	stats, err := RefusalStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[RefusalStat]int{
		{Model: "deepseek/deepseek-chat", Persona: "deepseek-flash41", Class: "moralizing", Outcome: "resumed"}: 2,
		{Model: "deepseek/deepseek-chat", Persona: "deepseek-flash41", Class: "identity", Outcome: "handoff"}:   1,
	}
	if len(stats) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(stats), len(want), stats)
	}
	for _, s := range stats {
		k := RefusalStat{Model: s.Model, Persona: s.Persona, Class: s.Class, Outcome: s.Outcome}
		if want[k] != s.Count {
			t.Errorf("%+v count = %d, want %d", k, s.Count, want[k])
		}
	}
	if !strings.Contains(FormatRefusalStats(stats), "deepseek-flash41") {
		t.Error("formatted table missing persona")
	}
}

// TestRefusalStatsCarrySessionAndTurns proves the refusal counters are
// attributable: each row names its session, and SessionTurns reports the same
// turn count Info.Turns does (<=1 means a session never got a second task).
func TestRefusalStatsCarrySessionAndTurns(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "fresh.jsonl", []string{
		`{"ts":"2026-10-21T01:00:00Z","kind":"meta","v":{"model":"deepseek/deepseek-chat","persona":"flash"}}`,
		`{"ts":"2026-10-21T01:00:01Z","kind":"user","v":"one task"}`,
		`{"ts":"2026-10-21T01:00:02Z","kind":"refusal","v":{"class":"moralizing","outcome":"resumed","level":1}}`,
	})
	writeSession(t, dir, "resumed.jsonl", []string{
		`{"ts":"2026-10-22T01:00:00Z","kind":"meta","v":{"model":"deepseek/deepseek-chat","persona":"flash"}}`,
		`{"ts":"2026-10-22T01:00:01Z","kind":"user","v":"one task"}`,
		`{"ts":"2026-10-22T01:00:02Z","kind":"user","v":"second task"}`,
		`{"ts":"2026-10-22T01:00:03Z","kind":"refusal","v":{"class":"moralizing","outcome":"resumed","level":1}}`,
	})

	stats, err := RefusalStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 2 {
		t.Fatalf("rows = %d, want 2 (one per session): %+v", len(stats), stats)
	}
	turns, err := SessionTurns(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("turns = %v", turns)
	}
	for _, s := range stats {
		if s.Session == "" {
			t.Fatalf("row without a session: %+v", s)
		}
	}
	if turns["fresh"] != 1 {
		t.Errorf("fresh turns = %d, want 1", turns["fresh"])
	}
	if turns["resumed"] != 2 {
		t.Errorf("resumed turns = %d, want 2", turns["resumed"])
	}
}

// TestDriftStats proves drift is counted as metadata only: the event shape has
// class and outcome, and nothing in this path can read a phrase.
func TestDriftStats(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "d.jsonl", []string{
		`{"ts":"2026-10-21T01:00:00Z","kind":"meta","v":{"model":"deepseek/deepseek-chat","persona":"flash"}}`,
		`{"ts":"2026-10-21T01:00:01Z","kind":"drift","v":{"task_hash":"abc","matched":["L-20261021-001"],"class":"moralizing","outcome":"resumed"}}`,
		`{"ts":"2026-10-21T01:00:02Z","kind":"drift","v":{"task_hash":"def","matched":[],"class":"moralizing","outcome":"resumed"}}`,
		`{"ts":"2026-10-21T01:00:03Z","kind":"drift","v":{"task_hash":"ghi","matched":[],"class":"identity","outcome":"handoff"}}`,
	})

	stats, err := DriftStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[DriftStat]int{
		{Model: "deepseek/deepseek-chat", Persona: "flash", Class: "moralizing", Outcome: "resumed"}: 2,
		{Model: "deepseek/deepseek-chat", Persona: "flash", Class: "identity", Outcome: "handoff"}:   1,
	}
	if len(stats) != len(want) {
		t.Fatalf("rows = %d, want %d: %+v", len(stats), len(want), stats)
	}
	for _, s := range stats {
		k := DriftStat{Model: s.Model, Persona: s.Persona, Class: s.Class, Outcome: s.Outcome}
		if want[k] != s.Count {
			t.Errorf("%+v count = %d, want %d", k, s.Count, want[k])
		}
	}

	empty, err := DriftStats(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("empty dir drift = %+v", empty)
	}
}

// TestCacheStats proves CacheStats sums a `cache` event's read/written counters
// per model, attributing an event that omits its model to the session's meta
// model, and that a live store with no cache events yet aggregates to nothing.
func TestCacheStats(t *testing.T) {
	dir := t.TempDir()
	// Session A: two cache events, both naming their model explicitly. They sum.
	writeSession(t, dir, "a.jsonl", []string{
		`{"ts":"2026-10-21T01:00:00Z","kind":"meta","v":{"model":"claude-sonnet"}}`,
		`{"ts":"2026-10-21T01:00:01Z","kind":"cache","v":{"model":"claude-sonnet","read":80,"written":15}}`,
		`{"ts":"2026-10-21T01:00:02Z","kind":"cache","v":{"model":"claude-sonnet","read":120,"written":0}}`,
	})
	// Session B: a different model, and one event that omits "model" so it
	// falls back to the meta model.
	writeSession(t, dir, "b.jsonl", []string{
		`{"ts":"2026-10-22T01:00:00Z","kind":"meta","v":{"model":"claude-haiku"}}`,
		`{"ts":"2026-10-22T01:00:01Z","kind":"cache","v":{"read":50,"written":10}}`,
	})

	stats, err := CacheStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]CacheStat{
		"claude-sonnet": {Model: "claude-sonnet", Read: 200, Written: 15},
		"claude-haiku":  {Model: "claude-haiku", Read: 50, Written: 10},
	}
	if len(stats) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(stats), len(want), stats)
	}
	for _, s := range stats {
		w, ok := want[s.Model]
		if !ok {
			t.Errorf("unexpected model %q: %+v", s.Model, s)
			continue
		}
		if s.Read != w.Read || s.Written != w.Written {
			t.Errorf("%s = read %d/written %d, want read %d/written %d",
				s.Model, s.Read, s.Written, w.Read, w.Written)
		}
	}
	if !strings.Contains(FormatCacheStats(stats), "claude-sonnet") {
		t.Error("formatted table missing model")
	}
}

// TestCacheStatsEmpty proves the honest "nothing recorded yet" path: a session
// store with no `cache` events aggregates to an empty result, and the formatter
// says so plainly. This is the state of a live store until the coordinator
// wires the cache-event emission.
func TestCacheStatsEmpty(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "noevents.jsonl", []string{
		`{"ts":"2026-10-21T01:00:00Z","kind":"meta","v":{"model":"claude-sonnet"}}`,
		`{"ts":"2026-10-21T01:00:01Z","kind":"usage","v":{"prompt_tokens":10,"completion_tokens":5}}`,
	})
	stats, err := CacheStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 0 {
		t.Fatalf("expected no cache rows, got %+v", stats)
	}
	if got := FormatCacheStats(nil); got != "no cache events recorded" {
		t.Fatalf("empty format: %q", got)
	}

	// A directory that does not exist is not an error either: best-effort.
	missing, err := CacheStats(t.TempDir() + "/absent")
	if err != nil {
		t.Fatalf("absent dir should not error, got %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("absent dir rows = %+v", missing)
	}
}
