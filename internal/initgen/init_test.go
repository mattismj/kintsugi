package initgen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	fctx "kintsugi/internal/context"
)

// requiredHeadings are the sections DESIGN §4.2 defines for CONTEXT.md.
// Order matters: Authorization & scope and Domain context come first because
// they are the two a model must read before anything else.
var requiredHeadings = []string{
	"## Authorization & scope",
	"## Domain context",
	"## Stack",
	"## Hard conventions",
	"## Current state",
	"## Lessons (active)",
}

// inventedClaims are authorization assertions the harness must never make up.
var inventedClaims = []string{
	"authorized",
	"dual-use",
	"red-team",
	"licensed practitioner",
	"penetration test",
	"we have permission",
}

// writeRepo creates a golden repo in t.TempDir() from name→content ("" means an
// empty file).
func writeRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func envFrom(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func TestDetect(t *testing.T) {
	readme := "# Title\n\nThis project builds a small lab fixture harness for testing.\n"

	tests := []struct {
		name  string
		files map[string]string
		want  string
		meta  []string
	}{
		{
			name:  "go only",
			files: map[string]string{"go.mod": "module x\n"},
			want:  "Go",
			meta:  []string{"go.mod"},
		},
		{
			name:  "go with readme",
			files: map[string]string{"go.mod": "module x\n", "README.md": readme},
			want:  "Go",
			meta:  []string{"go.mod", "README.md"},
		},
		{
			name:  "node and rust",
			files: map[string]string{"package.json": "{}\n", "Cargo.toml": "[package]\n"},
			want:  "Node + Rust",
			meta:  []string{"package.json", "Cargo.toml"},
		},
		{
			name:  "python and php",
			files: map[string]string{"pyproject.toml": "[project]\n", "composer.json": "{}\n"},
			want:  "Python + PHP",
			meta:  []string{"pyproject.toml", "composer.json"},
		},
		{
			name: "all five",
			files: map[string]string{
				"go.mod": "module x\n", "package.json": "{}\n", "Cargo.toml": "[package]\n",
				"pyproject.toml": "[project]\n", "composer.json": "{}\n", "README.md": readme,
			},
			want: "Go + Node + Rust + Python + PHP",
			meta: []string{"go.mod", "package.json", "Cargo.toml", "pyproject.toml", "composer.json", "README.md"},
		},
		{
			name:  "readme only",
			files: map[string]string{"README.md": readme},
			want:  "unknown",
			meta:  []string{"README.md"},
		},
		{
			name:  "empty dir",
			files: map[string]string{},
			want:  "unknown",
			meta:  nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeRepo(t, tc.files)
			got := Detect(dir)
			if got.Name != tc.want {
				t.Errorf("Name = %q, want %q", got.Name, tc.want)
			}
			if strings.Join(got.Files, ",") != strings.Join(tc.meta, ",") {
				t.Errorf("Files = %v, want %v", got.Files, tc.meta)
			}
		})
	}
}

func TestDetectMissingRoot(t *testing.T) {
	got := Detect(filepath.Join(t.TempDir(), "does-not-exist"))
	if got.Name != "unknown" || len(got.Files) != 0 {
		t.Fatalf("Detect(missing) = %+v, want empty stack", got)
	}
}

func TestDraftProjectSections(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
	}{
		{
			name: "go with readme paragraph",
			files: map[string]string{
				"go.mod": "module x\n",
				"README.md": "# AgentForge\n\n[![build](https://example.invalid/badge.svg)](https://example.invalid)\n\n" +
					"This repository builds an adversary-simulation framework for purple-team\nvalidation against synthetic lab fixtures.\n\n- a list item\n",
			},
		},
		{
			name:  "node without readme",
			files: map[string]string{"package.json": "{}\n"},
		},
		{
			name:  "empty dir",
			files: map[string]string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeRepo(t, tc.files)
			d, err := DraftProject(dir, "demo", envFrom(nil))
			if err != nil {
				t.Fatalf("DraftProject: %v", err)
			}
			if !strings.HasPrefix(d.ContextMD, "# Project: demo\n") {
				t.Errorf("missing `# Project: <name>` preamble:\n%s", d.ContextMD)
			}
			for _, h := range requiredHeadings {
				if !strings.Contains(d.ContextMD, "\n"+h+"\n") {
					t.Errorf("missing heading %q in:\n%s", h, d.ContextMD)
				}
			}
			// Order check: the two load-bearing sections come first.
			auth := strings.Index(d.ContextMD, "## Authorization & scope")
			dom := strings.Index(d.ContextMD, "## Domain context")
			stack := strings.Index(d.ContextMD, "## Stack")
			if !(auth < dom && dom < stack) {
				t.Errorf("section order wrong (auth=%d domain=%d stack=%d)", auth, dom, stack)
			}
			// The document must parse into the classes the compiler expects.
			doc, _ := fctx.Parse([]byte(d.ContextMD))
			for _, class := range []string{
				fctx.ClassAuthorization, fctx.ClassDomain, fctx.ClassStack,
				fctx.ClassConvention, fctx.ClassState, fctx.ClassLesson,
			} {
				if _, ok := doc.Find(class); !ok {
					t.Errorf("section class %q not parseable from draft", class)
				}
			}
			if doc.Project != "demo" {
				t.Errorf("parsed project = %q, want demo", doc.Project)
			}
		})
	}
}

