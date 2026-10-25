// Package kb implements the versioned, chunked knowledge base directory format
// (DESIGN §3.3 layer 3) and a tolerant loader for it.
//
// A knowledge base is a directory:
//
//	knowledgebases/attack/
//	  kb.yaml
//	  chunks/
//	    T1059.001.md
//	    T1027.md
//
// kb.yaml is the base descriptor (id, version, title, source, license, plus the
// chunking strategy that produced the files and an optional derived index
// path). Each chunk is a Markdown file whose YAML frontmatter carries provenance
// and the `adversary_prompt` field — how the technique would show up in a plan
// or in code, i.e. the review checklist the agent can actually use.
//
// Markdown is the source of truth; any index built over a KB is derived and
// disposable (the same doctrine as the memory and skills packages). This package
// deliberately does no indexing, retrieval, or prompt rendering: it only
// materializes the format so later layers can search, rank, and inject it.
//
// # Tolerance doctrine
//
// A knowledge base is content, not control flow. Load fails only when the KB
// itself is unusable — a missing or invalid kb.yaml, or a kb.yaml without id,
// version, or title. Everything below that line is best-effort: a chunk file
// that cannot be read, has no frontmatter, has unparseable frontmatter, is
// missing a required field (id, title, source, license), or repeats an already
// loaded id is skipped and recorded in KB.Warnings. One bad chunk never takes
// the whole KB down.
//
// # License policy
//
// Provenance is not decoration: KB content is injected verbatim into prompts,
// so some licenses forbid that. A chunk is injectable — eligible to be rendered
// into a prompt — unless its license says otherwise. Licenses are allowed
// unless the case-insensitive value contains "proprietary",
// "no-redistribution", or "non-distributable", or is exactly "none" (spacing,
// underscores, and hyphens are normalized first, so "No Redistribution" and
// "non_distributable" are caught too). Excluded chunks are still loaded and
// remain visible through KB.Chunks; they are only marked Chunk.Injectable =
// false and left out of KB.InjectionCandidates.
package kb

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// MetaFile is the base descriptor inside a KB directory.
	MetaFile = "kb.yaml"
	// ChunksDir is the subdirectory holding one Markdown file per chunk.
	ChunksDir = "chunks"
)

// forbiddenLicenseTokens mark a license as too restrictive for prompt
// injection. Matching is done on a normalized (lower-case, hyphenated) value.
var forbiddenLicenseTokens = []string{
	"proprietary",
	"no-redistribution",
	"non-distributable",
}

// Chunking records how the chunks were derived (e.g. strategy "per-technique"
// over fields ["description", "detection"]). It is descriptive metadata for
// rebuilds; the loader never depends on it.
type Chunking struct {
	Strategy string
	Fields   []string
}

// KB is a loaded knowledge base.
type KB struct {
	ID       string
	Version  string
	Title    string
	Source   string
	License  string
	Chunking Chunking
	// Index is the optional path (as written in kb.yaml, usually relative to
	// the KB directory) of a derived index. Empty means "no index declared".
	Index string
	// Dir is the directory the KB was loaded from, as passed to Load.
	Dir string
	// Chunks holds every chunk that loaded successfully, including chunks whose
	// license forbids injection (see Injectable). Chunks skipped as malformed
	// are not present here.
	Chunks []Chunk
	// Warnings records per-chunk problems found while loading. Warnings never
	// fail a load.
	Warnings []string
}

// Chunk is one retrievable unit of knowledge.
type Chunk struct {
	ID              string
	Title           string
	Tactic          string
	Platforms       []string
	Source          string
	License         string
	RetrievedAt     string
	AdversaryPrompt string
	// Body is the Markdown after the closing frontmatter delimiter, trimmed.
	Body string
	// Injectable reports whether the license permits rendering this chunk into
	// a prompt. Chunks are loaded either way.
	Injectable bool
	// Path is the chunk file (joined to the directory given to Load).
	Path string
}

// InjectionCandidates returns the chunks whose license permits prompt
// injection, in load order. It returns nil for a nil KB.
func (k *KB) InjectionCandidates() []Chunk {
	if k == nil {
		return nil
	}
	var out []Chunk
	for _, c := range k.Chunks {
		if c.Injectable {
			out = append(out, c)
		}
	}
	return out
}

