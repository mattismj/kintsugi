// Package workbench builds operator-facing snapshots of the two files that
// define an AgentForge session: the workspace CONTEXT.md (layer 5) and the
// model-keyed persona pack (layer 2). It is pure data — the TUI renders it and
// the editor remains the operator's tool.
package workbench

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	fctx "kintsugi/internal/context"
	"kintsugi/internal/persona"
	"kintsugi/internal/prompt"
)

// ContextSection is one section as it will be injected at layer 5.
type ContextSection struct {
	Title    string
	Class    string
	Index    int // occurrence of this class, 0-based (duplicate headings are legal)
	Tokens   int
	Gate     string // auto | review | gated
	Injected bool
	Hash     string
}

// ContextSnapshot is everything the context workbench shows without re-reading
// the file. Exists is false when CONTEXT.md is absent.
type ContextSnapshot struct {
	Path      string
	Exists    bool
	Project   string
	Hash      string
	Tokens    int
	Budget    int
	Truncated bool
	Warnings  []string
	Sections  []ContextSection
}

// SnapshotContext summarizes a parsed document. autoClasses mirrors
// context.auto_accept_classes; hard-gated sections are never auto-written.
func SnapshotContext(path string, doc *fctx.Document, warnings []string, budget int, autoClasses []string) ContextSnapshot {
	s := ContextSnapshot{Path: path, Budget: budget, Warnings: append([]string{}, warnings...)}
	if doc == nil {
		return s
	}
	s.Exists = true
	s.Project = doc.Project
	s.Hash = doc.SectionsHash()
	s.Tokens = (len(doc.Render()) + 3) / 4

	auto := map[string]bool{}
	for _, c := range autoClasses {
		auto[c] = true
	}

	// The compiler truncates whole sections at the inject budget. Mark what
	// survives so "injected" on screen matches what the model sees.
	limitBytes := 0
	if budget > 0 {
		limitBytes = budget * 4
	}
	prefix := 0
	if doc.Project != "" {
		prefix += len("# Project: "+doc.Project) + 2
	}
	if doc.Preamble != "" {
		prefix += len(doc.Preamble) + 2
	}
	counts := map[string]int{}
	for _, sec := range doc.Sections {
		chunk := len("## "+sec.Title) + 2 + len(sec.Body) + 2
		injected := true
		if limitBytes > 0 && prefix+chunk > limitBytes {
			injected = false
			s.Truncated = true
		}
		prefix += chunk
		idx := counts[sec.Class]
		counts[sec.Class]++
		s.Sections = append(s.Sections, ContextSection{
			Title:    sec.Title,
			Class:    sec.Class,
			Index:    idx,
			Tokens:   (len(sec.Title) + 2 + len(sec.Body) + 3) / 4,
			Gate:     gateFor(sec.Class, auto),
			Injected: injected,
			Hash:     sectionHash(sec),
		})
	}
	return s
}

func sectionHash(s fctx.Section) string {
	sum := sha256.Sum256([]byte(s.Title + "\n" + s.Body))
	return hex.EncodeToString(sum[:4])
}

func sectionKey(s ContextSection) string {
	return fmt.Sprintf("%s#%d", s.Class, s.Index)
}

func sectionLabel(s ContextSection) string {
	if s.Index == 0 {
		return s.Class
	}
	return fmt.Sprintf("%s[%d]", s.Class, s.Index+1)
}

func gateFor(class string, auto map[string]bool) string {
	if fctx.HardGated(class) {
		return "gated"
	}
	if auto[class] {
		return "auto"
	}
	return "review"
}

