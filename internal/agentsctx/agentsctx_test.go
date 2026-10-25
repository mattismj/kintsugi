package agentsctx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kintsugi/internal/persona"
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDirToleratesAMissingDirectory(t *testing.T) {
	docs, warns, err := LoadDir(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("a missing directory is the default state, not an error: %v", err)
	}
	if len(docs) != 0 || len(warns) != 0 {
		t.Fatalf("docs=%v warns=%v", docs, warns)
	}
}

func TestLoadFileSplitsFrontmatter(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "qwen.md", `---
note: qwen drift
match:
  family: [qwen]
  id_glob: ["qwen3-*"]
---
# Working style
Re-read files before editing.
`)
	d, err := LoadFile(p)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if d.Name != "qwen" || d.Note != "qwen drift" {
		t.Fatalf("meta = %+v", d)
	}
	if len(d.Match.Family) != 1 || d.Match.Family[0] != "qwen" {
		t.Fatalf("family = %v", d.Match.Family)
	}
	if strings.Contains(d.Body, "---") || strings.Contains(d.Body, "match:") {
		t.Fatalf("frontmatter leaked into the body: %q", d.Body)
	}
	if !strings.Contains(d.Body, "Re-read files") {
		t.Fatalf("body lost its content: %q", d.Body)
	}
}

func TestLoadFileWithoutFrontmatterIsStillValid(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "plain.md", "# Just a body\n")
	d, err := LoadFile(p)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !strings.Contains(d.Body, "Just a body") {
		t.Fatalf("body = %q", d.Body)
	}
	if len(d.Match.Family) != 0 || len(d.Match.IDGlob) != 0 {
		t.Fatal("a file with no frontmatter claimed match rules")
	}
}

func TestSelectRuleOrder(t *testing.T) {
	docs := []Doc{
		{Name: "fallback-doc", Body: "fallback"},
		{Name: "by-family", Match: persona.Match{Family: []string{"qwen"}}, Body: "family"},
		{Name: "by-glob", Match: persona.Match{IDGlob: []string{"qwen3-coder-*"}}, Body: "glob"},
		{Name: "by-cap", Match: persona.Match{Capabilities: []string{"tools"}}, Body: "cap"},
	}

	// explicit wins over everything
	sel, err := Select(docs, "by-family", "qwen", "qwen3-coder-plus", []string{"tools"})
	if err != nil || sel.Doc == nil || sel.Rule != "explicit" || sel.Body != "family" {
		t.Fatalf("explicit: %+v err=%v", sel, err)
	}
	// id_glob beats capability and family
	sel, _ = Select(docs, "", "qwen", "qwen3-coder-plus", []string{"tools"})
	if sel.Rule != "id_glob" || sel.Body != "glob" {
		t.Fatalf("id_glob: %+v", sel)
	}
	// capability beats family when the model carries the capability: a
	// capability doc states what the model IS (the small-model standalone
	// is the canonical case), and that outranks the family default. A
	// model-class override that lost to family would only ever apply to
	// families that have no doc at all.
	sel, _ = Select(docs, "", "qwen", "qwen3-max", []string{"tools"})
	if sel.Rule != "capability" || sel.Body != "cap" {
		t.Fatalf("capability over family: %+v", sel)
	}
	// family when the model does not carry a matching capability
	sel, _ = Select(docs, "", "qwen", "qwen3-max", nil)
	if sel.Rule != "family" || sel.Body != "family" {
		t.Fatalf("family: %+v", sel)
	}
	// fallback last
	sel, _ = Select(docs, "", "llama", "llama-3", nil)
	if sel.Rule != "fallback" || sel.Body != "fallback" {
		t.Fatalf("fallback: %+v", sel)
	}
}

func TestSelectUnknownExplicitNameIsAnError(t *testing.T) {
	docs := []Doc{{Name: "a", Body: "x"}}
	if _, err := Select(docs, "nope", "", "m", nil); err == nil {
		t.Fatal("an explicit name that does not exist was accepted")
	}
}

func TestSelectWithNoDocsSelectsNothing(t *testing.T) {
	sel, err := Select(nil, "", "qwen", "qwen3", nil)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if !sel.Absent || sel.Doc != nil {
		t.Fatalf("no documents should select nothing: %+v", sel)
	}
}

func TestSelectEmptyModelIDSelectsNothing(t *testing.T) {
	docs := []Doc{{Name: "f", Body: "fallback"}}
	sel, _ := Select(docs, "", "", "", nil)
	if !sel.Absent {
		t.Fatalf("an unidentified model selected %+v", sel)
	}
}

