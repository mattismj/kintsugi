// Package skills implements progressive disclosure for workspace skills.
//
// A skill is a Markdown file (`.kintsugi/skills/<name>/SKILL.md` or a
// top-level `.kintsugi/skills/<name>.md`). Only the name and description are
// compiled into the prompt; the body is loaded on demand through the `skill`
// tool. This keeps a large procedure library out of the cached prefix.
package skills

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultDir is workspace-relative.
const DefaultDir = ".kintsugi/skills"

// Skill is one discovered procedure.
type Skill struct {
	Name        string
	Description string
	Path        string // absolute
	Rel         string // workspace-relative, slash-separated
	Body        string // frontmatter stripped
}

// Dir resolves the configured skill directory. Empty config means DefaultDir.
func Dir(workspace, configured string) string {
	p := strings.TrimSpace(configured)
	if p == "" {
		p = DefaultDir
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(workspace, p)
	}
	return p
}

type frontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// Load discovers skills under dir. Malformed files are skipped rather than
// failing the session: a broken skill must never take the prompt down.
func Load(workspace, dir string) []Skill {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	var out []Skill
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !isSkillFile(dir, path, d.Name()) {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		sk, ok := parse(path, string(raw))
		if !ok {
			return nil
		}
		sk.Rel = relPath(workspace, path)
		out = append(out, sk)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	// Duplicate names are resolved deterministically: first in name/path order.
	uniq := out[:0]
	seen := map[string]bool{}
	for _, sk := range out {
		if seen[sk.Name] {
			continue
		}
		seen[sk.Name] = true
		uniq = append(uniq, sk)
	}
	return uniq
}

func isSkillFile(root, path, base string) bool {
	if strings.EqualFold(base, "SKILL.md") {
		return true
	}
	if !strings.HasSuffix(strings.ToLower(base), ".md") {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return !strings.Contains(rel, string(os.PathSeparator))
}

func parse(path, raw string) (Skill, bool) {
	name := ""
	desc := ""
	body := raw
	if strings.HasPrefix(raw, "---") {
		rest := strings.TrimPrefix(raw, "---")
		nl := strings.IndexByte(rest, '\n')
		if nl < 0 {
			return Skill{}, false
		}
		rest = rest[nl+1:]
		end := strings.Index(rest, "\n---")
		if end < 0 {
			return Skill{}, false
		}
		var fm frontmatter
		if err := yaml.Unmarshal([]byte(rest[:end]), &fm); err != nil {
			return Skill{}, false
		}
		name, desc = strings.TrimSpace(fm.Name), strings.TrimSpace(fm.Description)
		body = strings.TrimPrefix(rest[end+len("\n---"):], "\n")
	}
	if name == "" {
		if strings.EqualFold(filepath.Base(path), "SKILL.md") {
			name = filepath.Base(filepath.Dir(path))
		} else {
			name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		}
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return Skill{}, false
	}
	if desc == "" {
		desc = firstLine(body)
	}
	return Skill{
		Name:        name,
		Description: truncate(oneLine(desc), 180),
		Path:        path,
		Body:        body,
	}, true
}

// Index is the compact layer injected into the prompt. Bodies never appear.
func Index(list []Skill) string {
	if len(list) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Skills\n\n")
	b.WriteString("Workspace skills are available through the `skill` tool. Load a skill by name when its description matches the task; do not assume a skill's body.\n")
	for _, sk := range list {
		desc := sk.Description
		if desc == "" {
			desc = "(no description)"
		}
		fmt.Fprintf(&b, "\n- `%s` — %s", sk.Name, desc)
		if sk.Rel != "" {
			fmt.Fprintf(&b, " (`%s`)", sk.Rel)
		}
	}
	return b.String()
}

// Find resolves a skill by exact name, then case-insensitively.
func Find(list []Skill, name string) (Skill, bool) {
	name = strings.TrimSpace(name)
	for _, sk := range list {
		if sk.Name == name {
			return sk, true
		}
	}
	for _, sk := range list {
		if strings.EqualFold(sk.Name, name) {
			return sk, true
		}
	}
	return Skill{}, false
}

func relPath(workspace, path string) string {
	if workspace == "" {
		return filepath.ToSlash(path)
	}
	rel, err := filepath.Rel(workspace, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

func firstLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line = strings.TrimSpace(strings.TrimLeft(line, ">*- ")); line != "" {
			return line
		}
	}
	return ""
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
