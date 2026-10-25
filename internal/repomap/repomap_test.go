package repomap

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// tokens mirrors the prompt stack's conservative estimate (~4 bytes/token).
func tokens(s string) int { return (len(s) + 3) / 4 }

func fixture(name string) string { return filepath.Join("testdata", name) }

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// genTree writes files×syms Go files with long signatures: enough to blow a
// small token budget.
func genTree(t *testing.T, root string, files, syms int) {
	t.Helper()
	for i := 0; i < files; i++ {
		var b strings.Builder
		b.WriteString("package gen\n\n")
		for j := 0; j < syms; j++ {
			fmt.Fprintf(&b,
				"// Symbol%d_%d is a deliberately long symbol name so the map cannot cheat the budget.\nfunc Symbol%d_%d(alpha int, beta string, gamma []byte) (int, error) {\n\treturn alpha, nil\n}\n\n",
				i, j, i, j)
		}
		writeFile(t, filepath.Join(root, fmt.Sprintf("pkg%02d", i), "file.go"), b.String())
	}
}

func TestBuildListsSymbolsAndIgnoresJunk(t *testing.T) {
	m, err := Build(Options{Root: fixture("tree")})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	r := m.Render()

	for _, want := range []string{
		"type Store struct",
		"func Load(name string) (*Store, error)",
		"func (Store) Get(ctx context.Context) (string, error)",
		`const DefaultStore = "default"`,
		"var Registry = map[string]*Store{}",
		"def build_tool(name)",
		"class Runner",
		"# Fixture",
		"## Usage",
	} {
		if !strings.Contains(r, want) {
			t.Errorf("map missing %q:\n%s", want, r)
		}
	}

	for _, unwanted := range []string{"node_modules", "vendor/", "secret.go", "hidden.go", "junk"} {
		if strings.Contains(r, unwanted) {
			t.Errorf("map must not mention %q:\n%s", unwanted, r)
		}
	}

	// The header states the file count and that this is a map.
	if !strings.HasPrefix(r, "## Repository map — ") {
		t.Errorf("map header missing:\n%s", r)
	}
	if !strings.Contains(r, "3 files") {
		t.Errorf("map header does not state the file count:\n%s", r)
	}
	if m.Truncated {
		t.Errorf("small tree must not be truncated:\n%s", r)
	}
	if m.Bytes != len(r) {
		t.Errorf("Bytes = %d, rendered %d", m.Bytes, len(r))
	}
	if got := tokens(r); got > DefaultMaxTokens {
		t.Errorf("map exceeds the token budget: %d > %d", got, DefaultMaxTokens)
	}
	// Entries are grouped by file: main.go's entries come before app.py's.
	if len(m.Entries) == 0 {
		t.Fatal("no entries extracted")
	}
	seen := map[string]bool{}
	last := ""
	for _, e := range m.Entries {
		if e.Path != last {
			if seen[e.Path] {
				t.Errorf("file %q is not grouped: %+v", e.Path, m.Entries)
			}
			seen[e.Path] = true
			last = e.Path
		}
	}
}

func TestBuildSkipsBrokenFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "ok.go"), "package ok\n\nfunc Fine() {}\n")
	// The fixture keeps the broken source as ".txt" so repository-wide
	// formatters do not choke on it; here it becomes a real Go file.
	broken, err := os.ReadFile(fixture(filepath.Join("tree", "broken", "bad.go.txt")))
	if err != nil {
		t.Fatalf("read broken fixture: %v", err)
	}
	writeFile(t, filepath.Join(root, "bad.go"), string(broken))

	m, err := Build(Options{Root: root})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	r := m.Render()
	if !strings.Contains(r, "func Fine()") {
		t.Errorf("good file missing:\n%s", r)
	}
	if strings.Contains(r, "Broken") {
		t.Errorf("broken file was mapped:\n%s", r)
	}
}

