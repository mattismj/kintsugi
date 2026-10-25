package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeAgent(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadFindsBothTiers(t *testing.T) {
	ws := t.TempDir()
	global := t.TempDir()

	writeAgent(t, WorkspaceDir(ws), "local.md", "---\nname: local\ndescription: workspace agent\n---\nlocal body\n")
	writeAgent(t, global, "global.md", "---\nname: global\ndescription: user agent\n---\nglobal body\n")

	list, err := Load(WorkspaceDir(ws), global)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("Load returned %d agents, want 2: %+v", len(list), list)
	}
	names := []string{list[0].Name, list[1].Name}
	if names[0] != "global" || names[1] != "local" {
		t.Fatalf("names = %v, want [global local] (sorted)", names)
	}
}

func TestWorkspaceWinsOnClash(t *testing.T) {
	ws := t.TempDir()
	global := t.TempDir()

	writeAgent(t, WorkspaceDir(ws), "reviewer.md", "---\nname: reviewer\ndescription: workspace copy\n---\nworkspace body\n")
	writeAgent(t, global, "reviewer.md", "---\nname: reviewer\ndescription: global copy\n---\nglobal body\n")

	list, err := Load(WorkspaceDir(ws), global)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("Load returned %d agents, want 1: %+v", len(list), list)
	}
	if list[0].Description != "workspace copy" {
		t.Fatalf("description = %q, want the workspace copy", list[0].Description)
	}
	if !strings.Contains(list[0].Body, "workspace body") {
		t.Fatalf("body = %q, want the workspace body", list[0].Body)
	}
}

func TestBrokenFrontmatterIsSkipped(t *testing.T) {
	dir := t.TempDir()
	writeAgent(t, dir, "good.md", "---\nname: good\ndescription: fine\n---\nbody\n")
	// Unterminated frontmatter.
	writeAgent(t, dir, "unterminated.md", "---\nname: broken\ndescription: no closing delimiter\n")
	// Invalid YAML.
	writeAgent(t, dir, "badyaml.md", "---\nname: broken\ntools: [read, list\n---\nbody\n")

	list, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("Load returned %d agents, want only the good one: %+v", len(list), list)
	}
	if list[0].Name != "good" {
		t.Fatalf("name = %q, want good", list[0].Name)
	}
}

func TestSelectIsExact(t *testing.T) {
	list := []Agent{{Name: "reviewer"}, {Name: "Researcher"}}
	if a, ok := Select(list, "reviewer"); !ok || a.Name != "reviewer" {
		t.Fatalf("Select(reviewer) = %+v %v", a, ok)
	}
	if _, ok := Select(list, "Reviewer"); ok {
		t.Fatal("Select must match the name exactly")
	}
	if _, ok := Select(list, "review"); ok {
		t.Fatal("Select must not match a prefix")
	}
	if _, ok := Select(list, ""); ok {
		t.Fatal("Select must not match an empty name")
	}
}

func TestBodyIsPreserved(t *testing.T) {
	dir := t.TempDir()
	body := "You are a reviewer.\n\n- read the diff\n- report the findings\n"
	writeAgent(t, dir, "reviewer.md", "---\nname: reviewer\ndescription: reviews\nmodel: researcher\ntools: read, list, glob, grep\nmax_turns: 4\nbackground: true\npermission_mode: plan\n---\n"+body)

	list, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("Load returned %d agents, want 1", len(list))
	}
	a := list[0]
	if a.Body != body {
		t.Fatalf("body = %q, want %q", a.Body, body)
	}
	if a.Model != "researcher" {
		t.Fatalf("model = %q, want researcher", a.Model)
	}
	if a.MaxTurns != 4 {
		t.Fatalf("max_turns = %d, want 4", a.MaxTurns)
	}
	if !a.Background {
		t.Fatal("background = false, want true")
	}
	if a.PermissionMode != "plan" {
		t.Fatalf("permission_mode = %q, want plan", a.PermissionMode)
	}
	if !a.ReadOnly() {
		t.Fatal("permission_mode plan must hold the agent to the read-only floor")
	}
	want := []string{"read", "list", "glob", "grep"}
	got := a.ToolList()
	if len(got) != len(want) {
		t.Fatalf("tools = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tools = %v, want %v", got, want)
		}
	}
}

func TestToolsAcceptBothSpellings(t *testing.T) {
	dir := t.TempDir()
	writeAgent(t, dir, "seq.md", "---\nname: seq\ntools:\n  - read\n  - grep\n---\nbody\n")
	list, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("Load returned %d agents, want 1", len(list))
	}
	if got := list[0].ToolList(); len(got) != 2 || got[0] != "read" || got[1] != "grep" {
		t.Fatalf("tools = %v, want [read grep]", got)
	}
}

func TestEmptyToolListFallsBackToReadOnly(t *testing.T) {
	dir := t.TempDir()
	writeAgent(t, dir, "plain.md", "---\nname: plain\n---\nbody\n")
	list, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("Load returned %d agents, want 1", len(list))
	}
	got := list[0].ToolList()
	if len(got) != len(DefaultTools) {
		t.Fatalf("tools = %v, want the read-only default %v", got, DefaultTools)
	}
	if list[0].Allows("write") {
		t.Fatal("an agent that declares no tools must not be allowed to write")
	}
}

func TestMissingDirectoryIsNotAnError(t *testing.T) {
	list, err := Load(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("Load returned %d agents, want 0", len(list))
	}
}

func TestNameFallsBackToTheFileName(t *testing.T) {
	dir := t.TempDir()
	writeAgent(t, dir, "named-by-file.md", "---\ndescription: no name in the frontmatter\n---\nbody\n")
	list, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(list) != 1 || list[0].Name != "named-by-file" {
		t.Fatalf("Load returned %+v, want the file name as the agent name", list)
	}
}
