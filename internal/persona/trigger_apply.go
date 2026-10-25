package persona

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Trigger-map application (DESIGN §3.3 item 2, M8.5.4).
//
// ProposeTriggers derives trigger-map candidates from refusal telemetry;
// AppendTriggers is the operator-approved write half of that loop: it appends
// accepted term → rephrase pairs to a persona file's triggers.known list.
//
// Invariant — a refusal is never memory. Only term, class, instead and added are
// written: the Trigger type has no field for a refusal phrase or a transcript,
// and nothing in this file reads one. The one rule the loader also enforces is
// repeated here so a write can never leave a pack that fails to load: a
// rephrase that still contains its own trigger term is self-defeating (the
// filter fires on the term wherever it appears) and is rejected before anything
// touches disk.
//
// The frontmatter is edited as a YAML node tree instead of being re-marshalled
// from a fresh map so the operator's own file survives the write: key order,
// block scalars, flow lists and comments are all preserved, and keys this
// package does not know about are kept as they are. Only the Markdown body is
// copied through untouched by construction.

// AppendTriggers appends triggers to the persona file at path and reports how
// many entries were written.
//
// triggers.known is created when the key is missing (the triggers mapping is
// created too), and the Markdown body is preserved byte for byte. An entry with
// no term or no rephrase is skipped, an entry whose term already exists
// (case-insensitively) is skipped, and an entry whose rephrase still contains
// its own term is rejected with an error. Nothing is written unless at least one
// entry survives, and the write itself is atomic (temp file + rename in the same
// directory, original mode kept).
func AppendTriggers(path string, triggers []Trigger) (int, error) {
	if len(triggers) == 0 {
		return 0, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	root, body, err := splitFrontmatterNode(raw, path)
	if err != nil {
		return 0, err
	}
	known, err := triggersKnownNode(root, path)
	if err != nil {
		return 0, err
	}

	existing := map[string]bool{}
	for _, item := range known.Content {
		if term := mapValueNode(item, "term"); term != nil {
			if t := strings.ToLower(triggerField(term.Value)); t != "" {
				existing[t] = true
			}
		}
	}

	added := 0
	for _, t := range triggers {
		term := triggerField(t.Term)
		instead := triggerField(t.Instead)
		if term == "" || instead == "" {
			continue // an entry with nothing to say proposes nothing
		}
		if strings.Contains(strings.ToLower(instead), strings.ToLower(term)) {
			return 0, fmt.Errorf(
				"trigger %q: rephrase %q still contains the trigger term — self-defeating; describe the same work without the term",
				term, instead)
		}
		if existing[strings.ToLower(term)] {
			continue // the persona already knows this term
		}
		known.Content = append(known.Content, triggerNode(t, term, instead))
		existing[strings.ToLower(term)] = true
		added++
	}
	if added == 0 {
		return 0, nil
	}
	// A fresh list is written in block style so the next hand-edit reads like
	// the rest of the pack (an existing `known: []` stays `[]` while empty).
	known.Style = 0

	var fm bytes.Buffer
	enc := yaml.NewEncoder(&fm)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return 0, fmt.Errorf("persona %s: frontmatter: %w", path, err)
	}
	if err := enc.Close(); err != nil {
		return 0, fmt.Errorf("persona %s: frontmatter: %w", path, err)
	}

	out := make([]byte, 0, len(raw)+fm.Len())
	out = append(out, "---\n"...)
	out = append(out, fm.Bytes()...)
	out = append(out, "---\n"...)
	out = append(out, body...)

	mode := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	if err := writeFileAtomic(path, out, mode); err != nil {
		return 0, fmt.Errorf("persona %s: %w", path, err)
	}
	return added, nil
}

// splitFrontmatterNode separates the leading YAML frontmatter from the Markdown
// body, mirroring Parse's rules exactly, and returns the mapping node so it can
// be edited in place.
func splitFrontmatterNode(raw []byte, path string) (*yaml.Node, []byte, error) {
	if !bytes.HasPrefix(raw, []byte("---")) {
		return nil, nil, fmt.Errorf("persona %s: no YAML frontmatter to edit", path)
	}
	rest := bytes.TrimPrefix(raw, []byte("---"))
	nl := bytes.IndexByte(rest, '\n')
	if nl < 0 {
		return nil, nil, fmt.Errorf("persona %s: unterminated frontmatter", path)
	}
	rest = rest[nl+1:]
	end := bytes.Index(rest, []byte("\n---"))
	if end < 0 {
		return nil, nil, fmt.Errorf("persona %s: missing closing frontmatter delimiter", path)
	}
	fm := rest[:end]
	body := rest[end+len("\n---"):]
	body = bytes.TrimPrefix(body, []byte("\n"))

	var doc yaml.Node
	if err := yaml.Unmarshal(fm, &doc); err != nil {
		return nil, nil, fmt.Errorf("persona %s: frontmatter: %w", path, err)
	}
	if len(doc.Content) == 0 || doc.Content[0] == nil {
		return nil, nil, fmt.Errorf("persona %s: empty frontmatter", path)
	}
	return doc.Content[0], body, nil
}

// triggersKnownNode returns the triggers.known sequence, creating the triggers
// mapping and/or the preferred key when the file does not carry them yet.
func triggersKnownNode(root *yaml.Node, path string) (*yaml.Node, error) {
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("persona %s: frontmatter is not a mapping", path)
	}
	trig := mapValueNode(root, "triggers")
	if trig == nil {
		trig = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = append(root.Content, scalarNode("triggers"), trig)
	}
	if trig.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("persona %s: frontmatter.triggers is not a mapping", path)
	}
	known := mapValueNode(trig, "known")
	switch {
	case known == nil:
		known = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		trig.Content = append(trig.Content, scalarNode("known"), known)
	case known.Kind != yaml.SequenceNode:
		return nil, fmt.Errorf("persona %s: frontmatter.triggers.known is not a list", path)
	}
	return known, nil
}

// triggerNode renders one trigger as a frontmatter mapping. Only the fields the
// trigger map owns are written, and only when set: there is no place here for
// anything but a term, its rephrase, a class label and a date.
func triggerNode(t Trigger, term, instead string) *yaml.Node {
	item := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	add := func(key, value string) {
		if value == "" {
			return
		}
		item.Content = append(item.Content, scalarNode(key), scalarNode(value))
	}
	add("term", term)
	add("instead", instead)
	add("class", triggerField(t.Class))
	add("added", triggerField(t.Added))
	return item
}

// mapValueNode looks up key in a mapping node without reordering anything.
func mapValueNode(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// scalarNode builds a plain string scalar; the emitter quotes it when the value
// would otherwise resolve to a different type.
func scalarNode(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

// triggerField flattens a value onto one line: a trigger term is a phrase, and a
// stray newline in one must not be able to change the file's shape.
func triggerField(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// writeFileAtomic replaces path via a sibling temp file so a crash mid-write
// never leaves a half-edited persona.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