func TestComposeJoinsTiersMostSpecificLast(t *testing.T) {
	g := Selected{Tier: TierGlobal, Path: "/g.md", Body: "global rules"}
	m := Selected{Tier: TierModel, Path: "/m.md", Body: "model rules", Rule: "family"}
	p := Selected{Tier: TierProject, Path: "/p.md", Body: "project rules"}

	text, labels := Compose(g, m, p)
	if len(labels) != 3 {
		t.Fatalf("labels = %v", labels)
	}
	gi := strings.Index(text, "global rules")
	mi := strings.Index(text, "model rules")
	pi := strings.Index(text, "project rules")
	if !(gi < mi && mi < pi) {
		t.Fatalf("tiers are not ordered most-specific-last:\n%s", text)
	}
	if !strings.Contains(text, "AGENTS.md (global)") || !strings.Contains(text, "AGENTS.md (model)") {
		t.Fatalf("tiers are unlabelled:\n%s", text)
	}
}

func TestComposeSkipsAbsentTiersEntirely(t *testing.T) {
	text, labels := Compose(
		Selected{Tier: TierGlobal, Absent: true},
		Selected{Tier: TierModel, Path: "/m.md", Body: "only this"},
		Selected{Tier: TierProject, Absent: true},
	)
	if strings.Contains(text, "(global)") || strings.Contains(text, "(project)") {
		t.Fatalf("an absent tier emitted a header:\n%s", text)
	}
	if len(labels) != 1 || labels[0] != "/m.md" {
		t.Fatalf("labels = %v", labels)
	}
}

func TestComposeOfNothingIsEmpty(t *testing.T) {
	text, labels := Compose(Selected{Tier: TierGlobal, Absent: true})
	if text != "" || labels != nil {
		t.Fatalf("text=%q labels=%v", text, labels)
	}
}

func TestSaveDocRoundTrips(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "qwen.md")
	match := persona.Match{Family: []string{"qwen"}, IDGlob: []string{"qwen3-*"}}
	if err := SaveDoc(p, "qwen needs re-reads", match, "# Style\nRe-read before editing."); err != nil {
		t.Fatalf("SaveDoc: %v", err)
	}
	d, err := LoadFile(p)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if d.Note != "qwen needs re-reads" {
		t.Fatalf("note = %q", d.Note)
	}
	if len(d.Match.Family) != 1 || d.Match.Family[0] != "qwen" {
		t.Fatalf("family = %v", d.Match.Family)
	}
	if len(d.Match.IDGlob) != 1 || d.Match.IDGlob[0] != "qwen3-*" {
		t.Fatalf("id_glob = %v", d.Match.IDGlob)
	}
	if !strings.Contains(d.Body, "Re-read before editing.") {
		t.Fatalf("body = %q", d.Body)
	}
}

func TestSaveDocQuotesAwkwardValues(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.md")
	// A note with a colon would otherwise make the YAML parse as a mapping.
	if err := SaveDoc(p, "model: needs care", persona.Match{}, "body"); err != nil {
		t.Fatalf("SaveDoc: %v", err)
	}
	d, err := LoadFile(p)
	if err != nil {
		t.Fatalf("LoadFile after SaveDoc: %v", err)
	}
	if d.Note != "model: needs care" {
		t.Fatalf("note round-trip lost the colon: %q", d.Note)
	}
}

func TestSaveDocIsAtomicAndCreatesDirectories(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "nested", "deep", "m.md")
	if err := SaveDoc(p, "", persona.Match{}, "hello"); err != nil {
		t.Fatalf("SaveDoc: %v", err)
	}
	if b, err := os.ReadFile(p); err != nil || !strings.Contains(string(b), "hello") {
		t.Fatalf("SaveDoc did not write the file: %v", err)
	}
	// No temporary files left behind.
	ents, _ := os.ReadDir(filepath.Dir(p))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".agents-") {
			t.Fatalf("a temp file survived: %s", e.Name())
		}
	}
}

func TestLoadDirSkipsAMalformedFileAndKeepsGoing(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "good.md", "---\nnote: fine\n---\nbody\n")
	write(t, dir, "bad.md", "---\nnote: [unclosed\n---\nbody\n")
	docs, warns, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(docs) != 1 || docs[0].Name != "good" {
		t.Fatalf("the good document was lost: %+v", docs)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "bad.md") {
		t.Fatalf("the bad document was not reported: %v", warns)
	}
}
