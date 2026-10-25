package session

import (
	"strings"
	"testing"

	"kintsugi/internal/provider"
)

func newTestSession(t *testing.T) *Session {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestHistoryReattachesCallsAndResults(t *testing.T) {
	s := newTestSession(t)
	s.Log("meta", map[string]string{"model": "p/m", "persona": "flash", "role": "coder", "stack": "abc"})
	s.Log("user", "do the thing")
	tc := provider.ToolCall{
		ID: "call-1", Type: "function",
		Function: provider.Function{Name: "read", Arguments: `{"path":"README.md"}`},
	}
	s.Log("assistant", "")
	s.Log("tool_call", tc)
	s.Log("tool_result", "hello")
	s.Log("assistant", "done")
	s.Close()

	hist, err := History(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 4 {
		t.Fatalf("history = %d messages: %+v", len(hist), hist)
	}
	if hist[0].Role != "user" || hist[0].Content != "do the thing" {
		t.Fatalf("user message wrong: %+v", hist[0])
	}
	if hist[1].Role != "assistant" || len(hist[1].ToolCalls) != 1 || hist[1].ToolCalls[0].ID != "call-1" {
		t.Fatalf("assistant tool call not reattached: %+v", hist[1])
	}
	if hist[2].Role != "tool" || hist[2].ToolCallID != "call-1" || hist[2].Name != "read" || hist[2].Content != "hello" {
		t.Fatalf("tool result not paired: %+v", hist[2])
	}
	if hist[3].Role != "assistant" || hist[3].Content != "done" {
		t.Fatalf("final assistant wrong: %+v", hist[3])
	}

	info, err := InfoOf(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Model != "p/m" || info.Persona != "flash" || info.Role != "coder" || info.Turns != 1 {
		t.Fatalf("info = %+v", info)
	}
}

func TestSearchAndFork(t *testing.T) {
	s := newTestSession(t)
	s.Log("meta", map[string]string{"model": "p/m"})
	s.Log("user", "refactor the auth middleware")
	s.Log("assistant", "I will preserve the refusal-proofing sections.")
	s.Close()

	hits, err := Search("AUTH", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != s.ID || !strings.Contains(hits[0].Snippet, "auth") {
		t.Fatalf("hits = %+v", hits)
	}
	if _, err := Search("  ", 10); err == nil {
		t.Fatal("empty query accepted")
	}

	fork, err := Fork(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer fork.Close()
	if fork.ID == s.ID {
		t.Fatal("fork reused source id")
	}
	fh, err := History(fork.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(fh) != 2 {
		t.Fatalf("fork history = %d: %+v", len(fh), fh)
	}
	fi, err := InfoOf(fork.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Model != "p/m" {
		t.Fatalf("fork lost meta: %+v", fi)
	}
}

func TestFormatInfoTableShowsTheTitle(t *testing.T) {
	// Both sessions live in the same store, so the index sees them together.
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	titled, err := New()
	if err != nil {
		t.Fatal(err)
	}
	titled.Log("meta", map[string]string{"model": "p/m", "persona": "flash"})
	titled.Log("user", "refactor the auth middleware")
	titled.Close()

	// A session from before titles existed: a user turn with no title event.
	untitled, err := New()
	if err != nil {
		t.Fatal(err)
	}
	untitled.Log("meta", map[string]string{"model": "p/m"})
	if _, err := untitled.db.Exec(`INSERT INTO events (ts, kind, json) VALUES (?,?,?)`,
		nowTS(), "user", `"an old task"`); err != nil {
		t.Fatal(err)
	}
	untitled.Close()

	infos, err := ListInfo()
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 {
		t.Fatalf("ListInfo = %d sessions: %+v", len(infos), infos)
	}
	table := FormatInfoTable(infos)
	if !strings.Contains(table, "refactor the auth middleware") {
		t.Fatalf("the table has no title:\n%s", table)
	}
	if !strings.Contains(table, "—") {
		t.Fatalf("a session without a title does not show an em dash:\n%s", table)
	}
	if !strings.Contains(table, "TITLE") {
		t.Fatalf("the table has no title column:\n%s", table)
	}
	// The columns the table had before are still there.
	for _, col := range []string{"ID", "MODEL", "PERSONA", "TURNS", "SIZE", "STARTED"} {
		if !strings.Contains(table, col) {
			t.Fatalf("the table lost the %s column:\n%s", col, table)
		}
	}
}

func TestFormatInfoTableTruncatesLongTitles(t *testing.T) {
	infos := []Info{{ID: "20260101-120000-abc123", Title: strings.Repeat("x", 120)}}
	table := FormatInfoTable(infos)
	lines := strings.Split(table, "\n")
	if len(lines) != 2 {
		t.Fatalf("table lines = %d:\n%s", len(lines), table)
	}
	row := lines[1]
	if !strings.Contains(row, "…") {
		t.Fatalf("a long title is not truncated:\n%s", table)
	}
	if strings.Contains(row, strings.Repeat("x", 41)) {
		t.Fatalf("a long title was not cut:\n%s", table)
	}
}
