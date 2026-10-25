package workbench

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	fctx "kintsugi/internal/context"
	"kintsugi/internal/persona"
	"kintsugi/internal/prompt"
)

func doc(t *testing.T, body string) *fctx.Document {
	t.Helper()
	d, warns := fctx.Parse([]byte(body))
	if len(warns) > 0 {
		t.Logf("warnings: %v", warns)
	}
	return d
}

func TestContextSnapshotLabelsGatesAndTruncation(t *testing.T) {
	d := doc(t, `# Project: lab

## Authorization & scope

Authorized local sandbox work.

## Domain context

`+strings.Repeat("domain detail ", 80)+`

## Current state

Working.
`)
	s := SnapshotContext("/ws/CONTEXT.md", d, nil, 80, []string{"state"})
	if !s.Exists || s.Project != "lab" {
		t.Fatalf("snapshot header wrong: %#v", s)
	}
	if len(s.Sections) != 3 {
		t.Fatalf("sections = %d, want 3", len(s.Sections))
	}
	if s.Sections[0].Gate != "gated" {
		t.Fatalf("authorization gate = %q, want gated", s.Sections[0].Gate)
	}
	if s.Sections[1].Gate != "review" {
		t.Fatalf("domain gate = %q, want review", s.Sections[1].Gate)
	}
	if s.Sections[2].Gate != "auto" {
		t.Fatalf("state gate = %q, want auto", s.Sections[2].Gate)
	}
	if !s.Truncated {
		t.Fatal("expected truncation under a tight budget")
	}
	if !s.Sections[0].Injected {
		t.Fatal("authorization should fit the budget")
	}
	if s.Sections[2].Injected {
		t.Fatal("trailing state section should be marked dropped")
	}
}

func TestDiffContext(t *testing.T) {
	old := ContextSnapshot{Sections: []ContextSection{
		{Class: "authorization", Title: "Authorization & scope", Tokens: 10, Hash: "a"},
		{Class: "state", Title: "Current state", Tokens: 10, Hash: "s1"},
	}}
	cur := ContextSnapshot{Sections: []ContextSection{
		{Class: "authorization", Title: "Authorization & scope", Tokens: 10, Hash: "a"},
		{Class: "state", Title: "Current state", Tokens: 18, Hash: "s2"},
		{Class: "stack", Title: "Stack", Tokens: 5, Hash: "k"},
	}}
	got := strings.Join(DiffContext(old, cur), "\n")
	for _, want := range []string{"~ state (10 → 18 tok)", "+ stack (5 tok)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("diff missing %q:\n%s", want, got)
		}
	}
}

func TestDiffContextDuplicateClass(t *testing.T) {
	old := SnapshotContext("x", doc(t, `# Project: p

## Current state

one
`), nil, 0, nil)
	cur := SnapshotContext("x", doc(t, `# Project: p

## Current state

one

## Current state

two
`), nil, 0, nil)
	got := strings.Join(DiffContext(old, cur), "\n")
	if !strings.Contains(got, "+ state[2]") {
		t.Fatalf("duplicate section not reported:\n%s", got)
	}
}

func TestPersonaSnapshotEffectiveStackAndTriggers(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("_base.md", "---\nname: _base\n---\nBASE\n")
	write("child.md", `---
name: child
inherits: [_base]
description: test persona
triggers:
  known:
    - term: "C2"
      instead: "tasking harness"
      class: moralizing
---
CHILD
`)
	pack, err := persona.Load(dir, "_base")
	if err != nil {
		t.Fatal(err)
	}
	sel, err := pack.Select("child", "", "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	comp := &prompt.Compiled{Manifest: prompt.Manifest{Layers: []prompt.ManifestLayer{
		{ID: "persona", Tokens: 123},
	}}}
	s := SnapshotPersonas(pack, sel, comp, "mock/deepseek-chat")
	if strings.Join(s.Chain, "→") != "_base→child" {
		t.Fatalf("chain = %v", s.Chain)
	}
	if s.Tokens != 123 || s.Selected != "child" || s.Rule != "explicit" {
		t.Fatalf("snapshot = %#v", s)
	}
	if len(s.Triggers) != 1 || s.Triggers[0].Term != "C2" {
		t.Fatalf("triggers = %#v", s.Triggers)
	}
	if len(s.Available) != 2 {
		t.Fatalf("available = %d, want 2", len(s.Available))
	}
	var marked bool
	for _, c := range s.Available {
		if c.Name == "child" && c.Matched && c.Effective {
			marked = true
		}
	}
	if !marked {
		t.Fatal("effective/matched persona not marked")
	}
}
