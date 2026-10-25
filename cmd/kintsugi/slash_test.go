package main

// The slash-command reference guard: docs/tui.md must cover every slash
// command the TUI dispatch switch accepts, the same way the man-page guard
// covers every CLI verb. A new command cannot ship undocumented.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// slashCommandsFromSource reads the handleSlash cases out of the TUI source
// the same way the man-page guard reads the dispatch switch: from the code,
// not a hand-maintained list.
func slashCommandsFromSource(t *testing.T) []string {
	t.Helper()
	src, err := readFileAt("internal/tui/update.go")
	if err != nil {
		t.Skipf("source layout: %v", err)
	}
	re := regexp.MustCompile(`case "(/[a-z-]+)"`)
	seen := map[string]bool{}
	var out []string
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		cmd := m[1]
		if !seen[cmd] {
			seen[cmd] = true
			out = append(out, cmd)
		}
	}
	return out
}

func readFileAt(rel string) (string, error) {
	for _, base := range []string{"../..", "."} {
		b, err := os.ReadFile(base + "/" + rel)
		if err == nil {
			return string(b), nil
		}
	}
	return "", os.ErrNotExist
}

func TestTUIReferenceCoversEverySlashCommand(t *testing.T) {
	cmds := slashCommandsFromSource(t)
	doc, err := readFileAt("docs/tui.md")
	if err != nil {
		t.Fatalf("docs/tui.md: %v (run from the repo root)", err)
	}
	for _, cmd := range cmds {
		if !strings.Contains(doc, cmd) {
			t.Errorf("the TUI reference is missing %q — add it to docs/tui.md", cmd)
		}
	}
}
