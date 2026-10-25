package main

// Tests for `kintsugi refusal corpus`: storage, coverage stats, and the
// proposal loop that turns missed texts into lexicon patterns.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCorpusAddStatsAndCoverage(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// One covered text (the lexicon matches it) and one genuine miss.
	out := captureStdout(t, func() {
		cmdRefusalCorpus(ws, []string{"add",
			"I can't assist with that request.",
			"This request falls outside the kinds of work I can help with.",
		})
	})
	if !strings.Contains(out, "added 2") {
		t.Fatalf("add output = %q", out)
	}

	stats := captureStdout(t, func() { cmdRefusalCorpus(ws, []string{"stats"}) })
	if !strings.Contains(stats, "2 text(s)") || !strings.Contains(stats, "1 matched") || !strings.Contains(stats, "1 missed") {
		t.Fatalf("stats = %q", stats)
	}
}

func TestCorpusProposeAndApply(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// Two misses sharing a 3-word phrase: a proposal must find it.
	texts := []string{
		"This request falls outside the kinds of work I can help with.",
		"That falls outside the kinds of assistance I provide.",
	}
	for _, tx := range texts {
		cmdRefusalCorpus(ws, []string{"add", tx})
	}

	before := captureStdout(t, func() { cmdRefusalCorpus(ws, []string{"stats"}) })
	if !strings.Contains(before, "2 missed") {
		t.Fatalf("precondition: %q", before)
	}

	prop := captureStdout(t, func() { cmdRefusalCorpus(ws, []string{"propose", "--min", "2"}) })
	if !strings.Contains(prop, "falls outside the") && !strings.Contains(prop, "outside the kinds") {
		t.Fatalf("the proposal did not find the shared phrase:\n%s", prop)
	}
	if !strings.Contains(prop, "--apply") {
		t.Fatalf("the proposal does not name the apply step:\n%s", prop)
	}

	applied := captureStdout(t, func() { cmdRefusalCorpus(ws, []string{"propose", "--min", "2", "--apply"}) })
	if !strings.Contains(applied, "applied 1 pattern") {
		t.Fatalf("apply output = %q", applied)
	}
	// Coverage rose: both texts now match.
	after := captureStdout(t, func() { cmdRefusalCorpus(ws, []string{"stats"}) })
	if strings.Contains(after, "0 matched") {
		t.Fatalf("coverage did not rise:\n%s", after)
	}
	// The pattern landed in the override file.
	path := filepath.Join(ws, ".kintsugi", "refusal_patterns.toml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the override file was not written: %v", err)
	}
	if !strings.Contains(string(raw), "outside the kinds") && !strings.Contains(string(raw), "falls outside the") {
		t.Fatalf("the pattern is missing from %s:\n%s", path, raw)
	}
}

func TestCorpusExtractsExportReports(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	report := filepath.Join(t.TempDir(), "report.md")
	body := "# Refusal report\n\n**refusal text**\n\n```\nI will not write that. The request falls outside scope.\n```\n\n---\n"
	if err := os.WriteFile(report, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		cmdRefusalCorpus(ws, []string{"add", "--file", report})
	})
	if !strings.Contains(out, "added 1") {
		t.Fatalf("report import = %q", out)
	}
}
