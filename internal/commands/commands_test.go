package commands

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"kintsugi/internal/tools"
)

// ---------- helpers ----------

// writeCommand drops a command file into a tier directory.
func writeCommand(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// tiers returns a workspace and a user tier directory, both empty.
func tiers(t *testing.T) (ws, user string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ws = t.TempDir()
	user = GlobalDir()
	return ws, user
}

func opts(ws string, list []Command) Options {
	return Options{Workspace: ws, Commands: list}
}

// ---------- discovery ----------

func TestLoadDiscoversBothTiers(t *testing.T) {
	ws, user := tiers(t)
	writeCommand(t, WorkspaceDir(ws), "review.md",
		"---\ndescription: review a diff\nargument-hint: <path>\n---\nReview $1.\n")
	writeCommand(t, user, "ship.md",
		"---\ndescription: ship it\n---\nShip $ARGUMENTS.\n")

	list := Load(Dirs(ws)...)
	if len(list) != 2 {
		t.Fatalf("loaded %d commands, want 2: %+v", len(list), list)
	}
	if list[0].Name != "review" || list[1].Name != "ship" {
		t.Fatalf("commands are not sorted by name: %+v", list)
	}
	if list[0].Tier != TierWorkspace {
		t.Fatalf("review tier = %q, want %q", list[0].Tier, TierWorkspace)
	}
	if list[1].Tier != TierUser {
		t.Fatalf("ship tier = %q, want %q", list[1].Tier, TierUser)
	}
	if list[0].Description != "review a diff" {
		t.Fatalf("description = %q", list[0].Description)
	}
	if list[0].ArgumentHint != "<path>" {
		t.Fatalf("argument hint = %q", list[0].ArgumentHint)
	}
}

func TestWorkspaceTierWinsOnANameClash(t *testing.T) {
	ws, user := tiers(t)
	writeCommand(t, WorkspaceDir(ws), "review.md", "---\ndescription: workspace one\n---\nWorkspace body.\n")
	writeCommand(t, user, "review.md", "---\ndescription: user one\n---\nUser body.\n")

	list := Load(Dirs(ws)...)
	if len(list) != 1 {
		t.Fatalf("loaded %d commands, want 1: %+v", len(list), list)
	}
	if list[0].Description != "workspace one" {
		t.Fatalf("the user tier won the clash: %q", list[0].Description)
	}
	if !strings.Contains(list[0].Body, "Workspace body") {
		t.Fatalf("body = %q", list[0].Body)
	}
}

func TestMalformedCommandFileIsSkipped(t *testing.T) {
	ws, _ := tiers(t)
	dir := WorkspaceDir(ws)
	// Broken frontmatter: the closing delimiter is missing.
	writeCommand(t, dir, "broken.md", "---\ndescription: no closing delimiter\nbody\n")
	// Frontmatter that is not YAML at all.
	writeCommand(t, dir, "notyaml.md", "---\n: : :\n\tbroken\n---\nbody\n")
	// A good one, so the tier is not empty.
	writeCommand(t, dir, "good.md", "---\ndescription: fine\n---\nGood body.\n")

	list := Load(Dirs(ws)...)
	if len(list) != 1 {
		t.Fatalf("loaded %d commands, want 1 (the malformed ones are skipped): %+v", len(list), list)
	}
	if list[0].Name != "good" {
		t.Fatalf("kept %q, want good", list[0].Name)
	}
}

func TestFindResolvesByName(t *testing.T) {
	list := []Command{{Name: "review"}, {Name: "ship"}}
	if c, ok := Find(list, "/review"); !ok || c.Name != "review" {
		t.Fatalf("Find(/review) = %v %v", c, ok)
	}
	if c, ok := Find(list, "SHIP"); !ok || c.Name != "ship" {
		t.Fatalf("Find(SHIP) = %v %v", c, ok)
	}
	if _, ok := Find(list, "nope"); ok {
		t.Fatal("Find found a command that does not exist")
	}
}

func TestIndexListsCommandsWithHintAndDescription(t *testing.T) {
	list := []Command{
		{Name: "review", Description: "review a diff", ArgumentHint: "<path> [focus]"},
		{Name: "ship", Description: "ship the branch"},
	}
	out := Index(list)
	for _, want := range []string{"/review <path> [focus]", "review a diff", "/ship", "ship the branch"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the listing does not mention %q:\n%s", want, out)
		}
	}
}

// ---------- expansion ----------

