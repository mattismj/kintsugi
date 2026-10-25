package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseLesson(t *testing.T) {
	e, ok := ParseLesson("- [2026-10-08] Vendor policy text is not enforcement fact. _models: [deepseek, qwen] _source: session 7f3a, confidence=medium")
	if !ok {
		t.Fatal("lesson not parsed")
	}
	if e.Date != "2026-10-08" {
		t.Fatalf("date: %q", e.Date)
	}
	if strings.Contains(e.Text, "_models") || strings.Contains(e.Text, "_source") {
		t.Fatalf("metadata leaked into text: %q", e.Text)
	}
	if e.Models != "deepseek, qwen" {
		t.Fatalf("models: %q", e.Models)
	}
	if !strings.Contains(e.Source, "session 7f3a") {
		t.Fatalf("source: %q", e.Source)
	}
	if _, ok := ParseLesson("not a lesson"); ok {
		t.Fatal("non-lesson line parsed")
	}
}

func TestAddSearchAndReindex(t *testing.T) {
	ws := t.TempDir()
	old := timeNow
	timeNow = func() time.Time { return time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC) }
	defer func() { timeNow = old }()

	s, err := Open(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.AddLesson("Tool errors must carry the workspace root; models cannot see CWD.", "deepseek", "session a1, confidence=high"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddLesson("Never preprocess tool arguments before validation.", "qwen", ""); err != nil {
		t.Fatal(err)
	}

	// A skill file is indexed too.
	skills := SkillsDir(ws)
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skills, "lab-run.md"), []byte("# Running the lab\n\nUse docker compose with egress denied."), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reindex(); err != nil {
		t.Fatal(err)
	}

	got, err := s.Search("workspace root CWD", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || !strings.Contains(got[0].Text, "workspace root") {
		t.Fatalf("search missed lesson: %+v", got)
	}

	got, err = s.Search("docker egress", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0].Kind != "skill" {
		t.Fatalf("skill not searchable: %+v", got)
	}

	// Hostile query strings must not break FTS syntax.
	if _, err := s.Search(`"root" AND ( OR *`, 5); err != nil {
		t.Fatalf("hostile query errored: %v", err)
	}
}

func TestReindexAfterManualEdit(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(Dir(ws), 0o755); err != nil {
		t.Fatal(err)
	}
	manual := "- [2026-10-01] Manual lesson about manifest hashing.\n"
	if err := os.WriteFile(LearnedPath(ws), []byte(manual), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	n, err := s.Reindex()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("indexed %d, want 1", n)
	}
	got, _ := s.Search("manifest hashing", 3)
	if len(got) != 1 || got[0].Date != "2026-10-01" {
		t.Fatalf("manual lesson not indexed: %+v", got)
	}
}
