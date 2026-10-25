// Package initgen scaffolds a new workspace for the harness (DESIGN §4.2):
// a CONTEXT.md draft, a minimal kintsugi.toml, and a persona suggestion.
//
// The package is deterministic and offline. It reads what the working tree
// already says about itself (manifests, README) and the injected environment
// accessor; it never calls a provider, never runs a model, and never writes.
//
// Trust boundary (DESIGN §4): the "Authorization & scope" section is emitted as
// an operator placeholder by construction. The harness will not invent a
// permission claim on the operator's behalf — there is no code path in this
// package that produces one.
package initgen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Files written into a workspace by Write.
const (
	ContextFile = "CONTEXT.md"
	ConfigFile  = "kintsugi.toml"

	// OperatorPlaceholder marks the one section the harness never writes.
	OperatorPlaceholder = "<!-- operator: complete this section; the harness never writes it -->"

	// DomainFallback is used when the README carries no usable paragraph.
	DomainFallback = "- TODO: describe the project's domain in plain terms."

	// StackFallback is used when no manifest was detected.
	StackFallback = "- TODO: record languages, builds, storage, infra."

	// DefaultModel is used when no provider key is present in the environment.
	DefaultModel = "ollama/qwen3:latest"

	// ModelIDComment flags a catalog placeholder that the operator must fill in.
	ModelIDComment = "fill in the exact model id"

	// DomainBudget is the approximate character budget for the README excerpt.
	DomainBudget = 400
)

// Stack is what detection found in a working tree. Multiple stacks are normal
// (a Go service with a Node tooling directory, say).
type Stack struct {
	Name  string   // "Go + Node", or "unknown"
	Files []string // detected files, relative to the root, in detection order
}

// Draft is a fully rendered, not-yet-written set of workspace files.
type Draft struct {
	ContextMD  string // CONTEXT.md content
	ConfigTOML string // kintsugi.toml content
	Stack      Stack
	Persona    string // suggested persona family inferred from the model id
}

// manifest is one detection rule.
type manifest struct {
	lang string
	file string
}

// Detection order is stable so output is deterministic.
var manifests = []manifest{
	{"Go", "go.mod"},
	{"Node", "package.json"},
	{"Rust", "Cargo.toml"},
	{"Python", "pyproject.toml"},
	{"PHP", "composer.json"},
}

// readmeNames are probed in order; the first that exists is used.
var readmeNames = []string{"README.md", "README.markdown", "README.txt", "README", "readme.md"}

// Detect inspects a working tree and returns the detected stack. It never
// returns an error: an unreadable or missing root simply detects nothing.
func Detect(root string) Stack {
	langs, files := detect(root)
	name := "unknown"
	if len(langs) > 0 {
		name = strings.Join(langs, " + ")
	}
	return Stack{Name: name, Files: files}
}

func detect(root string) (langs, files []string) {
	for _, m := range manifests {
		if exists(filepath.Join(root, m.file)) {
			langs = append(langs, m.lang)
			files = append(files, m.file)
		}
	}
	if name, ok := readmeName(root); ok {
		files = append(files, name)
	}
	return langs, files
}

func exists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func readmeName(root string) (string, bool) {
	for _, n := range readmeNames {
		if exists(filepath.Join(root, n)) {
			return n, true
		}
	}
	return "", false
}

// modelChoice is one provider-key → model mapping, in precedence order.
type modelChoice struct {
	envVar  string
	model   string
	comment string
}

// modelChoices is the key precedence: the first variable present in the
// environment decides the default model.
var modelChoices = []modelChoice{
	{"DEEPSEEK_API_KEY", "deepseek/deepseek-chat", ""},
	{"MOONSHOT_API_KEY", "moonshot/kimi-k2-turbo-preview", ""},
	{"ZHIPU_API_KEY", "zhipu/glm-4.6", ""},
	{"DASHSCOPE_API_KEY", "dashscope/qwen3-coder-plus", ""},
	{"OPENROUTER_API_KEY", "openrouter/qwen/qwen3-coder", ""},
	{"CLINE_PASS_API_KEY", "cline-pass/cline-model-id", ModelIDComment},
	{"OPENCODE_GO_API_KEY", "opencode-go/model-id", ModelIDComment},
	{"COMMANDCODE_API_KEY", "commandcode/model-id", ModelIDComment},
}

// families are the model families the persona pack keys off (DESIGN §8.4).
var families = []string{"deepseek", "kimi", "glm", "qwen", "minimax", "mimo", "llama", "mistral"}

// DraftProject renders the workspace files for root. projectName defaults to
// the directory name. env is injected so tests stay hermetic; a nil env means
// "empty environment" (the offline fallback model).
func DraftProject(root, projectName string, env func(string) string) (Draft, error) {
	if strings.TrimSpace(root) == "" {
		return Draft{}, fmt.Errorf("initgen: workspace path is required")
	}
	if env == nil {
		env = func(string) string { return "" }
	}

	name := strings.TrimSpace(projectName)
	if name == "" {
		abs, err := filepath.Abs(root)
		if err != nil {
			return Draft{}, fmt.Errorf("initgen: resolve %s: %w", root, err)
		}
		name = filepath.Base(abs)
	}
	if name == "" || name == string(filepath.Separator) {
		name = "project"
	}

	st := Detect(root)
	model, comment := ChooseModel(env)

	return Draft{
		ContextMD:  renderContext(name, st, readmeParagraph(root)),
		ConfigTOML: renderConfig(model, comment),
		Stack:      st,
		Persona:    PersonaFor(model),
	}, nil
}