func TestSubstituteArguments(t *testing.T) {
	body := "Review $1 with a focus on $2.\nAll of it: $ARGUMENTS"
	out := substitute(body, "src/main.go auth", identity)
	if !strings.Contains(out, "Review src/main.go with a focus on auth.") {
		t.Fatalf("$1/$2 were not substituted:\n%s", out)
	}
	if !strings.Contains(out, "All of it: src/main.go auth") {
		t.Fatalf("$ARGUMENTS was not substituted:\n%s", out)
	}
}

func TestSubstituteLeavesUnknownPlaceholdersAlone(t *testing.T) {
	out := substitute("cost is $5 and $NAME", "", identity)
	if !strings.Contains(out, "$NAME") {
		t.Fatalf("an unknown placeholder was rewritten:\n%s", out)
	}
	if strings.Contains(out, "$5 ") {
		t.Fatalf("a missing argument was not emptied:\n%s", out)
	}
}

func TestExpandCommandSubstitutesTheBody(t *testing.T) {
	ws, _ := tiers(t)
	writeCommand(t, WorkspaceDir(ws), "review.md",
		"---\ndescription: review a diff\nargument-hint: <path>\n---\nReview $1.\nAll of it: $ARGUMENTS\n")

	list := Load(Dirs(ws)...)
	exp, ok := ExpandCommand("/review src/main.go", opts(ws, list))
	if !ok {
		t.Fatal("the custom command did not resolve")
	}
	if exp.Command != "review" {
		t.Fatalf("command = %q", exp.Command)
	}
	if !strings.Contains(exp.Prompt, "Review src/main.go.") {
		t.Fatalf("$1 was not substituted:\n%s", exp.Prompt)
	}
	if !strings.Contains(exp.Prompt, "All of it: src/main.go") {
		t.Fatalf("$ARGUMENTS was not substituted:\n%s", exp.Prompt)
	}
}

func TestBangCommandRunsAndIsSubstituted(t *testing.T) {
	ws, _ := tiers(t)
	writeCommand(t, WorkspaceDir(ws), "status.md",
		"---\ndescription: status\n---\nThe branch is:\n!`echo alpha`\n")

	list := Load(Dirs(ws)...)
	o := opts(ws, list)
	o.Policy = tools.Policy{AllowCommands: []string{"echo"}}

	exp, ok := ExpandCommand("/status", o)
	if !ok {
		t.Fatal("the custom command did not resolve")
	}
	if !strings.Contains(exp.Prompt, "The branch is:\nalpha") {
		t.Fatalf("the `!` line was not run and substituted:\n%s", exp.Prompt)
	}
	if len(exp.Notes) != 0 {
		t.Fatalf("notes = %v, want none", exp.Notes)
	}
}

