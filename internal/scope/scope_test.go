package scope

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kintsugi/internal/config"
	"kintsugi/internal/context"
)

func TestEmptyRulesAllowEverything(t *testing.T) {
	var r Rules
	if !r.Empty() {
		t.Fatal("the zero rule set is not empty")
	}
	if !r.AllowsPath("../../etc/passwd") {
		t.Error("an empty scope must not narrow paths")
	}
	if !r.AllowsCommand("rm -rf /") {
		t.Error("an empty scope must not narrow commands")
	}
	if !r.AllowsHost("example.com") {
		t.Error("an empty scope must not narrow hosts")
	}
}

func TestPathRuleDeniesOutsidePaths(t *testing.T) {
	r := Rules{Paths: []string{"src/**", "docs/*.md"}}
	for _, ok := range []string{"src/main.go", "src/deep/nested.go", "docs/intro.md"} {
		if !r.AllowsPath(ok) {
			t.Errorf("%q should be in scope", ok)
		}
	}
	for _, bad := range []string{"README.md", "secrets/key.pem", "docs/sub/deep.md", "srcx/main.go"} {
		if r.AllowsPath(bad) {
			t.Errorf("%q should be out of scope", bad)
		}
	}
}

func TestPathRuleCoversDirectory(t *testing.T) {
	r := Rules{Paths: []string{"src"}}
	if !r.AllowsPath("src/main.go") {
		t.Error("a directory rule must cover everything under it")
	}
	if r.AllowsPath("srcx/main.go") {
		t.Error("a directory rule must not cover a sibling with the same prefix")
	}
}

func TestCommandRuleDeniesOutsideCommands(t *testing.T) {
	r := Rules{Commands: []string{"go test", "git*"}}
	for _, ok := range []string{"go test ./...", "go test", "git status", "git diff --stat"} {
		if !r.AllowsCommand(ok) {
			t.Errorf("%q should be in scope", ok)
		}
	}
	for _, bad := range []string{"curl https://example.com", "rm -rf /", "python3 x.py"} {
		if r.AllowsCommand(bad) {
			t.Errorf("%q should be out of scope", bad)
		}
	}
}

func TestCompoundCommandIsCheckedSegmentBySegment(t *testing.T) {
	r := Rules{Commands: []string{"go test"}}
	if !r.AllowsCommand("go test ./... && go test ./internal/...") {
		t.Error("a compound command of allowed commands is in scope")
	}
	if r.AllowsCommand("go test ./... && curl https://evil.example") {
		t.Error("a denied command must not ride along behind an allowed one")
	}
	if r.AllowsCommand("go test ./... | sh") {
		t.Error("a denied command must not ride along behind a pipe")
	}
	if r.AllowsCommand("echo $(curl https://evil.example)") {
		t.Error("a denied command must not hide inside a substitution")
	}
}

func TestHostRuleDeniesOtherHosts(t *testing.T) {
	r := Rules{Hosts: []string{"example.com", "*.lab.internal"}}
	for _, ok := range []string{"example.com", "api.example.com", "db.lab.internal"} {
		if !r.AllowsHost(ok) {
			t.Errorf("%q should be in scope", ok)
		}
	}
	for _, bad := range []string{"evil.com", "notexample.com", "example.com.evil.net", "example.org"} {
		if r.AllowsHost(bad) {
			t.Errorf("%q should be out of scope", bad)
		}
	}
}

func TestOutOfScopeAlwaysWins(t *testing.T) {
	r := Rules{
		Paths:      []string{"src/**"},
		Commands:   []string{"go test"},
		Hosts:      []string{"example.com"},
		OutOfScope: []string{"src/secrets/**", "production", "evil.com"},
	}
	if !r.AllowsPath("src/main.go") {
		t.Error("an in-scope path should still be allowed")
	}
	if r.AllowsPath("src/secrets/key.pem") {
		t.Error("out_of_scope must win over an allowed path")
	}
	if r.AllowsCommand("go test ./production/...") {
		t.Error("out_of_scope must win over an allowed command")
	}
	if r.AllowsHost("evil.com") {
		t.Error("out_of_scope must win over an allowed host")
	}
	if !r.AllowsHost("example.com") {
		t.Error("an in-scope host should still be allowed")
	}
}

