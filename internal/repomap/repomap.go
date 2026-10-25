// Package repomap builds a token-budgeted map of a repository's files and key
// symbols (M10.1, ROADMAP §M10.1; idea I-37).
//
// The map is context engineering for weak open-weights models: instead of
// dumping files, it hands the model a compact, ranked index of the
// repository's shape — "which file holds what" — at a hard token cost the
// caller controls.
//
// Discipline (mirrors internal/prompt's layer budgeting):
//
//   - Deterministic. The same tree always yields the same bytes, so the
//     caller may cache the map and keep the provider's cache prefix warm.
//     Build holds no cache of its own: caching is the caller's decision.
//   - Bounded. The rendered map always fits Options.MaxTokens. When it cannot,
//     the lowest-ranked entries are dropped and Map.Truncated is set.
//   - Tolerant. A file that cannot be parsed is skipped, never fatal: one
//     broken file must not take a session down.
//
// The map is context, never authority: it names files and symbols, it does
// not authorize anything.
package repomap

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Defaults for the budget knobs.
const (
	// DefaultMaxTokens is the token ceiling of the rendered map.
	DefaultMaxTokens = 1000
	// DefaultMaxFiles is the ceiling on mapped files.
	DefaultMaxFiles = 200
)

// Ranking weights. A symbol's score is its reference count plus these
// bonuses; ties break on path then symbol so the order never wobbles.
const (
	exportedBonus = 2 // exported-looking names (leading upper case)
	includeBonus  = 5 // the caller's task-relevant paths
)

// Entry is one symbol in the map.
type Entry struct {
	Path   string // workspace-relative, slash-separated
	Kind   string // func | method | type | const | var | def | class | heading | ...
	Symbol string // the bare identifier
	Sig    string // the signature line, e.g. "func (Map) Render() string"
	Refs   int    // how many other symbols mention this one
	Line   int    // 1-based line of the declaration
}

// Map is the built map. Entries are in render order: files grouped together,
// best-ranked file first, best-ranked symbol first inside a file.
type Map struct {
	Entries   []Entry
	Bytes     int  // rendered size in bytes
	Truncated bool // true when the map had to drop entries
}

// Options configures a build.
type Options struct {
	// Root is the repository root to map. Required.
	Root string
	// MaxTokens is the token ceiling for the rendered map. Defaults to
	// DefaultMaxTokens.
	MaxTokens int
	// MaxFiles is the ceiling on mapped files. Defaults to DefaultMaxFiles.
	MaxFiles int
	// Include lists extra path prefixes to prioritize (typically the task's
	// files). Matching is prefix-based: "internal/repo" covers
	// "internal/repo/x.go", and an exact path matches itself.
	Include []string
}

// Build walks the repository and returns a token-budgeted map. The result is
// deterministic for a given tree: same tree in, same bytes out.
func Build(opts Options) (Map, error) {
	root := strings.TrimSpace(opts.Root)
	if root == "" {
		return Map{}, fmt.Errorf("repomap: no root configured")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return Map{}, fmt.Errorf("repomap: resolve root: %w", err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return Map{}, fmt.Errorf("repomap: root %q: %w", root, err)
	}
	if !st.IsDir() {
		return Map{}, fmt.Errorf("repomap: root %q is not a directory", root)
	}

	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}
	maxFiles := opts.MaxFiles
	if maxFiles <= 0 {
		maxFiles = DefaultMaxFiles
	}

	files, err := discover(abs)
	if err != nil {
		return Map{}, err
	}

	var raws []rawEntry
	for _, rel := range files {
		raws = append(raws, extractFile(abs, rel)...)
	}

	computeRefs(raws)

	inc := newIncludes(abs, opts.Include)
	groups := groupEntries(raws, inc)
	return assemble(groups, maxFiles, maxTokens), nil
}

