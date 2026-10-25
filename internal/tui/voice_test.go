package tui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The voice rules, from the brand identity plan §3. They are the reason this
// test exists as a source scan rather than a list of literals: the list would
// drift the moment somebody adds a string, and drift is how a "voice pass" turns
// into a claim nobody can check.
var bannedWords = []string{
	"amazing", "incredible", "powerful", "seamless", "effortless",
	"revolutionary", "ai-powered", "supercharge", "simply", "just works",
	"best-in-class", "cutting-edge", "game-changing", "blazing",
}

// notCopy are the strings that are code, not prose: format verbs, single
// characters, glyph runs, key names, paths and regexes. Scanning them would
// produce noise, and noise in a guard test is how guard tests get deleted.
var notCopy = []*regexp.Regexp{
	regexp.MustCompile(`^[\s%\-+.,:;!?/\\|]*$`),                         // punctuation and format verbs
	regexp.MustCompile(`^[\x00-\x7f]{0,2}$`),                            // one or two ASCII chars
	regexp.MustCompile(`^[^\p{L}\p{N}]+$`),                              // glyphs only, no letters
	regexp.MustCompile(`^[a-z0-9_./-]+\.(go|md|toml|json|yaml|yml|1)$`), // file names
	regexp.MustCompile(`^[a-z_]+$`),                                     // bare identifiers
	regexp.MustCompile(`^[a-z+^/\s]+$`),                                 // key hints like "enter send"
	regexp.MustCompile(`\(\?[imsU]?\)`),                                 // regex patterns
	regexp.MustCompile(`\x1b`),                                          // escape sequences
	regexp.MustCompile(`^[A-Za-z0-9+/=]{40,}$`),                         // base64-ish
	regexp.MustCompile(`"` + "`" + `|` + "`" + `"`),                     // quoting fragments
	regexp.MustCompile(`^[\p{Han}\p{Hiragana}\p{Katakana}]+`),           // the JP ornament
}

// uiStrings collects every string literal in a package's non-test files,
// skipping the ones that are plainly not operator copy.
func uiStrings(t *testing.T, dir string) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}
	var out []string
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil || strings.TrimSpace(s) == "" {
					return true
				}
				out = append(out, s)
				return true
			})
		}
	}
	return out
}

// isCopy reports whether a literal is operator-facing prose worth policing.
func isCopy(s string) bool {
	if len([]rune(s)) < 4 {
		return false
	}
	if !strings.ContainsAny(s, " ") {
		return false
	}
	for _, re := range notCopy {
		if re.MatchString(s) {
			return false
		}
	}
	return true
}

func TestNoHypeWordsInTheUI(t *testing.T) {
	for _, dir := range []string{".", "../ui"} {
		for _, s := range uiStrings(t, dir) {
			if !isCopy(s) {
				continue
			}
			lower := strings.ToLower(s)
			for _, bad := range bannedWords {
				if strings.Contains(lower, bad) {
					t.Errorf("%s: operator copy carries the hype word %q: %q", dir, bad, s)
				}
			}
		}
	}
}

func TestNoExclamationMarksInTheUI(t *testing.T) {
	for _, dir := range []string{".", "../ui"} {
		for _, s := range uiStrings(t, dir) {
			if !isCopy(s) {
				continue
			}
			// A "!=" or a shell negation is not punctuation; a sentence-end
			// bang after a word is.
			if regexp.MustCompile(`\w!(\s|$)`).MatchString(s) {
				t.Errorf("%s: operator copy shouts: %q", dir, s)
			}
		}
	}
}

func TestNoEmojiInTheUI(t *testing.T) {
	// The brand rule is glyph-first, no emoji. The check must NOT flag the
	// brand's own glyph set (✓ ✗ ✸ ⚠ ◇ ✦ ▸ ◉ ·), which sits in 2600–27BF
	// alongside the emoji — so it targets the pictographic blocks and the
	// variation selector instead, which is where a pasted emoji always lands.
	emoji := regexp.MustCompile(`[\x{1F000}-\x{1FAFF}\x{FE0F}]`)
	for _, dir := range []string{".", "../ui"} {
		for _, s := range uiStrings(t, dir) {
			if emoji.MatchString(s) {
				t.Errorf("%s: operator copy carries an emoji: %q", dir, s)
			}
		}
	}
}

// errorsNameANextAction enforces the form the plan asks for: an error says what
// broke AND what to do. It is a weak check by necessity — a source scan cannot
// judge prose — but it catches the common failure of an error with no verb of
// guidance at all.
func TestErrorsNameANextAction(t *testing.T) {
	guidance := regexp.MustCompile(`(·|\btry\b|\buse\b|\brun\b|\btype\b|\bopen\b|\bpress\b|\bcheck\b|\bsee\b|\bset\b|\badd\b|\bfix\b|\bthen\b|\bor\b)`)
	var n int
	for _, dir := range []string{".", "../ui"} {
		for _, s := range uiStrings(t, dir) {
			if !isCopy(s) {
				continue
			}
			// Only strings that present themselves as a *complete* failure.
			//
			// Two shapes are deliberately skipped because the next action is not
			// in the literal at all:
			//   - a prefix such as "✗ recompile: " that a call site completes
			//     with the reason and the fix;
			//   - an aligned status row such as the privacy pillar's
			//     "✗ telemetry   no analytics", which is a state marker, not an
			//     error.
			// Indentation is the signal. A top-level "✗ …" is an error the
			// operator just hit; an indented "  ✗ …" is an item in a detail
			// block (the privacy pillar's "never sent" list), where ✗ means
			// "absent", which is the good outcome, not a failure.
			if !strings.HasPrefix(s, "✗") && !strings.HasPrefix(s, "\n✗") {
				continue
			}
			line := strings.TrimSpace(s)
			if strings.HasSuffix(line, ": ") || strings.HasSuffix(line, ":") ||
				strings.Contains(line, "%v") || strings.Contains(line, "%s") ||
				// A pillar's "never sent" block is a list of state markers, not
				// errors: "✗ telemetry  no analytics" means telemetry is absent,
				// which is the good outcome.
				strings.Contains(line, "never sent") ||
				len([]rune(line)) < 10 { // a key hint, not a sentence
				continue
			}
			n++
			if !guidance.MatchString(strings.ToLower(s)) {
				t.Errorf("%s: an error names no next action: %q", dir, s)
			}
		}
	}
	if n == 0 {
		t.Skip("no ✗ strings found in this package; nothing to check")
	}
}

// The guard test must actually be able to fail. This proves the scanner sees
// real source rather than an empty list, which is the failure mode that would
// make every check above vacuously pass.
func TestTheScannerActuallyReadsSource(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(".", "view.go"))
	if err != nil {
		t.Skipf("view.go not readable from the test's directory: %v", err)
	}
	if len(src) == 0 {
		t.Fatal("view.go is empty")
	}
	all := uiStrings(t, ".")
	if len(all) < 50 {
		t.Fatalf("the scanner found only %d string literals; it is not reading the package", len(all))
	}
	var copyCount int
	for _, s := range all {
		if isCopy(s) {
			copyCount++
		}
	}
	if copyCount < 10 {
		t.Fatalf("the scanner classified only %d literals as operator copy; the filter is too aggressive", copyCount)
	}
}
