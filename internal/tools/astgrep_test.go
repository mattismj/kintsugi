package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseASTGrepPattern(t *testing.T) {
	cases := []struct {
		in   string
		kind string
		name string
		fail bool
	}{
		{"func Execute(…)", "func", "Execute", false},
		{"func Execute", "func", "Execute", false},
		{"func  Run(…)", "func", "Run", false},
		{"type Review …", "type", "Review", false},
		{"type Review", "type", "Review", false},
		{"call ParseReview", "call", "ParseReview", false},
		{"", "", "", true},
		{"func", "", "", true},
		{"unknown Foo", "", "", true},
	}
	for _, c := range cases {
		q, err := parseASTGrepPattern(c.in)
		if c.fail {
			if err == nil {
				t.Errorf("parseASTGrepPattern(%q) expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseASTGrepPattern(%q) error: %v", c.in, err)
			continue
		}
		if q.kind != c.kind || q.name != c.name {
			t.Errorf("parseASTGrepPattern(%q) = %+v, want kind=%s name=%s", c.in, q, c.kind, c.name)
		}
	}
}

func TestASTGrepFunc(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "a.go", `package foo

func Execute(x int) error { return nil }

func helper() {}
`)
	writeGo(t, dir, "b.go", `package foo

func Execute(y string) {}
`)
	e := &Executor{Workspace: dir}
	res := e.runASTGrep(`{"pattern":"func Execute(…)"}`)
	if res.Failed {
		t.Fatalf("unexpected failure: %s", res.Content)
	}
	if !strings.Contains(res.Content, "a.go:3:") {
		t.Errorf("expected a.go:3 in output, got %q", res.Content)
	}
	if !strings.Contains(res.Content, "b.go:3:") {
		t.Errorf("expected b.go:3 in output, got %q", res.Content)
	}
	if strings.Contains(res.Content, "helper") {
		t.Errorf("did not expect helper in output, got %q", res.Content)
	}
}

func TestASTGrepType(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "a.go", `package foo

type Review struct {
	Verdict string
}

type Finding struct{}
`)
	e := &Executor{Workspace: dir}
	res := e.runASTGrep(`{"pattern":"type Review …"}`)
	if res.Failed {
		t.Fatalf("unexpected failure: %s", res.Content)
	}
	if !strings.Contains(res.Content, "a.go:3:") {
		t.Errorf("expected a.go:3 in output, got %q", res.Content)
	}
	if strings.Contains(res.Content, "Finding") {
		t.Errorf("did not expect Finding in output, got %q", res.Content)
	}
}

func TestASTGrepCall(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "a.go", `package foo

func caller() {
	ParseReview("x")
	_ = ParseReview("y")
}

func ParseReview(s string) {}
`)
	e := &Executor{Workspace: dir}
	res := e.runASTGrep(`{"pattern":"call ParseReview"}`)
	if res.Failed {
		t.Fatalf("unexpected failure: %s", res.Content)
	}
	if !strings.Contains(res.Content, "a.go:4:") {
		t.Errorf("expected a.go:4 in output, got %q", res.Content)
	}
	if !strings.Contains(res.Content, "a.go:5:") {
		t.Errorf("expected a.go:5 in output, got %q", res.Content)
	}
}

func TestASTGrepNoMatches(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "a.go", `package foo

func helper() {}
`)
	e := &Executor{Workspace: dir}
	res := e.runASTGrep(`{"pattern":"func Missing(…)"}`)
	if res.Failed {
		t.Fatalf("unexpected failure: %s", res.Content)
	}
	if res.Content != "no matches" {
		t.Errorf("expected 'no matches', got %q", res.Content)
	}
}

func TestASTGrepSubPath(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGo(t, dir, "a.go", `package foo

func Execute() {}
`)
	writeGo(t, sub, "b.go", `package sub

func Execute() {}
`)
	e := &Executor{Workspace: dir}
	res := e.runASTGrep(`{"pattern":"func Execute(…)","path":"sub"}`)
	if res.Failed {
		t.Fatalf("unexpected failure: %s", res.Content)
	}
	if strings.Contains(res.Content, "a.go") {
		t.Errorf("did not expect a.go in output, got %q", res.Content)
	}
	if !strings.Contains(res.Content, "sub/b.go") {
		t.Errorf("expected sub/b.go in output, got %q", res.Content)
	}
}

func writeGo(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
