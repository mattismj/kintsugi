// Package agentsctx resolves the operator's AGENTS.md context documents per
// model (I-80).
//
// The idea is a layered instruction document that follows the model, not just
// the workspace. Three tiers, most specific last:
//
//	global    ~/.config/kintsugi/AGENTS.md            every model, every project
//	model     <models dir>/<name>.md                    one document per model
//	project   <workspace>/AGENTS.md                     this project
//
// The model tier is the missing piece this package exists for. A 9B local model
// and a 400B cloud model need different instructions for the same project — one
// drifts on long context and needs to re-read, the other does not — and a single
// workspace AGENTS.md cannot say both. The model tier is keyed by frontmatter
// match rules using exactly the same rule order the persona pack uses
// (`explicit` → `id_glob` → `family` → `capability` → `fallback`), so an
// operator learns one matching model, not two.
//
// This package is a file store and a matcher. It never runs a model, never
// writes outside its own directory unless asked to Save, and never reads the
// harness's credentials.
package agentsctx

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"kintsugi/internal/persona"
)

// Tier names. They double as the label each block carries into the prompt, so
// the operator can see in the manifest which document said what.
const (
	TierGlobal  = "global"
	TierModel   = "model"
	TierProject = "project"
)

// Doc is one model-keyed context document.
type Doc struct {
	// Name is the file's base name without the extension.
	Name string
	// Path is where it lives on disk.
	Path string
	// Match selects which models this document applies to.
	Match persona.Match
	// Note is an optional one-line description, shown in the TUI list.
	Note string
	// Body is the document text with the frontmatter removed.
	Body string
}

// Selected is one resolved tier: the document that applied and why.
type Selected struct {
	Tier   string // global | model | project
	Doc    *Doc   // nil for the project tier when it is a bare file
	Path   string
	Body   string
	Rule   string // which match rule selected the model document
	Absent bool   // the tier had nothing to contribute
}

// Dir returns the model-document directory. It sits beside the persona pack's
// global directory so an operator finds all model-keyed context in one place.
func Dir(workspace string) string {
	return filepath.Join(workspace, ".kintsugi", "agents")
}

// GlobalPath is the global AGENTS.md, next to the global config.
func GlobalPath(globalConfigPath string) string {
	return filepath.Join(filepath.Dir(globalConfigPath), "AGENTS.md")
}

// LoadDir reads every model document in dir. A missing directory is not an
// error: it means the operator has no model-tier documents yet, which is the
// default. A malformed file is skipped and reported, never fatal — one bad
// document must not stop a session.
func LoadDir(dir string) ([]Doc, []string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("agents: %w", err)
	}
	var docs []Doc
	var warns []string
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		d, err := LoadFile(p)
		if err != nil {
			warns = append(warns, fmt.Sprintf("%s: %v", e.Name(), err))
			continue
		}
		docs = append(docs, d)
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].Name < docs[j].Name })
	return docs, warns, nil
}

// LoadFile reads one model document: YAML frontmatter plus a Markdown body.
// A file with no frontmatter is still valid and matches nothing by rule; it is
// reachable only by an explicit name, which is the honest reading of a document
// that does not say which models it is for.
func LoadFile(path string) (Doc, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Doc{}, err
	}
	name := strings.TrimSuffix(filepath.Base(path), ".md")
	meta, body := splitFrontmatter(string(b))
	d := Doc{Name: name, Path: path, Body: strings.TrimSpace(body)}
	if strings.TrimSpace(meta) != "" {
		var f struct {
			Note  string        `yaml:"note"`
			Match persona.Match `yaml:"match"`
		}
		if err := yaml.Unmarshal([]byte(meta), &f); err != nil {
			return Doc{}, fmt.Errorf("frontmatter: %w", err)
		}
		d.Note = strings.TrimSpace(f.Note)
		d.Match = f.Match
	}
	return d, nil
}

// splitFrontmatter separates a leading YAML block from the body. It mirrors the
// persona loader: a file with no leading "---" has no frontmatter.
func splitFrontmatter(s string) (meta, body string) {
	t := strings.TrimPrefix(s, "\ufeff")
	if !strings.HasPrefix(t, "---") {
		return "", s
	}
	rest := strings.TrimPrefix(t, "---")
	i := strings.Index(rest, "\n---")
	if i < 0 {
		return "", s
	}
	meta = rest[:i]
	body = strings.TrimPrefix(rest[i+len("\n---"):], "\n")
	return meta, body
}

