// Package blueprints scaffolds `kintsugi new` project workspaces (DESIGN §6.2):
// starter trees for the tool classes this harness is actually used to build.
//
// Two blueprints ship and no more (the §6.2 cut line): "c2" (adversary-
// simulation tasking client/server) and "recon" (lab-scoped discovery/scan).
// Each one is a directory of templates under templates/<name>/ embedded with
// go:embed, so a scaffold is a plain file copy — the package is deterministic,
// offline, and never calls a provider.
//
// Trust boundary (DESIGN §4, §6.2): every template's "Authorization & scope"
// section is an operator placeholder by construction. The harness will not
// invent a permission claim on the operator's behalf — there is no code path in
// this package that produces one, and nothing here rewrites that section later.
package blueprints

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// templates holds one subdirectory per blueprint.
//
//go:embed templates
var templates embed.FS

const (
	// ProjectToken is substituted with the project name in every file body.
	ProjectToken = "{{PROJECT}}"

	// OperatorPlaceholder marks the one section the harness never writes. It is
	// byte-identical to initgen.OperatorPlaceholder on purpose: the operator
	// greps for one string, not two.
	OperatorPlaceholder = "<!-- operator: complete this section; the harness never writes it -->"

	// Files every blueprint scaffolds (slash-separated, relative to the root).
	ContextFile  = "CONTEXT.md"
	ConfigFile   = "kintsugi.toml"
	ReadmeFile   = "README.md"
	FixturesFile = "fixtures/README.md"

	// templateRoot is the embed prefix holding the per-blueprint directories.
	templateRoot = "templates/"

	// fallbackProjectName is used when neither the caller nor the target
	// directory yields a usable name.
	fallbackProjectName = "project"
)

// Blueprint is one scaffold offered by `kintsugi new`.
type Blueprint struct {
	Name    string // the argument to `kintsugi new`; also the template directory
	Summary string // one line, shown in the blueprint table
}

// catalog is the whole set, in presentation order. Four blueprints: the
// original §6.2 cut line was two, widened by the operator's direction to
// cover the detection-engineering and offensive-tooling audiences the
// harness actually serves. Each is a whole project shape, not a file
// template: same structure (CONTEXT, config, fixtures, lab), same
// lab-only conventions, different domain.
var catalog = []Blueprint{
	{
		Name:    "c2",
		Summary: "adversary-simulation tasking client/server harness for purple-team validation against synthetic lab fixtures",
	},
	{
		Name:    "recon",
		Summary: "lab-scoped discovery/scan utility with lab-safe defaults",
	},
	{
		Name:    "detection",
		Summary: "detection engineering workspace: sigma rules, network signatures, hunts, and the lab that validates them",
	},
	{
		Name:    "redteam",
		Summary: "offensive-security tooling workspace: scanners, fingerprinters, fuzzers, exploit development against lab targets",
	},
}

// List returns the available blueprints in presentation order. The slice is a
// copy: callers cannot reorder the catalog.
func List() []Blueprint {
	return append([]Blueprint(nil), catalog...)
}

// Lookup returns the blueprint with this name.
func Lookup(name string) (Blueprint, bool) {
	name = strings.TrimSpace(name)
	for _, b := range catalog {
		if b.Name == name {
			return b, true
		}
	}
	return Blueprint{}, false
}

// Names returns the blueprint names in catalog order.
func Names() []string {
	out := make([]string, 0, len(catalog))
	for _, b := range catalog {
		out = append(out, b.Name)
	}
	return out
}

// Render writes the named blueprint into targetDir, creating the directory and
// any subdirectories it needs. projectName replaces ProjectToken in every file
// body; an empty projectName defaults to the target directory's base name.
//
// Existing files are never clobbered unless force is set: they are reported in
// skipped instead of written. Both lists hold paths relative to the target
// directory (slash-separated), in template order.
func Render(name, targetDir, projectName string, force bool) (written, skipped []string, err error) {
	name = strings.TrimSpace(name)
	if _, ok := Lookup(name); !ok {
		return nil, nil, fmt.Errorf("blueprints: unknown blueprint %q (valid: %s)",
			name, strings.Join(Names(), ", "))
	}
	if strings.TrimSpace(targetDir) == "" {
		return nil, nil, fmt.Errorf("blueprints: target directory is required")
	}

	root, err := filepath.Abs(targetDir)
	if err != nil {
		return nil, nil, fmt.Errorf("blueprints: resolve %s: %w", targetDir, err)
	}
	if projectName = strings.TrimSpace(projectName); projectName == "" {
		projectName = filepath.Base(root)
	}
	if projectName == "" || projectName == string(filepath.Separator) || projectName == "." {
		projectName = fallbackProjectName
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, nil, fmt.Errorf("blueprints: %w", err)
	}

	rel, err := templateFiles(name)
	if err != nil {
		return nil, nil, err
	}

	for _, rel := range rel {
		raw, err := templates.ReadFile(path.Join(templateRoot, name, rel))
		if err != nil {
			return written, skipped, fmt.Errorf("blueprints: read template %s/%s: %w", name, rel, err)
		}
		body := strings.ReplaceAll(string(raw), ProjectToken, projectName)

		dest := filepath.Join(root, filepath.FromSlash(rel))
		if st, statErr := os.Stat(dest); statErr == nil {
			if st.IsDir() {
				return written, skipped, fmt.Errorf("blueprints: %s exists and is a directory", rel)
			}
			if !force {
				skipped = append(skipped, rel)
				continue
			}
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return written, skipped, fmt.Errorf("blueprints: %w", err)
		}
		if err := writeAtomic(dest, []byte(body)); err != nil {
			return written, skipped, fmt.Errorf("blueprints: %w", err)
		}
		written = append(written, rel)
	}
	// The render marker names the blueprint this workspace came from. It is
	// metadata, not scaffolding, so it is not reported in written/skipped.
	// internal/lab uses it to decide whether the blueprint's compose file may
	// stand in for a missing one: a workspace that was never rendered from a
	// blueprint must never inherit a lab it did not ask for.
	if err := writeMarker(root, name); err != nil {
		return written, skipped, err
	}
	return written, skipped, nil
}

// MarkerPath is where Render records the blueprint a workspace came from.
func MarkerPath(dir string) string {
	return filepath.Join(dir, ".kintsugi", "blueprint")
}

// RenderedFrom reports the blueprint name recorded in dir's render marker.
func RenderedFrom(dir string) (string, bool) {
	b, err := os.ReadFile(MarkerPath(dir))
	if err != nil {
		return "", false
	}
	name := strings.TrimSpace(string(b))
	if _, ok := Lookup(name); !ok {
		return "", false
	}
	return name, true
}

func writeMarker(root, name string) error {
	p := MarkerPath(root)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("blueprints: marker dir: %w", err)
	}
	return writeAtomic(p, []byte(name+"\n"))
}

// templateFiles lists the template files of one blueprint, relative to its
// directory and slash-separated. WalkDir visits lexically, so the order — and
// therefore the written/skipped lists — is deterministic.
func templateFiles(name string) ([]string, error) {
	root := path.Join(templateRoot, name)
	var out []string
	err := fs.WalkDir(templates, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(p, root+"/")
		out = append(out, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("blueprints: walk template %s: %w", name, err)
	}
	sort.Strings(out)
	return out, nil
}

// writeAtomic writes the file through a temp sibling so a failed render never
// leaves a half-written scaffold behind.
func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