// DiffContext returns human-readable section deltas between two snapshots.
func DiffContext(old, new ContextSnapshot) []string {
	newS := contextMap(new)
	var out []string
	seen := map[string]bool{}
	for _, sec := range old.Sections {
		key := sectionKey(sec)
		seen[key] = true
		n, ok := newS[key]
		switch {
		case !ok:
			out = append(out, "- "+sectionLabel(sec)+fmt.Sprintf(" (%d tok)", sec.Tokens))
		case n.Hash != sec.Hash || n.Title != sec.Title:
			if n.Title != sec.Title {
				out = append(out, fmt.Sprintf("~ %s title: %s → %s", sectionLabel(sec), sec.Title, n.Title))
			} else {
				out = append(out, fmt.Sprintf("~ %s (%d → %d tok)", sectionLabel(sec), sec.Tokens, n.Tokens))
			}
		}
	}
	for _, sec := range new.Sections {
		if !seen[sectionKey(sec)] {
			out = append(out, "+ "+sectionLabel(sec)+fmt.Sprintf(" (%d tok)", sec.Tokens))
		}
	}
	if len(out) == 0 {
		out = append(out, "no context section changes")
	}
	return out
}

func contextMap(s ContextSnapshot) map[string]ContextSection {
	out := map[string]ContextSection{}
	for _, sec := range s.Sections {
		out[sectionKey(sec)] = sec
	}
	return out
}

// PersonaCard is one file available in the pack.
type PersonaCard struct {
	Name        string
	Description string
	Path        string
	Effective   bool
	Matched     bool
	Triggers    int
}

// TriggerRow is one flattened trigger from the effective chain.
type TriggerRow struct {
	Persona string
	Term    string
	Instead string
	Class   string
	Added   string
}

// PersonaSnapshot is the effective model-keyed stack and the pack around it.
type PersonaSnapshot struct {
	PackDir      string
	Model        string
	Selected     string
	Rule         string
	Chain        []string
	Tokens       int
	Dropped      bool
	SelectedPath string
	Base         string
	BasePath     string
	Available    []PersonaCard
	Triggers     []TriggerRow
	Lint         []persona.Lint
	Warnings     []string
}

// SnapshotPersonas describes the selection that produced comp. When comp is
// nil the caller can still pass a selection for inspection.
func SnapshotPersonas(pack *persona.Pack, sel *persona.Selection, comp *prompt.Compiled, model string) PersonaSnapshot {
	s := PersonaSnapshot{Model: model}
	if pack != nil {
		s.PackDir = pack.Dir
		s.Base = pack.Fallbak
		if p, ok := pack.Get(pack.Fallbak); ok {
			s.BasePath = p.Path
		}
	}
	if sel != nil {
		s.Selected = sel.Persona.Name
		s.Rule = sel.Rule
		s.SelectedPath = sel.Persona.Path
		s.Warnings = append([]string{}, sel.Warnings...)
		for _, per := range sel.Chain {
			s.Chain = append(s.Chain, per.Name)
			for _, t := range per.Triggers.Known {
				s.Triggers = append(s.Triggers, TriggerRow{
					Persona: per.Name, Term: t.Term, Instead: t.Instead,
					Class: t.Class, Added: t.Added,
				})
			}
		}
		if pack != nil {
			s.Lint = pack.LintSelection(sel)
		}
	}
	if comp != nil {
		for _, l := range comp.Manifest.Layers {
			if l.ID == "persona" {
				s.Tokens = l.Tokens
				s.Dropped = l.Dropped
				break
			}
		}
	}
	if pack != nil {
		effective := map[string]bool{}
		for _, name := range s.Chain {
			effective[name] = true
		}
		for _, name := range pack.Names() {
			per, _ := pack.Get(name)
			if per == nil {
				continue
			}
			s.Available = append(s.Available, PersonaCard{
				Name: per.Name, Description: per.Description, Path: per.Path,
				Effective: effective[name], Matched: name == s.Selected,
				Triggers: len(per.Triggers.Known),
			})
		}
	}
	return s
}

// Summary is the one-line stack label used in headers.
func (s PersonaSnapshot) Summary() string {
	if len(s.Chain) == 0 {
		return "no persona"
	}
	return strings.Join(s.Chain, " → ")
}