// ChooseModel picks the default model from the first provider key present in
// the environment, plus a fill-in comment when the id is a placeholder.
func ChooseModel(env func(string) string) (model, comment string) {
	if env == nil {
		return DefaultModel, ""
	}
	for _, c := range modelChoices {
		if strings.TrimSpace(env(c.envVar)) != "" {
			return c.model, c.comment
		}
	}
	return DefaultModel, ""
}

// PersonaFor returns the persona family suggested by a model ref, or "" when
// the id names no family the pack knows about.
func PersonaFor(model string) string {
	low := strings.ToLower(model)
	for _, fam := range families {
		if strings.Contains(low, fam) {
			return fam
		}
	}
	return ""
}

func renderContext(name string, st Stack, domain string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Project: %s\n\n", name)

	b.WriteString("## Authorization & scope\n\n")
	b.WriteString(OperatorPlaceholder + "\n")
	b.WriteString("- TODO: state the authorization and scope for this workspace in your own words; the harness never fills this in.\n\n")

	b.WriteString("## Domain context\n\n")
	if strings.TrimSpace(domain) == "" {
		b.WriteString(DomainFallback + "\n\n")
	} else {
		b.WriteString(domain + "\n\n")
	}

	b.WriteString("## Stack\n\n")
	for _, line := range stackBullets(st) {
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")

	b.WriteString("## Hard conventions\n\n")
	b.WriteString("- Do not commit; leave the working tree for operator review.\n\n")

	b.WriteString("## Current state\n\n")
	b.WriteString("- Freshly initialized; nothing recorded yet.\n\n")

	b.WriteString("## Lessons (active)\n\n")
	b.WriteString("- Nothing recorded yet.\n")
	return b.String()
}

// stackBullets renders `- Go (go.mod)` lines; README.md is a doc hint, not a
// language, so it is not listed as a stack entry.
func stackBullets(st Stack) []string {
	var out []string
	for _, f := range st.Files {
		for _, m := range manifests {
			if m.file == f {
				out = append(out, fmt.Sprintf("- %s (%s)", m.lang, m.file))
			}
		}
	}
	if len(out) == 0 {
		out = append(out, StackFallback)
	}
	return out
}

func renderConfig(model, comment string) string {
	var b strings.Builder
	b.WriteString("# generated by `kintsugi init` — review and edit freely.\n")
	b.WriteString("# Secrets are never stored here: providers name the env var (api_key_env) that holds the key.\n\n")
	b.WriteString("[core]\n")
	b.WriteString("mode = \"offline\"\n")
	if comment != "" {
		b.WriteString("# " + comment + "\n")
	}
	fmt.Fprintf(&b, "default_model = %q\n", model)
	return b.String()
}

// readmeParagraph returns the domain bullet from the README, or "" when there
// is no usable paragraph.
func readmeParagraph(root string) string {
	name, ok := readmeName(root)
	if !ok {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		return ""
	}
	para := firstParagraph(string(data))
	if para == "" {
		return ""
	}
	return "- " + truncate(para, DomainBudget)
}

// firstParagraph returns the first block of prose in a Markdown document,
// skipping headings, badges, lists, quotes, fences and rules.
func firstParagraph(md string) string {
	var block []string
	inFence := false

	flush := func() (string, bool) {
		var prose []string
		for _, l := range block {
			if isProseLine(l) {
				prose = append(prose, l)
			}
		}
		if len(prose) == 0 {
			return "", false
		}
		return strings.Join(strings.Fields(strings.Join(prose, " ")), " "), true
	}

	for _, raw := range strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if line == "" {
			if s, ok := flush(); ok {
				return s
			}
			block = nil
			continue
		}
		block = append(block, line)
	}
	if s, ok := flush(); ok {
		return s
	}
	return ""
}

func isProseLine(l string) bool {
	switch {
	case strings.HasPrefix(l, "#"),
		strings.HasPrefix(l, "<"),
		strings.HasPrefix(l, "["),
		strings.HasPrefix(l, "!"),
		strings.HasPrefix(l, "|"),
		strings.HasPrefix(l, ">"),
		strings.HasPrefix(l, "---"),
		strings.HasPrefix(l, "==="),
		strings.HasPrefix(l, "- "),
		strings.HasPrefix(l, "* "),
		strings.HasPrefix(l, "+ "):
		return false
	}
	return true
}

// truncate cuts s to at most max runes on a word boundary.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	cut := string(r[:max])
	if i := strings.LastIndex(cut, " "); i > max/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,;:.") + "…"
}

// Write writes the draft into root. Existing files are never clobbered unless
// force is set; both lists hold paths relative to root.
func Write(root string, d Draft, force bool) (written []string, skipped []string, err error) {
	if strings.TrimSpace(root) == "" {
		return nil, nil, fmt.Errorf("initgen: workspace path is required")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, nil, fmt.Errorf("initgen: %w", err)
	}

	for _, t := range []struct{ name, body string }{
		{ContextFile, d.ContextMD},
		{ConfigFile, d.ConfigTOML},
	} {
		if strings.TrimSpace(t.body) == "" {
			continue
		}
		path := filepath.Join(root, t.name)
		if _, statErr := os.Stat(path); statErr == nil && !force {
			skipped = append(skipped, t.name)
			continue
		}
		if err := writeAtomic(path, []byte(t.body)); err != nil {
			return written, skipped, fmt.Errorf("initgen: %w", err)
		}
		written = append(written, t.name)
	}
	return written, skipped, nil
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
