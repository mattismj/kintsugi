package context

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The state section is a snapshot, never an accumulation: it is rebuilt from
// signals the workspace itself emits, so two refreshes of an unchanged
// workspace produce the same bytes.
const (
	refreshRecentLimit = 10             // files listed under "- Recent:"
	refreshWindow      = 72 * time.Hour // "recently modified" horizon
	gitSignalTimeout   = 10 * time.Second
	buildSignalTimeout = 30 * time.Second
)

// StateFileName is the workspace-root CONTEXT.md that Refresh and ApplyRefresh
// read and rewrite (the default `context.path`).
const StateFileName = "CONTEXT.md"

// RefreshResult is the proposed replacement for the Current state section.
type RefreshResult struct {
	Section string // section class; always "state"
	Old     string // current body, verbatim
	New     string // replacement body
	Changed bool   // Old != New
}

// skipDirs are directories whose contents say nothing about the project's
// current state (or are regenerated on every build).
var skipDirs = map[string]bool{
	".git":         true,
	".hg":          true,
	".svn":         true,
	".kintsugi":    true,
	".idea":        true,
	".vscode":      true,
	"node_modules": true,
	"vendor":       true,
	"dist":         true,
	"build":        true,
	"bin":          true,
	"out":          true,
	"target":       true,
	"__pycache__":  true,
}

// ContextFile is the CONTEXT.md path Refresh/ApplyRefresh operate on.
func ContextFile(workspace string) string {
	return filepath.Join(workspace, StateFileName)
}

// Refresh gathers deterministic workspace signals and renders them as a
// replacement body for the Current state section. It never writes: ApplyRefresh
// does, and only to that one section.
func Refresh(workspace string) (RefreshResult, error) {
	path := ContextFile(workspace)
	doc, _, err := LoadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return RefreshResult{}, fmt.Errorf("context refresh: %s not found (run `kintsugi context init`)", path)
		}
		return RefreshResult{}, fmt.Errorf("context refresh: %w", err)
	}
	sec, ok := doc.Find(ClassState)
	if !ok {
		return RefreshResult{}, fmt.Errorf("context refresh: %s has no Current state section (add a `## Current state` heading)", path)
	}
	body := StateBody(workspace)
	return RefreshResult{
		Section: ClassState,
		Old:     sec.Body,
		New:     body,
		Changed: sec.Body != body,
	}, nil
}

