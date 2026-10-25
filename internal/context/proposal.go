package context

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kintsugi/internal/skills"
)

// Proposal is one curator-suggested change. Proposals are data, diffable and
// reviewable; nothing here is applied by the model.
type Proposal struct {
	Section    string `json:"section"`    // class ("state") or title ("Current state"), or the skill name for class "skill"
	Op         string `json:"op"`         // replace | append
	Content    string `json:"content"`    // Markdown body (no headings)
	Reason     string `json:"reason"`     // why the change is warranted
	Confidence string `json:"confidence"` // low | medium | high
	Class      string `json:"class,omitempty"`
	// Description is the one-line summary a skill needs in its frontmatter
	// (WP-3.7). It is ignored for every other class: a CONTEXT.md section has no
	// frontmatter to put it in.
	Description string `json:"description,omitempty"`
}

// File is one curator run's proposal set.
type File struct {
	Created   string     `json:"created"`
	Session   string     `json:"session"`
	Model     string     `json:"model"`
	Proposals []Proposal `json:"proposals"`
}

func ProposalsDir(workspace string) string {
	return filepath.Join(workspace, ".kintsugi", "proposals")
}

func (f *File) Save(workspace string) (string, error) {
	dir := ProposalsDir(workspace)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if f.Created == "" {
		f.Created = time.Now().UTC().Format(time.RFC3339)
	}
	name := strings.ReplaceAll(f.Created, ":", "") + ".json"
	path := filepath.Join(dir, name)
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return "", err
	}
	return path, os.WriteFile(path, append(b, '\n'), 0o644)
}

func LoadProposalFile(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f File
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("proposal file %s: %w", path, err)
	}
	return &f, nil
}

// LatestProposal finds the most recent proposal file in the workspace.
func LatestProposal(workspace string) (string, error) {
	dir := ProposalsDir(workspace)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var names []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "", fmt.Errorf("no pending proposals in %s", dir)
	}
	// Timestamped names sort lexically.
	max := names[0]
	for _, n := range names[1:] {
		if n > max {
			max = n
		}
	}
	return filepath.Join(dir, max), nil
}

type ApplyMode struct {
	AutoClasses  []string // classes applied without review (e.g. state, convention, lesson)
	IncludeAll   bool     // apply every non-gated proposal
	IncludeGated bool     // operator explicitly approved gated (authorization/scope)
	// SkillsDir is where a class-"skill" proposal is written
	// (<dir>/<name>/SKILL.md). It is required for that class only: a skill
	// proposal with nowhere to go is an error, never a silent no-op.
	SkillsDir string
}

type ApplyResult struct {
	Applied []Proposal
	Pending []Proposal
	Gated   []Proposal
	// Skills records the skill files written by this apply, in proposal order.
	Skills []SkillWrite
}

// SkillWrite is one skill file Apply wrote (WP-3.7).
type SkillWrite struct {
	Name     string // skill name, also the directory name
	Path     string // absolute path of the SKILL.md
	Replaced bool   // a skill of this name already existed
}

// Apply mutates the document according to the mode. Gated sections are only
// ever touched with IncludeGated, which the CLI only sets on an explicit
// operator flag.
//
// It is also the one gate every proposal class passes through: a "skill"
// proposal is auto-written only when "skill" is listed in AutoClasses, and
// needs IncludeAll or an explicit class otherwise. There is no path that turns a
// curator's suggestion into a workspace file without passing this function.
func Apply(d *Document, props []Proposal, mode ApplyMode) (ApplyResult, error) {
	auto := map[string]bool{}
	for _, c := range mode.AutoClasses {
		auto[c] = true
	}

	var res ApplyResult
	for _, p := range props {
		class := resolveClass(d, p.Section)
		if p.Class != "" {
			class = p.Class
		}

		if HardGated(class) || hardGatedListed(class) {
			if !mode.IncludeGated {
				res.Gated = append(res.Gated, p)
				continue
			}
		} else if !auto[class] && !mode.IncludeAll {
			res.Pending = append(res.Pending, p)
			continue
		}

		if class == ClassSkill {
			if strings.TrimSpace(mode.SkillsDir) == "" {
				// No destination is configured for this apply, so the skill cannot
				// be written. It stays pending rather than becoming a silent
				// no-op or a half-applied proposal set; every caller with a
				// workspace (App.Curate, `kintsugi context apply`) sets SkillsDir.
				res.Pending = append(res.Pending, p)
				continue
			}
			w, err := applySkill(mode, p)
			if err != nil {
				return res, err
			}
			res.Skills = append(res.Skills, w)
			res.Applied = append(res.Applied, p)
			continue
		}
		if err := applyOne(d, class, p); err != nil {
			return res, err
		}
		res.Applied = append(res.Applied, p)
	}
	return res, nil
}