// Find returns the chunk with the given id: exact match first, then
// case-insensitive.
func (k *KB) Find(id string) (Chunk, bool) {
	if k == nil {
		return Chunk{}, false
	}
	id = strings.TrimSpace(id)
	for _, c := range k.Chunks {
		if c.ID == id {
			return c, true
		}
	}
	for _, c := range k.Chunks {
		if strings.EqualFold(c.ID, id) {
			return c, true
		}
	}
	return Chunk{}, false
}

// LicenseAllowsInjection reports whether a license string permits a chunk to be
// rendered into a prompt. See the package doc for the exact rule: injection is
// allowed unless the (case-insensitive, whitespace/hyphen normalized) license
// contains "proprietary", "no-redistribution", or "non-distributable", or is
// exactly "none".
func LicenseAllowsInjection(license string) bool {
	norm := normalizeLicense(license)
	if norm == "" {
		return true
	}
	if norm == "none" {
		return false
	}
	for _, tok := range forbiddenLicenseTokens {
		if strings.Contains(norm, tok) {
			return false
		}
	}
	return true
}

// Load reads the KB rooted at dir. It returns an error only when the KB itself
// is unusable: dir is not a readable directory, kb.yaml is missing or is not
// valid YAML, or it lacks id, version, or title. Chunk-level problems are
// skipped and reported through KB.Warnings.
func Load(dir string) (*KB, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("kb: empty directory")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("kb: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("kb: %s: not a directory", dir)
	}

	metaPath := filepath.Join(dir, MetaFile)
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		// `kb.yml` is not the documented name, but tolerating it costs nothing
		// and keeps an otherwise valid base usable.
		if alt := filepath.Join(dir, "kb.yml"); alt != metaPath {
			if altRaw, altErr := os.ReadFile(alt); altErr == nil {
				raw, metaPath, err = altRaw, alt, nil
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("kb: %s: %w", metaPath, err)
	}

	meta := map[string]any{}
	if err := yaml.Unmarshal(raw, &meta); err != nil {
		return nil, fmt.Errorf("kb: %s: invalid YAML: %w", metaPath, err)
	}

	k := &KB{
		ID:      text(meta, "id"),
		Version: text(meta, "version"),
		Title:   text(meta, "title"),
		Source:  text(meta, "source"),
		License: text(meta, "license"),
		Index:   text(meta, "index"),
		Dir:     dir,
	}
	if chunking, ok := lookup(meta, "chunking"); ok {
		if cm, ok := chunking.(map[string]any); ok {
			k.Chunking = Chunking{
				Strategy: text(cm, "strategy"),
				Fields:   list(cm, "fields"),
			}
		}
	}

	var missing []string
	for _, f := range []struct{ name, val string }{
		{"id", k.ID}, {"version", k.Version}, {"title", k.Title},
	} {
		if f.val == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("kb: %s: missing required field(s): %s", metaPath, strings.Join(missing, ", "))
	}

	k.loadChunks()
	return k, nil
}

// loadChunks reads every *.md under chunks/, skipping and recording anything it
// cannot use. Load order is filename order, so results are deterministic.
func (k *KB) loadChunks() {
	chunksDir := filepath.Join(k.Dir, ChunksDir)
	entries, err := os.ReadDir(chunksDir)
	if err != nil {
		k.Warnings = append(k.Warnings, fmt.Sprintf("%s: %v", ChunksDir, err))
		return
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	seen := make(map[string]string, len(names))
	for _, name := range names {
		rel := ChunksDir + "/" + name
		path := filepath.Join(chunksDir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			k.warn(rel, err)
			continue
		}
		c, err := parseChunk(string(raw), path)
		if err != nil {
			k.warn(rel, err)
			continue
		}
		if prev, ok := seen[c.ID]; ok {
			k.warn(rel, fmt.Errorf("duplicate chunk id %q (already loaded from %s)", c.ID, prev))
			continue
		}
		seen[c.ID] = rel
		k.Chunks = append(k.Chunks, c)
	}
}

func (k *KB) warn(rel string, err error) {
	k.Warnings = append(k.Warnings, fmt.Sprintf("%s: %v", rel, err))
}

// parseChunk splits frontmatter from body and validates the required fields.
func parseChunk(raw, path string) (Chunk, error) {
	raw = strings.TrimPrefix(raw, "\ufeff")
	raw = strings.ReplaceAll(raw, "\r\n", "\n")

	// Tolerate leading blank lines before the opening delimiter.
	trimmed := strings.TrimLeft(raw, " \t\n")
	if !strings.HasPrefix(trimmed, "---") {
		return Chunk{}, fmt.Errorf("missing YAML frontmatter")
	}
	rest := trimmed[len("---"):]
	nl := strings.IndexByte(rest, '\n')
	if nl < 0 {
		return Chunk{}, fmt.Errorf("unterminated frontmatter (no closing '---')")
	}
	if strings.TrimSpace(rest[:nl]) != "" {
		return Chunk{}, fmt.Errorf("frontmatter must open with a bare '---' line")
	}
	fmText, body, ok := splitFrontmatter(rest[nl+1:])
	if !ok {
		return Chunk{}, fmt.Errorf("unterminated frontmatter (no closing '---')")
	}

	fm := map[string]any{}
	if doc := strings.TrimSpace(fmText); doc != "" {
		if err := yaml.Unmarshal([]byte(fmText), &fm); err != nil {
			return Chunk{}, fmt.Errorf("bad frontmatter: %w", err)
		}
	}

	c := Chunk{
		ID:              text(fm, "id"),
		Title:           text(fm, "title"),
		Tactic:          text(fm, "tactic"),
		Platforms:       list(fm, "platforms"),
		Source:          text(fm, "source"),
		License:         text(fm, "license"),
		RetrievedAt:     text(fm, "retrieved_at"),
		AdversaryPrompt: text(fm, "adversary_prompt"),
		Body:            strings.TrimSpace(body),
		Path:            path,
	}

	var missing []string
	for _, f := range []struct{ name, val string }{
		{"id", c.ID}, {"title", c.Title}, {"source", c.Source}, {"license", c.License},
	} {
		if f.val == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return Chunk{}, fmt.Errorf("missing required frontmatter field(s): %s", strings.Join(missing, ", "))
	}

	c.Injectable = LicenseAllowsInjection(c.License)
	return c, nil
}

// splitFrontmatter returns the text before the closing delimiter line and the
// body after it. Both "---" and YAML's "..." end markers are accepted.
func splitFrontmatter(rest string) (front, body string, ok bool) {
	off := 0
	for off <= len(rest) {
		line := rest[off:]
		next := len(rest)
		hasNL := false
		if nl := strings.IndexByte(line, '\n'); nl >= 0 {
			line, next, hasNL = line[:nl], off+nl+1, true
		}
		switch strings.TrimSpace(line) {
		case "---", "...":
			return rest[:off], rest[next:], true
		}
		if !hasNL {
			return "", "", false
		}
		off = next
	}
	return "", "", false
}

// normalizeLicense lower-cases a license string and folds the spellings that
// mean the same restriction into their hyphenated form.
func normalizeLicense(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "_", "-")
	s = strings.Join(strings.Fields(s), " ")
	for _, pair := range [][2]string{
		{"no redistribution", "no-redistribution"},
		{"no re-distribution", "no-redistribution"},
		{"non distributable", "non-distributable"},
		{"non-distributable", "non-distributable"},
		{"nondistributable", "non-distributable"},
	} {
		s = strings.ReplaceAll(s, pair[0], pair[1])
	}
	s = strings.Trim(s, ".\"'")
	return s
}

// lookup finds a frontmatter key tolerantly: exact, then by a normalized key
// (case-insensitive, '-' and '_' interchangeable).
func lookup(m map[string]any, key string) (any, bool) {
	if v, ok := m[key]; ok {
		return v, true
	}
	want := normKey(key)
	for k, v := range m {
		if normKey(k) == want {
			return v, true
		}
	}
	return nil, false
}

func normKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.ReplaceAll(s, "-", "_")
}

// text returns a trimmed scalar value for key, or "" when absent or non-scalar.
func text(m map[string]any, key string) string {
	v, ok := lookup(m, key)
	if !ok {
		return ""
	}
	return scalar(v)
}

// list returns a list value for key. A scalar is accepted too: a comma
// separated string splits, any other scalar becomes a one-element list.
func list(m map[string]any, key string) []string {
	v, ok := lookup(m, key)
	if !ok {
		return nil
	}
	switch x := v.(type) {
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			if s := scalar(item); s != "" {
				out = append(out, s)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case []string:
		return append([]string(nil), x...)
	case string:
		var out []string
		for _, part := range strings.Split(x, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
		return out
	default:
		if s := scalar(x); s != "" {
			return []string{s}
		}
		return nil
	}
}

// scalar renders a YAML scalar as a string; non-scalars render as "".
func scalar(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case uint64:
		return strconv.FormatUint(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	default:
		return ""
	}
}