// Render returns the map as Markdown-ish text: a header line with the file
// count, then one line per symbol, grouped by file. A truncated map says so
// on its last line.
func (m Map) Render() string {
	if len(m.Entries) == 0 && m.Truncated {
		// The budget could not hold even the header: say nothing rather than
		// blow the budget.
		return ""
	}
	files := distinctPaths(m.Entries)
	parts := make([]string, 0, len(m.Entries)+2)
	parts = append(parts, headerLine(len(files), len(m.Entries)))
	for _, e := range m.Entries {
		parts = append(parts, entryLine(e))
	}
	if m.Truncated {
		parts = append(parts, truncationNote)
	}
	return strings.Join(parts, "\n")
}

// ---------- rendering ----------

// truncationNote is the last line of a map that had to drop entries.
const truncationNote = "… truncated to fit the token budget"

func headerLine(files, symbols int) string {
	return fmt.Sprintf("## Repository map — %d %s, %d %s",
		files, plural(files, "file", "files"),
		symbols, plural(symbols, "symbol", "symbols"))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func entryLine(e Entry) string {
	return fmt.Sprintf("%s:%d %s", e.Path, e.Line, e.Sig)
}

func distinctPaths(entries []Entry) []string {
	seen := make(map[string]bool, len(entries))
	var out []string
	for _, e := range entries {
		if seen[e.Path] {
			continue
		}
		seen[e.Path] = true
		out = append(out, e.Path)
	}
	return out
}

// ---------- budget ----------

// assemble turns ranked file groups into the final map: the file limit is
// applied first, then the token budget drops the lowest-ranked symbols until
// the render fits.
func assemble(groups []fileGroup, maxFiles, maxTokens int) Map {
	totalFiles := len(groups)
	kept := groups
	if len(kept) > maxFiles {
		kept = kept[:maxFiles]
	}
	filesOverLimit := totalFiles - len(kept)

	var entries []rawEntry
	var lines []string
	for _, g := range kept {
		for _, e := range g.entries {
			entries = append(entries, e)
			lines = append(lines, entryLine(e.Entry))
		}
	}

	maxBytes := maxTokens * 4 // ~4 bytes per token, same rule as the prompt stack

	// Provisional header: it carries the largest counts the map can have, so
	// it is an upper bound on the header the final map will print.
	prov := headerLine(len(kept), len(entries))
	if maxBytes < len(prov)+1+len(truncationNote) {
		// Not even the header and the truncation note fit: the map says
		// nothing at all rather than blowing the budget.
		return Map{Truncated: true}
	}

	// Plain header: what the map looks like when nothing was dropped.
	plainLen := len(prov)
	for _, l := range lines {
		plainLen += 1 + len(l)
	}
	if filesOverLimit == 0 && plainLen <= maxBytes {
		m := Map{Entries: publicEntries(entries)}
		m.Bytes = len(m.Render())
		return m
	}

	// Over budget (or over the file limit): reserve room for the header and
	// the truncation note, then keep the highest-ranked lines that fit. The
	// lowest-ranked entries are the ones dropped.
	avail := maxBytes - len(prov) - 1 - len(truncationNote)
	used := 0
	keptEntries := make([]rawEntry, 0, len(entries))
	for i, l := range lines {
		if used+1+len(l) > avail {
			break
		}
		used += 1 + len(l)
		keptEntries = append(keptEntries, entries[i])
	}

	m := Map{Entries: publicEntries(keptEntries), Truncated: true}
	m.Bytes = len(m.Render())
	return m
}

func publicEntries(raws []rawEntry) []Entry {
	if len(raws) == 0 {
		return nil
	}
	out := make([]Entry, 0, len(raws))
	for _, r := range raws {
		out = append(out, r.Entry)
	}
	return out
}

// ---------- ranking ----------

// rawEntry is an entry plus the bookkeeping the ranking needs.
type rawEntry struct {
	Entry
	score   int
	context string // declaration context, used for reference counting
}

// fileGroup is one file's entries, ranked.
type fileGroup struct {
	path    string
	entries []rawEntry
	score   int
}

// computeRefs counts, for every symbol, how many *other* symbols mention it.
// A symbol's context is the text of its declaration (signature plus body for
// Go, the declaration block for the heuristic languages).
func computeRefs(raws []rawEntry) {
	counts := make(map[string]int, len(raws))
	for _, r := range raws {
		for _, tok := range uniqueTokens(r.context) {
			counts[tok]++
		}
	}
	for i := range raws {
		n := counts[raws[i].Symbol] - 1 // its own declaration always mentions it
		if n < 0 {
			n = 0
		}
		raws[i].Refs = n
	}
}

// groupEntries buckets entries per file and ranks everything deterministically:
// score desc, then path, then symbol.
func groupEntries(raws []rawEntry, inc includes) []fileGroup {
	byPath := make(map[string][]rawEntry)
	for _, r := range raws {
		byPath[r.Path] = append(byPath[r.Path], r)
	}
	groups := make([]fileGroup, 0, len(byPath))
	for p, es := range byPath {
		g := fileGroup{path: p, entries: es}
		for i := range g.entries {
			g.entries[i].score = scoreOf(g.entries[i], inc)
			if g.entries[i].score > g.score {
				g.score = g.entries[i].score
			}
		}
		sort.SliceStable(g.entries, func(i, j int) bool {
			a, b := g.entries[i], g.entries[j]
			if a.score != b.score {
				return a.score > b.score
			}
			if a.Symbol != b.Symbol {
				return a.Symbol < b.Symbol
			}
			return a.Line < b.Line
		})
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].score != groups[j].score {
			return groups[i].score > groups[j].score
		}
		return groups[i].path < groups[j].path
	})
	return groups
}