func TestBangCommandRunsInTheWorkspace(t *testing.T) {
	ws, _ := tiers(t)
	if err := os.WriteFile(filepath.Join(ws, "marker.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCommand(t, WorkspaceDir(ws), "where.md",
		"---\ndescription: where\n---\n!`ls`\n")

	list := Load(Dirs(ws)...)
	o := opts(ws, list)
	o.Policy = tools.Policy{AllowCommands: []string{"ls"}}

	exp, _ := ExpandCommand("/where", o)
	if !strings.Contains(exp.Prompt, "marker.txt") {
		t.Fatalf("the command did not run in the workspace root:\n%s", exp.Prompt)
	}
}

func TestBangCommandNeedsApprovalIsRefused(t *testing.T) {
	ws, _ := tiers(t)
	writeCommand(t, WorkspaceDir(ws), "status.md",
		"---\ndescription: status\n---\n!`touch marker.txt`\n")

	list := Load(Dirs(ws)...)
	o := opts(ws, list)
	// The default mode asks before a shell command and no approver is wired:
	// the line must not run.
	o.Policy = tools.Policy{}

	exp, _ := ExpandCommand("/status", o)
	if _, err := os.Stat(filepath.Join(ws, "marker.txt")); err == nil {
		t.Fatal("a command that was not approved ran")
	}
	if len(exp.Notes) == 0 || !strings.Contains(exp.Notes[0], "approval") {
		t.Fatalf("the refusal was not reported: %v", exp.Notes)
	}
}

func TestBangCommandWithoutAPolicyIsRefused(t *testing.T) {
	ws, _ := tiers(t)
	writeCommand(t, WorkspaceDir(ws), "status.md",
		"---\ndescription: status\n---\n!`touch marker.txt`\n")

	list := Load(Dirs(ws)...)
	// No permission matrix at all: the gate fails closed.
	exp, _ := ExpandCommand("/status", opts(ws, list))
	if _, err := os.Stat(filepath.Join(ws, "marker.txt")); err == nil {
		t.Fatal("a command ran without a permission policy")
	}
	if len(exp.Notes) == 0 {
		t.Fatalf("the refusal was not reported: %v", exp.Notes)
	}
}

func TestBangCommandIsRefusedByPolicy(t *testing.T) {
	ws, _ := tiers(t)
	writeCommand(t, WorkspaceDir(ws), "bad.md",
		"---\ndescription: bad\n---\n!`rm -rf /`\n")

	list := Load(Dirs(ws)...)
	o := opts(ws, list)
	o.Policy = tools.Policy{DenyCommands: []string{`rm -rf`}}

	exp, _ := ExpandCommand("/bad", o)
	if len(exp.Notes) == 0 || !strings.Contains(exp.Notes[0], "denied") {
		t.Fatalf("the denial was not reported: %v", exp.Notes)
	}
}

func TestBangCommandArgumentIsShellQuoted(t *testing.T) {
	ws, _ := tiers(t)
	writeCommand(t, WorkspaceDir(ws), "echo.md",
		"---\ndescription: echo\n---\n!`echo $1`\n")

	list := Load(Dirs(ws)...)
	o := opts(ws, list)
	o.Policy = tools.Policy{AllowCommands: []string{"echo"}}

	exp, _ := ExpandCommand("/echo '; touch pwned'", o)
	if strings.Contains(exp.Prompt, "pwned\n") && !strings.Contains(exp.Prompt, "; touch pwned") {
		t.Fatalf("the argument was not quoted:\n%s", exp.Prompt)
	}
	if _, err := os.Stat(filepath.Join(ws, "pwned")); err == nil {
		t.Fatal("the argument broke out of the command")
	}
}

// ---------- context references ----------

func TestFileReferenceInjectsContentsAndByteCount(t *testing.T) {
	ws, _ := tiers(t)
	body := "package main\n\nfunc main() {}\n"
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	exp := ExpandReferences("look at @file main.go please", opts(ws, nil))
	if !strings.Contains(exp.Prompt, "func main() {}") {
		t.Fatalf("the file contents were not injected:\n%s", exp.Prompt)
	}
	if !strings.Contains(exp.Prompt, "look at ") || !strings.Contains(exp.Prompt, " please") {
		t.Fatalf("the surrounding text was lost:\n%s", exp.Prompt)
	}
	if len(exp.Refs) != 1 {
		t.Fatalf("refs = %d, want 1", len(exp.Refs))
	}
	ref := exp.Refs[0]
	if ref.Kind != KindFile || ref.Path != "main.go" {
		t.Fatalf("ref = %+v", ref)
	}
	if ref.Bytes != len(body) {
		t.Fatalf("byte count = %d, want %d", ref.Bytes, len(body))
	}
	if !strings.Contains(ref.Summary(), "main.go") {
		t.Fatalf("summary = %q", ref.Summary())
	}
	if !strings.Contains(exp.Summary(), "bytes") {
		t.Fatalf("the summary does not report the byte count: %q", exp.Summary())
	}
}

func TestFolderReferenceInjectsAListingNotContents(t *testing.T) {
	ws, _ := tiers(t)
	dir := filepath.Join(ws, "internal")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	exp := ExpandReferences("@folder internal", opts(ws, nil))
	if !strings.Contains(exp.Prompt, "a.go") {
		t.Fatalf("the listing does not name the file:\n%s", exp.Prompt)
	}
	if !strings.Contains(exp.Prompt, "dir") {
		t.Fatalf("the listing does not name the kind:\n%s", exp.Prompt)
	}
	if strings.Contains(exp.Prompt, "package a") {
		t.Fatalf("the folder contents were injected:\n%s", exp.Prompt)
	}
	ref := exp.Refs[0]
	if ref.Kind != KindFolder || ref.Count != 2 {
		t.Fatalf("ref = %+v", ref)
	}
	if !strings.Contains(ref.Summary(), "entries") {
		t.Fatalf("summary = %q", ref.Summary())
	}
}

func TestUnreadablePathStaysLiteral(t *testing.T) {
	ws, _ := tiers(t)
	line := "look at @file missing.txt please"

	exp := ExpandReferences(line, opts(ws, nil))
	if !strings.Contains(exp.Prompt, "@file missing.txt") {
		t.Fatalf("an unreadable path did not stay literal:\n%s", exp.Prompt)
	}
	if len(exp.Refs) != 1 || !exp.Refs[0].Failed {
		t.Fatalf("the failure was not reported: %+v", exp.Refs)
	}
	if !strings.Contains(exp.Summary(), "left literal") {
		t.Fatalf("the summary does not say the path stayed literal: %q", exp.Summary())
	}
}

func TestPathOutsideTheWorkspaceStaysLiteral(t *testing.T) {
	ws, _ := tiers(t)
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("TOP-SECRET-CONTENT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	exp := ExpandReferences("@file "+outside, opts(ws, nil))
	if strings.Contains(exp.Prompt, "TOP-SECRET-CONTENT") {
		t.Fatalf("a path outside the workspace was injected:\n%s", exp.Prompt)
	}
	if !strings.Contains(exp.Prompt, outside) {
		t.Fatalf("the path did not stay literal:\n%s", exp.Prompt)
	}
}

func TestOversizeFileIsTruncatedWithAMarker(t *testing.T) {
	ws, _ := tiers(t)
	big := strings.Repeat("a", ReadCap+4096)
	if err := os.WriteFile(filepath.Join(ws, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}

	exp := ExpandReferences("@file big.txt", opts(ws, nil))
	tail := exp.Prompt
	if len(tail) > 200 {
		tail = tail[len(tail)-200:]
	}
	if !strings.Contains(exp.Prompt, "truncated at") {
		t.Fatalf("the truncation marker is missing:\n%s", tail)
	}
	ref := exp.Refs[0]
	if !ref.Truncated {
		t.Fatalf("the reference is not marked truncated: %+v", ref)
	}
	if ref.Bytes > ReadCap+64 {
		t.Fatalf("injected %d bytes, want about %d", ref.Bytes, ReadCap)
	}
	if ref.Total != len(big) {
		t.Fatalf("total = %d, want %d", ref.Total, len(big))
	}
}

func TestDiffOutsideARepoDegradesGracefully(t *testing.T) {
	ws, _ := tiers(t)
	exp := ExpandReferences("@diff", opts(ws, nil))
	if len(exp.Refs) != 1 {
		t.Fatalf("refs = %d, want 1", len(exp.Refs))
	}
	ref := exp.Refs[0]
	if !ref.Failed {
		t.Fatalf("the diff did not degrade: %+v", ref)
	}
	if ref.Note == "" {
		t.Fatal("the degradation says nothing")
	}
	if !strings.Contains(exp.Prompt, "git") {
		t.Fatalf("the prompt does not say plainly what happened:\n%s", exp.Prompt)
	}
}

func TestDiffInjectsTheWorkingTreeDiff(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ws, _ := tiers(t)
	run := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = ws
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	exp := ExpandReferences("@diff", opts(ws, nil))
	if !strings.Contains(exp.Prompt, "a.txt") {
		t.Fatalf("the diff was not injected:\n%s", exp.Prompt)
	}
	if !strings.Contains(exp.Prompt, "-two") && !strings.Contains(exp.Prompt, "+two") {
		t.Fatalf("the diff does not show the change:\n%s", exp.Prompt)
	}
	ref := exp.Refs[0]
	if ref.Failed {
		t.Fatalf("the diff failed: %+v", ref)
	}
	if ref.Bytes == 0 {
		t.Fatalf("the diff reported no bytes: %+v", ref)
	}
}

func TestDiffRefIsOnlyTakenWhenItIsNotProse(t *testing.T) {
	ws, _ := tiers(t)
	// "fix" is the next word of a sentence, not a ref.
	exp := ExpandReferences("@diff fix the failing test", opts(ws, nil))
	if !strings.Contains(exp.Prompt, "fix the failing test") {
		t.Fatalf("the prose was eaten:\n%s", exp.Prompt)
	}
	// A ref-looking token is a ref.
	exp = ExpandReferences("@diff HEAD~1", opts(ws, nil))
	if len(exp.Refs) != 1 || exp.Refs[0].Arg != "HEAD~1" {
		t.Fatalf("the ref was not taken: %+v", exp.Refs)
	}
}

func TestReferencesAreNotExpandedTwice(t *testing.T) {
	ws, _ := tiers(t)
	if err := os.WriteFile(filepath.Join(ws, "note.txt"), []byte("see @file other.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	exp := ExpandReferences("@file note.txt", opts(ws, nil))
	if strings.Count(exp.Prompt, "other.txt") != 1 {
		t.Fatalf("the injected text was expanded again:\n%s", exp.Prompt)
	}
}

func TestExpandCombinesACommandAndAReference(t *testing.T) {
	ws, _ := tiers(t)
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCommand(t, WorkspaceDir(ws), "check.md",
		"---\ndescription: check\n---\nCheck @file a.txt for $ARGUMENTS\n")

	list := Load(Dirs(ws)...)
	exp := Expand("/check the parser", opts(ws, list))
	if exp.Command != "check" {
		t.Fatalf("command = %q", exp.Command)
	}
	if !strings.Contains(exp.Prompt, "alpha") {
		t.Fatalf("the reference inside the body was not expanded:\n%s", exp.Prompt)
	}
	if !strings.Contains(exp.Prompt, "the parser") {
		t.Fatalf("the argument was not substituted:\n%s", exp.Prompt)
	}
}

func TestIsReference(t *testing.T) {
	if !IsReference("@file main.go") {
		t.Fatal("@file is not a reference")
	}
	if !IsReference("@folder internal") {
		t.Fatal("@folder is not a reference")
	}
	if !IsReference("@diff") {
		t.Fatal("@diff is not a reference")
	}
	if IsReference("@reviewer look at this") {
		t.Fatal("an agent mention is not a reference")
	}
	if IsReference("mail me at a@file.com") {
		t.Fatal("an address is not a reference")
	}
	if IsReference("@file") {
		t.Fatal("a reference without a path is not a usable reference")
	}
	// The token check still recognises it: a reference keyword is never an
	// agent, so the line explains itself instead of naming an unknown agent.
	if !IsReferenceToken("@file") {
		t.Fatal("@file is not a reference token")
	}
	if IsReferenceToken("@reviewer look at this") {
		t.Fatal("an agent mention is not a reference token")
	}
}

func TestReferenceWithoutAPathExplainsItself(t *testing.T) {
	ws, _ := tiers(t)
	exp := ExpandReferences("@file", opts(ws, nil))
	if !strings.Contains(exp.Prompt, "@file") {
		t.Fatalf("the token did not stay literal:\n%s", exp.Prompt)
	}
	if len(exp.Refs) != 1 || !exp.Refs[0].Failed {
		t.Fatalf("the failure was not reported: %+v", exp.Refs)
	}
	if !strings.Contains(exp.Summary(), "no path given") {
		t.Fatalf("the summary does not say what is wrong: %q", exp.Summary())
	}
}

// ---------- @url references ----------

// fakeFetcher answers a URL reference without a network. It records what was
// asked for so a test can prove the reference went through the bounded fetch.
type fakeFetcher struct {
	pages    map[string]tools.Page
	errs     map[string]error
	requests []string
	maxBytes []int
}

func (f *fakeFetcher) FetchPage(_ context.Context, raw string, maxBytes int) (tools.Page, error) {
	f.requests = append(f.requests, raw)
	f.maxBytes = append(f.maxBytes, maxBytes)
	if err, ok := f.errs[raw]; ok {
		return tools.Page{}, err
	}
	if p, ok := f.pages[raw]; ok {
		return p, nil
	}
	return tools.Page{}, fmt.Errorf("GET %s: status 404", raw)
}

// fakeFetcherFor builds a fake fetcher that answers one URL with one reduction.
func fakeFetcherFor(target string, page tools.Page) *fakeFetcher {
	return &fakeFetcher{pages: map[string]tools.Page{target: page}}
}

func TestURLReferenceInjectsTheReduction(t *testing.T) {
	ws, _ := tiers(t)
	target := "https://docs.example.com/sandbox"
	fake := fakeFetcherFor(target, tools.Page{
		URL: target, Host: "docs.example.com", Site: "docs", Extracted: true,
		Status: 200, Markdown: "# Sandbox modes\n\nA sandbox binds what a command can touch.",
		Citations: []string{"https://docs.example.com/sandbox", "https://example.com/spec"},
	})

	line := "read this first @url " + target + " then fix it"
	exp := ExpandReferences(line, Options{Workspace: ws, Fetcher: fake})

	if !strings.Contains(exp.Prompt, "--- url: "+target) {
		t.Fatalf("the source header is missing:\n%s", exp.Prompt)
	}
	if !strings.Contains(exp.Prompt, "# Sandbox modes") {
		t.Fatalf("the reduction was not injected:\n%s", exp.Prompt)
	}
	if !strings.Contains(exp.Prompt, "read this first ") || !strings.Contains(exp.Prompt, " then fix it") {
		t.Fatalf("the surrounding text was lost:\n%s", exp.Prompt)
	}
	if len(exp.Refs) != 1 {
		t.Fatalf("refs = %d, want 1", len(exp.Refs))
	}
	ref := exp.Refs[0]
	if ref.Kind != KindURL || ref.URL != target {
		t.Fatalf("ref = %+v", ref)
	}
	if ref.Failed || ref.Literal {
		t.Fatalf("the reference reported a failure: %+v", ref)
	}
	if ref.Citations != 2 {
		t.Fatalf("citations = %d, want 2", ref.Citations)
	}
	if ref.Bytes != len(ref.Text) || ref.Bytes == 0 {
		t.Fatalf("bytes = %d, text = %d", ref.Bytes, len(ref.Text))
	}
	// The note reads like the file and folder notes do.
	if !strings.Contains(ref.Summary(), "url "+target) || !strings.Contains(ref.Summary(), "docs") ||
		!strings.Contains(ref.Summary(), "2 citations") {
		t.Fatalf("summary = %q", ref.Summary())
	}
	if !strings.Contains(exp.Summary(), "injected 1 context reference(s)") {
		t.Fatalf("the transcript summary does not count the reference: %q", exp.Summary())
	}
	// The reference went through the bounded fetch, and only the bounded
	// fetch: no second path to the network.
	if len(fake.requests) != 1 || fake.requests[0] != target {
		t.Fatalf("requests = %v, want %q", fake.requests, target)
	}
	if fake.maxBytes[0] != URLFetchBytes {
		t.Fatalf("max_bytes = %d, want %d", fake.maxBytes[0], URLFetchBytes)
	}
}

func TestURLReferenceAcceptsTheAngleBracketForm(t *testing.T) {
	ws, _ := tiers(t)
	target := "https://arxiv.org/abs/2401.00042"
	fake := fakeFetcherFor(target, tools.Page{URL: target, Site: "arxiv", Extracted: true, Markdown: "# A paper"})

	exp := ExpandReferences("@url <"+target+">", Options{Workspace: ws, Fetcher: fake})
	if len(exp.Refs) != 1 || exp.Refs[0].Failed {
		t.Fatalf("the angle-bracket form was not read: %+v", exp.Refs)
	}
	if exp.Refs[0].URL != target {
		t.Fatalf("url = %q, want %q", exp.Refs[0].URL, target)
	}
	if strings.Contains(exp.Prompt, "<https://") {
		t.Fatalf("the brackets survived into the prompt:\n%s", exp.Prompt)
	}
	if !strings.Contains(exp.Prompt, "# A paper") {
		t.Fatalf("the reduction was not injected:\n%s", exp.Prompt)
	}
}

func TestURLReferenceTrimsSentencePunctuation(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{"a full stop ends the sentence", "see @url https://a.example/x. then stop", "https://a.example/x"},
		{"a comma", "see @url https://a.example/x, please", "https://a.example/x"},
		{"a trailing colon stays off", "@url https://a.example/x:", "https://a.example/x"},
		{"a path is kept whole", "@url https://a.example/x/y", "https://a.example/x/y"},
		{"a query is kept whole", "@url https://a.example/x?y=1", "https://a.example/x?y=1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// The reduction answers so the reference succeeds: this test is
			// about the token the fetch is asked for, not about failure paths
			// (those have their own test below).
			fake := fakeFetcherFor(tc.want, tools.Page{
				URL: tc.want, Site: "generic", Markdown: "# trimmed",
			})
			exp := ExpandReferences(tc.line, Options{Workspace: t.TempDir(), Fetcher: fake})
			if len(fake.requests) != 1 {
				t.Fatalf("requests = %v, want exactly %q", fake.requests, tc.want)
			}
			if fake.requests[0] != tc.want {
				t.Fatalf("fetched %q, want %q", fake.requests[0], tc.want)
			}
			if len(exp.Refs) != 1 || exp.Refs[0].Failed {
				t.Fatalf("refs = %+v", exp.Refs)
			}
			if !strings.Contains(exp.Prompt, "# trimmed") {
				t.Fatalf("the reduction was not injected:\n%s", exp.Prompt)
			}
			// The operator's token is gone, punctuation and all; what remains
			// of it is only the provenance header the reference writes.
			if strings.Contains(exp.Prompt, tc.line) {
				t.Fatalf("the reference token was not replaced:\n%s", exp.Prompt)
			}
		})
	}
}

func TestUnreadableURLStaysLiteralAndNeverFailsThePrompt(t *testing.T) {
	tests := []struct {
		name string
		fake PageFetcher
		line string
		note string
	}{
		{
			name: "a fetch the guard refused",
			fake: &fakeFetcher{errs: map[string]error{
				"https://example.com/x": fmt.Errorf("offline mode: example.com is not loopback; set core.mode = \"normal\" to allow web egress"),
			}},
			line: "@url https://example.com/x",
			note: "offline mode",
		},
		{
			name: "a fetch the operator denied",
			fake: &fakeFetcher{errs: map[string]error{
				"https://example.com/x": fmt.Errorf("webfetch denied by operator"),
			}},
			line: "@url https://example.com/x",
			note: "denied by operator",
		},
		{
			name: "a page that reduced to nothing",
			fake: &fakeFetcher{pages: map[string]tools.Page{
				"https://example.com/x": {URL: "https://example.com/x", Site: "generic"},
			}},
			line: "@url https://example.com/x",
			note: "reduced to nothing",
		},
		{
			name: "a token that is not a URL",
			fake: &fakeFetcher{},
			line: "@url example.com/x",
			note: "not an http(s) URL",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			exp := ExpandReferences(tc.line, Options{Workspace: t.TempDir(), Fetcher: tc.fake})
			if len(exp.Refs) != 1 {
				t.Fatalf("refs = %d, want 1: %+v", len(exp.Refs), exp.Refs)
			}
			ref := exp.Refs[0]
			if !ref.Failed || !ref.Literal {
				t.Fatalf("an unreadable URL did not stay literal: %+v", ref)
			}
			if exp.Prompt != tc.line {
				t.Fatalf("the token did not stay in the text:\n%s", exp.Prompt)
			}
			if !strings.Contains(ref.Note, tc.note) {
				t.Fatalf("note = %q, want %q", ref.Note, tc.note)
			}
			if !strings.Contains(exp.Summary(), "left literal") {
				t.Fatalf("the summary does not say the URL stayed literal: %q", exp.Summary())
			}
		})
	}
}

func TestURLReferenceWithoutAPolicyFailsClosed(t *testing.T) {
	ws, _ := tiers(t)
	// No policy and no fetcher: the default fetcher is built from the session's
	// own matrix, which allows no egress, so the token stays literal instead of
	// reaching the network unguarded.
	exp := ExpandReferences("@url https://docs.example.com/sandbox", Options{Workspace: ws})
	if len(exp.Refs) != 1 || !exp.Refs[0].Failed || !exp.Refs[0].Literal {
		t.Fatalf("refs = %+v, want one failed literal reference", exp.Refs)
	}
	if !strings.Contains(exp.Prompt, "@url https://docs.example.com/sandbox") {
		t.Fatalf("the token did not stay literal:\n%s", exp.Prompt)
	}
}

func TestURLReferenceGoesThroughTheEgressPermission(t *testing.T) {
	ws, _ := tiers(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><head><title>Loopback docs</title></head><body><main>
			<h1>Sandbox modes</h1><p>A sandbox binds what a command can touch.</p>
			<p>See the <a href="/network">network guide</a>.</p></main></body></html>`)
	}))
	defer srv.Close()

	// Without an approver the default matrix asks, and the question has no one
	// to answer it: the reference stays literal.
	exp := ExpandReferences("@url "+srv.URL+"/docs", Options{
		Workspace: ws,
		Policy:    tools.Policy{},
	})
	if len(exp.Refs) != 1 || !exp.Refs[0].Failed || !exp.Refs[0].Literal {
		t.Fatalf("refs = %+v, want a refused literal reference", exp.Refs)
	}
	if !strings.Contains(exp.Refs[0].Note, "approval") {
		t.Fatalf("note = %q, want the approval refusal", exp.Refs[0].Note)
	}

	// With the operator's answer the same reference fetches. The approver is
	// the one the reference asks, exactly as a webfetch tool call would.
	asked := 0
	exp = ExpandReferences("@url "+srv.URL+"/docs", Options{
		Workspace: ws,
		Policy:    tools.Policy{},
		Approve:   func(detail string) bool { asked++; return strings.Contains(detail, srv.URL) },
	})
	if asked != 1 {
		t.Fatalf("the egress permission was asked %d times, want 1", asked)
	}
	if len(exp.Refs) != 1 || exp.Refs[0].Failed {
		t.Fatalf("refs = %+v, want one injected reference", exp.Refs)
	}
	if !strings.Contains(exp.Prompt, "# Sandbox modes") {
		t.Fatalf("the reduction was not injected:\n%s", exp.Prompt)
	}
	if !strings.Contains(exp.Prompt, "[network guide]("+srv.URL+"/network)") {
		t.Fatalf("the link target was not preserved:\n%s", exp.Prompt)
	}
}

func TestURLReferenceIsRefusedOfflineThroughTheRealFetcher(t *testing.T) {
	ws, _ := tiers(t)
	// The workspace carries no config and XDG_CONFIG_HOME points at an empty
	// directory, so the default is offline: a non-loopback host is refused
	// before a byte is read.
	line := "@url https://docs.example.com/sandbox"
	exp := ExpandReferences(line, Options{
		Workspace: ws,
		Policy:    tools.Policy{Yolo: true},
	})
	if len(exp.Refs) != 1 {
		t.Fatalf("refs = %+v", exp.Refs)
	}
	ref := exp.Refs[0]
	if !ref.Failed || !ref.Literal {
		t.Fatalf("an offline session fetched a URL: %+v", ref)
	}
	if !strings.Contains(ref.Note, "offline mode") {
		t.Fatalf("note = %q, want the offline refusal", ref.Note)
	}
	if !strings.Contains(exp.Prompt, line) {
		t.Fatalf("the token did not stay literal:\n%s", exp.Prompt)
	}
}

func TestURLReferenceInsideACommandBody(t *testing.T) {
	ws, _ := tiers(t)
	target := "https://arxiv.org/abs/2401.00042"
	writeCommand(t, WorkspaceDir(ws), "read.md",
		"---\ndescription: read a URL\nargument-hint: <url>\n---\nSummarise @url $1\n")

	list := Load(Dirs(ws)...)
	fake := fakeFetcherFor(target, tools.Page{
		URL: target, Site: "arxiv", Extracted: true, Markdown: "# A paper\n\nContinuity beats tricks.",
	})
	exp := Expand("/read "+target, Options{Workspace: ws, Commands: list, Fetcher: fake})
	if exp.Command != "read" {
		t.Fatalf("command = %q", exp.Command)
	}
	if !strings.Contains(exp.Prompt, "# A paper") {
		t.Fatalf("the URL inside the body was not expanded:\n%s", exp.Prompt)
	}
	if !strings.Contains(exp.Prompt, "Summarise ") {
		t.Fatalf("the body's prose was lost:\n%s", exp.Prompt)
	}
}

func TestIsReferenceRecognisesURL(t *testing.T) {
	tests := []struct {
		line  string
		ref   bool
		token bool
	}{
		{"@url https://a.example/x", true, true},
		{"@url <https://a.example/x>", true, true},
		{"@url", false, true},                                // a reference with nothing to read explains itself
		{"summarise @url https://a.example/x", false, false}, // not at the line's start
		{"mail me at a@url.example", false, false},
		{"@reviewer look at this", false, false},
	}
	for _, tc := range tests {
		if got := IsReference(tc.line); got != tc.ref {
			t.Errorf("IsReference(%q) = %v, want %v", tc.line, got, tc.ref)
		}
		if got := IsReferenceToken(tc.line); got != tc.token {
			t.Errorf("IsReferenceToken(%q) = %v, want %v", tc.line, got, tc.token)
		}
	}
}

func TestURLReferenceWithoutAnArgumentExplainsItself(t *testing.T) {
	ws, _ := tiers(t)
	exp := ExpandReferences("@url", Options{Workspace: ws})
	if !strings.Contains(exp.Prompt, "@url") {
		t.Fatalf("the token did not stay literal:\n%s", exp.Prompt)
	}
	if len(exp.Refs) != 1 || !exp.Refs[0].Failed {
		t.Fatalf("refs = %+v", exp.Refs)
	}
	if !strings.Contains(exp.Summary(), "no URL given") {
		t.Fatalf("the summary does not say what is wrong: %q", exp.Summary())
	}
}

func TestURLReferenceIsCapped(t *testing.T) {
	ws, _ := tiers(t)
	target := "https://docs.example.com/big"
	fake := fakeFetcherFor(target, tools.Page{
		URL: target, Site: "docs", Extracted: true,
		Markdown: strings.Repeat("a", URLCap+4096),
	})
	exp := ExpandReferences("@url "+target, Options{Workspace: ws, Fetcher: fake})
	ref := exp.Refs[0]
	if !ref.Truncated {
		t.Fatalf("the reference is not marked truncated: %+v", ref)
	}
	if ref.Bytes > URLCap+64 {
		t.Fatalf("injected %d bytes, want about %d", ref.Bytes, URLCap)
	}
	if !strings.Contains(exp.Prompt, "truncated at") {
		t.Fatalf("the truncation marker is missing: %.200s", exp.Prompt[len(exp.Prompt)-200:])
	}
	if !strings.Contains(ref.Summary(), "truncated") {
		t.Fatalf("summary = %q", ref.Summary())
	}
}
