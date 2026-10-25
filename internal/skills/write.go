package skills

// Writing skills back to disk (WP-3.7). A repeated successful procedure can be
// promoted into a skill through the curator; the curator's proposal is the
// authority, and this file only knows how to render one as a SKILL.md.
//
// Nothing here applies a proposal by itself. The caller (context.Apply, via the
// same gate that guards every other class) decides whether the proposal is
// auto-accepted, pending operator review, or refused.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// NameError reports a skill name that cannot be a directory component.
type NameError struct {
	Name   string
	Reason string
}

func (e *NameError) Error() string {
	return fmt.Sprintf("skill name %q %s", e.Name, e.Reason)
}

// ValidateName checks that a proposed skill name is usable as a single path
// element under the skills directory. A curator writes the name, so it is not
// trusted: a name with a separator, a traversal, or a leading dot would write
// outside the skills directory or hide itself from discovery.
func ValidateName(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return &NameError{Name: name, Reason: "is empty"}
	}
	if trimmed != name {
		return &NameError{Name: name, Reason: "has leading or trailing whitespace"}
	}
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, string(os.PathSeparator)) {
		return &NameError{Name: name, Reason: "contains a path separator"}
	}
	if name == "." || name == ".." || strings.HasPrefix(name, ".") {
		return &NameError{Name: name, Reason: "is a hidden or relative path element"}
	}
	if strings.ContainsAny(name, "\x00\n\r\t") {
		return &NameError{Name: name, Reason: "contains control characters"}
	}
	// The name becomes a directory and is rendered into YAML frontmatter, so a
	// quote or a colon would corrupt the file the next load has to parse.
	if strings.ContainsAny(name, `:"'#`) {
		return &NameError{Name: name, Reason: "contains a character that would break the frontmatter"}
	}
	return nil
}

// Render builds the SKILL.md body for a promoted procedure: the frontmatter the
// loader reads (name, description) and the procedure itself as Markdown.
func Render(name, description, body string) string {
	name = strings.TrimSpace(name)
	description = strings.TrimSpace(description)
	body = strings.TrimSpace(body)

	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("name: " + name + "\n")
	if description != "" {
		// The description is a single YAML line; a newline inside it would end
		// the key early and leave the document unparseable.
		b.WriteString("description: " + strings.Join(strings.Fields(description), " ") + "\n")
	}
	b.WriteString("---\n\n")
	if !strings.HasPrefix(body, "# ") {
		b.WriteString("# " + name + "\n\n")
	}
	b.WriteString(body)
	b.WriteString("\n")
	return b.String()
}

// Write creates (or replaces) <dir>/<name>/SKILL.md and returns the absolute
// path it wrote. It is the only place skill promotion touches the filesystem, so
// the naming rules and the file layout are enforced together.
func Write(dir, name, description, body string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	if strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("skill %q has no procedure body", name)
	}
	if strings.TrimSpace(dir) == "" {
		return "", fmt.Errorf("skill %q has no skills directory", name)
	}
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(path, []byte(Render(name, description, body)), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// Exists reports whether a skill of this name is already on disk under dir, as
// either a directory SKILL.md or a flat <name>.md. Promotion uses it to say
// whether it created a skill or replaced one.
func Exists(dir, name string) bool {
	if strings.TrimSpace(dir) == "" || strings.TrimSpace(name) == "" {
		return false
	}
	if st, err := os.Stat(filepath.Join(dir, name, "SKILL.md")); err == nil && !st.IsDir() {
		return true
	}
	if st, err := os.Stat(filepath.Join(dir, name+".md")); err == nil && !st.IsDir() {
		return true
	}
	return false
}
