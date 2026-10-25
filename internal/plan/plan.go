// Package plan stores the plans the harness writes (M9.2). A plan is a
// Markdown document under <workspace>/.kintsugi/plans: the operator reads
// it, reviews it, and hands it back to the model to implement.
//
// The package is a file store and nothing else — it never runs a model and
// never touches the workspace outside its own directory. One rule governs
// every document it writes: a plan is recognizable. Every plan carries the
// `<!-- kintsugi:plan -->` marker and a level-one heading, so a file that is not
// a plan is simply ignored rather than guessed at.
package plan

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Marker is the comment every plan document carries. It is an HTML comment so
// it survives Markdown rendering without showing up in the text.
const Marker = "<!-- kintsugi:plan -->"

// Dir returns the plan directory of a workspace.
func Dir(workspace string) string {
	return filepath.Join(workspace, ".kintsugi", "plans")
}

// Artifact is one saved plan: its slug, where it lives, what it says and
// when it was written.
type Artifact struct {
	Name    string    // slug, without the .md suffix
	Path    string    // absolute path of the document
	Content string    // the full document
	Created time.Time // when it was written
}

// Slug turns a human title into a file-name-safe slug: lower case, runs of
// non-alphanumerics collapsed into single dashes. It is idempotent, so a slug
// can be slugged again without changing.
func Slug(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = slugSep.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 80 {
		s = strings.Trim(s[:80], "-")
	}
	return s
}

var slugSep = regexp.MustCompile(`[^a-z0-9]+`)

// Document renders the on-disk form of a plan: the marker, a level-one title
// and the body. A body that already carries a level-one heading keeps it —
// the model's own title wins over the caller's.
func Document(name, content string) string {
	body := strings.TrimSpace(content)
	var b strings.Builder
	if !strings.Contains(body, Marker) {
		b.WriteString(Marker + "\n")
	}
	if hasHeading(body) {
		b.WriteString(body + "\n")
		return b.String()
	}
	title := strings.TrimSpace(name)
	if title == "" {
		title = "Plan"
	}
	b.WriteString("# " + title + "\n\n")
	b.WriteString(body + "\n")
	return b.String()
}

// Save writes one plan into the workspace and returns it. The file name is
// the slugified name; the write is atomic (a temporary file in the same
// directory, then a rename), so a plan is never half-written. The plan
// directory is created on demand.
func Save(workspace, name, content string) (Artifact, error) {
	slug := Slug(name)
	if slug == "" {
		return Artifact{}, fmt.Errorf("plan: %q has no usable characters for a file name", name)
	}
	dir := Dir(workspace)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Artifact{}, fmt.Errorf("plan: %w", err)
	}
	body := Document(name, content)
	path := filepath.Join(dir, slug+".md")
	if err := writeAtomic(path, []byte(body)); err != nil {
		return Artifact{}, err
	}
	created := time.Now()
	if info, err := os.Stat(path); err == nil {
		created = info.ModTime()
	}
	return Artifact{Name: slug, Path: path, Content: body, Created: created}, nil
}

// List returns the workspace's plans, newest first. A file that is not a
// recognizable plan — no marker, no heading — is ignored: a stray Markdown
// file in the plan directory must not become a plan.
func List(workspace string) ([]Artifact, error) {
	dir := Dir(workspace)
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("plan: %w", err)
	}
	var out []Artifact
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		full := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		content := string(raw)
		if !IsPlan(content) {
			continue
		}
		created := time.Time{}
		if info, err := e.Info(); err == nil {
			created = info.ModTime()
		}
		out = append(out, Artifact{
			Name:    strings.TrimSuffix(e.Name(), ".md"),
			Path:    full,
			Content: content,
			Created: created,
		})
	}
	// Newest first; the name breaks a tie so the order is stable.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.After(out[j].Created)
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// Load reads one plan by name. The name may be the slug, the original title
// or the file name with its suffix.
func Load(workspace, name string) (Artifact, error) {
	trimmed := strings.TrimSpace(name)
	trimmed = strings.TrimSuffix(trimmed, ".md")
	slug := Slug(trimmed)
	if slug == "" {
		return Artifact{}, fmt.Errorf("plan: %q is not a plan name", name)
	}
	path := filepath.Join(Dir(workspace), slug+".md")
	raw, err := os.ReadFile(path)
	if err != nil {
		return Artifact{}, fmt.Errorf("plan: %w", err)
	}
	content := string(raw)
	if !IsPlan(content) {
		return Artifact{}, fmt.Errorf("plan: %s is not a plan document", path)
	}
	created := time.Time{}
	if info, err := os.Stat(path); err == nil {
		created = info.ModTime()
	}
	return Artifact{Name: slug, Path: path, Content: content, Created: created}, nil
}

// IsPlan reports whether a document is a recognizable plan: it carries the
// marker and a level-one heading.
func IsPlan(content string) bool {
	return strings.Contains(content, Marker) && hasHeading(content)
}

// Title returns the plan's level-one heading text, without the "# ". It is
// "" when the document has no heading.
func Title(content string) string {
	for _, line := range strings.Split(content, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(t, "# "))
		}
	}
	return ""
}

// Summary returns the first meaningful line of a plan: the first non-empty
// line that is neither the marker nor the heading. It is "" when the plan has
// no body.
func Summary(content string) string {
	for _, line := range strings.Split(content, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || t == Marker || strings.HasPrefix(t, "# ") {
			continue
		}
		return t
	}
	return ""
}

// hasHeading reports whether the text carries a level-one heading.
func hasHeading(s string) bool {
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "# ") {
			return true
		}
	}
	return false
}

// writeAtomic writes data to path through a temporary file in the same
// directory and renames it into place. A reader never sees a half-written
// plan.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".plan-*.tmp")
	if err != nil {
		return fmt.Errorf("plan: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("plan: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("plan: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("plan: %w", err)
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return fmt.Errorf("plan: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("plan: %w", err)
	}
	return nil
}
