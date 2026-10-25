package harness

// WP-3.7 end to end: a curator-emitted "skill" proposal becomes a workspace
// skill through App.Curate — the same gated path that applies CONTEXT.md
// proposals — and is loadable by the same skill index the prompt uses.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	fctx "kintsugi/internal/context"
	"kintsugi/internal/skills"
)

// TestCuratePromotesRepeatedProcedureToSkill proves the fixture proposal lands
// as .kintsugi/skills/<name>/SKILL.md on apply, that the document is left
// alone, and that App.Skills() sees it — the same loader the prompt index uses.
func TestCuratePromotesRepeatedProcedureToSkill(t *testing.T) {
	app, _ := curatorApp(t, `[{"section":"lab-rebuild","op":"replace","class":"skill",`+
		`"content":"## Steps\n\n1. run `+"`make lab-reset`"+`\n2. wait for the health probe\n",`+
		`"description":"Rebuild the lab stack from scratch.",`+
		`"reason":"the same rebuild succeeded three times","confidence":"high"}]`)
	app.Cfg.Context.CuratorOnSessionEnd = true
	// The operator has approved skills for auto-accept.
	app.Cfg.Context.AutoAcceptClasses = []string{"state", "skill"}

	before, err := os.ReadFile(filepath.Join(app.Workspace, "CONTEXT.md"))
	if err != nil {
		t.Fatal(err)
	}

	res := MaybeCurate(app, testSession(t))
	if res == nil {
		t.Fatal("curation returned nil")
	}
	if len(res.Apply.Skills) != 1 {
		t.Fatalf("no skill was written: %+v", res.Apply)
	}
	want := filepath.Join(app.Workspace, ".kintsugi", "skills", "lab-rebuild", "SKILL.md")
	if res.Apply.Skills[0].Path != want {
		t.Fatalf("wrote %s, want %s", res.Apply.Skills[0].Path, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("skill file missing: %v", err)
	}

	// CONTEXT.md is byte-identical: a skill is not a document section.
	after, err := os.ReadFile(filepath.Join(app.Workspace, "CONTEXT.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("a skill promotion rewrote CONTEXT.md:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}

	// The existing skill path loads what the curator wrote.
	loaded, ok := skills.Find(app.Skills(), "lab-rebuild")
	if !ok {
		t.Fatalf("the promoted skill does not load: %+v", app.Skills())
	}
	if loaded.Description != "Rebuild the lab stack from scratch." {
		t.Fatalf("description = %q", loaded.Description)
	}
	if !strings.Contains(loaded.Body, "make lab-reset") {
		t.Fatalf("procedure lost:\n%s", loaded.Body)
	}
	if idx, _ := app.skillsIndex(); !strings.Contains(idx, "`lab-rebuild`") {
		t.Fatalf("the promoted skill is not in the injected index:\n%s", idx)
	}
}

// TestCurateDoesNotAutoApplyUnlistedSkillClass proves a skill proposal whose
// class is not in auto_accept_classes is never written silently, at the app
// level: it is saved as a pending proposal and nothing lands on disk.
func TestCurateDoesNotAutoApplyUnlistedSkillClass(t *testing.T) {
	app, calls := curatorApp(t, `[{"section":"lab-rebuild","op":"replace","class":"skill",`+
		`"content":"## Steps\n\n1. run `+"`make lab-reset`"+`\n",`+
		`"description":"Rebuild the lab stack.","reason":"repeated","confidence":"high"}]`)
	app.Cfg.Context.CuratorOnSessionEnd = true
	app.Cfg.Context.AutoAcceptClasses = []string{"state"}

	res := MaybeCurate(app, testSession(t))
	if res == nil {
		t.Fatal("curation returned nil")
	}
	if n := *calls; n != 1 {
		t.Fatalf("provider calls = %d, want 1", n)
	}
	if len(res.Apply.Skills) != 0 || len(res.Apply.Applied) != 0 {
		t.Fatalf("a gated skill proposal was applied: %+v", res.Apply)
	}
	if len(res.Apply.Pending) != 1 {
		t.Fatalf("the skill proposal is not pending: %+v", res.Apply)
	}
	if _, err := os.Stat(filepath.Join(app.Workspace, ".kintsugi", "skills")); !os.IsNotExist(err) {
		t.Fatal("a pending skill proposal wrote a skills directory")
	}
	// The proposal is still reviewable on disk, which is the whole contract.
	if _, err := os.Stat(res.ProposalPath); err != nil {
		t.Fatalf("proposal file missing: %v", err)
	}
}

// TestDocTouchedIgnoresSkillOnlyApplies pins the invariant that keeps
// CONTEXT.md stable: only a real section proposal triggers a document write.
func TestDocTouchedIgnoresSkillOnlyApplies(t *testing.T) {
	if docTouched(nil) {
		t.Fatal("an empty applied set touched the document")
	}
	if docTouched([]fctx.Proposal{{Class: fctx.ClassSkill}}) {
		t.Fatal("a skill-only apply touched the document")
	}
	if !docTouched([]fctx.Proposal{{Class: fctx.ClassSkill}, {Class: fctx.ClassState}}) {
		t.Fatal("a mixed apply did not touch the document")
	}
}
