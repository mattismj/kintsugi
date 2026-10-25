// Package persona loads and resolves the model-keyed AGENT.md pack (DESIGN
// §3.2/§3.3). Files are Markdown with YAML frontmatter; selection is
// first-match-wins across the configured chain, then inheritance is
// flattened base-first so the compiled layer is deterministic.
package persona

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type Persona struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Match       Match    `yaml:"match"`
	Inherits    []string `yaml:"inherits"`
	Standalone  bool     `yaml:"standalone"` // deliberate: no shared doctrine inherited
	ModelNotes  string   `yaml:"model_notes"`
	Triggers    Triggers `yaml:"triggers"`
	Body        string   `yaml:"-"`
	Path        string   `yaml:"-"`
}

type Match struct {
	Family       []string `yaml:"family"`
	IDGlob       []string `yaml:"id_glob"`
	Capabilities []string `yaml:"capabilities"`
}

type Triggers struct {
	Policy string    `yaml:"policy"`
	Known  []Trigger `yaml:"known"`
}

type Trigger struct {
	Term    string `yaml:"term"`
	Instead string `yaml:"instead"`
	Class   string `yaml:"class"`
	Added   string `yaml:"added"`
}

// Pack is an in-memory index of a pack directory.
type Pack struct {
	Dir     string
	byName  map[string]*Persona
	names   []string
	Fallbak string // fallback persona name, without .md
}

func Load(dir, fallback string) (*Pack, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("persona pack %s: %w", dir, err)
	}
	p := &Pack{Dir: dir, byName: map[string]*Persona{}, Fallbak: strings.TrimSuffix(fallback, ".md")}

	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		full := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(full)
		if err != nil {
			return nil, err
		}
		per, err := Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		per.Path = full
		if per.Name == "" {
			per.Name = strings.TrimSuffix(e.Name(), ".md")
		}
		if strings.Contains(per.Name, " ") {
			return nil, fmt.Errorf("%s: persona name must not contain spaces (%q)", e.Name(), per.Name)
		}
		if _, dup := p.byName[per.Name]; dup {
			return nil, fmt.Errorf("duplicate persona name %q", per.Name)
		}
		if err := per.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		p.byName[per.Name] = per
		p.names = append(p.names, per.Name)
	}
	sort.Strings(p.names)
	return p, nil
}

// LoadFS loads a pack from an fs.FS (used for the embedded default pack).
// dir is a slash-separated path inside fsys, e.g. "agents/global".
func LoadFS(fsys fs.FS, dir, fallback string) (*Pack, error) {
	ents, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("persona pack %s: %w", dir, err)
	}
	p := &Pack{Dir: dir, byName: map[string]*Persona{}, Fallbak: strings.TrimSuffix(fallback, ".md")}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		full := path.Join(dir, e.Name())
		raw, err := fs.ReadFile(fsys, full)
		if err != nil {
			return nil, err
		}
		per, err := Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		per.Path = full
		if per.Name == "" {
			per.Name = strings.TrimSuffix(e.Name(), ".md")
		}
		if strings.Contains(per.Name, " ") {
			return nil, fmt.Errorf("%s: persona name must not contain spaces (%q)", e.Name(), per.Name)
		}
		if _, dup := p.byName[per.Name]; dup {
			return nil, fmt.Errorf("duplicate persona name %q", per.Name)
		}
		if err := per.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		p.byName[per.Name] = per
		p.names = append(p.names, per.Name)
	}
	sort.Strings(p.names)
	return p, nil
}

