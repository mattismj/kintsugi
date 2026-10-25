package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kintsugi/internal/config"
	"kintsugi/internal/harness"
)

// refusalEvidence names what is missing, so the operator can act. It never
// invents context that is not there.
func TestRefusalEvidenceReportsMissingSections(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := testModel(120, 34)
	m.app = &harness.App{Cfg: config.Defaults(), Workspace: t.TempDir()}

	ev := m.refusalEvidence()
	if !strings.Contains(ev, "authorization missing") {
		t.Fatalf("missing authorization not reported: %q", ev)
	}
	if !strings.Contains(ev, "domain missing") {
		t.Fatalf("missing domain not reported: %q", ev)
	}
}

func TestRefusalEvidenceReportsPresentAndTruncated(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "CONTEXT.md"), []byte(
		"# Engagement\n\n## Authorization\n\n- do the thing\n\n## Domain context\n\n- a lab\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	m := testModel(120, 34)
	m.app = &harness.App{Cfg: config.Defaults(), Workspace: ws}
	// Force truncation by a tiny budget.
	m.app.Cfg.Context.InjectBudgetTokens = 1

	ev := m.refusalEvidence()
	if strings.Contains(ev, "authorization missing") {
		t.Fatalf("authorization is present but reported missing: %q", ev)
	}
	if strings.Contains(ev, "domain missing") {
		t.Fatalf("domain is present but reported missing: %q", ev)
	}
	if !strings.Contains(ev, "truncated") {
		t.Fatalf("truncated context not flagged: %q", ev)
	}
}

func TestRefusalEvidenceHasNoWorkspaceGuard(t *testing.T) {
	m := testModel(120, 34)
	m.app = nil
	if got := m.refusalEvidence(); !strings.Contains(got, "no workspace") {
		t.Fatalf("nil app gave %q", got)
	}
}