func TestAuthorizationIsOperatorPlaceholderOnly(t *testing.T) {
	dir := writeRepo(t, map[string]string{"go.mod": "module x\n"})
	d, err := DraftProject(dir, "demo", envFrom(nil))
	if err != nil {
		t.Fatalf("DraftProject: %v", err)
	}

	body := sectionBody(d.ContextMD, "## Authorization & scope")
	if !strings.Contains(body, OperatorPlaceholder) {
		t.Fatalf("authorization body lacks the operator placeholder:\n%s", body)
	}
	if !strings.Contains(body, "- TODO:") {
		t.Fatalf("authorization body lacks a TODO bullet:\n%s", body)
	}
	low := strings.ToLower(body)
	for _, claim := range inventedClaims {
		if strings.Contains(low, claim) {
			t.Errorf("authorization body invents a claim (%q):\n%s", claim, body)
		}
	}
	// The body must be small: a placeholder, not a drafted policy.
	if n := len(strings.Fields(body)); n > 60 {
		t.Errorf("authorization body is %d words — too long for a placeholder:\n%s", n, body)
	}
}

func TestDomainFromReadme(t *testing.T) {
	tests := []struct {
		name     string
		readme   string
		want     string
		wantTODO bool
	}{
		{
			name:   "heading then paragraph",
			readme: "# Title\n\nThis project is a fixture harness.\nSecond line.\n\n## Install\n\nfoo\n",
			want:   "- This project is a fixture harness. Second line.",
		},
		{
			name:   "badges and list before prose",
			readme: "# Title\n\n[![ci](x)](y)\n\n- bullet one\n- bullet two\n\nA real description paragraph.\n",
			want:   "- A real description paragraph.",
		},
		{
			name:     "headings only",
			readme:   "# Title\n\n## Usage\n\n## Install\n",
			wantTODO: true,
		},
		{
			name:     "no readme",
			readme:   "",
			wantTODO: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{"go.mod": "module x\n"}
			if tc.readme != "" {
				files["README.md"] = tc.readme
			}
			dir := writeRepo(t, files)
			d, err := DraftProject(dir, "demo", envFrom(nil))
			if err != nil {
				t.Fatalf("DraftProject: %v", err)
			}
			body := strings.TrimSpace(sectionBody(d.ContextMD, "## Domain context"))
			if tc.wantTODO {
				if body != DomainFallback {
					t.Fatalf("domain body = %q, want fallback %q", body, DomainFallback)
				}
				return
			}
			if body != tc.want {
				t.Fatalf("domain body = %q, want %q", body, tc.want)
			}
		})
	}
}

func TestDomainTruncated(t *testing.T) {
	long := strings.TrimSpace(strings.Repeat("word ", 200))
	dir := writeRepo(t, map[string]string{"README.md": long + "\n"})
	d, err := DraftProject(dir, "demo", envFrom(nil))
	if err != nil {
		t.Fatalf("DraftProject: %v", err)
	}
	body := strings.TrimSpace(sectionBody(d.ContextMD, "## Domain context"))
	if !strings.HasSuffix(body, "…") {
		t.Errorf("long domain excerpt not truncated: %q", body)
	}
	if n := len([]rune(body)); n > DomainBudget+8 {
		t.Errorf("domain excerpt is %d runes, budget is %d", n, DomainBudget+8)
	}
}

func TestStackBullets(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{
			name:  "manifests",
			files: map[string]string{"go.mod": "m\n", "package.json": "{}\n", "README.md": "x\n"},
			want:  []string{"- Go (go.mod)", "- Node (package.json)"},
		},
		{
			name:  "fallback",
			files: map[string]string{"README.md": "just docs\n"},
			want:  []string{StackFallback},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeRepo(t, tc.files)
			d, err := DraftProject(dir, "demo", envFrom(nil))
			if err != nil {
				t.Fatalf("DraftProject: %v", err)
			}
			body := sectionBody(d.ContextMD, "## Stack")
			for _, w := range tc.want {
				if !strings.Contains(body, w) {
					t.Errorf("stack body missing %q:\n%s", w, body)
				}
			}
			if strings.Contains(d.ContextMD, "README.md)") {
				t.Errorf("README.md listed as a language stack entry:\n%s", d.ContextMD)
			}
		})
	}
}