func scoreOf(e rawEntry, inc includes) int {
	s := e.Refs
	if isExported(e) {
		s += exportedBonus
	}
	if inc.match(e.Path) {
		s += includeBonus
	}
	return s
}

// isExported reports whether a name looks exported: a leading upper-case
// rune (Go, TS, JS, Rust conventions) or an explicit "pub" in Rust.
func isExported(e rawEntry) bool {
	if e.Symbol == "" {
		return false
	}
	for _, r := range e.Symbol {
		if unicode.IsUpper(r) {
			return true
		}
		break
	}
	return strings.HasPrefix(e.Sig, "pub ")
}

// includes is the caller's prioritized-path list.
type includes struct {
	pats []string
}

func newIncludes(root string, raw []string) includes {
	inc := includes{}
	for _, p := range raw {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		p = filepath.ToSlash(p)
		if filepath.IsAbs(p) {
			if rel, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(rel, "..") {
				p = filepath.ToSlash(rel)
			}
		}
		p = strings.TrimPrefix(p, "./")
		p = strings.TrimSuffix(p, "/")
		if p == "" {
			continue
		}
		inc.pats = append(inc.pats, p)
	}
	return inc
}

func (i includes) match(rel string) bool {
	for _, p := range i.pats {
		if rel == p {
			return true
		}
		if strings.HasPrefix(rel, p+"/") {
			return true
		}
		if strings.ContainsAny(p, "*?[") && globMatch(p, rel) {
			return true
		}
	}
	return false
}

// ---------- discovery ----------

// skipDirs are never mapped: they are dependency trees, build output or
// harness state, not source.
var skipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	"dist":         true,
	"build":        true,
	"target":       true,
	".kintsugi":    true,
}

// supportedExts are the languages the map knows how to read.
var supportedExts = map[string]bool{
	".go":       true,
	".py":       true,
	".pyi":      true,
	".ts":       true,
	".tsx":      true,
	".js":       true,
	".jsx":      true,
	".mjs":      true,
	".cjs":      true,
	".rs":       true,
	".md":       true,
	".markdown": true,
}

// maxFileBytes bounds how much of a file is read for symbol extraction.
const maxFileBytes = 1 << 20