// Select resolves the model tier for one model. The rule order matches the
// persona pack exactly: an explicit name wins, then an id glob, then a family,
// then a capability, then the document marked as a fallback.
//
// An empty model id or family selects nothing: a document must not apply to a
// model the harness could not identify.
func Select(docs []Doc, explicit, family, modelID string, capabilities []string) (Selected, error) {
	if len(docs) == 0 {
		return Selected{Tier: TierModel, Absent: true}, nil
	}
	// 1. explicit name
	if name := strings.TrimSpace(explicit); name != "" {
		for i := range docs {
			if strings.EqualFold(docs[i].Name, name) {
				return Selected{Tier: TierModel, Doc: &docs[i], Path: docs[i].Path, Body: docs[i].Body, Rule: "explicit"}, nil
			}
		}
		return Selected{}, fmt.Errorf("agents: no model document named %q (have %s)", name, names(docs))
	}
	if strings.TrimSpace(modelID) == "" {
		return Selected{Tier: TierModel, Absent: true}, nil
	}
	// 2. id glob
	for i := range docs {
		for _, g := range docs[i].Match.IDGlob {
			if persona.GlobMatch(g, modelID) {
				return Selected{Tier: TierModel, Doc: &docs[i], Path: docs[i].Path, Body: docs[i].Body, Rule: "id_glob"}, nil
			}
		}
	}
	// 3. capability — before family, mirroring the persona selector: the
	// standalone small-model persona must win over the family persona for a
	// <=14B model, or it would only ever apply to families without a persona.
	for i := range docs {
		for _, want := range docs[i].Match.Capabilities {
			for _, have := range capabilities {
				if strings.EqualFold(want, have) {
					return Selected{Tier: TierModel, Doc: &docs[i], Path: docs[i].Path, Body: docs[i].Body, Rule: "capability"}, nil
				}
			}
		}
	}
	// 4. family
	if fam := strings.TrimSpace(family); fam != "" {
		for i := range docs {
			for _, f := range docs[i].Match.Family {
				if strings.EqualFold(f, fam) {
					return Selected{Tier: TierModel, Doc: &docs[i], Path: docs[i].Path, Body: docs[i].Body, Rule: "family"}, nil
				}
			}
		}
	}
	// 5. fallback: a document with no match rules at all, taken as the default.
	for i := range docs {
		m := docs[i].Match
		if len(m.Family) == 0 && len(m.IDGlob) == 0 && len(m.Capabilities) == 0 {
			return Selected{Tier: TierModel, Doc: &docs[i], Path: docs[i].Path, Body: docs[i].Body, Rule: "fallback"}, nil
		}
	}
	return Selected{Tier: TierModel, Absent: true}, nil
}

func names(docs []Doc) string {
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.Name)
	}
	return strings.Join(out, ", ")
}

// SaveDoc writes one model document with its frontmatter, atomically. An empty
// Match keeps whatever rules the caller passed; the TUI edits the body and the
// rules together.
func SaveDoc(path, note string, match persona.Match, body string) error {
	var b strings.Builder
	b.WriteString("---\n")
	if n := strings.TrimSpace(note); n != "" {
		fmt.Fprintf(&b, "note: %s\n", yamlScalar(n))
	}
	if len(match.Family) > 0 || len(match.IDGlob) > 0 || len(match.Capabilities) > 0 {
		b.WriteString("match:\n")
		if len(match.Family) > 0 {
			fmt.Fprintf(&b, "  family: %s\n", yamlList(match.Family))
		}
		if len(match.IDGlob) > 0 {
			fmt.Fprintf(&b, "  id_glob: %s\n", yamlList(match.IDGlob))
		}
		if len(match.Capabilities) > 0 {
			fmt.Fprintf(&b, "  capabilities: %s\n", yamlList(match.Capabilities))
		}
	}
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(body))
	b.WriteString("\n")

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("agents: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".agents-*.tmp")
	if err != nil {
		return fmt.Errorf("agents: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return fmt.Errorf("agents: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("agents: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("agents: %w", err)
	}
	if err := os.Chmod(name, 0o644); err != nil {
		return fmt.Errorf("agents: %w", err)
	}
	return os.Rename(name, path)
}

// yamlList renders a list inline so the frontmatter stays readable and diffable.
func yamlList(in []string) string {
	parts := make([]string, 0, len(in))
	for _, s := range in {
		parts = append(parts, yamlScalar(s))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// yamlScalar quotes a string when YAML would otherwise misinterpret it.
func yamlScalar(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, ":#[]{},&*!|>'\"%@`\n") || strings.HasPrefix(s, " ") || strings.HasSuffix(s, " ") {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return s
}

// Compose joins the tiers that apply into one text block, most specific last,
// and returns the labels that were used. Empty tiers are skipped entirely: a
// tier with no document must not emit a header in the prompt.
func Compose(sel ...Selected) (text string, labels []string) {
	var parts []string
	for _, s := range sel {
		if s.Absent || strings.TrimSpace(s.Body) == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("# AGENTS.md (%s)\n\n%s", s.Tier, strings.TrimSpace(s.Body)))
		labels = append(labels, s.Path)
	}
	if len(parts) == 0 {
		return "", nil
	}
	return strings.Join(parts, "\n\n"), labels
}
