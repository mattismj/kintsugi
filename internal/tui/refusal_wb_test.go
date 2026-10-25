package tui

// Tests for the /refusal workbench. Hermetic: a temp workspace, the default
// lexicon, no network.

import (
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbletea"

	"kintsugi/internal/intercept"
)

// refusalModel is a transcript bound to a temp workspace so the override
// file writes somewhere real.
func refusalModel(t *testing.T) *Model {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	ws := t.TempDir()
	m := testModel(100, 30)
	m.app.Workspace = ws
	m.app.Cfg.Intercept.Enabled = true
	m.loop = nil // the rewire path guards for nil like the fixture-free path
	return m
}

func TestSlashRefusalOpensTheWorkbench(t *testing.T) {
	m := refusalModel(t)
	_, _ = m.handleSlash("/refusal")
	if m.refusal == nil {
		t.Fatal("/refusal did not open the workbench")
	}
	// The default lexicon's classes are listed with their patterns.
	out := plain(m.View())
	for _, want := range []string{"// refusal · lexicon", "moralizing", "identity",
		"legal-lecture", "safety-lecture", "off-topic-safety"} {
		if !strings.Contains(out, want) {
			t.Errorf("workbench missing %q:\n%s", want, out)
		}
	}
}

func TestRefusalTestComposerScansTheSample(t *testing.T) {
	m := refusalModel(t)
	_, _ = m.handleSlash("/refusal")
	// t opens the test composer; type a refusal; enter scans it.
	pressKeys(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	pressKeys(m, keyRunes("I can't assist with that request"))
	pressKeys(m, keyEnter())
	if m.refusal == nil || m.refusal.result == "" {
		t.Fatal("the test composer produced no verdict")
	}
	if !strings.Contains(m.refusal.result, "moralizing") {
		t.Errorf("the verdict does not name the class:\n%s", m.refusal.result)
	}
	// The scrubbing rule holds: a refusal inside fenced code does not match.
	pressKeys(m, tea.KeyMsg{Type: tea.KeyCtrlU})
	pressKeys(m, keyRunes("```\nI can't assist\n```"))
	pressKeys(m, keyEnter())
	if !strings.Contains(m.refusal.result, "no patterns matched") {
		t.Errorf("the scan fired inside fenced code:\n%s", m.refusal.result)
	}
}

func TestRefusalAddComposerWritesAndRewires(t *testing.T) {
	m := refusalModel(t)
	_, _ = m.handleSlash("/refusal")
	pressKeys(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	pressKeys(m, keyRunes("moralizing | \\bI am not permitted to\\b"))
	pressKeys(m, keyEnter())
	if m.refusal == nil {
		t.Fatal("the add composer closed the workbench")
	}
	if !strings.Contains(m.refusal.note, "added to moralizing") {
		t.Fatalf("the add produced no confirmation: %q", m.refusal.note)
	}
	// The override file on disk carries the pattern.
	path := m.app.EffectiveLexiconPath()
	classes, err := intercept.OverrideClasses(path)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range classes["moralizing"] {
		if strings.Contains(p, "not permitted") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the pattern was not written to %s: %+v", path, classes)
	}
	// The workbench's list refreshed: the new pattern is visible, marked as
	// operator-origin.
	var listed bool
	for _, e := range m.refusal.entries {
		if strings.Contains(e.Src, "not permitted") && e.Origin == "operator" {
			listed = true
		}
	}
	if !listed {
		t.Fatal("the refreshed list does not show the new operator pattern")
	}

	// A malformed pattern is refused before anything is written.
	before, _ := intercept.OverrideClasses(path)
	pressKeys(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	pressKeys(m, keyRunes("moralizing | \\bunclosed("))
	pressKeys(m, keyEnter())
	after, _ := intercept.OverrideClasses(path)
	if len(after["moralizing"]) != len(before["moralizing"]) {
		t.Fatal("a broken pattern was written to the file")
	}
	if !strings.Contains(m.refusal.note, "✗") {
		t.Fatalf("a broken pattern produced no error: %q", m.refusal.note)
	}
}

func TestRefusalEscCloses(t *testing.T) {
	m := refusalModel(t)
	_, _ = m.handleSlash("/refusal")
	pressKeys(m, keyEsc())
	if m.refusal != nil {
		t.Fatal("esc did not close the workbench")
	}
}

func TestRefusalEditCreatesTheOverrideFile(t *testing.T) {
	m := refusalModel(t)
	_, _ = m.handleSlash("/refusal")
	// e creates the file so $EDITOR never opens a dead end. The editor itself
	// cannot run here (no TTY), so assert the creation half of the flow.
	v := m.refusal
	pressKeys(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	path := m.app.EffectiveLexiconPath()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("e did not create the override file at %s: %v", path, err)
	}
	_ = v
}