// discover walks the tree and returns the candidate files (slash-separated,
// relative to root) in lexical order.
func discover(root string) ([]string, error) {
	ig := loadIgnore(root)
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable entry is skipped, never fatal.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return fs.SkipDir
			}
			if ig.match(rel, true) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if ig.match(rel, false) {
			return nil
		}
		if !supportedExts[strings.ToLower(path.Ext(rel))] {
			return nil
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// ---------- .gitignore ----------

// ignoreRule is one parsed .gitignore line.
type ignoreRule struct {
	pattern  string
	negate   bool
	dirOnly  bool
	anchored bool
	hasSlash bool
}

type ignoreSet []ignoreRule

func loadIgnore(root string) ignoreSet {
	data, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		return nil
	}
	return parseIgnore(string(data))
}

func parseIgnore(body string) ignoreSet {
	var out ignoreSet
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		line = strings.TrimRight(line, " \t")
		if line == "" {
			continue
		}
		r := ignoreRule{}
		if strings.HasPrefix(line, "!") {
			r.negate = true
			line = line[1:]
		}
		if strings.HasSuffix(line, "/") {
			r.dirOnly = true
			line = strings.TrimSuffix(line, "/")
		}
		if strings.HasPrefix(line, "/") {
			r.anchored = true
			line = strings.TrimPrefix(line, "/")
		}
		if line == "" {
			continue
		}
		r.pattern = line
		r.hasSlash = strings.Contains(line, "/")
		out = append(out, r)
	}
	return out
}

// match reports whether a path is ignored. The last matching rule wins, so a
// later "!" pattern can re-include a path. A path below an ignored directory
// is ignored too: git never descends into an excluded directory, and neither
// does the walker.
func (ig ignoreSet) match(rel string, isDir bool) bool {
	ignored := false
	for _, r := range ig {
		if r.matches(rel, isDir) {
			ignored = !r.negate
		}
	}
	if ignored {
		return true
	}
	for _, anc := range ancestors(rel) {
		hit := false
		for _, r := range ig {
			if r.matches(anc, true) {
				hit = !r.negate
			}
		}
		if hit {
			return true
		}
	}
	return false
}

// ancestors lists the parent directories of a path, top-down.
func ancestors(rel string) []string {
	var out []string
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		out = append(out, strings.Join(parts[:i], "/"))
	}
	return out
}

func (r ignoreRule) matches(rel string, isDir bool) bool {
	if r.dirOnly && !isDir {
		return false
	}
	if r.anchored || r.hasSlash {
		return globMatch(r.pattern, rel)
	}
	return globMatch(r.pattern, path.Base(rel))
}