// Parse splits YAML frontmatter from the Markdown body.
func Parse(raw []byte) (*Persona, error) {
	if !bytes.HasPrefix(raw, []byte("---")) {
		return &Persona{Body: string(raw)}, nil
	}
	rest := bytes.TrimPrefix(raw, []byte("---"))
	nl := bytes.IndexByte(rest, '\n')
	if nl < 0 {
		return nil, fmt.Errorf("unterminated frontmatter")
	}
	rest = rest[nl+1:]
	end := bytes.Index(rest, []byte("\n---"))
	if end < 0 {
		return nil, fmt.Errorf("missing closing frontmatter delimiter")
	}
	fm := rest[:end]
	body := rest[end+len("\n---"):]
	body = bytes.TrimPrefix(body, []byte("\n"))

	var per Persona
	if err := yaml.Unmarshal(fm, &per); err != nil {
		return nil, fmt.Errorf("frontmatter: %w", err)
	}
	per.Body = string(body)
	return &per, nil
}

type Selection struct {
	Persona  *Persona   // the matched persona (not the base)
	Chain    []*Persona // base-first resolution order
	Rule     string     // which rule matched: explicit|id_glob|family|capability|fallback
	Warnings []string
}

// Select applies the configured rule order. modelID, family and capabilities
// describe the model being compiled for.
func (p *Pack) Select(explicit, family, modelID string, capabilities []string) (*Selection, error) {
	// Capability precedes family on purpose: the standalone small persona
	// exists so a <=14B model does not receive the full inherited doctrine —
	// an overloaded prompt makes small tool-capable models answer in prose
	// instead of calling tools. If family ranked first, every small qwen or
	// deepseek model would match its family persona before the capability
	// rule could fire, and the standalone persona would only ever apply to
	// small models of families that have no persona at all. Large models are
	// unaffected: they do not carry the "small" capability, so the capability
	// step never matches for them and family decides as before.
	order := []string{"explicit", "id_glob", "capability", "family", "fallback"}

	var matched *Persona
	var rule string

	for _, r := range order {
		switch r {
		case "explicit":
			if explicit == "" {
				continue
			}
			if per, ok := p.byName[explicit]; ok {
				matched, rule = per, "explicit"
			} else {
				return nil, fmt.Errorf("persona %q not found in %s", explicit, p.Dir)
			}
		case "id_glob":
			for _, name := range p.names {
				per := p.byName[name]
				for _, g := range per.Match.IDGlob {
					if globMatch(g, modelID) {
						matched, rule = per, "id_glob"
						break
					}
				}
				if matched != nil {
					break
				}
			}
		case "family":
			for _, name := range p.names {
				per := p.byName[name]
				for _, f := range per.Match.Family {
					if f != "" && f == family {
						matched, rule = per, "family"
						break
					}
				}
				if matched != nil {
					break
				}
			}
		case "capability":
			for _, name := range p.names {
				per := p.byName[name]
				if len(per.Match.Capabilities) == 0 {
					continue
				}
				if subset(per.Match.Capabilities, capabilities) {
					matched, rule = per, "capability"
					break
				}
			}
		case "fallback":
			if matched == nil {
				per, ok := p.byName[p.Fallbak]
				if !ok {
					return nil, fmt.Errorf("no persona matched and fallback %q missing from %s", p.Fallbak, p.Dir)
				}
				matched, rule = per, "fallback"
			}
		}
		if matched != nil {
			break
		}
	}

	if matched == nil {
		return nil, fmt.Errorf("no persona matched model %q", modelID)
	}

	sel := &Selection{Persona: matched, Rule: rule}
	seen := map[string]bool{}
	var chain []*Persona
	var walk func(pm *Persona, depth int) error
	walk = func(pm *Persona, depth int) error {
		if depth > 8 {
			return fmt.Errorf("inheritance deeper than 8 at %q", pm.Name)
		}
		if seen[pm.Name] {
			return nil
		}
		seen[pm.Name] = true
		for _, in := range pm.Inherits {
			base, ok := p.byName[in]
			if !ok {
				return fmt.Errorf("persona %q inherits missing %q", pm.Name, in)
			}
			if err := walk(base, depth+1); err != nil {
				return err
			}
		}
		chain = append(chain, pm)
		return nil
	}
	if err := walk(matched, 0); err != nil {
		return nil, err
	}
	sel.Chain = chain

	if matched.Name != p.Fallbak && !matched.Standalone && !contains(chain, p.Fallbak) {
		sel.Warnings = append(sel.Warnings,
			fmt.Sprintf("persona %q does not inherit %q — shared doctrine will not be compiled in", matched.Name, p.Fallbak))
	}
	return sel, nil
}

