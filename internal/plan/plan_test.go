package plan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSaveListLoadRoundTrip(t *testing.T) {
	ws := t.TempDir()

	art, err := Save(ws, "Add OAuth login", "1. read the config\n2. add the callback\n")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if art.Name != "add-oauth-login" {
		t.Fatalf("slug = %q, want add-oauth-login", art.Name)
	}
	if want := filepath.Join(ws, ".kintsugi", "plans", "add-oauth-login.md"); art.Path != want {
		t.Fatalf("path = %q, want %q", art.Path, want)
	}
	if art.Created.IsZero() {
		t.Fatal("the artifact carries no creation time")
	}
	if !strings.Contains(art.Content, Marker) {
		t.Fatalf("the document carries no marker:\n%s", art.Content)
	}
	if !strings.HasPrefix(art.Content, Marker) {
		t.Fatalf("the marker must open the document:\n%s", art.Content)
	}
	if Title(art.Content) != "Add OAuth login" {
		t.Fatalf("title = %q, want %q", Title(art.Content), "Add OAuth login")
	}

	list, err := List(ws)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List returned %d plans, want 1", len(list))
	}
	if list[0].Name != art.Name || list[0].Path != art.Path {
		t.Fatalf("List returned %+v, want %+v", list[0], art)
	}

	loaded, err := Load(ws, "add-oauth-login")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Content != art.Content {
		t.Fatalf("loaded content differs:\n%s\n---\n%s", loaded.Content, art.Content)
	}
	if loaded.Name != art.Name {
		t.Fatalf("loaded name = %q, want %q", loaded.Name, art.Name)
	}

	// The original title resolves to the same document.
	if _, err := Load(ws, "Add OAuth login"); err != nil {
		t.Fatalf("Load by title: %v", err)
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Add OAuth login":     "add-oauth-login",
		"  Fix   the  bug!  ": "fix-the-bug",
		"UPPER_snake.kebab":   "upper-snake-kebab",
		"---edges---":         "edges",
		"a/b\\c":              "a-b-c",
		"日本語":                 "",
	}
	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
	// A slug is idempotent: slugging a slug changes nothing.
	for in := range cases {
		once := Slug(in)
		if twice := Slug(once); twice != once {
			t.Errorf("Slug is not idempotent: %q -> %q -> %q", in, once, twice)
		}
	}
}

func TestListIgnoresMalformedPlans(t *testing.T) {
	ws := t.TempDir()
	dir := Dir(ws)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A file with no marker, a file with a marker but no heading and a
	// non-Markdown file: none of them is a plan.
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("# just notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "half.md"), []byte(Marker+"\nno heading here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("ignore me"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Save(ws, "real plan", "body"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	list, err := List(ws)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Name != "real-plan" {
		t.Fatalf("List returned %+v, want only the real plan", list)
	}
}

func TestPlanDirIsCreatedOnDemand(t *testing.T) {
	ws := t.TempDir()
	if _, err := os.Stat(Dir(ws)); !os.IsNotExist(err) {
		t.Fatalf("the plan directory exists before the first save (err=%v)", err)
	}
	if _, err := Save(ws, "first", "body"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(Dir(ws))
	if err != nil || !info.IsDir() {
		t.Fatalf("the plan directory was not created on demand: %v", err)
	}
}

func TestListIsNewestFirst(t *testing.T) {
	ws := t.TempDir()
	old, err := Save(ws, "older plan", "one")
	if err != nil {
		t.Fatal(err)
	}
	newer, err := Save(ws, "newer plan", "two")
	if err != nil {
		t.Fatal(err)
	}
	// Force a clear age difference: the file times may otherwise be equal.
	older := old.Created.Add(-time.Hour)
	if err := os.Chtimes(old.Path, older, older); err != nil {
		t.Fatal(err)
	}
	list, err := List(ws)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List returned %d plans, want 2", len(list))
	}
	if list[0].Name != newer.Name {
		t.Fatalf("newest plan = %q, want %q", list[0].Name, newer.Name)
	}
}

func TestLoadRejectsUnknownAndNonPlan(t *testing.T) {
	ws := t.TempDir()
	if _, err := Load(ws, "nothing"); err == nil {
		t.Fatal("loading a missing plan must fail")
	}
	dir := Dir(ws)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "junk.md"), []byte("not a plan"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(ws, "junk"); err == nil {
		t.Fatal("loading a file that is not a plan must fail")
	}
}

func TestDocumentKeepsTheModelsOwnHeading(t *testing.T) {
	doc := Document("Fallback title", "# Real title\n\nbody")
	if Title(doc) != "Real title" {
		t.Fatalf("title = %q, want %q", Title(doc), "Real title")
	}
	if !strings.Contains(doc, Marker) {
		t.Fatal("the marker is missing")
	}
}