// globMatch matches a slash-separated path against a pattern where "*" and
// "?" never cross a "/" and "**" spans any number of segments.
func globMatch(pattern, name string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			if len(pat) == 1 {
				return true
			}
			for i := 0; i <= len(name); i++ {
				if matchSegments(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if !matchSegment(pat[0], name[0]) {
			return false
		}
		pat = pat[1:]
		name = name[1:]
	}
	return len(name) == 0
}

// matchSegment matches a single path segment ("*" and "?" only).
func matchSegment(pattern, s string) bool {
	px, sx := 0, 0
	star, match := -1, 0
	for sx < len(s) {
		switch {
		case px < len(pattern) && (pattern[px] == '?' || pattern[px] == s[sx]):
			px++
			sx++
		case px < len(pattern) && pattern[px] == '*':
			star = px
			match = sx
			px++
		case star >= 0:
			px = star + 1
			match++
			sx = match
		default:
			return false
		}
	}
	for px < len(pattern) && pattern[px] == '*' {
		px++
	}
	return px == len(pattern)
}

// ---------- extraction ----------

// extractFile reads one file and returns its symbols. A file that cannot be
// read or parsed yields nothing and is skipped.
func extractFile(root, rel string) []rawEntry {
	full := filepath.Join(root, filepath.FromSlash(rel))
	st, err := os.Stat(full)
	if err != nil || st.Size() > maxFileBytes {
		return nil
	}
	src, err := os.ReadFile(full)
	if err != nil || isBinary(src) {
		return nil
	}
	switch strings.ToLower(path.Ext(rel)) {
	case ".go":
		return extractGo(full, rel, src)
	case ".py", ".pyi":
		return extractHeuristic(rel, src, pyRules, langPython)
	case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs":
		return extractHeuristic(rel, src, jsRules, langBraces)
	case ".rs":
		return extractHeuristic(rel, src, rustRules, langBraces)
	case ".md", ".markdown":
		return extractHeuristic(rel, src, mdRules, langMarkdown)
	}
	return nil
}

func isBinary(src []byte) bool {
	head := src
	if len(head) > 512 {
		head = head[:512]
	}
	for _, b := range head {
		if b == 0 {
			return true
		}
	}
	return false
}

// ---------- Go extraction ----------

// extractGo parses a Go file and returns its top-level functions, methods,
// types and exported constants/variables.
func extractGo(path, rel string, src []byte) []rawEntry {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		return nil // broken file: skipped, never fatal
	}
	var out []rawEntry
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			sig := goFuncSig(fset, src, d)
			if sig == "" {
				continue
			}
			kind := "func"
			if d.Recv != nil && len(d.Recv.List) > 0 {
				kind = "method"
			}
			out = append(out, rawEntry{
				Entry: Entry{
					Path:   rel,
					Kind:   kind,
					Symbol: d.Name.Name,
					Sig:    sig,
					Line:   fset.Position(d.Pos()).Line,
				},
				context: srcSlice(src, fset, d.Pos(), d.End()),
			})
		case *ast.GenDecl:
			out = append(out, goGenDecl(fset, src, rel, d)...)
		}
	}
	return out
}

// goGenDecl extracts types and exported constants/variables.
func goGenDecl(fset *token.FileSet, src []byte, rel string, d *ast.GenDecl) []rawEntry {
	var out []rawEntry
	switch d.Tok {
	case token.TYPE:
		for _, spec := range d.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			out = append(out, rawEntry{
				Entry: Entry{
					Path:   rel,
					Kind:   "type",
					Symbol: ts.Name.Name,
					Sig:    goTypeSig(fset, src, ts),
					Line:   fset.Position(ts.Pos()).Line,
				},
				context: srcSlice(src, fset, d.Pos(), d.End()),
			})
		}
	case token.CONST, token.VAR:
		kind := "const"
		if d.Tok == token.VAR {
			kind = "var"
		}
		for _, spec := range d.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			body := srcSlice(src, fset, vs.Pos(), vs.End())
			if body == "" {
				continue
			}
			if len(body) > 120 {
				body = body[:120] + "…"
			}
			for _, name := range vs.Names {
				if !exportedName(name.Name) {
					continue
				}
				out = append(out, rawEntry{
					Entry: Entry{
						Path:   rel,
						Kind:   kind,
						Symbol: name.Name,
						Sig:    kind + " " + body,
						Line:   fset.Position(vs.Pos()).Line,
					},
					context: srcSlice(src, fset, d.Pos(), d.End()),
				})
			}
		}
	}
	return out
}

func exportedName(name string) bool {
	for _, r := range name {
		return unicode.IsUpper(r)
	}
	return false
}

// goFuncSig renders a declaration's signature line, e.g.
// "func (Store) Get(ctx context.Context) (string, error)".
func goFuncSig(fset *token.FileSet, src []byte, d *ast.FuncDecl) string {
	var b strings.Builder
	b.WriteString("func ")
	if d.Recv != nil && len(d.Recv.List) > 0 {
		b.WriteString("(")
		b.WriteString(recvTypeName(d.Recv.List[0].Type))
		b.WriteString(") ")
	}
	b.WriteString(d.Name.Name)
	if d.Type.TypeParams != nil {
		if s := srcBetween(src, fset, d.Type.TypeParams.Opening, d.Type.TypeParams.Closing); s != "" {
			b.WriteString(s)
		}
	}
	if d.Type.Params != nil {
		if s := srcBetween(src, fset, d.Type.Params.Opening, d.Type.Params.Closing); s != "" {
			b.WriteString(s)
		} else {
			b.WriteString("()")
		}
	} else {
		b.WriteString("()")
	}
	if d.Type.Results != nil && len(d.Type.Results.List) > 0 {
		if s := srcBetween(src, fset, d.Type.Results.Opening, d.Type.Results.Closing); s != "" {
			b.WriteString(" ")
			b.WriteString(s)
		} else if f := d.Type.Results.List[0]; f.Type != nil {
			b.WriteString(" ")
			b.WriteString(srcSlice(src, fset, f.Type.Pos(), f.Type.End()))
		}
	}
	return b.String()
}