// ApplyRefresh rewrites only the Current state section body. Every byte outside
// that section — Authorization, Scope, headings, spacing, trailing newline — is
// copied through untouched.
func ApplyRefresh(workspace string, r RefreshResult) error {
	if r.Section != "" && r.Section != ClassState {
		return fmt.Errorf("context refresh: refuses to rewrite %q; only the state section is refreshable", r.Section)
	}
	if !r.Changed {
		return nil
	}
	path := ContextFile(workspace)
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("context refresh: %w", err)
	}
	out, err := replaceSectionBody(string(raw), ClassState, r.New)
	if err != nil {
		return fmt.Errorf("context refresh: %s: %w", path, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(out), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// StateBody composes the bullet list for the Current state section:
//
//   - Build: passing
//   - Uncommitted: 3 files
//   - Diff: 3 files changed, 12 insertions(+), 4 deletions(-)
//   - Recent: cmd/kintsugi/main.go, internal/kintsugi/app.go
//
// A signal that cannot be gathered (no git, no go.mod, a tool that is missing
// or slow) is dropped rather than reported as an error: a refresh must never
// fail because a subprocess did.
func StateBody(workspace string) string {
	var bullets []string

	if _, err := os.Stat(filepath.Join(workspace, "go.mod")); err == nil {
		bullets = append(bullets, "- Build: "+buildStatus(workspace))
	}

	if isGitRepo(workspace) {
		if n, ok := uncommittedCount(workspace); ok {
			bullets = append(bullets, fmt.Sprintf("- Uncommitted: %d %s", n, plural(n, "file")))
		}
		if stat, ok := diffStat(workspace); ok {
			bullets = append(bullets, "- Diff: "+stat)
		}
	}

	if recent := recentFiles(workspace, refreshRecentLimit, refreshWindow); len(recent) > 0 {
		bullets = append(bullets, "- Recent: "+strings.Join(recent, ", "))
	}

	if len(bullets) == 0 {
		bullets = append(bullets, "- No signals (no git checkout, no go.mod, no edits in the last 72h)")
	}
	return strings.Join(bullets, "\n")
}

// replaceSectionBody swaps the body of the first section of the given class,
// leaving every byte outside it identical. The original leading blank line and
// the section's trailing blank lines are preserved, so a refresh never reflows
// the document or adds/removes the file's trailing newline.
func replaceSectionBody(raw, class, body string) (string, error) {
	lines := strings.Split(raw, "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "## ") && classify(strings.TrimSpace(strings.TrimPrefix(line, "## "))) == class {
			start = i
			break
		}
	}
	if start < 0 {
		return "", fmt.Errorf("no section of class %q", class)
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			end = i
			break
		}
	}

	// Trailing blank lines belong to the old body region; keep their count so
	// the spacing before the next heading (and the file's final newline) is
	// byte-identical.
	trail := 0
	for i := end - 1; i > start && lines[i] == ""; i-- {
		trail++
	}
	if trail == 0 && end < len(lines) {
		trail = 1
	}

	out := make([]string, 0, len(lines)+4)
	out = append(out, lines[:start+1]...)
	out = append(out, "")
	if b := strings.TrimSpace(body); b != "" {
		out = append(out, strings.Split(b, "\n")...)
	}
	for ; trail > 0; trail-- {
		out = append(out, "")
	}
	out = append(out, lines[end:]...)
	return strings.Join(out, "\n"), nil
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// runSignal runs one best-effort subprocess with a timeout. Failures are the
// caller's to ignore; a subprocess that exits non-zero but still prints to
// stdout (git status in an empty repo) counts as output.
func runSignal(dir string, timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stdout strings.Builder
	cmd.Stdout = &stdout
	err := cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && stdout.Len() > 0 {
			return stdout.String(), nil
		}
		return "", err
	}
	return stdout.String(), nil
}

func isGitRepo(workspace string) bool {
	out, err := runSignal(workspace, gitSignalTimeout, "git", "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "true"
}

func uncommittedCount(workspace string) (int, bool) {
	out, err := runSignal(workspace, gitSignalTimeout, "git", "status", "--short")
	if err != nil {
		return 0, false
	}
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n, true
}

// diffStat returns the summary line of `git diff --stat HEAD`
// ("3 files changed, 12 insertions(+), 4 deletions(-)"), or false when the
// checkout has no HEAD or no changes.
func diffStat(workspace string) (string, bool) {
	out, err := runSignal(workspace, gitSignalTimeout, "git", "diff", "--stat", "HEAD")
	if err != nil {
		return "", false
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			return s, true
		}
	}
	return "", false
}

// buildStatus reports only pass/fail of `go build ./...`: the state section is
// a snapshot, not a build log.
func buildStatus(workspace string) string {
	_, err := runSignal(workspace, buildSignalTimeout, "go", "build", "./...")
	switch {
	case err == nil:
		return "passing"
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	default:
		return "failing"
	}
}

// recentFiles lists workspace-relative paths touched within the window, newest
// first. CONTEXT.md itself is excluded: rewriting it must not change the signal
// that produced the rewrite.
func recentFiles(workspace string, limit int, window time.Duration) []string {
	cutoff := time.Now().Add(-window)
	type hit struct {
		rel string
		mod time.Time
	}
	var hits []hit

	_ = filepath.WalkDir(workspace, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if p == workspace {
				return nil
			}
			if skipDirs[name] || strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.ModTime().Before(cutoff) {
			return nil
		}
		rel, err := filepath.Rel(workspace, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == StateFileName {
			return nil
		}
		hits = append(hits, hit{rel: rel, mod: info.ModTime()})
		return nil
	})

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].mod.Equal(hits[j].mod) {
			return hits[i].rel < hits[j].rel
		}
		return hits[i].mod.After(hits[j].mod)
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.rel)
	}
	return out
}
