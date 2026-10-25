package session

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDeriveTitleTrimsAndCollapses(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"  refactor the auth middleware  ", "refactor the auth middleware"},
		{"first line\nsecond line", "first line second line"},
		{"tabs\tand\nnewlines\r\nhere", "tabs and newlines here"},
		{"", ""},
		{"   \n\t ", ""},
	}
	for _, c := range cases {
		got := DeriveTitle(c.in)
		if got != c.want {
			t.Fatalf("DeriveTitle(%q) = %q, want %q", c.in, got, c.want)
		}
		if strings.ContainsAny(got, "\n\r\t") {
			t.Fatalf("title is not single line: %q", got)
		}
	}
}

func TestDeriveTitleIsBounded(t *testing.T) {
	long := strings.Repeat("word ", 40)
	got := DeriveTitle(long)
	if utf8.RuneCountInString(got) > TitleMaxRunes+1 { // +1 for the ellipsis
		t.Fatalf("title is %d runes: %q", utf8.RuneCountInString(got), got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("a cut title must end with an ellipsis: %q", got)
	}
	if strings.Contains(got, "  ") {
		t.Fatalf("title has a double space: %q", got)
	}
	// A short task is kept whole.
	if got := DeriveTitle("short task"); got != "short task" {
		t.Fatalf("DeriveTitle = %q", got)
	}
}

func TestFirstUserTurnSetsTheTitle(t *testing.T) {
	s := newTestSession(t)
	s.Log("meta", map[string]string{"model": "p/m"})
	s.Log("user", "refactor the auth middleware")
	s.Close()

	entries, err := readEntries(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Kind != "meta" {
			continue
		}
		if got := metaString(e.V, "title"); got != "" {
			found = true
			if got != "refactor the auth middleware" {
				t.Fatalf("title = %q", got)
			}
		}
	}
	if !found {
		t.Fatal("the first user turn did not set a title")
	}
}

func TestTitleIsStoredOnlyOnce(t *testing.T) {
	s := newTestSession(t)
	s.Log("user", "first task")
	s.Log("user", "second task")
	s.Close()

	entries, err := readEntries(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	titles := 0
	for _, e := range entries {
		if e.Kind == "meta" && metaString(e.V, "title") != "" {
			titles++
		}
	}
	if titles != 1 {
		t.Fatalf("titles in the log = %d, want 1", titles)
	}
	info, err := InfoOf(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Title != "first task" {
		t.Fatalf("title = %q, want the first task", info.Title)
	}
}

func TestInfoTitleIsPopulatedFromTheMetaEvent(t *testing.T) {
	s := newTestSession(t)
	s.Log("meta", map[string]string{"model": "p/m", "persona": "flash"})
	s.Log("user", "write the release notes")
	s.Close()

	info, err := InfoOf(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Title != "write the release notes" {
		t.Fatalf("Info.Title = %q", info.Title)
	}
	if info.DisplayTitle() != "write the release notes" {
		t.Fatalf("DisplayTitle = %q", info.DisplayTitle())
	}
}

func TestSessionWithoutATitleShowsADash(t *testing.T) {
	s := newTestSession(t)
	s.Log("meta", map[string]string{"model": "p/m"})
	// A session written before titles existed: the user turn is in the log,
	// but no title was ever derived for it.
	if _, err := s.db.Exec(`INSERT INTO events (ts, kind, json) VALUES (?,?,?)`,
		nowTS(), "user", `"the old task"`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	info, err := InfoOf(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Title != "" {
		t.Fatalf("title = %q, want none", info.Title)
	}
	if info.DisplayTitle() != "—" {
		t.Fatalf("DisplayTitle = %q, want an em dash", info.DisplayTitle())
	}
}

func TestRenameWritesAMetaEventAndIsReadBack(t *testing.T) {
	s := newTestSession(t)
	s.Log("user", "the original task")
	s.Close()

	if err := Rename(s.ID, "A better title"); err != nil {
		t.Fatal(err)
	}
	info, err := InfoOf(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Title != "A better title" {
		t.Fatalf("title after rename = %q", info.Title)
	}

	entries, err := readEntries(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Kind == "meta" && metaString(e.V, "title") == "A better title" {
			found = true
		}
	}
	if !found {
		t.Fatal("rename wrote no meta event")
	}

	if err := Rename(s.ID, "  "); err == nil {
		t.Fatal("an empty title was accepted")
	}
}

func TestRenameOfAnUnknownSessionFails(t *testing.T) {
	if err := Rename("no-such-session-000000", "x"); err == nil {
		t.Fatal("renaming an unknown session succeeded")
	}
}

func TestSetTitleKeepsAnExistingTitle(t *testing.T) {
	s := newTestSession(t)
	s.Log("user", "first task")
	if s.SetTitle("second title") {
		t.Fatal("SetTitle overwrote an existing title")
	}
	s.Close()
	info, err := InfoOf(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Title != "first task" {
		t.Fatalf("title = %q", info.Title)
	}
}