// goTypeSig renders a type declaration's head, e.g. "type Store struct".
func goTypeSig(fset *token.FileSet, src []byte, ts *ast.TypeSpec) string {
	var b strings.Builder
	b.WriteString("type ")
	b.WriteString(ts.Name.Name)
	if ts.TypeParams != nil {
		if s := srcBetween(src, fset, ts.TypeParams.Opening, ts.TypeParams.Closing); s != "" {
			b.WriteString(s)
		}
	}
	switch ts.Type.(type) {
	case *ast.StructType:
		b.WriteString(" struct")
	case *ast.InterfaceType:
		b.WriteString(" interface")
	default:
		if s := srcSlice(src, fset, ts.Type.Pos(), ts.Type.End()); s != "" {
			b.WriteString(" ")
			b.WriteString(s)
		}
	}
	return b.String()
}

// recvTypeName is the receiver's base type name ("Store" for "*Store").
func recvTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return recvTypeName(t.X)
	case *ast.IndexExpr:
		return recvTypeName(t.X)
	case *ast.IndexListExpr:
		return recvTypeName(t.X)
	case *ast.SelectorExpr:
		return t.Sel.Name
	}
	return ""
}

// srcSlice returns the source text between two positions (half-open).
func srcSlice(src []byte, fset *token.FileSet, from, to token.Pos) string {
	if !from.IsValid() || !to.IsValid() {
		return ""
	}
	f := fset.File(from)
	if f == nil {
		return ""
	}
	a, b := f.Offset(from), f.Offset(to)
	if a < 0 || b > len(src) || a > b {
		return ""
	}
	return string(src[a:b])
}

// srcBetween returns the source text from the token at `from` through the
// token at `to`, inclusive. Field lists (parameter and result lists) use it
// so the closing bracket is kept.
func srcBetween(src []byte, fset *token.FileSet, from, to token.Pos) string {
	if !from.IsValid() || !to.IsValid() {
		return ""
	}
	f := fset.File(from)
	if f == nil {
		return ""
	}
	a, b := f.Offset(from), f.Offset(to)+1
	if a < 0 || b > len(src) || a > b {
		return ""
	}
	return string(src[a:b])
}

// ---------- heuristic extraction ----------

// lang selects how a declaration's context block is delimited.
type lang int

const (
	langPython lang = iota
	langBraces
	langMarkdown
)

// heurRule matches one declaration form.
type heurRule struct {
	re   *regexp.Regexp
	kind string
	name int // capture group holding the symbol name
}

var pyRules = []heurRule{
	{regexp.MustCompile(`^\s*async\s+def\s+([A-Za-z_]\w*)\s*\(`), "def", 1},
	{regexp.MustCompile(`^\s*def\s+([A-Za-z_]\w*)\s*\(`), "def", 1},
	{regexp.MustCompile(`^\s*class\s+([A-Za-z_]\w*)\b`), "class", 1},
}

var jsRules = []heurRule{
	{regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*([A-Za-z_$][\w$]*)`), "function", 1},
	{regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][\w$]*)`), "class", 1},
	{regexp.MustCompile(`^\s*(?:export\s+)?(?:declare\s+)?const\s+([A-Za-z_$][\w$]*)\s*[:=]`), "const", 1},
}

