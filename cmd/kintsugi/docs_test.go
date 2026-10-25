package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestManPageSyntax validates that all man pages have valid roff syntax.
func TestManPageSyntax(t *testing.T) {
	dir := docsDir()
	if dir == "" {
		t.Skip("man page directory not found")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read man dir: %v", err)
	}

	// Required sections for a valid man page.
	requiredSections := []string{".TH", ".SH NAME", ".SH SYNOPSIS", ".SH DESCRIPTION"}

	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".1") {
			continue
		}

		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			content := string(data)

			// Check for required sections.
			for _, section := range requiredSections {
				if !strings.Contains(content, section) {
					t.Errorf("missing required section %s", section)
				}
			}

			// Check for .TH line (title header).
			if !regexp.MustCompile(`\.TH\s+\S+\s+\d+`).MatchString(content) {
				t.Error("invalid or missing .TH line")
			}

			// Check that .SH sections have content.
			lines := strings.Split(content, "\n")
			for i, line := range lines {
				if strings.HasPrefix(line, ".SH ") {
					// Next non-empty line should exist.
					found := false
					for j := i + 1; j < len(lines); j++ {
						if strings.TrimSpace(lines[j]) != "" {
							found = true
							break
						}
					}
					if !found {
						t.Errorf(".SH at line %d has no content", i+1)
					}
				}
			}

			// Check for balanced quotes in .SH headers.
			for _, line := range lines {
				if strings.HasPrefix(line, ".SH ") {
					rest := strings.TrimPrefix(line, ".SH ")
					quoteCount := strings.Count(rest, "\"")
					if quoteCount%2 != 0 {
						t.Errorf("unbalanced quotes in .SH: %s", line)
					}
				}
			}
		})
	}
}

// TestManPageCompleteness checks that every top-level command in main.go has a man
// page. The command list is parsed out of the dispatch switch, not restated here:
// a hardcoded list drifts, and a command with no page is exactly the kind of
// dead surface that a stale list hides.
func TestManPageCompleteness(t *testing.T) {
	dir := docsDir()
	if dir == "" {
		t.Skip("man page directory not found")
	}

	commands, err := dispatchedCommands()
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) < 20 {
		t.Fatalf("parsed only %d commands from main.go; the dispatch shape probably changed", len(commands))
	}

	// Commands with no page of their own: the interactive TUI is documented in
	// kintsugi(1), and version/help are flags rather than commands.
	skip := map[string]bool{"tui": true, "version": true, "help": true}

	for _, cmd := range commands {
		if skip[cmd] {
			continue
		}
		t.Run(cmd, func(t *testing.T) {
			page := manPageName(cmd)
			path := filepath.Join(dir, page)
			if _, err := os.Stat(path); err != nil {
				t.Errorf("missing man page for %q (expected %s)", cmd, page)
			}
		})
	}
}

// dispatchedCommands reads the top-level command names out of main.go's dispatch
// switch. A top-level case is one whose body calls the dispatcher named after it
// — `case "run":` calls `cmdRun(...)`. Nested subcommand switches call a longer
// two-word name (`case "refresh":` calls `cmdContextRefresh`), so requiring the
// names to line up keeps this list to real top-level commands without restating
// them by hand.
func dispatchedCommands() ([]string, error) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(raw), "\n")
	// A top-level dispatch case: one tab, a lowercase name, a quoted alias at
	// most, and the matching cmdXxx call on the following line.
	caseRe := regexp.MustCompile(`^\tcase "([a-z][a-z0-9_-]*)"(?:, "[^"]*")*:$`)
	callRe := regexp.MustCompile(`^\t\tcmd([A-Z][A-Za-z0-9]*)\(`)

	var out []string
	seen := map[string]bool{}
	for i := 0; i+1 < len(lines); i++ {
		cm := caseRe.FindStringSubmatch(lines[i])
		if cm == nil {
			continue
		}
		name := cm[1]
		call := callRe.FindStringSubmatch(lines[i+1])
		if call == nil {
			continue
		}
		// cmdRun -> "run": the dispatcher must be the case name itself.
		if !strings.EqualFold(call[1], strings.ReplaceAll(name, "-", "")) {
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	// kintsugi(1) documents the program itself, which no case names.
	if !seen["kintsugi"] {
		out = append([]string{"kintsugi"}, out...)
	}
	return out, nil
}