func TestRankingUsesReferencesAndIncludes(t *testing.T) {
	m, err := Build(Options{Root: fixture("rank")})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(m.Entries) == 0 {
		t.Fatal("no entries extracted")
	}
	// Hub is mentioned by two other symbols; it ranks first.
	if m.Entries[0].Symbol != "Hub" {
		t.Errorf("first entry = %q, want Hub:\n%s", m.Entries[0].Symbol, m.Render())
	}
	var refs int
	for _, e := range m.Entries {
		if e.Symbol == "Hub" {
			refs = e.Refs
		}
		if e.Symbol == "Lonely" && e.Refs != 0 {
			t.Errorf("unreferenced symbol has refs = %d", e.Refs)
		}
	}
	if refs != 2 {
		t.Errorf("Hub refs = %d, want 2", refs)
	}

	// Include promotes a path that would otherwise rank lower.
	inc, err := Build(Options{Root: fixture("rank"), Include: []string{"lonely.go"}})
	if err != nil {
		t.Fatalf("build with include: %v", err)
	}
	if inc.Entries[0].Symbol != "Lonely" {
		t.Errorf("include did not promote lonely.go: first = %+v", inc.Entries[0])
	}

	// A directory prefix promotes everything under it.
	dir, err := Build(Options{Root: fixture("rank"), Include: []string{"pkg"}})
	if err != nil {
		t.Fatalf("build with dir include: %v", err)
	}
	if dir.Entries[0].Path != "pkg/other.go" {
		t.Errorf("include did not promote pkg/: first = %+v", dir.Entries[0])
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	a, err := Build(Options{Root: fixture("tree")})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	b, err := Build(Options{Root: fixture("tree")})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if a.Render() != b.Render() {
		t.Errorf("renders differ:\n%s\n---\n%s", a.Render(), b.Render())
	}
	if !reflect.DeepEqual(a.Entries, b.Entries) {
		t.Errorf("entries differ:\n%+v\n%+v", a.Entries, b.Entries)
	}
	if a.Bytes != b.Bytes || a.Truncated != b.Truncated {
		t.Errorf("metadata differs: %+v vs %+v", a, b)
	}
}

func TestBudgetTruncatesAndSaysSo(t *testing.T) {
	root := t.TempDir()
	genTree(t, root, 40, 8)

	// The untruncated ranking, for comparison.
	full, err := Build(Options{Root: root, MaxTokens: 1 << 20})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if full.Truncated {
		t.Fatal("large budget should not truncate")
	}
	if len(full.Entries) == 0 {
		t.Fatal("no entries extracted")
	}

	m, err := Build(Options{Root: root, MaxTokens: 60})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	r := m.Render()
	if !m.Truncated {
		t.Errorf("small budget did not truncate:\n%s", r)
	}
	if !strings.Contains(r, "truncated") {
		t.Errorf("truncated map does not say so:\n%s", r)
	}
	if got := tokens(r); got > 60 {
		t.Errorf("truncated map over budget: %d > 60", got)
	}
	if m.Bytes != len(r) {
		t.Errorf("Bytes = %d, rendered %d", m.Bytes, len(r))
	}
	if len(m.Entries) == 0 {
		t.Error("truncated map dropped everything")
	}
	// The lowest-ranked entries are the ones dropped: the survivors are a
	// prefix of the untruncated ranking.
	for i, e := range m.Entries {
		if i >= len(full.Entries) {
			t.Fatalf("more entries than the full map (%d > %d)", len(m.Entries), len(full.Entries))
		}
		if e != full.Entries[i] {
			t.Fatalf("entry %d = %+v, want %+v (lowest-ranked entries must be dropped)", i, e, full.Entries[i])
		}
	}
}

func TestBudgetAlwaysHolds(t *testing.T) {
	root := t.TempDir()
	genTree(t, root, 20, 6)
	for _, budget := range []int{1, 5, 20, 64, 128, 512, 1000, 4096} {
		m, err := Build(Options{Root: root, MaxTokens: budget})
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		r := m.Render()
		if got := tokens(r); got > budget {
			t.Errorf("budget %d exceeded: %d tokens", budget, got)
		}
		if m.Bytes != len(r) {
			t.Errorf("budget %d: Bytes = %d, rendered %d", budget, m.Bytes, len(r))
		}
	}
}

func TestMaxFilesLimit(t *testing.T) {
	root := t.TempDir()
	genTree(t, root, 6, 2)

	m, err := Build(Options{Root: root, MaxFiles: 2})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	r := m.Render()
	if !m.Truncated {
		t.Errorf("file limit did not truncate:\n%s", r)
	}
	if !strings.Contains(r, "2 files") {
		t.Errorf("map does not state the capped file count:\n%s", r)
	}
	paths := distinctPaths(m.Entries)
	if len(paths) != 2 {
		t.Errorf("mapped %d files, want 2: %v", len(paths), paths)
	}
}

func TestBuildRejectsBadRoots(t *testing.T) {
	if _, err := Build(Options{}); err == nil {
		t.Error("empty root should fail")
	}
	if _, err := Build(Options{Root: filepath.Join(t.TempDir(), "nope")}); err == nil {
		t.Error("missing root should fail")
	}
}

func TestDefaultsAreApplied(t *testing.T) {
	m, err := Build(Options{Root: fixture("tree"), MaxTokens: 0, MaxFiles: 0})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if m.Truncated {
		t.Errorf("default budget truncated the fixture tree:\n%s", m.Render())
	}
	if len(m.Entries) == 0 {
		t.Error("default options produced no entries")
	}
}

func TestIgnoreMatching(t *testing.T) {
	ig := parseIgnore(`
# a comment
ignored/
hidden.go
/rooted.txt
*.log
!keep.log
`)
	cases := []struct {
		path string
		dir  bool
		want bool
	}{
		{"ignored", true, true},
		{"ignored/secret.go", false, true},
		{"deep/ignored", true, true},
		{"hidden.go", false, true},
		{"deep/hidden.go", false, true},
		{"rooted.txt", false, true},
		{"deep/rooted.txt", false, false},
		{"debug.log", false, true},
		{"keep.log", false, false},
		{"main.go", false, false},
	}
	for _, c := range cases {
		if got := ig.match(c.path, c.dir); got != c.want {
			t.Errorf("ignore(%q, dir=%v) = %v, want %v", c.path, c.dir, got, c.want)
		}
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"*.go", "main.go", true},
		{"*.go", "internal/main.go", false},
		{"internal/**", "internal/repo/main.go", true},
		{"internal/*.go", "internal/main.go", true},
		{"internal/*.go", "internal/repo/main.go", false},
		{"?ain.go", "main.go", true},
		{"?.go", "main.go", false},
	}
	for _, c := range cases {
		if got := globMatch(c.pattern, c.name); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func TestRankingTieBreakIsDeterministic(t *testing.T) {
	root := t.TempDir()
	// Two files with identical symbols: the path decides, every time.
	writeFile(t, filepath.Join(root, "b.go"), "package p\n\nfunc Same() {}\n")
	writeFile(t, filepath.Join(root, "a.go"), "package p\n\nfunc Same() {}\n")

	m, err := Build(Options{Root: root})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(m.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(m.Entries))
	}
	if m.Entries[0].Path != "a.go" || m.Entries[1].Path != "b.go" {
		t.Errorf("tie-break by path failed: %+v", m.Entries)
	}
}