var rustRules = []heurRule{
	{regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?(?:default\s+)?(?:const\s+)?(?:async\s+)?(?:unsafe\s+)?(?:extern\s+"[^"]*"\s+)?fn\s+([A-Za-z_]\w*)`), "fn", 1},
	{regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?struct\s+([A-Za-z_]\w*)`), "struct", 1},
	{regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?enum\s+([A-Za-z_]\w*)`), "enum", 1},
}

var mdRules = []heurRule{
	{regexp.MustCompile(`^(#{1,6})\s+(\S.*?)\s*$`), "heading", 2},
}

// maxSigBytes bounds a captured signature line.
const maxSigBytes = 160

// extractHeuristic pulls symbols out of a non-Go file with line patterns.
// It never fails: an unreadable shape simply yields fewer symbols.
func extractHeuristic(rel string, src []byte, rules []heurRule, l lang) []rawEntry {
	lines := strings.Split(strings.ReplaceAll(string(src), "\r\n", "\n"), "\n")
	var out []rawEntry
	inFence := false
	for i, raw := range lines {
		trimmed := strings.TrimSpace(raw)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		for _, rule := range rules {
			m := rule.re.FindStringSubmatch(raw)
			if m == nil {
				continue
			}
			name := m[rule.name]
			if name == "" {
				continue
			}
			sig := heuristicSig(raw, rule.kind, l)
			if sig == "" {
				continue
			}
			end := contextEnd(lines, i, l)
			out = append(out, rawEntry{
				Entry: Entry{
					Path:   rel,
					Kind:   rule.kind,
					Symbol: name,
					Sig:    sig,
					Line:   i + 1,
				},
				context: strings.Join(lines[i:end], "\n"),
			})
			break
		}
	}
	return out
}

// heuristicSig cleans a matched source line into a signature line.
func heuristicSig(raw, kind string, l lang) string {
	sig := strings.TrimSpace(raw)
	switch l {
	case langPython:
		sig = strings.TrimSuffix(sig, ":")
	case langBraces:
		sig = strings.TrimSuffix(sig, "{")
		sig = strings.TrimSpace(sig)
	case langMarkdown:
		// keep the heading markers: "## Usage"
	}
	if kind == "const" {
		// A constant's value is noise in a map: keep the name and type only.
		if i := strings.Index(sig, "="); i >= 0 {
			sig = strings.TrimSpace(sig[:i])
		}
	}
	if len(sig) > maxSigBytes {
		sig = sig[:maxSigBytes] + "…"
	}
	return sig
}

// contextEnd returns the end (exclusive) of a declaration's context block.
func contextEnd(lines []string, start int, l lang) int {
	switch l {
	case langPython:
		base := indentOf(lines[start])
		end := start + 1
		for end < len(lines) {
			line := lines[end]
			if strings.TrimSpace(line) == "" {
				end++
				continue
			}
			if indentOf(line) > base {
				end++
				continue
			}
			break
		}
		return end
	case langBraces:
		depth := 0
		end := start
		for end < len(lines) {
			depth += braceDelta(lines[end])
			end++
			if depth <= 0 {
				break
			}
			if end-start > 200 {
				break
			}
		}
		return end
	default:
		return start + 1
	}
}

func indentOf(line string) int {
	n := 0
	for _, r := range line {
		if r == ' ' {
			n++
			continue
		}
		if r == '\t' {
			n += 4
			continue
		}
		break
	}
	return n
}

func braceDelta(line string) int {
	d := 0
	inStr := rune(0)
	for _, r := range line {
		switch {
		case inStr != 0:
			if r == inStr {
				inStr = 0
			}
		case r == '"' || r == '\'' || r == '`':
			inStr = r
		case r == '{':
			d++
		case r == '}':
			d--
		}
	}
	return d
}

// ---------- tokens ----------

// identTokens splits text into identifier-like tokens.
func identTokens(s string) []string {
	var out []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r == '_' || unicode.IsLetter(r):
			b.WriteRune(r)
		case b.Len() > 0 && unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return out
}

func uniqueTokens(s string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, t := range identTokens(s) {
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}