func TestParseIsTolerant(t *testing.T) {
	src := `
# the operator's scope
// another comment style

paths = ["src/**", "docs/**"]   # trailing comment
commands = ["go test"]
hosts = ["example.com"]
out_of_scope = ["**/prod/**"]

[permissions]
allow_paths = ["ignored/**"]
`
	r, err := Parse(src)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if len(r.Paths) != 2 || r.Paths[0] != "src/**" || r.Paths[1] != "docs/**" {
		t.Errorf("paths = %q", r.Paths)
	}
	if len(r.Commands) != 1 || r.Commands[0] != "go test" {
		t.Errorf("commands = %q", r.Commands)
	}
	if len(r.Hosts) != 1 || r.Hosts[0] != "example.com" {
		t.Errorf("hosts = %q", r.Hosts)
	}
	if len(r.OutOfScope) != 1 || r.OutOfScope[0] != "**/prod/**" {
		t.Errorf("out_of_scope = %q", r.OutOfScope)
	}
}

func TestParseLineOrientedAndListShapes(t *testing.T) {
	r, err := Parse("paths: src/**\npaths: docs/**\ncommands:\n  - go test\n  - git status\n")
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if len(r.Paths) != 2 {
		t.Errorf("paths = %q", r.Paths)
	}
	if len(r.Commands) != 2 || r.Commands[1] != "git status" {
		t.Errorf("commands = %q", r.Commands)
	}
}

func TestParseIgnoresProse(t *testing.T) {
	src := `
- This is an authorized dual-use workspace operated by a licensed practitioner.
- Scope: defensive development, lab validation and adversarial self-assessment.
`
	r, err := Parse(src)
	if err != nil {
		t.Fatalf("prose must not fail the parse: %v", err)
	}
	if !r.Empty() {
		t.Errorf("prose must not become rules: %+v", r)
	}
}

func TestParseRejectsMalformedValue(t *testing.T) {
	if _, err := Parse(`paths = ["src/**"`); err == nil {
		t.Fatal("an unterminated list must be an error")
	}
}

func TestFromConfig(t *testing.T) {
	c := config.Config{}
	c.Scope = config.Scope{
		Paths:      []string{"src/**"},
		Commands:   []string{"go test"},
		Hosts:      []string{"example.com"},
		OutOfScope: []string{"**/prod/**"},
	}
	r := FromConfig(c)
	if len(r.Paths) != 1 || len(r.Commands) != 1 || len(r.Hosts) != 1 || len(r.OutOfScope) != 1 {
		t.Fatalf("config scope not carried over: %+v", r)
	}
	if !r.AllowsPath("src/main.go") || r.AllowsPath("README.md") {
		t.Error("the config scope is not enforced")
	}
}

func TestFromContextScopeSection(t *testing.T) {
	doc, _ := context.Parse([]byte(`# Project: demo

## Authorization & scope

- Operator prose that is not a rule.

## Scope

paths = ["src/**"]
out_of_scope = ["**/secrets/**"]
`))
	r := FromContext(doc)
	if len(r.Paths) != 1 || r.Paths[0] != "src/**" {
		t.Fatalf("paths = %q", r.Paths)
	}
	if len(r.OutOfScope) != 1 {
		t.Fatalf("out_of_scope = %q", r.OutOfScope)
	}
	if r.AllowsPath("src/secrets/key.pem") {
		t.Error("the CONTEXT.md deny list is not enforced")
	}
}

func TestFromWorkspaceMergesConfigAndContext(t *testing.T) {
	ws := t.TempDir()
	body := "## Scope\n\npaths = [\"docs/**\"]\n"
	if err := os.WriteFile(filepath.Join(ws, "CONTEXT.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c := config.Config{}
	c.Context = config.Context{Enabled: true, Path: "CONTEXT.md"}
	c.Scope = config.Scope{Paths: []string{"src/**"}}

	r := FromWorkspace(c, ws)
	if !r.AllowsPath("src/main.go") || !r.AllowsPath("docs/intro.md") {
		t.Errorf("the merged scope does not cover both sources: %+v", r)
	}
	if r.AllowsPath("README.md") {
		t.Error("the merged scope lets an undeclared path through")
	}
}

func TestMergeUnionsRules(t *testing.T) {
	a := Rules{Paths: []string{"src/**"}}
	b := Rules{Paths: []string{"docs/**"}, OutOfScope: []string{"prod"}}
	m := Merge(a, b)
	if len(m.Paths) != 2 || len(m.OutOfScope) != 1 {
		t.Fatalf("merge lost rules: %+v", m)
	}
}

func TestScopeReasonNamesTheBoundary(t *testing.T) {
	r := Rules{Paths: []string{"src/**"}}
	if r.AllowsPath("README.md") {
		t.Fatal("README.md should be out of scope")
	}
	// The message the tool layer builds carries the "out of scope" prefix.
	msg := "out of scope: path \"README.md\" is not in the declared scope"
	if !strings.Contains(msg, "out of scope") {
		t.Fatal("the refusal must say what went wrong")
	}
}
