package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCommandReferenceCompleteness checks that every top-level command has a reference doc.
func TestCommandReferenceCompleteness(t *testing.T) {
	// Find the reference directory.
	candidates := []string{}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates,
			filepath.Join(cwd, "docs", "reference"),
			filepath.Join(cwd, "..", "..", "docs", "reference"),
		)
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates,
			filepath.Join(filepath.Dir(exe), "..", "..", "docs", "reference"),
		)
	}

	var refDir string
	for _, dir := range candidates {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			refDir = dir
			break
		}
	}
	if refDir == "" {
		t.Skip("reference directory not found")
	}

	// Commands that should have reference docs.
	commands := []string{
		"kintsugi", "run", "plan", "hooks", "audit", "prompt", "models", "pack",
		"skills", "context", "ledger", "taste", "recall", "persona", "memory",
		"session", "doctor", "repair", "refusal", "gate", "dag", "rules",
		"quota", "report", "init", "connect", "setup", "new", "validate",
		"checkpoint", "rewind", "kb", "lab", "mods", "docs",
	}

	for _, cmd := range commands {
		t.Run(cmd, func(t *testing.T) {
			filename := "kintsugi"
			if cmd != "kintsugi" {
				filename = "kintsugi-" + cmd
			}
			filename += ".md"
			path := filepath.Join(refDir, filename)

			if _, err := os.Stat(path); err != nil {
				t.Errorf("missing reference doc for %q (expected %s)", cmd, filename)
				return
			}

			// Check that the file has required sections.
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", filename, err)
			}
			content := string(data)

			required := []string{"# kintsugi", "## Synopsis", "## Description", "## Examples"}
			for _, section := range required {
				if !strings.Contains(content, section) {
					t.Errorf("missing required section %q in %s", section, filename)
				}
			}

			// Check for code examples.
			if !strings.Contains(content, "```bash") {
				t.Errorf("no bash code examples in %s", filename)
			}

			// Check for See Also section.
			if !strings.Contains(content, "## See Also") {
				t.Errorf("missing 'See Also' section in %s", filename)
			}
		})
	}
}

// TestRoffToPlain tests the roff to plain text conversion.
func TestRoffToPlain(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "title",
			input:    ".TH FORGE 1",
			expected: "FORGE(1)",
		},
		{
			name:     "section header",
			input:    ".SH NAME",
			expected: "\nNAME",
		},
		{
			name:     "bold text",
			input:    ".B kintsugi",
			expected: "kintsugi",
		},
		{
			name:     "italic text",
			input:    ".I option",
			expected: "option",
		},
		{
			name:     "bullet",
			input:    ".IP \\(bu",
			expected: "  •",
		},
		{
			name:     "comment",
			input:    ".\\\" this is a comment",
			expected: "",
		},
		{
			name:     "paragraph break",
			input:    ".PP",
			expected: "",
		},
		{
			name:     "plain text",
			input:    "This is plain text",
			expected: "This is plain text",
		},
		{
			name:     "escape sequences",
			input:    "text\\-with\\-dashes",
			expected: "text-with-dashes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := roffToPlain(tt.input)
			if result != tt.expected {
				t.Errorf("roffToPlain(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

// TestManPageName tests the man page name mapping.
func TestManPageName(t *testing.T) {
	tests := []struct {
		cmd      string
		expected string
	}{
		{"", "kintsugi.1"},
		{"kintsugi", "kintsugi.1"},
		{"run", "kintsugi-run.1"},
		{"connect", "kintsugi-connect.1"},
		{"session", "kintsugi-session.1"},
	}

	for _, tt := range tests {
		t.Run(tt.cmd, func(t *testing.T) {
			result := manPageName(tt.cmd)
			if result != tt.expected {
				t.Errorf("manPageName(%q) = %q, want %q", tt.cmd, result, tt.expected)
			}
		})
	}
}
