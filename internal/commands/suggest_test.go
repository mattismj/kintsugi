package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kintsugi/internal/config"
	"kintsugi/internal/tools"
)

// ---------- helpers ----------

// suggestWorkspace builds a small workspace tree for the completion tests.
func suggestWorkspace(t *testing.T) string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ws := t.TempDir()
	files := []string{
		"main.go",
		"Makefile",
		"README.md",
		"internal/tui/view.go",
		"internal/tui/complete.go",
		"internal/tui/Main_test.go",
		"docs/guide.md",
		"id_rsa",
		"secrets.env",
	}
	for _, f := range files {
		full := filepath.Join(ws, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return ws
}

// values is the candidate values in order, for a compact assertion.
func values(list []Candidate) []string {
	out := make([]string, 0, len(list))
	for _, c := range list {
		out = append(out, c.Value)
	}
	return out
}

// hasValue reports whether the candidate list offers exactly value.
func hasValue(list []Candidate, value string) bool {
	for _, c := range list {
		if c.Value == value {
			return true
		}
	}
	return false
}

// suggestAt is Suggest with the cursor at the end of the line, the common case.
func suggestAt(line string, opts Options) []Candidate {
	return Suggest(line, len(line), opts)
}

// ---------- kind detection ----------

func TestSuggestOffersTheFourKindsAtABareAtSign(t *testing.T) {
	opts := Options{Workspace: suggestWorkspace(t)}
	for _, line := range []string{"@", "look at this @", "@file x @"} {
		got := suggestAt(line, opts)
		if len(got) != 4 {
			t.Fatalf("%q offered %v, want the four kinds", line, values(got))
		}
		for _, k := range referenceKinds {
			if !hasValue(got, "@"+k) {
				t.Fatalf("%q does not offer @%s: %v", line, k, values(got))
			}
		}
	}
}

func TestSuggestFiltersPartlyTypedKinds(t *testing.T) {
	opts := Options{Workspace: suggestWorkspace(t)}
	// "f" is a prefix of two keywords, so both are offered: the filter is a
	// prefix filter, not a guess at which one was meant.
	got := suggestAt("@f", opts)
	if len(got) != 2 || !hasValue(got, "@file") || !hasValue(got, "@folder") {
		t.Fatalf("@f offered %v, want @file and @folder", values(got))
	}
	if narrower := suggestAt("@fi", opts); len(narrower) != 1 || narrower[0].Value != "@file" {
		t.Fatalf("@fi offered %v, want just @file", values(narrower))
	}
	// The case of what was typed must not matter: the reference keywords are
	// matched case-insensitively by the expander too.
	got = suggestAt("@FO", opts)
	if len(got) != 1 || got[0].Value != "@folder" {
		t.Fatalf("@FO offered %v, want just @folder", values(got))
	}
	// Every kind carries a hint, so the popup can say what it will inject.
	for _, c := range suggestAt("@", opts) {
		if c.Hint == "" {
			t.Fatalf("%s has no hint", c.Value)
		}
		if c.Kind == "" {
			t.Fatalf("%s has no kind", c.Value)
		}
	}
}

func TestSuggestOffersNothingForAnOrdinaryTask(t *testing.T) {
	opts := Options{Workspace: suggestWorkspace(t)}
	for _, line := range []string{
		"refactor the auth layer",
		"",
		"   ",
		"look at main.go",        // a bare path is not a reference
		"@reviewer do the thing", // an agent mention is not a reference
		"a@file.com is an address",
	} {
		if got := suggestAt(line, opts); len(got) != 0 {
			t.Fatalf("%q offered %v, want nothing", line, values(got))
		}
	}
}

// ---------- path filtering ----------

func TestSuggestFiltersPathsByPrefix(t *testing.T) {
	ws := suggestWorkspace(t)
	opts := Options{Workspace: ws}

	got := suggestAt("@file mai", opts)
	if len(got) != 1 || got[0].Value != "main.go" {
		t.Fatalf("@file mai offered %v, want main.go", values(got))
	}
	if got[0].Kind != KindFile || got[0].Hint == "" {
		t.Fatalf("candidate = %+v", got[0])
	}

	// The match is case-insensitive on the path and applies to each entry, so
	// "ma" finds both spellings of the same beginning.
	both := suggestAt("@file ma", opts)
	for _, want := range []string{"main.go", "Makefile"} {
		if !hasValue(both, want) {
			t.Fatalf("@file ma is missing %q: %v", want, values(both))
		}
	}

	// The prefix is matched case-insensitively on the path, so the operator
	// does not have to remember how the file was spelled.
	got = suggestAt("@file MAIN", opts)
	if !hasValue(got, "main.go") {
		t.Fatalf("@file MAIN offered %v, want main.go", values(got))
	}

	// A prefix that names a directory matches what is inside it.
	got = suggestAt("@file internal/tui/", opts)
	for _, want := range []string{"internal/tui/view.go", "internal/tui/complete.go", "internal/tui/Main_test.go"} {
		if !hasValue(got, want) {
			t.Fatalf("@file internal/tui/ is missing %q: %v", want, values(got))
		}
	}

	// A prefix nothing matches offers nothing at all — an empty popup would be
	// a lie about there being a completion.
	if got := suggestAt("@file zzz", opts); len(got) != 0 {
		t.Fatalf("@file zzz offered %v", values(got))
	}
}

func TestSuggestMarksDirectoriesWithATrailingSlash(t *testing.T) {
	opts := Options{Workspace: suggestWorkspace(t)}

	got := suggestAt("@file ", opts)
	if !hasValue(got, "internal/") {
		t.Fatalf("the directory is not offered with a trailing slash: %v", values(got))
	}
	for _, c := range got {
		if strings.HasSuffix(c.Value, "/") && c.Kind != KindFolder {
			t.Fatalf("%q is not marked as a folder: %+v", c.Value, c)
		}
		if !strings.HasSuffix(c.Value, "/") && c.Kind == KindFolder {
			t.Fatalf("%q is marked as a folder without the slash: %+v", c.Value, c)
		}
	}
	// @folder is a directory listing, so it offers directories only: the
	// expansion refuses a file there ("is not a directory").
	folders := suggestAt("@folder ", opts)
	if len(folders) == 0 {
		t.Fatal("@folder offered nothing")
	}
	for _, c := range folders {
		if !strings.HasSuffix(c.Value, "/") {
			t.Fatalf("@folder offered the file %q", c.Value)
		}
	}
}

// ---------- the policy gate ----------

func TestSuggestNeverOffersAPathTheExpansionWouldRefuse(t *testing.T) {
	ws := suggestWorkspace(t)
	opts := Options{
		Workspace: ws,
		// The built-in floor plus one explicit rule: secrets.env is denied.
		Policy: tools.Policy{DenyPaths: append([]string{"**/*.env"}, tools.DefaultDenyPaths...)},
	}

	// The denied path exists on disk and matches the prefix, so only the gate
	// can keep it out of the popup.
	got := suggestAt("@file secret", opts)
	if hasValue(got, "secrets.env") {
		t.Fatalf("a path the policy denies was offered: %v", values(got))
	}
	// Bare listing: the credential files are excluded there too.
	all := suggestAt("@file ", opts)
	for _, denied := range []string{"secrets.env", "id_rsa"} {
		if hasValue(all, denied) {
			t.Fatalf("%q is denied by the policy but was offered: %v", denied, values(all))
		}
	}

	// The popup and the expansion must agree, one by one: every candidate the
	// popup offers is one ExpandReferences reads rather than refuses.
	for _, c := range all {
		arg := strings.TrimSuffix(c.Value, "/")
		ref := buildReference(KindFile, arg, opts)
		if ref.Failed && strings.Contains(ref.Note, "denied") {
			t.Fatalf("the popup offered %q but the expansion refuses it: %s", c.Value, ref.Note)
		}
	}
}

func TestSuggestAppliesAnAllowListPolicy(t *testing.T) {
	ws := suggestWorkspace(t)
	// An allow list turns everything else into a deny (Policy.PathDenied).
	opts := Options{Workspace: ws, Policy: tools.Policy{AllowPaths: []string{"internal/**"}}}

	got := suggestAt("@file ", opts)
	if len(got) == 0 {
		t.Fatal("the allow list pruned everything, including the allowed tree")
	}
	for _, c := range got {
		if !strings.HasPrefix(c.Value, "internal/") {
			t.Fatalf("a path outside the allow list was offered: %q", c.Value)
		}
	}
}

func TestSuggestHonoursARuleThatDeniesReads(t *testing.T) {
	ws := suggestWorkspace(t)
	opts := Options{
		Workspace: ws,
		Policy: tools.Policy{
			Rules: []config.PermRule{{Tool: "read", Spec: "**/*.md", Action: tools.ActionDeny}},
		},
	}
	got := suggestAt("@file REA", opts)
	if hasValue(got, "README.md") {
		t.Fatalf("a rule that denies read did not keep README.md out: %v", values(got))
	}
	// A file the rule does not name is still offered: the gate is specific.
	if !hasValue(suggestAt("@file mai", opts), "main.go") {
		t.Fatal("the rule leaked onto a path it does not name")
	}
}

// ---------- punctuation and the token boundary ----------

func TestSuggestKeepsTrailingPunctuationInTheTerm(t *testing.T) {
	opts := Options{Workspace: suggestWorkspace(t)}

	// takeArg reads a path up to whitespace, so "main.go," is the path
	// "main.go," — nothing matches it, and nothing may be offered for it.
	line := "@file main.go,"
	if got := suggestAt(line, opts); len(got) != 0 {
		t.Fatalf("%q offered %v, but the comma is part of the path", line, values(got))
	}
	// The same term without the comma does match, which proves the comma is
	// what stopped it rather than the path being wrong.
	if got := suggestAt("@file main.go", opts); !hasValue(got, "main.go") {
		t.Fatalf("@file main.go offered %v", values(got))
	}
	// A quoted argument is a different token shape: no candidate is offered
	// for an argument the popup cannot replace by word boundary.
	if got := suggestAt(`@file "main`, opts); len(got) != 0 {
		t.Fatalf("a quoted argument offered %v", values(got))
	}
}

func TestSuggestFollowsTheCaretNotTheLineEnd(t *testing.T) {
	opts := Options{Workspace: suggestWorkspace(t)}

	// The caret sits inside the token "main.go": the suggestion is for the part
	// already typed, and the token span covers the whole word so accepting
	// replaces all of it.
	line := "@file main.go and then some"
	cursor := len("@file main")
	got := Suggest(line, cursor, opts)
	if !hasValue(got, "main.go") {
		t.Fatalf("a mid-line caret offered %v, want main.go", values(got))
	}
	start, end := TokenSpan(line, cursor)
	if line[start:end] != "main.go" {
		t.Fatalf("TokenSpan = %q, want the whole token main.go", line[start:end])
	}

	// With the caret on the task text after the reference, there is nothing to
	// complete: the word there is prose, not a reference argument.
	if got := Suggest(line, len(line), opts); len(got) != 0 {
		t.Fatalf("a caret in the trailing prose offered %v", values(got))
	}

	// A caret past the end of the line is clamped, not a panic.
	if got := Suggest("@fi", 99, opts); len(got) != 1 || got[0].Value != "@file" {
		t.Fatalf("a caret past the end offered %v", values(got))
	}
	if got := Suggest("@f", -5, opts); got != nil {
		t.Fatalf("a negative caret offered %v", values(got))
	}
}

func TestSuggestHandlesAMultiLineDraft(t *testing.T) {
	opts := Options{Workspace: suggestWorkspace(t)}

	// A reference on the second line completes like one on the first: the
	// expander scans every line.
	line := "first line\n@file mai"
	got := Suggest(line, len(line), opts)
	if !hasValue(got, "main.go") {
		t.Fatalf("a reference on a later line offered %v", values(got))
	}
	// A slash command is only a command as the whole draft's first token, so a
	// slash after a newline offers nothing: the submit path would not read it.
	slash := "first line\n/rev"
	if got := Suggest(slash, len(slash), opts); len(got) != 0 {
		t.Fatalf("a slash on a later line offered %v", values(got))
	}
}

// ---------- diff and url ----------

func TestSuggestIsHonestAboutDiffAndURL(t *testing.T) {
	opts := Options{Workspace: suggestWorkspace(t)}

	// @url offers nothing: a host nobody typed is an egress request nobody
	// made.
	if got := suggestAt("@url https://", opts); len(got) != 0 {
		t.Fatalf("@url offered %v", values(got))
	}
	// @diff offers only the refs that always exist, and only matching ones.
	got := suggestAt("@diff HEAD", opts)
	if len(got) == 0 {
		t.Fatal("@diff HEAD offered nothing")
	}
	for _, c := range got {
		if !strings.HasPrefix(c.Value, "HEAD") || c.Kind != KindDiff {
			t.Fatalf("a diff candidate = %+v", c)
		}
	}
	// A prefix no ref matches offers nothing rather than a guess.
	if got := suggestAt("@diff fix", opts); len(got) != 0 {
		t.Fatalf("@diff fix offered %v, and a branch name would be a guess", values(got))
	}
}

// ---------- slash commands ----------

func TestSuggestOffersCommandsWithTheirArgumentHint(t *testing.T) {
	ws := suggestWorkspace(t)
	opts := Options{
		Workspace: ws,
		Commands:  []Command{{Name: "review", ArgumentHint: "<path> [focus]", Description: "review a diff"}},
	}

	got := suggestAt("/rev", opts)
	if len(got) != 1 || got[0].Value != "/review" {
		t.Fatalf("/rev offered %v, want /review", values(got))
	}
	if got[0].Hint != "<path> [focus]" {
		t.Fatalf("hint = %q, want the ArgumentHint", got[0].Hint)
	}
	if got[0].Kind != KindCommand {
		t.Fatalf("kind = %q", got[0].Kind)
	}

	// A bare slash lists the built-ins and the custom commands together.
	all := suggestAt("/", opts)
	if !hasValue(all, "/help") || !hasValue(all, "/review") {
		t.Fatalf("/ offered %v, want both built-ins and custom commands", values(all))
	}
	// A built-in shadows a custom command of the same name, because
	// handleSlash resolves the built-ins first.
	opts.Commands = append(opts.Commands, Command{Name: "help", ArgumentHint: "shadowed"})
	for _, c := range suggestAt("/hel", opts) {
		if c.Value == "/help" && c.Hint == "shadowed" {
			t.Fatalf("the custom command shadowed the built-in: %+v", c)
		}
	}

	// A slash that is not the first token of the draft is not a command.
	if got := suggestAt("review /rev", opts); len(got) != 0 {
		t.Fatalf("a slash inside a task offered %v", values(got))
	}
}

func TestBuiltinsMatchTheHelpInventory(t *testing.T) {
	list := Builtins()
	if len(list) == 0 {
		t.Fatal("Builtins is empty")
	}
	for i := 1; i < len(list); i++ {
		if list[i-1].Name >= list[i].Name {
			t.Fatalf("Builtins is not sorted: %q before %q", list[i-1].Name, list[i].Name)
		}
	}
	for _, c := range list {
		if strings.HasPrefix(c.Name, "/") || strings.TrimSpace(c.Name) == "" {
			t.Fatalf("built-in name %q is not a bare name", c.Name)
		}
	}
}

// ---------- never panic ----------

func TestSuggestNeverPanicsOnOddInput(t *testing.T) {
	opts := Options{Workspace: suggestWorkspace(t)}
	lines := []string{
		"", "@", "@file", "@file:", "@:", "@@@", "/", "//", "@folder:", "@diff:",
		"\n", "\x00", "@file \t\t", strings.Repeat("@file ", 50), "@file internal/",
	}
	for _, line := range lines {
		for _, cursor := range []int{-1, 0, 1, len(line) / 2, len(line), len(line) + 10} {
			_ = Suggest(line, cursor, opts)
		}
	}
	// No workspace bound at all: the generator answers nothing, and does not
	// walk the process's own directory either.
	if got := suggestAt("@file main", Options{}); len(got) != 0 {
		t.Fatalf("no workspace offered %v", values(got))
	}
}

// ---------- colon form ----------

func TestSuggestUsesTheColonFormWhenTheOperatorDid(t *testing.T) {
	opts := Options{Workspace: suggestWorkspace(t)}
	got := suggestAt("@file:mai", opts)
	if len(got) != 1 || got[0].Value != "@file:main.go" {
		t.Fatalf("@file:mai offered %v, want the colon form preserved", values(got))
	}
}
