package context

// WP-3.7: the "skill" proposal class. It goes through the same gate as every
// other class — auto-accepted only when its class is listed in AutoClasses,
// otherwise pending operator review — and on apply it writes a SKILL.md.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kintsugi/internal/skills"
)

// skillDoc is a minimal CONTEXT.md the apply tests mutate.
func skillDoc() *Document {
	return &Document{
		Project: "lab",
		Sections: []Section{
			{Title: "Authorization & scope", Class: ClassAuthorization, Body: "- AUTH: local sandbox only."},
			{Title: "Current state", Class: ClassState, Body: "- stale"},
		},
	}
}

// skillProposal is one curator-emitted promotion: the class carries the intent,
// Section carries the skill name, and Description feeds the index line.
func skillProposal() Proposal {
	return Proposal{
		Section:     "lab-rebuild",
		Op:          "replace",
		Class:       ClassSkill,
		Content:     "## Steps\n\n1. `docker compose down -v`\n2. `make lab`\n",
		Reason:      "the same rebuild sequence succeeded three times this session",
		Confidence:  "high",
		Description: "Rebuild the lab stack from scratch.",
	}
}

// skillDir is the workspace skills directory a test writes into.
func skillDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), ".kintsugi", "skills")
}

// TestSkillProposalWritesALoadableSkill proves the fixture proposal produces a
// skill file, and that the file loads through the existing skill path.
func TestSkillProposalWritesALoadableSkill(t *testing.T) {
	dir := skillDir(t)
	doc := skillDoc()
	res, err := Apply(doc, []Proposal{skillProposal()}, ApplyMode{
		AutoClasses: []string{ClassSkill},
		SkillsDir:   dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 1 || len(res.Skills) != 1 {
		t.Fatalf("apply result = %+v", res)
	}
	w := res.Skills[0]
	if w.Name != "lab-rebuild" || w.Replaced {
		t.Fatalf("skill write = %+v", w)
	}
	if want := filepath.Join(dir, "lab-rebuild", "SKILL.md"); w.Path != want {
		t.Fatalf("wrote %s, want %s", w.Path, want)
	}

	// The document is untouched: a skill is not a CONTEXT.md section.
	if got := doc.Render(); strings.Contains(got, "lab-rebuild") || strings.Contains(got, "docker compose") {
		t.Fatalf("a skill proposal leaked into CONTEXT.md:\n%s", got)
	}

	// The existing skill path is the acceptance test.
	ws := filepath.Dir(filepath.Dir(dir))
	loaded, ok := skills.Find(skills.Load(ws, skills.Dir(ws, "")), "lab-rebuild")
	if !ok {
		t.Fatalf("promoted skill does not load: %+v", skills.Load(ws, skills.Dir(ws, "")))
	}
	if loaded.Description != "Rebuild the lab stack from scratch." {
		t.Fatalf("loaded description = %q", loaded.Description)
	}
	if !strings.Contains(loaded.Body, "make lab") {
		t.Fatalf("loaded body = %q", loaded.Body)
	}
}

// TestSkillProposalIsGatedLikeEveryOtherClass is the honesty test: a class that
// is not in auto_accept_classes is never written silently, and IncludeAll is the
// operator's explicit "apply the pending ones" action.
func TestSkillProposalIsGatedLikeEveryOtherClass(t *testing.T) {
	dir := skillDir(t)
	doc := skillDoc()

	// Operator auto-accepts state/lesson but has never approved skills.
	res, err := Apply(doc, []Proposal{skillProposal()}, ApplyMode{
		AutoClasses: []string{ClassState, ClassLesson},
		SkillsDir:   dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 0 || len(res.Skills) != 0 {
		t.Fatalf("a gated skill proposal was applied: %+v", res)
	}
	if len(res.Pending) != 1 {
		t.Fatalf("the skill proposal is not pending review: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "lab-rebuild", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatal("a pending skill proposal wrote a file")
	}

	// A gated class is never auto-applied even with nothing configured at all.
	res, err = Apply(doc, []Proposal{skillProposal()}, ApplyMode{SkillsDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 0 || len(res.Pending) != 1 {
		t.Fatalf("unconfigured apply = %+v", res)
	}

	// The operator's explicit --all does apply it.
	res, err = Apply(doc, []Proposal{skillProposal()}, ApplyMode{IncludeAll: true, SkillsDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skills) != 1 {
		t.Fatalf("IncludeAll did not write the skill: %+v", res)
	}

	// IncludeGated is for authorization/scope only and must not be a back door
	// around the auto list.
	other := skillDir(t)
	res, err = Apply(skillDoc(), []Proposal{skillProposal()}, ApplyMode{IncludeGated: true, SkillsDir: other})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skills) != 0 {
		t.Fatalf("IncludeGated applied a skill proposal: %+v", res)
	}

	// With no destination configured the proposal cannot become a silent no-op:
	// it stays pending.
	res, err = Apply(skillDoc(), []Proposal{skillProposal()}, ApplyMode{AutoClasses: []string{ClassSkill}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 0 || len(res.Pending) != 1 {
		t.Fatalf("a skill proposal with no skills directory = %+v", res)
	}
}

// TestSkillProposalReplacesAndKeepsOtherClassesWorking proves a mixed proposal
// set behaves: the section changes, the skill is written, and a second run
// updates the same file rather than duplicating the procedure.
func TestSkillProposalReplacesAndKeepsOtherClassesWorking(t *testing.T) {
	dir := skillDir(t)
	doc := skillDoc()
	props := []Proposal{
		{Section: "state", Op: "replace", Class: ClassState, Content: "- fresh"},
		skillProposal(),
	}
	res, err := Apply(doc, props, ApplyMode{
		AutoClasses: []string{ClassSkill, ClassState},
		SkillsDir:   dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 2 || len(res.Skills) != 1 {
		t.Fatalf("mixed apply = %+v", res)
	}
	if sec, ok := doc.Find(ClassState); !ok || !strings.Contains(sec.Body, "fresh") {
		t.Fatalf("the state proposal did not apply: %+v", doc)
	}

	second := skillProposal()
	second.Content = "## Steps\n\n1. run `make lab-reset`\n"
	res, err = Apply(doc, []Proposal{second}, ApplyMode{
		AutoClasses: []string{ClassSkill},
		SkillsDir:   dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skills) != 1 || !res.Skills[0].Replaced {
		t.Fatalf("second promotion did not report a replace: %+v", res.Skills)
	}
	raw, err := os.ReadFile(res.Skills[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "docker compose down") || !strings.Contains(string(raw), "make lab-reset") {
		t.Fatalf("the promoted procedure was appended rather than replaced:\n%s", raw)
	}
}

// TestSkillProposalRejectsUnsafeNames proves a curator cannot write outside the
// skills directory: the name is validated where the file would be created.
func TestSkillProposalRejectsUnsafeNames(t *testing.T) {
	dir := skillDir(t)
	for _, name := range []string{"../escape", "nested/path", ".hidden", "", ".."} {
		p := skillProposal()
		p.Section = name
		_, err := Apply(skillDoc(), []Proposal{p}, ApplyMode{
			AutoClasses: []string{ClassSkill},
			SkillsDir:   dir,
		})
		if err == nil {
			t.Fatalf("unsafe skill name %q was applied", name)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape")); !os.IsNotExist(err) {
		t.Fatal("a traversal name wrote outside the skills directory")
	}
}

// TestSkillProposalDiffShowsItsDestination proves the reviewable diff names the
// file a pending skill proposal would create — the operator's approval decision
// depends on it.
func TestSkillProposalDiffShowsItsDestination(t *testing.T) {
	dir := skillDir(t)
	doc := skillDoc()
	out := Diff(doc, []Proposal{skillProposal()}, ApplyMode{SkillsDir: dir})
	for _, want := range []string{
		"lab-rebuild → replace",
		"would require --all",
		"writes: " + filepath.Join(dir, "lab-rebuild", "SKILL.md"),
		"description: Rebuild the lab stack from scratch.",
		"reason: the same rebuild sequence succeeded three times this session",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("skill diff is missing %q:\n%s", want, out)
		}
	}
	// Once the class is auto-accepted the diff says so.
	auto := Diff(doc, []Proposal{skillProposal()}, ApplyMode{AutoClasses: []string{ClassSkill}, SkillsDir: dir})
	if !strings.Contains(auto, "auto-accepted") {
		t.Fatalf("auto-accepted skill proposal reported as gated:\n%s", auto)
	}
}

// TestParseProposalsAcceptsASkillAndValidatesIt proves the curator's JSON path
// carries the class and the description, and rejects a skill that names nothing
// writable.
func TestParseProposalsAcceptsASkillAndValidatesIt(t *testing.T) {
	got, err := ParseProposals(`[{"section":"lab-rebuild","op":"replace","class":"skill",` +
		`"content":"## Steps\n\n1. do it","description":"Rebuild   the\nlab stack.","reason":"repeated","confidence":"high"}]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Class != ClassSkill {
		t.Fatalf("parsed = %#v", got)
	}
	if got[0].Description != "Rebuild the lab stack." {
		t.Fatalf("description not normalised: %q", got[0].Description)
	}

	for _, bad := range []string{
		// A skill with no description would render an index line the operator
		// cannot act on.
		`[{"section":"x","op":"replace","class":"skill","content":"body","reason":"r","confidence":"low"}]`,
		// A skill whose name would escape the skills directory.
		`[{"section":"../x","op":"replace","class":"skill","content":"body","description":"d","reason":"r","confidence":"low"}]`,
	} {
		if _, err := ParseProposals(bad); err == nil {
			t.Fatalf("accepted a malformed skill proposal: %s", bad)
		}
	}

	// The non-skill path is unchanged: description is simply ignored.
	plain, err := ParseProposals(`[{"section":"state","op":"replace","content":"- a","reason":"r","confidence":"low"}]`)
	if err != nil {
		t.Fatal(err)
	}
	if plain[0].Class != ClassState || plain[0].Description != "" {
		t.Fatalf("a state proposal changed shape: %#v", plain[0])
	}
}