func TestModelSelectionOrder(t *testing.T) {
	tests := []struct {
		name        string
		env         map[string]string
		wantModel   string
		wantPersona string
		wantCommit  string
	}{
		{
			name:        "nothing set falls back to local ollama",
			env:         map[string]string{},
			wantModel:   DefaultModel,
			wantPersona: "qwen",
		},
		{
			name:        "deepseek wins over everything after it",
			env:         map[string]string{"DEEPSEEK_API_KEY": "k", "MOONSHOT_API_KEY": "k", "OPENROUTER_API_KEY": "k"},
			wantModel:   "deepseek/deepseek-chat",
			wantPersona: "deepseek",
		},
		{
			name:        "moonshot",
			env:         map[string]string{"MOONSHOT_API_KEY": "k", "ZHIPU_API_KEY": "k"},
			wantModel:   "moonshot/kimi-k2-turbo-preview",
			wantPersona: "kimi",
		},
		{
			name:        "zhipu",
			env:         map[string]string{"ZHIPU_API_KEY": "k", "DASHSCOPE_API_KEY": "k"},
			wantModel:   "zhipu/glm-4.6",
			wantPersona: "glm",
		},
		{
			name:        "dashscope",
			env:         map[string]string{"DASHSCOPE_API_KEY": "k", "OPENROUTER_API_KEY": "k"},
			wantModel:   "dashscope/qwen3-coder-plus",
			wantPersona: "qwen",
		},
		{
			name:        "openrouter",
			env:         map[string]string{"OPENROUTER_API_KEY": "k", "CLINE_PASS_API_KEY": "k"},
			wantModel:   "openrouter/qwen/qwen3-coder",
			wantPersona: "qwen",
		},
		{
			name:        "cline-pass needs a fill-in comment",
			env:         map[string]string{"CLINE_PASS_API_KEY": "k", "COMMANDCODE_API_KEY": "k"},
			wantModel:   "cline-pass/cline-model-id",
			wantCommit:  ModelIDComment,
			wantPersona: "",
		},
		{
			name:       "opencode-go needs a fill-in comment",
			env:        map[string]string{"OPENCODE_GO_API_KEY": "k"},
			wantModel:  "opencode-go/model-id",
			wantCommit: ModelIDComment,
		},
		{
			name:       "commandcode needs a fill-in comment",
			env:        map[string]string{"COMMANDCODE_API_KEY": "k"},
			wantModel:  "commandcode/model-id",
			wantCommit: ModelIDComment,
		},
		{
			name:        "empty value does not count as set",
			env:         map[string]string{"DEEPSEEK_API_KEY": "  "},
			wantModel:   DefaultModel,
			wantPersona: "qwen",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeRepo(t, map[string]string{"go.mod": "module x\n"})
			d, err := DraftProject(dir, "demo", envFrom(tc.env))
			if err != nil {
				t.Fatalf("DraftProject: %v", err)
			}
			if !strings.Contains(d.ConfigTOML, "[core]") {
				t.Fatalf("config missing [core]:\n%s", d.ConfigTOML)
			}
			if !strings.Contains(d.ConfigTOML, `mode = "offline"`) {
				t.Fatalf("config missing offline mode:\n%s", d.ConfigTOML)
			}
			wantLine := `default_model = "` + tc.wantModel + `"`
			if !strings.Contains(d.ConfigTOML, wantLine) {
				t.Fatalf("config missing %q:\n%s", wantLine, d.ConfigTOML)
			}
			if got := PersonaFor(tc.wantModel); got != tc.wantPersona {
				t.Errorf("PersonaFor(%q) = %q, want %q", tc.wantModel, got, tc.wantPersona)
			}
			if d.Persona != tc.wantPersona {
				t.Errorf("Draft.Persona = %q, want %q", d.Persona, tc.wantPersona)
			}
			if tc.wantCommit == "" {
				if strings.Contains(d.ConfigTOML, ModelIDComment) {
					t.Errorf("unexpected fill-in comment for a known model id:\n%s", d.ConfigTOML)
				}
			} else if !strings.Contains(d.ConfigTOML, "# "+tc.wantCommit) {
				t.Errorf("config missing TOML comment %q:\n%s", tc.wantCommit, d.ConfigTOML)
			}
		})
	}
}