// applySkill writes one proposal as a workspace skill. The proposal is already
// past the gate when this runs; the checks here are about the file it would
// write, not about permission.
func applySkill(mode ApplyMode, p Proposal) (SkillWrite, error) {
	dir := strings.TrimSpace(mode.SkillsDir)
	if dir == "" {
		return SkillWrite{}, fmt.Errorf("skill proposal %q: no skills directory configured for this apply", p.Section)
	}
	name := strings.TrimSpace(p.Section)
	if err := skills.ValidateName(name); err != nil {
		return SkillWrite{}, err
	}
	body := strings.TrimSpace(p.Content)
	switch p.Op {
	case "append":
		// A promoted procedure replaces the file it names, so "append" would
		// duplicate a procedure every time the curator ran. It is accepted as
		// an alias for the same write rather than failing the whole apply.
	case "replace", "":
	default:
		return SkillWrite{}, fmt.Errorf("skill proposal %q: op must be replace|append, got %q", name, p.Op)
	}
	if body == "" {
		return SkillWrite{}, fmt.Errorf("skill proposal %q: empty procedure", name)
	}
	replaced := skills.Exists(dir, name)
	path, err := skills.Write(dir, name, p.Description, body)
	if err != nil {
		return SkillWrite{}, err
	}
	return SkillWrite{Name: name, Path: path, Replaced: replaced}, nil
}

func hardGatedListed(class string) bool {
	// Config-level gating is handled by the caller's auto list: anything not in
	// auto and not IncludeAll lands in Pending, so no extra table is needed.
	return false
}

func resolveClass(d *Document, name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return ClassOther
	}
	if n == ClassAuthorization || n == ClassScope || n == ClassDomain || n == ClassStack ||
		n == ClassConvention || n == ClassState || n == ClassLesson || n == ClassOther ||
		n == ClassSkill {
		return n
	}
	for _, s := range d.Sections {
		if strings.EqualFold(s.Title, strings.TrimSpace(name)) || strings.Contains(strings.ToLower(s.Title), n) {
			return s.Class
		}
	}
	return classify(n)
}

func applyOne(d *Document, class string, p Proposal) error {
	if strings.Contains(p.Content, "\n## ") || strings.HasPrefix(strings.TrimSpace(p.Content), "## ") {
		return fmt.Errorf("proposal for %q contains a section heading — proposals may only touch one section", p.Section)
	}
	title := titleFor(class)
	if p.Op == "append" {
		if s, ok := d.Find(class); ok {
			body := strings.TrimRight(s.Body, "\n")
			content := strings.TrimSpace(p.Content)
			if body == "" {
				s.Body = content
			} else {
				s.Body = body + "\n" + content
			}
			return nil
		}
		d.Sections = append(d.Sections, Section{Title: title, Class: class, Body: strings.TrimSpace(p.Content)})
		return nil
	}
	// replace (default)
	if s, ok := d.Find(class); ok {
		s.Body = strings.TrimSpace(p.Content)
		return nil
	}
	d.Sections = append(d.Sections, Section{Title: title, Class: class, Body: strings.TrimSpace(p.Content)})
	return nil
}

func titleFor(class string) string {
	switch class {
	case ClassAuthorization:
		return "Authorization & scope"
	case ClassDomain:
		return "Domain context"
	case ClassStack:
		return "Stack"
	case ClassConvention:
		return "Hard conventions"
	case ClassState:
		return "Current state"
	case ClassLesson:
		return "Lessons (active)"
	default:
		return "Notes"
	}
}

// Diff renders a proposal set as reviewable text.
func Diff(d *Document, props []Proposal, mode ApplyMode) string {
	var b strings.Builder
	auto := map[string]bool{}
	for _, c := range mode.AutoClasses {
		auto[c] = true
	}
	for i, p := range props {
		class := resolveClass(d, p.Section)
		if p.Class != "" {
			class = p.Class
		}
		status := "would require --all"
		switch {
		case HardGated(class) || class == ClassAuthorization || class == ClassScope:
			status = "GATED — operator edit required"
		case auto[class]:
			status = "auto-accepted"
		}
		fmt.Fprintf(&b, "[%d] %s → %s (%s) · %s\n", i+1, p.Section, p.Op, status, p.Confidence)
		if p.Reason != "" {
			fmt.Fprintf(&b, "    reason: %s\n", p.Reason)
		}
		if class == ClassSkill {
			// A skill proposal's destination is the point of reviewing it: the
			// operator needs to see the file it would create.
			target := filepath.Join(strings.TrimSpace(mode.SkillsDir), strings.TrimSpace(p.Section), "SKILL.md")
			fmt.Fprintf(&b, "    writes: %s\n", target)
			if p.Description != "" {
				fmt.Fprintf(&b, "    description: %s\n", strings.Join(strings.Fields(p.Description), " "))
			}
		}
		for _, line := range strings.Split(strings.TrimSpace(p.Content), "\n") {
			fmt.Fprintf(&b, "    + %s\n", line)
		}
		b.WriteString("\n")
	}
	return b.String()
}
