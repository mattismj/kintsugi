package skills

// WP-3.7: the writing half of skill promotion. A promoted procedure must land
// as a file the existing loader reads back — not as a second, parallel format.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteProducesALoadableSkill(t *testing.T) {
	ws := t.TempDir()
	dir := Dir(ws, "")

	path, err := Write(dir, "lab-rebuild", "Rebuild the lab stack from scratch.",
		"## Steps\n\n1. `docker compose down -v`\n2. `make lab`\n3. wait for the health probe\n")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(
		ws, ".kintsugi", "skills", "lab-rebuild", "SKILL.md"); path != want {
		t.Fatalf("wrote %s, want %s", path, want)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"---\n",
		"name: lab-rebuild\n",
		"description: Rebuild the lab stack from scratch.\n",
		"# lab-rebuild\n",
		"1. `docker compose down -v`",
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("SKILL.md is missing %q:\n%s", want, raw)
		}
	}

	// The existing loader is the acceptance test: promotion is only useful if
	// the next session's skill index picks the result up.
	list := Load(ws, dir)
	sk, ok := Find(list, "lab-rebuild")
	if !ok {
		t.Fatalf("promoted skill is not discoverable: %+v", list)
	}
	if sk.Name != "lab-rebuild" || sk.Description != "Rebuild the lab stack from scratch." {
		t.Fatalf("loaded skill = %+v", sk)
	}
	if !strings.Contains(sk.Body, "make lab") {
		t.Fatalf("loaded skill lost its procedure:\n%s", sk.Body)
	}
	if idx := Index(list); !strings.Contains(idx, "`lab-rebuild` — Rebuild the lab stack from scratch.") {
		t.Fatalf("promoted skill is absent from the prompt index:\n%s", idx)
	}
}

func TestWriteRejectsUnsafeNames(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills")
	for _, name := range []string{
		"", " ", "../escape", "a/b", `a\b`, ".hidden", ".", "..", "bad:name", `quo"te`, "tab\there", "new\nline",
	} {
		if err := ValidateName(name); err == nil {
			t.Fatalf("name %q was accepted", name)
		}
		if _, err := Write(dir, name, "d", "body"); err == nil {
			t.Fatalf("Write accepted the unsafe name %q", name)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("a rejected name created the skills directory")
	}
	for _, name := range []string{"deploy", "lab-rebuild", "Fix_Bug", "a.b"} {
		if err := ValidateName(name); err != nil {
			t.Fatalf("name %q was rejected: %v", name, err)
		}
	}
}

func TestWriteRequiresABodyAndAWritableDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills")
	if _, err := Write(dir, "empty", "d", "   \n"); err == nil {
		t.Fatal("an empty procedure was written")
	}
	if _, err := Write("", "deploy", "d", "body"); err == nil {
		t.Fatal("a skill was written with no skills directory")
	}
	if _, err := os.Stat(filepath.Join(dir, "empty")); !os.IsNotExist(err) {
		t.Fatal("the empty skill left a directory behind")
	}
}

func TestWriteReplacesAndReportsExistence(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills")
	if Exists(dir, "deploy") {
		t.Fatal("Exists reported a skill that was never written")
	}
	first, err := Write(dir, "deploy", "v1", "step one")
	if err != nil {
		t.Fatal(err)
	}
	if Exists(dir, "deploy") {
		// expected: the write above created it. The check below is what matters.
		_ = first
	} else {
		t.Fatal("Exists missed a skill that was just written")
	}
	if _, err := Write(dir, "deploy", "v2", "step two"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "step one") || !strings.Contains(string(raw), "step two") {
		t.Fatalf("a second write did not replace the procedure:\n%s", raw)
	}

	// A flat skill file also counts as existing: the loader accepts either form,
	// so promotion must not create a duplicate alongside it.
	flatDir := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(flatDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(flatDir, "triage.md"), []byte("# Triage\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !Exists(flatDir, "triage") {
		t.Fatal("Exists missed the flat <name>.md form")
	}
}

func TestRenderKeepsFrontmatterParseable(t *testing.T) {
	// A description with a newline would end the YAML key early; Render
	// collapses it rather than writing a file the loader cannot parse.
	got := Render("deploy", "Build   the\nlab  stack.", "run make deploy")
	if !strings.Contains(got, "description: Build the lab stack.\n") {
		t.Fatalf("description not collapsed onto one line:\n%s", got)
	}
	if !strings.HasPrefix(got, "---\nname: deploy\n") {
		t.Fatalf("frontmatter malformed:\n%s", got)
	}
	dir := filepath.Join(t.TempDir(), "skills")
	path, err := Write(dir, "multi", "a\nb", "body line")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := parse(path, mustRead(t, path)); !ok {
		t.Fatalf("the rendered file does not parse back:\n%s", mustRead(t, path))
	}

	// A body that already begins with its own H1 is not given a second one.
	withHeading := Render("deploy", "d", "# Deploy\n\nrun make deploy")
	if strings.Count(withHeading, "# Deploy") != 1 {
		t.Fatalf("duplicate heading:\n%s", withHeading)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
