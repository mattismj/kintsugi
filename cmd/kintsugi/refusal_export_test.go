package main

// Tests for `kintsugi refusal export`. Hermetic: a fixture session in a
// redirected store, a synthetic refusal event, and the live lexicon.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kintsugi/internal/session"
)

// exportFixtureSession writes a session with one refusal event, the user task,
// and the assistant content that carries the refusal text.
func exportFixtureSession(t *testing.T, refusalText, matchedPhrase string) string {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	s, err := session.New()
	if err != nil {
		t.Fatal(err)
	}
	s.Log("user", "write a port scanner for the lab")
	s.Log("assistant", refusalText)
	s.Log("refusal", map[string]any{
		"class": "moralizing", "phrase": matchedPhrase, "level": 1, "outcome": "resumed",
	})
	s.Close()
	return s.ID
}

func TestRefusalExportRendersVerdictAndSuggestion(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ws := t.TempDir()
	refusal := "This request falls outside the kinds of work I can help with, so I will have to pass on it."
	id := exportFixtureSession(t, refusal, "outside the kinds of work")

	// The default report: verbatim refusal text, coverage verdict, and the
	// add-command line for a miss.
	var out strings.Builder
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	cmdRefusalExportWithOut(t, ws, id, false, &out)
	w.Close()
	os.Stdout = old
	_ = r
	_ = old

	report := out.String()
	for _, want := range []string{
		"# Refusal report — session " + id,
		"Operator-initiated export for corpus sharing",
		"class: moralizing",
		"matched phrase: \"outside the kinds of work\"",
		refusal,
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the report missing %q:\n%s", want, report)
		}
	}
	// The lexicon matches "I can't assist" — wait, the phrase is "can't assist"
	// and the lexicon pattern is \bI (?:can't|cannot...)\b — the phrase without
	// the leading "I" does NOT match. The coverage verdict names the miss and
	// suggests the command.
	if !strings.Contains(report, "coverage: MISSED") {
		t.Errorf("the coverage verdict did not fire on a real miss:\n%s", report)
	}
	if !strings.Contains(report, "kintsugi refusal add moralizing") {
		t.Errorf("the suggested add command is missing:\n%s", report)
	}
}

// TestRefusalExportCoverageHit: an event whose text the lexicon matches
// reports covered, not missed.
func TestRefusalExportCoverageHit(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ws := t.TempDir()
	refusal := "I can't assist with that request, and I won't provide code for it."
	id := exportFixtureSession(t, refusal, "can't assist")

	var out strings.Builder
	cmdRefusalExportWithOut(t, ws, id, false, &out)
	if !strings.Contains(out.String(), "coverage: matched") {
		t.Fatalf("a covered refusal reported a miss:\n%s", out.String())
	}
}

// cmdRefusalExportWithOut drives the command with a captured writer.
func cmdRefusalExportWithOut(t *testing.T, ws, id string, full bool, out *strings.Builder) {
	t.Helper()
	// The command writes to stdout or --out; use --out into a temp file and
	// read it back.
	p := filepath.Join(t.TempDir(), "report.md")
	if full {
		cmdRefusalExport(ws, []string{id, "--full", "--out", p})
	} else {
		cmdRefusalExport(ws, []string{id, "--out", p})
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("the report was not written: %v", err)
	}
	out.WriteString(string(raw))
}

var _ = json.Marshal