func TestDraftIsDeterministic(t *testing.T) {
	dir := writeRepo(t, map[string]string{
		"go.mod":    "module x\n",
		"README.md": "# T\n\nA deterministic fixture project.\n",
	})
	a, err := DraftProject(dir, "demo", envFrom(map[string]string{"ZHIPU_API_KEY": "k"}))
	if err != nil {
		t.Fatalf("DraftProject: %v", err)
	}
	b, err := DraftProject(dir, "demo", envFrom(map[string]string{"ZHIPU_API_KEY": "k"}))
	if err != nil {
		t.Fatalf("DraftProject: %v", err)
	}
	if a.ContextMD != b.ContextMD || a.ConfigTOML != b.ConfigTOML || a.Persona != b.Persona || a.Stack.Name != b.Stack.Name {
		t.Fatal("DraftProject is not deterministic across identical inputs")
	}
}

func TestDraftProjectDefaultName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "my-project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := DraftProject(dir, "", envFrom(nil))
	if err != nil {
		t.Fatalf("DraftProject: %v", err)
	}
	if !strings.HasPrefix(d.ContextMD, "# Project: my-project\n") {
		t.Fatalf("empty projectName did not default to the dir name:\n%s", d.ContextMD[:40])
	}
}

func TestDraftProjectRequiresRoot(t *testing.T) {
	if _, err := DraftProject("", "demo", nil); err == nil {
		t.Fatal("DraftProject(\"\") should error")
	}
}

func TestWriteNoClobber(t *testing.T) {
	dir := writeRepo(t, map[string]string{"go.mod": "module x\n"})
	d, err := DraftProject(dir, "demo", envFrom(nil))
	if err != nil {
		t.Fatalf("DraftProject: %v", err)
	}

	// Fresh workspace: both files land.
	written, skipped, err := Write(dir, d, false)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if strings.Join(written, ",") != ContextFile+","+ConfigFile {
		t.Errorf("written = %v, want [%s %s]", written, ContextFile, ConfigFile)
	}
	if len(skipped) != 0 {
		t.Errorf("skipped = %v, want none", skipped)
	}

	// Second pass without force: nothing is touched.
	before := readFileT(t, filepath.Join(dir, ContextFile))
	written, skipped, err = Write(dir, d, false)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(written) != 0 {
		t.Errorf("second Write written = %v, want none", written)
	}
	if strings.Join(skipped, ",") != ContextFile+","+ConfigFile {
		t.Errorf("second Write skipped = %v, want both files", skipped)
	}
	if after := readFileT(t, filepath.Join(dir, ContextFile)); after != before {
		t.Error("existing CONTEXT.md was modified without --force")
	}

	// --force rewrites both.
	written, skipped, err = Write(dir, d, true)
	if err != nil {
		t.Fatalf("Write(force): %v", err)
	}
	if len(skipped) != 0 {
		t.Errorf("force Write skipped = %v, want none", skipped)
	}
	if strings.Join(written, ",") != ContextFile+","+ConfigFile {
		t.Errorf("force Write written = %v", written)
	}
}

func TestWriteKeepsPreexistingWords(t *testing.T) {
	dir := t.TempDir()
	contextPath := filepath.Join(dir, ContextFile)
	const original = "# Project: hand-written\n\n## Authorization & scope\n\n- operator text\n"
	if err := os.WriteFile(contextPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := DraftProject(dir, "demo", envFrom(nil))
	if err != nil {
		t.Fatalf("DraftProject: %v", err)
	}

	written, skipped, err := Write(dir, d, false)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if strings.Join(skipped, ",") != ContextFile {
		t.Errorf("skipped = %v, want [%s]", skipped, ContextFile)
	}
	if strings.Join(written, ",") != ConfigFile {
		t.Errorf("written = %v, want [%s]", written, ConfigFile)
	}
	if got := readFileT(t, contextPath); got != original {
		t.Errorf("operator CONTEXT.md was overwritten:\n%s", got)
	}
}

func TestWriteRequiresRoot(t *testing.T) {
	if _, _, err := Write("", Draft{ContextMD: "x"}, false); err == nil {
		t.Fatal("Write(\"\") should error")
	}
}

func readFileT(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// sectionBody returns the Markdown body of the "## <heading>" block.
func sectionBody(doc, heading string) string {
	idx := strings.Index(doc, heading)
	if idx < 0 {
		return ""
	}
	rest := doc[idx+len(heading):]
	if next := strings.Index(rest, "\n## "); next >= 0 {
		rest = rest[:next]
	}
	return rest
}
