package persona

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func packWith(t *testing.T, files map[string]string) *Pack {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p, err := Load(dir, "_base")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

const base = `---
name: _base
---
BASE
`

func TestSelectionOrderExplicitWins(t *testing.T) {
	p := packWith(t, map[string]string{
		"_base.md":   base,
		"qwen.md":    "---\nname: qwen\nmatch:\n  family: [qwen]\n---\nQWEN\n",
		"special.md": "---\nname: special\n---\nSPECIAL\n",
	})
	sel, err := p.Select("special", "qwen", "qwen3-coder", []string{"tools"})
	if err != nil {
		t.Fatal(err)
	}
	if sel.Persona.Name != "special" || sel.Rule != "explicit" {
		t.Fatalf("got %s via %s", sel.Persona.Name, sel.Rule)
	}
}

func TestSelectionIDGlobBeforeFamily(t *testing.T) {
	p := packWith(t, map[string]string{
		"_base.md":     base,
		"qwen.md":      "---\nname: qwen\nmatch:\n  family: [qwen]\n---\nQWEN\n",
		"coder-glm.md": "---\nname: coder-glm\nmatch:\n  id_glob: [\"glm-4*\"]\n---\nGLMCODER\n",
	})
	sel, err := p.Select("", "qwen", "glm-4.6", []string{"tools"})
	if err != nil {
		t.Fatal(err)
	}
	if sel.Persona.Name != "coder-glm" || sel.Rule != "id_glob" {
		t.Fatalf("got %s via %s", sel.Persona.Name, sel.Rule)
	}
}

func TestSelectionFamilyAndFallback(t *testing.T) {
	p := packWith(t, map[string]string{
		"_base.md": base,
		"qwen.md":  "---\nname: qwen\nmatch:\n  family: [qwen]\n---\nQWEN\n",
	})
	if sel, _ := p.Select("", "qwen", "x", nil); sel.Persona.Name != "qwen" {
		t.Fatalf("family match failed: %s", sel.Persona.Name)
	}
	sel, _ := p.Select("", "unknownfam", "x", nil)
	if sel.Persona.Name != "_base" || sel.Rule != "fallback" {
		t.Fatalf("fallback failed: %s via %s", sel.Persona.Name, sel.Rule)
	}
}

func TestCapabilityMatch(t *testing.T) {
	p := packWith(t, map[string]string{
		"_base.md": base,
		"small.md": "---\nname: small\nmatch:\n  capabilities: [no_tools]\n---\nSMALL\n",
	})
	sel, err := p.Select("", "nofam", "x", []string{"no_tools", "tools"})
	if err != nil {
		t.Fatal(err)
	}
	if sel.Persona.Name != "small" || sel.Rule != "capability" {
		t.Fatalf("got %s via %s", sel.Persona.Name, sel.Rule)
	}
}

func TestInheritanceChainIsBaseFirst(t *testing.T) {
	p := packWith(t, map[string]string{
		"_base.md": base,
		"mid.md":   "---\nname: mid\ninherits: [_base]\n---\nMID\n",
		"leaf.md":  "---\nname: leaf\ninherits: [mid]\n---\nLEAF\n",
	})
	sel, err := p.Select("leaf", "", "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, per := range sel.Chain {
		got = append(got, per.Name)
	}
	if strings.Join(got, ",") != "_base,mid,leaf" {
		t.Fatalf("chain order wrong: %v", got)
	}
}

func TestMissingBaseInheritanceWarns(t *testing.T) {
	p := packWith(t, map[string]string{
		"_base.md":  base,
		"orphan.md": "---\nname: orphan\nmatch:\n  family: [orphan]\n---\nORPHAN\n",
	})
	sel, err := p.Select("orphan", "", "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Warnings) == 0 {
		t.Fatal("expected a warning for a persona that does not inherit the base")
	}
}

func TestMalformedFrontmatterFails(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "broken.md"), []byte("---\nname: [unclosed\n---\nbody"), 0o644)
	if _, err := Load(dir, "_base"); err == nil {
		t.Fatal("expected frontmatter parse error")
	}
}

func TestSelfDefeatingTriggerRejected(t *testing.T) {
	p := "---\nname: bad\ninherits: [_base]\ntriggers:\n  known:\n    - term: \"payload\"\n      instead: \"the payload artifact\"\n---\nBAD\n"
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "_base.md"), []byte(base), 0o644)
	os.WriteFile(filepath.Join(dir, "bad.md"), []byte(p), 0o644)
	if _, err := Load(dir, "_base"); err == nil {
		t.Fatal("expected a self-defeating trigger map to be rejected")
	}
}

func TestTriggerMapAcceptsCleanRephrase(t *testing.T) {
	p := "---\nname: good\ninherits: [_base]\ntriggers:\n  known:\n    - term: \"payload\"\n      instead: \"the compiled artifact written to artifacts/\"\n---\nGOOD\n"
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "_base.md"), []byte(base), 0o644)
	os.WriteFile(filepath.Join(dir, "good.md"), []byte(p), 0o644)
	if _, err := Load(dir, "_base"); err != nil {
		t.Fatalf("clean rephrase rejected: %v", err)
	}
}

func TestStandaloneSuppressesMissingBaseWarning(t *testing.T) {
	p := packWith(t, map[string]string{
		"_base.md": base,
		"solo.md":  "---\nname: solo\nstandalone: true\nmatch:\n  family: [solo]\n---\nSOLO\n",
	})
	sel, err := p.Select("solo", "", "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Warnings) != 0 {
		t.Fatalf("standalone persona warned: %v", sel.Warnings)
	}
}

func TestLintEffectiveStack(t *testing.T) {
	p := packWith(t, map[string]string{
		"_base.md": base,
		"child.md": `---
name: child
inherits: [_base]
triggers:
  known:
    - term: "C2"
      instead: "tasking harness"
      class: moralizing
    - term: "payload"
      instead: "compiled artifact"
---
CHILD
`,
	})
	sel, err := p.Select("child", "", "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	lint := p.LintSelection(sel)
	found := false
	for _, l := range lint {
		if l.Level == "warning" && strings.Contains(l.Message, "no class") {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing-class lint not reported: %#v", lint)
	}

	clean := packWith(t, map[string]string{
		"_base.md": base,
		"child.md": `---
name: child
inherits: [_base]
triggers:
  known:
    - term: "C2"
      instead: "tasking harness"
      class: moralizing
---
CHILD
`,
	})
	sel, err = clean.Select("child", "", "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := clean.LintSelection(sel); len(got) != 0 {
		t.Fatalf("clean stack reported findings: %#v", got)
	}
}