// Section is a portion of the persona layer that can be dropped by budget
// policy without breaking the rest.
type Section struct {
	Kind string // core | notes | triggers
	Text string
}

// Sections materializes the resolved chain. Core (bodies) is never dropped;
// notes and triggers are optional and droppable.
func (s *Selection) Sections() []Section {
	var core, notes, triggers strings.Builder
	for _, per := range s.Chain {
		if b := strings.TrimSpace(per.Body); b != "" {
			core.WriteString(b)
			core.WriteString("\n\n")
		}
		if n := strings.TrimSpace(per.ModelNotes); n != "" {
			fmt.Fprintf(&notes, "## %s — model notes\n\n%s\n\n", per.Name, n)
		}
		if len(per.Triggers.Known) > 0 {
			fmt.Fprintf(&triggers, "## Trigger map for %s\n\n", per.Name)
			fmt.Fprintf(&triggers, "Known filter-triggering phrasings for this model, and the honest task-parameter phrasing that keeps the work identical:\n\n")
			for _, t := range per.Triggers.Known {
				fmt.Fprintf(&triggers, "- `%s` → %s\n", t.Term, t.Instead)
			}
			triggers.WriteString("\n")
		}
	}

	out := []Section{{Kind: "core", Text: strings.TrimRight(core.String(), "\n")}}
	if notes.Len() > 0 {
		out = append(out, Section{Kind: "notes", Text: strings.TrimRight(notes.String(), "\n")})
	}
	if triggers.Len() > 0 {
		out = append(out, Section{Kind: "triggers", Text: strings.TrimRight(triggers.String(), "\n")})
	}
	return out
}

// GlobMatch reports whether s matches a glob pattern with '*' and '?'. It is
// exported so other model-keyed lookups (the per-model AGENTS.md context docs)
// use the same matching rule the persona pack does — two subtly different
// globbers in one codebase would be a bug waiting to happen.
func GlobMatch(pattern, s string) bool { return globMatch(pattern, s) }

func globMatch(pattern, s string) bool {
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(strings.ToLower(s), strings.ToLower(strings.TrimSuffix(pattern, "*")))
	}
	return strings.EqualFold(pattern, s)
}

func subset(need, have []string) bool {
	set := map[string]bool{}
	for _, h := range have {
		set[h] = true
	}
	for _, n := range need {
		if !set[n] {
			return false
		}
	}
	return true
}

func contains(ps []*Persona, name string) bool {
	for _, p := range ps {
		if p.Name == name {
			return true
		}
	}
	return false
}

// Validate enforces trigger-map invariants. The important one: a rephrase that
// still contains the trigger term is self-defeating — the filter fires on the
// term, wherever it appears.
func (p *Persona) Validate() error {
	for i, t := range p.Triggers.Known {
		term := strings.TrimSpace(t.Term)
		instead := strings.TrimSpace(t.Instead)
		if term == "" {
			return fmt.Errorf("triggers.known[%d]: empty term", i)
		}
		if instead == "" {
			return fmt.Errorf("triggers.known[%d]: empty rephrase for %q", i, term)
		}
		if strings.Contains(strings.ToLower(instead), strings.ToLower(term)) {
			return fmt.Errorf(
				"triggers.known[%d]: rephrase for %q still contains the trigger term — self-defeating; describe the same work without the term",
				i, term)
		}
	}
	return nil
}

// Names lists persona names (sorted).
func (p *Pack) Names() []string { return p.names }

// Get returns a persona by name.
func (p *Pack) Get(name string) (*Persona, bool) {
	per, ok := p.byName[name]
	return per, ok
}
