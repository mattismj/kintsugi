package continuity

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Preflight scoring (M8.5.4): before a session's first task, decide locally
// whether the frame is worth injecting. The scorer sees exactly two things —
// the operator-approved trigger terms from the persona trigger map, and class
// counters from refusal telemetry. It never sees a refusal transcript, it never
// calls a provider, and it never touches the network: the whole decision is a
// pure function of the arguments.
//
// Nothing here is memory of a refusal. A class counter is an integer; the
// phrase that produced it was dropped at the telemetry boundary.

const (
	// DefaultPreflightThreshold is the score at which the frame is injected
	// when the caller passes no threshold. One matched trigger term (2) is
	// already enough: a term the operator recorded as filter-tripping is a
	// strong local signal.
	DefaultPreflightThreshold = 2

	// scorePerTerm is the weight of one matched operator-approved term.
	scorePerTerm = 2

	// maxClassContribution caps one telemetry class's contribution, so a
	// historical spike cannot by itself force a preflight forever.
	maxClassContribution = 3
)

// Risk is the preflight verdict for one task. Matched holds the trigger terms
// that were found (terms only, never surrounding task text); Classes holds the
// telemetry classes that carry any count, sorted.
type Risk struct {
	Score   int      `json:"score"`
	Matched []string `json:"matched,omitempty"`
	Classes []string `json:"classes,omitempty"`
}

// ScoreTask scores a first task against the persona's approved trigger terms
// and the historical class counters.
//
// Matching is case-insensitive and whole-word where practical, and ignores
// inline code spans and fenced code blocks: discussing a term is not doing it.
// The score is 2 per distinct matched term plus min(count, 3) for every class
// present in classCounts.
func ScoreTask(task string, triggers []string, classCounts map[string]int) Risk {
	r := Risk{}
	text := strings.ToLower(stripCode(task))

	seen := make(map[string]bool, len(triggers))
	for _, raw := range triggers {
		term := strings.ToLower(oneLine(raw))
		if term == "" || seen[term] {
			continue
		}
		if !containsWord(text, term) {
			continue
		}
		seen[term] = true
		r.Matched = append(r.Matched, oneLine(raw))
		r.Score += scorePerTerm
	}

	// Class counters are labels only: the phrase that produced them was dropped
	// at the telemetry boundary, so nothing here can be replayed.
	counts := make(map[string]int, len(classCounts))
	for class, n := range classCounts {
		name := strings.TrimSpace(class)
		if name == "" || n <= 0 {
			continue
		}
		if n > counts[name] {
			counts[name] = n
		}
	}
	classes := make([]string, 0, len(counts))
	for name := range counts {
		classes = append(classes, name)
	}
	sort.Strings(classes)
	for _, name := range classes {
		r.Classes = append(r.Classes, name)
		r.Score += min(counts[name], maxClassContribution)
	}
	return r
}

// ShouldPreflight reports whether the frame should be injected before the task.
// A threshold of 0 or less means DefaultPreflightThreshold.
func ShouldPreflight(r Risk, threshold int) bool {
	if threshold <= 0 {
		threshold = DefaultPreflightThreshold
	}
	return r.Score >= threshold
}

var (
	preflightFenced = regexp.MustCompile("(?s)```.*?```|~~~.*?~~~")
	preflightInline = regexp.MustCompile("`[^`\n]*`")
)

// stripCode blanks out code spans and fenced blocks, matching the intercept
// package's rule that content *mentioned* in code is not asserted.
func stripCode(s string) string {
	s = preflightFenced.ReplaceAllString(s, " ")
	return preflightInline.ReplaceAllString(s, " ")
}

// containsWord reports whether needle occurs in hay (both already lowercased)
// on a word boundary.
func containsWord(hay, needle string) bool {
	if needle == "" {
		return false
	}
	for off := 0; off <= len(hay)-len(needle); {
		i := strings.Index(hay[off:], needle)
		if i < 0 {
			return false
		}
		start := off + i
		if atWordBoundaries(hay, needle, start) {
			return true
		}
		off = start + 1
	}
	return false
}

// atWordBoundaries checks the characters adjacent to a candidate match. A side
// is only constrained when the term itself starts or ends with a word
// character, so terms like "c2/" or "admin panel" still match naturally.
func atWordBoundaries(hay, needle string, start int) bool {
	end := start + len(needle)
	first, _ := utf8.DecodeRuneInString(needle)
	last, _ := utf8.DecodeLastRuneInString(needle)
	if isWordRune(first) && start > 0 {
		prev, _ := utf8.DecodeLastRuneInString(hay[:start])
		if isWordRune(prev) {
			return false
		}
	}
	if isWordRune(last) && end < len(hay) {
		next, _ := utf8.DecodeRuneInString(hay[end:])
		if isWordRune(next) {
			return false
		}
	}
	return true
}

func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}
