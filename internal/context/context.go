// Package context is the project-level memory layer (DESIGN §4): one
// Markdown file, auto-curated but always operator-visible, injected at layer 5.
//
// Trust boundary: memory is context, never policy. Nothing in this file grants
// permissions; tool/profile/network policy lives in config. Sections that read
// authorization/scope are operator-gated: the harness proposes, never writes.
package context

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Sections classes. Authorization & scope are the two the model must read
// first, and the two the harness may never widen on its own.
const (
	ClassAuthorization = "authorization"
	ClassScope         = "scope"
	ClassDomain        = "domain"
	ClassStack         = "stack"
	ClassConvention    = "convention"
	ClassState         = "state"
	ClassLesson        = "lesson"
	ClassOther         = "other"
	// ClassSkill is not a CONTEXT.md section: it is a repeated successful
	// procedure promoted into a workspace skill (WP-3.7). It travels through the
	// same proposal file and the same gate as every other class, but on apply it
	// writes a SKILL.md instead of touching the document. Being a class rather
	// than a side channel is the point: it is auto-accepted only if the operator
	// listed "skill" in context.auto_accept_classes, and it is never written
	// silently.
	ClassSkill = "skill"
)

// Section is one "## " block of CONTEXT.md.
type Section struct {
	Title string // verbatim heading text
	Class string
	Body  string // Markdown, no heading
}

// Document is a parsed CONTEXT.md.
type Document struct {
	Project  string // "# Project: <name>" preamble
	Preamble string // any prose before the first section
	Sections []Section
}

// Hard-gated classes: operator-only, regardless of config.
func HardGated(class string) bool {
	return class == ClassAuthorization || class == ClassScope
}

// DefaultTemplate is the file written by `kintsugi context init`. kind picks
// the Authorization & scope template ("general" when empty or unknown); the
// section carries its own instruction to replace the example with the
// operator's own words.
func DefaultTemplate(project, kind string) string {
	if project == "" {
		project = "project"
	}
	return `# Project: ` + project + `

## Authorization & scope

` + AuthorizationTemplate(kind) + `
## Domain context

- Describe what this project IS, in plain practitioner terms — the honest
  description is the one that gives the model the right frame.

## Stack

- Languages, builds, storage, infra.

## Hard conventions

- Never-commit rules, coding style, paths that must not be touched.

## Current state

- What is in flight, what is broken, what is next. Refreshed, never accumulated.

## Lessons (active)

- Durable lessons, dated, with a source. New lessons are proposed, not invented.
`
}

// Parse reads a CONTEXT.md. Warnings are non-fatal (missing recommended
// sections, malformed headings).
func Parse(data []byte) (*Document, []string) {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	d := &Document{}
	var cur *Section
	var body []string
	var pre []string

	flush := func() {
		if cur != nil {
			cur.Body = strings.TrimSpace(strings.Join(body, "\n"))
			d.Sections = append(d.Sections, *cur)
		}
		cur, body = nil, nil
	}

	for _, line := range lines {
		if strings.HasPrefix(line, "# ") && !strings.HasPrefix(line, "## ") {
			if strings.HasPrefix(strings.TrimSpace(line), "# Project:") {
				d.Project = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "# Project:"))
			} else {
				pre = append(pre, line)
			}
			continue
		}
		if strings.HasPrefix(line, "## ") {
			flush()
			title := strings.TrimSpace(strings.TrimPrefix(line, "## "))
			cur = &Section{Title: title, Class: classify(title)}
			continue
		}
		if cur != nil {
			body = append(body, line)
		} else {
			pre = append(pre, line)
		}
	}
	flush()
	d.Preamble = strings.TrimSpace(strings.Join(pre, "\n"))

	var warnings []string
	if _, ok := d.Find(ClassAuthorization); !ok {
		warnings = append(warnings, "missing section: Authorization & scope (layer 5 loses its load-bearing frame)")
	}
	if _, ok := d.Find(ClassDomain); !ok {
		warnings = append(warnings, "missing section: Domain context")
	}
	return d, warnings
}

