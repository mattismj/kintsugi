package rules

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seed(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	files := map[string]string{
		"AGENTS.md":                       "# House rules\n\n- never commit secrets\n",
		".clinerules/style.md":            "Use tabs.\n",
		".clinerules/nested/more.md":      "Prefer small diffs.\n",
		".cursor/rules/x.mdc":             "---\nalwaysApply: true\n---\nCite sources.\n",
		".codex/AGENTS.md":                "Codex rules.\n",
		".github/copilot-instructions.md": "Copilot rules.\n",
	}
	for name, body := range files {
		p := filepath.Join(ws, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return ws
}

func TestScanFindsSources(t *testing.T) {
	ws := seed(t)
	rep, err := Scan(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Sources) != 6 {
		t.Fatalf("sources = %d: %+v", len(rep.Sources), rep.Sources)
	}
	kinds := map[string]int{}
	for _, s := range rep.Sources {
		kinds[s.Kind]++
		if s.Bytes == 0 || len(s.SHA256) != 64 {
			t.Fatalf("bad source: %+v", s)
		}
	}
	if kinds["agents-md"] != 1 || kinds["cline"] != 2 || kinds["cursor"] != 1 || kinds["codex"] != 1 || kinds["copilot"] != 1 {
		t.Fatalf("kinds = %+v", kinds)
	}
	if rep.Applied {
		t.Fatal("scan applied changes")
	}
	if _, err := os.Stat(filepath.Join(ws, OutputRel)); !os.IsNotExist(err) {
		t.Fatal("scan wrote the inherited file")
	}
}

func TestImportApplyAndDedup(t *testing.T) {
	ws := seed(t)
	// A duplicate source must be reported and not repeated in the output.
	if err := os.WriteFile(filepath.Join(ws, ".windsurfrules"), []byte("Use tabs.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := Import(ws, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Applied {
		t.Fatal("import without apply wrote")
	}
	if _, err := os.Stat(filepath.Join(ws, OutputRel)); !os.IsNotExist(err) {
		t.Fatal("import without apply created output")
	}

	rep, err = Import(ws, true)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Applied || len(rep.Warnings) == 0 || !strings.Contains(strings.Join(rep.Warnings, "\n"), "duplicate content") {
		t.Fatalf("applied=%v warnings=%v", rep.Applied, rep.Warnings)
	}
	body, err := os.ReadFile(filepath.Join(ws, OutputRel))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Inherited repository rules", "never commit secrets", "Prefer small diffs", "Cite sources"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("inherited output missing %q:\n%s", want, body)
		}
	}
	if strings.Count(string(body), "Use tabs.") != 1 {
		t.Fatalf("duplicate content repeated:\n%s", body)
	}

	text, path := LoadInherited(ws)
	if !strings.Contains(text, "never commit secrets") || !strings.HasSuffix(path, OutputRel) {
		t.Fatalf("load = %q %q", text, path)
	}

	raw, err := os.ReadFile(filepath.Join(ws, ManifestRel))
	if err != nil {
		t.Fatal(err)
	}
	var manifest Report
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if !manifest.Applied || len(manifest.Sources) != 6 {
		t.Fatalf("manifest = %+v", manifest)
	}
}

func TestScanEmptyWorkspace(t *testing.T) {
	rep, err := Scan(t.TempDir())
	if err != nil || len(rep.Sources) != 0 {
		t.Fatalf("empty scan = %+v, %v", rep, err)
	}
	text, path := LoadInherited(t.TempDir())
	if text != "" || path != "" {
		t.Fatalf("load with no import = %q %q", text, path)
	}
}
