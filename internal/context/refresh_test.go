package context

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const refreshSample = `# Project: lab

Preamble prose the harness must not touch.

## Authorization & scope

- AUTH: authorized local sandbox work only, no egress.
- SCOPE: lab validation.
<!-- Operator-authored section. The harness never writes or widens this. -->

## Stack

- Go 1.26

## Current state

- stale bullet written three sessions ago

## Lessons (active)

- [2026-01-01] durable lesson from an earlier session
`

func writeSample(t *testing.T) (string, string) {
	t.Helper()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ws, StateFileName)
	if err := os.WriteFile(path, []byte(refreshSample), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws, path
}

func TestRefreshRewritesOnlyTheStateSection(t *testing.T) {
	ws, path := writeSample(t)

	res, err := Refresh(ws)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatal("expected Changed for a stale state section")
	}
	if res.Section != ClassState {
		t.Fatalf("section = %q, want %q", res.Section, ClassState)
	}
	if !strings.Contains(res.New, "- Recent: main.go") {
		t.Fatalf("new body has no file-scan signal:\n%s", res.New)
	}
	if res.Old == "" || strings.Contains(res.Old, "Build:") {
		t.Fatalf("Old should be the verbatim previous body:\n%s", res.Old)
	}

	// Snapshot every other class before applying.
	before, _, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rawBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	head := string(rawBefore[:strings.Index(string(rawBefore), "## Current state")])
	tail := string(rawBefore[strings.Index(string(rawBefore), "## Lessons (active)"):])

	if err := ApplyRefresh(ws, res); err != nil {
		t.Fatal(err)
	}
	rawAfter, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(rawAfter), head) {
		t.Fatalf("bytes before the state section changed:\n%s\n---\n%s", head, rawAfter)
	}
	if !strings.HasSuffix(string(rawAfter), tail) {
		t.Fatalf("bytes after the state section changed:\n%s\n---\n%s", tail, rawAfter)
	}
	if strings.Contains(string(rawAfter), "stale bullet") {
		t.Fatal("stale state body survived the refresh")
	}

	after, _, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, class := range []string{ClassAuthorization, ClassScope, ClassDomain, ClassStack, ClassConvention, ClassLesson} {
		oldSec, oldOK := before.Find(class)
		newSec, newOK := after.Find(class)
		if oldOK != newOK {
			t.Fatalf("%s presence changed", class)
		}
		if oldOK && (oldSec.Body != newSec.Body || oldSec.Title != newSec.Title) {
			t.Fatalf("%s body changed:\n%q\n%q", class, oldSec.Body, newSec.Body)
		}
		if oldOK && before.Hash(class) != after.Hash(class) {
			t.Fatalf("%s hash changed", class)
		}
	}

	// The state section now carries the signals.
	state, ok := after.Find(ClassState)
	if !ok {
		t.Fatal("state section lost")
	}
	if !strings.Contains(state.Body, "- Recent: main.go") {
		t.Fatalf("state body not written:\n%s", state.Body)
	}
}

func TestRefreshIsDeterministicAndNonGitWorks(t *testing.T) {
	ws, _ := writeSample(t)

	res, err := Refresh(ws)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.New, "- Uncommitted:") || strings.Contains(res.New, "- Diff:") {
		t.Fatalf("git signals leaked into a non-git workspace:\n%s", res.New)
	}
	if !strings.Contains(res.New, "- Recent: main.go") {
		t.Fatalf("file-scan path produced no signal:\n%s", res.New)
	}
	if err := ApplyRefresh(ws, res); err != nil {
		t.Fatal(err)
	}

	// Rewriting CONTEXT.md must not feed back into the next refresh.
	again, err := Refresh(ws)
	if err != nil {
		t.Fatal(err)
	}
	if again.Changed {
		t.Fatalf("refresh is not idempotent:\nold:\n%s\nnew:\n%s", again.Old, again.New)
	}
	if err := ApplyRefresh(ws, again); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshMissingFileErrors(t *testing.T) {
	if _, err := Refresh(t.TempDir()); err == nil {
		t.Fatal("expected an error for a workspace without CONTEXT.md")
	} else if !strings.Contains(err.Error(), "CONTEXT.md") {
		t.Fatalf("error should name the file: %v", err)
	}
}

func TestRefreshMissingStateSectionErrors(t *testing.T) {
	ws := t.TempDir()
	body := "# Project: lab\n\n## Authorization & scope\n\n- AUTH: lab only.\n"
	if err := os.WriteFile(filepath.Join(ws, StateFileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Refresh(ws); err == nil {
		t.Fatal("expected an error for a document without a state section")
	}
}

func TestApplyRefreshRefusesNonStateSection(t *testing.T) {
	ws, path := writeSample(t)
	res := RefreshResult{Section: ClassAuthorization, New: "- widened scope", Changed: true}
	if err := ApplyRefresh(ws, res); err == nil {
		t.Fatal("refresh must refuse to rewrite the authorization section")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != refreshSample {
		t.Fatal("refused refresh still modified the document")
	}
}

func TestRefreshPreservesTrailingNewlineAndSpacing(t *testing.T) {
	ws := t.TempDir()
	// No trailing newline, no blank line after the heading.
	doc := "# Project: lab\n\n## Current state\n- old"
	path := filepath.Join(ws, StateFileName)
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Refresh(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyRefresh(ws, res); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(string(raw), "\n") {
		t.Fatalf("refresh added a trailing newline: %q", raw)
	}
	if !strings.Contains(string(raw), "## Current state\n\n- No signals (no git checkout") {
		t.Fatalf("state heading not followed by the new body:\n%s", raw)
	}
	if !strings.HasPrefix(string(raw), "# Project: lab\n\n") {
		t.Fatalf("preamble changed:\n%s", raw)
	}
}