func classify(title string) string {
	t := strings.ToLower(title)
	switch {
	case strings.Contains(t, "authorization"):
		return ClassAuthorization
	case strings.Contains(t, "scope"):
		return ClassScope
	case strings.Contains(t, "domain"):
		return ClassDomain
	case strings.Contains(t, "stack"):
		return ClassStack
	case strings.Contains(t, "convention"):
		return ClassConvention
	case strings.Contains(t, "current state"):
		return ClassState
	case strings.Contains(t, "lesson"):
		return ClassLesson
	default:
		return ClassOther
	}
}

// Render emits canonical Markdown: preamble, then sections in stored order.
func (d *Document) Render() string {
	var b strings.Builder
	if d.Project != "" {
		fmt.Fprintf(&b, "# Project: %s\n\n", d.Project)
	}
	if d.Preamble != "" {
		b.WriteString(d.Preamble)
		b.WriteString("\n\n")
	}
	for i, s := range d.Sections {
		fmt.Fprintf(&b, "## %s\n\n", s.Title)
		if s.Body != "" {
			b.WriteString(s.Body)
			b.WriteString("\n")
		}
		if i < len(d.Sections)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// Find returns the first section of a class.
func (d *Document) Find(class string) (*Section, bool) {
	for i := range d.Sections {
		if d.Sections[i].Class == class {
			return &d.Sections[i], true
		}
	}
	return nil, false
}

// Hash returns a short content hash of one section (or "" if absent).
func (d *Document) Hash(class string) string {
	s, ok := d.Find(class)
	if !ok {
		return ""
	}
	sum := sha256.Sum256([]byte(s.Title + "\n" + s.Body))
	return hex.EncodeToString(sum[:4])
}

// SectionsHash is the hash of the whole document as rendered.
func (d *Document) SectionsHash() string {
	sum := sha256.Sum256([]byte(d.Render()))
	return hex.EncodeToString(sum[:4])
}

// Tracking returns per-section hashes, sorted by class, for manifests/debug.
func (d *Document) Tracking() map[string]string {
	out := map[string]string{}
	for _, s := range d.Sections {
		out[s.Class] = d.Hash(s.Class)
	}
	return out
}

func (d *Document) TrackingKeys() []string {
	keys := make([]string, 0, len(d.Sections))
	for _, s := range d.Sections {
		keys = append(keys, s.Class)
	}
	sort.Strings(keys)
	return keys
}

// Why matches a free-text query against section titles and lines, returning
// hits with their provenance (section, line, whether the class is injected).
type WhyHit struct {
	Section string `json:"section"`
	Class   string `json:"class"`
	Line    string `json:"line"`
	LineNo  int    `json:"line_no"`
}

func (d *Document) Why(query string) []WhyHit {
	words := strings.Fields(strings.ToLower(strings.TrimSpace(query)))
	// Ignore filler so "why did it assume the isolated sandbox" still matches.
	var terms []string
	for _, w := range words {
		w = strings.Trim(w, "\"'`.,;:!?()[]{}")
		if len(w) >= 3 && !isFiller(w) {
			terms = append(terms, w)
		}
	}
	if len(terms) == 0 {
		return nil
	}

	matches := func(text string) bool {
		t := strings.ToLower(text)
		for _, term := range terms {
			if !strings.Contains(t, term) {
				return false
			}
		}
		return true
	}

	var hits []WhyHit
	for _, s := range d.Sections {
		if matches(s.Title) {
			hits = append(hits, WhyHit{Section: s.Title, Class: s.Class, Line: "(section heading)", LineNo: 0})
		}
		for i, line := range strings.Split(s.Body, "\n") {
			if matches(line) {
				hits = append(hits, WhyHit{Section: s.Title, Class: s.Class, Line: strings.TrimSpace(line), LineNo: i + 1})
			}
		}
	}
	return hits
}

func isFiller(w string) bool {
	switch w {
	case "why", "did", "does", "the", "and", "for", "that", "this", "with", "assume", "assumed", "what", "when", "how", "from":
		return true
	}
	return false
}

// LoadFile reads and parses a CONTEXT.md.
func LoadFile(path string) (*Document, []string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	d, warns := Parse(b)
	return d, warns, nil
}

// WriteDocument writes the document atomically (temp + rename).
func WriteDocument(path string, d *Document) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(d.Render()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
