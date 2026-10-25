package persona

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const appendFixture = `---
name: skeleton
description: >
  A tiny persona whose trigger map starts empty; used by the apply tests.
match:
  family: [mock]
inherits: [_base]
triggers:
  policy: |
    Entries are added only from operator-approved telemetry.
  known: []
model_notes: |
  Nothing to see here.
---
# Skeleton

Body text that must survive an append verbatim.

## A section

- a bullet
`

func writeFixture(t *testing.T, body string) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "skeleton.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, path
}

func TestAppendTriggersRoundTripsThroughLoad(t *testing.T) {
	dir, path := writeFixture(t, appendFixture)

	n, err := AppendTriggers(path, []Trigger{{
		Term:    "I can't assist with that",
		Instead: "the same work described by its task parameters",
		Class:   "moralizing",
		Added:   "2026-02-01",
	}})
	if err != nil {
		t.Fatalf("AppendTriggers: %v", err)
	}
	if n != 1 {
		t.Fatalf("appended = %d, want 1", n)
	}

	// The written file must load: AppendTriggers cannot be able to create a
	// pack that the loader rejects.
	pack, err := Load(dir, "skeleton")
	if err != nil {
		t.Fatalf("Load after append: %v\n%s", err, mustRead(t, path))
	}
	per, ok := pack.Get("skeleton")
	if !ok {
		t.Fatal("skeleton persona missing after append")
	}
	if len(per.Triggers.Known) != 1 {
		t.Fatalf("triggers.known = %+v, want one entry", per.Triggers.Known)
	}
	got := per.Triggers.Known[0]
	if got.Term != "I can't assist with that" || got.Instead != "the same work described by its task parameters" ||
		got.Class != "moralizing" || got.Added != "2026-02-01" {
		t.Fatalf("appended trigger = %+v", got)
	}
	// Policy (a block scalar) and the other keys must be untouched.
	if !strings.Contains(per.Triggers.Policy, "operator-approved telemetry") {
		t.Fatalf("triggers.policy mangled: %q", per.Triggers.Policy)
	}
	if per.Name != "skeleton" || len(per.Match.Family) != 1 || per.Match.Family[0] != "mock" {
		t.Fatalf("frontmatter mangled: %+v", per)
	}
	if !strings.Contains(per.ModelNotes, "Nothing to see here") {
		t.Fatalf("model_notes mangled: %q", per.ModelNotes)
	}
}

func TestAppendTriggersPreservesBody(t *testing.T) {
	_, path := writeFixture(t, appendFixture)
	wantBody := appendFixture[strings.Index(appendFixture, "---\n# Skeleton")+len("---\n"):]

	if _, err := AppendTriggers(path, []Trigger{{
		Term: "as an AI assistant", Instead: "the work item named by its task parameters", Class: "identity",
	}}); err != nil {
		t.Fatalf("AppendTriggers: %v", err)
	}
	raw := mustRead(t, path)
	if !strings.HasSuffix(raw, wantBody) {
		t.Fatalf("body not preserved verbatim:\n--- got ---\n%s\n--- want tail ---\n%s", raw, wantBody)
	}
	if !strings.HasPrefix(raw, "---\n") {
		t.Fatalf("file no longer starts with a frontmatter fence:\n%s", raw)
	}
}

func TestAppendTriggersNoOpOnEmpty(t *testing.T) {
	_, path := writeFixture(t, appendFixture)
	before := mustRead(t, path)

	n, err := AppendTriggers(path, nil)
	if err != nil || n != 0 {
		t.Fatalf("empty append = (%d, %v), want (0, nil)", n, err)
	}
	if got := mustRead(t, path); got != before {
		t.Fatal("empty append rewrote the file")
	}

	// A trigger with no term or no rephrase proposes nothing and must not touch
	// the file either.
	n, err = AppendTriggers(path, []Trigger{{Term: "  ", Instead: "something"}})
	if err != nil || n != 0 {
		t.Fatalf("blank trigger = (%d, %v), want (0, nil)", n, err)
	}
	if got := mustRead(t, path); got != before {
		t.Fatal("blank trigger rewrote the file")
	}
}

func TestAppendTriggersRejectsSelfDefeating(t *testing.T) {
	_, path := writeFixture(t, appendFixture)
	before := mustRead(t, path)

	_, err := AppendTriggers(path, []Trigger{{
		Term:    "malware",
		Instead: "write the malware analysis harness",
		Class:   "moralizing",
	}})
	if err == nil {
		t.Fatal("self-defeating rephrase was accepted")
	}
	if !strings.Contains(err.Error(), "self-defeating") {
		t.Fatalf("error should name the rule: %v", err)
	}
	if got := mustRead(t, path); got != before {
		t.Fatal("a rejected append wrote to the file")
	}
}

func TestAppendTriggersSkipsDuplicateTerm(t *testing.T) {
	dir, path := writeFixture(t, appendFixture)
	if _, err := AppendTriggers(path, []Trigger{{Term: "C2", Instead: "tasking harness", Class: "moralizing"}}); err != nil {
		t.Fatalf("first append: %v", err)
	}
	n, err := AppendTriggers(path, []Trigger{{Term: "c2", Instead: "operator tasking harness", Class: "moralizing"}})
	if err != nil {
		t.Fatalf("second append: %v", err)
	}
	if n != 0 {
		t.Fatalf("duplicate term appended (%d)", n)
	}
	pack, err := Load(dir, "skeleton")
	if err != nil {
		t.Fatal(err)
	}
	per, _ := pack.Get("skeleton")
	if len(per.Triggers.Known) != 1 {
		t.Fatalf("triggers.known = %+v, want one entry", per.Triggers.Known)
	}
}

func TestAppendTriggersCreatesTriggersKey(t *testing.T) {
	body := `---
name: bare
description: no trigger map at all
---
# Bare

body
`
	dir, path := writeFixture(t, body)
	n, err := AppendTriggers(path, []Trigger{{Term: "identity", Instead: "the task's inputs and outputs", Class: "identity", Added: "2026-02-01"}})
	if err != nil || n != 1 {
		t.Fatalf("append = (%d, %v)", n, err)
	}
	pack, err := Load(dir, "bare")
	if err != nil {
		t.Fatalf("Load: %v\n%s", err, mustRead(t, path))
	}
	per, _ := pack.Get("bare")
	if len(per.Triggers.Known) != 1 || per.Triggers.Known[0].Term != "identity" {
		t.Fatalf("triggers = %+v", per.Triggers.Known)
	}
	if !strings.HasSuffix(mustRead(t, path), "# Bare\n\nbody\n") {
		t.Fatalf("body not preserved:\n%s", mustRead(t, path))
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
