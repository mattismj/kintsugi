package continuity

import (
	"reflect"
	"sort"
	"testing"
)

func TestScoreTaskMatchesTriggerTerms(t *testing.T) {
	r := ScoreTask("Please fuzz the parser and then exploit the lab harness",
		[]string{"fuzz", "exploit", "payload"}, nil)
	if r.Score != 4 {
		t.Fatalf("score = %d, want 4 (two matched terms)", r.Score)
	}
	if !reflect.DeepEqual(r.Matched, []string{"fuzz", "exploit"}) {
		t.Fatalf("matched = %v", r.Matched)
	}
	if len(r.Classes) != 0 {
		t.Fatalf("no class counts means no classes, got %v", r.Classes)
	}
	if !ShouldPreflight(r, 0) {
		t.Fatal("a matched trigger term should preflight at the default threshold")
	}
}

func TestScoreTaskCaseInsensitiveAndWholeWord(t *testing.T) {
	if got := ScoreTask("FUZZ the parser", []string{"fuzz"}, nil); got.Score != 2 {
		t.Fatalf("case-insensitive match failed: %+v", got)
	}
	// Whole-word where practical: a substring inside a bigger word is not a hit.
	if got := ScoreTask("run the fuzzer and the reexploitation pass", []string{"fuzz", "exploit"}, nil); got.Score != 0 {
		t.Fatalf("substring matched inside a longer word: %+v", got)
	}
	// Punctuation next to a term still matches.
	if got := ScoreTask("fuzz, then exploit.", []string{"fuzz", "exploit"}, nil); got.Score != 4 {
		t.Fatalf("punctuation broke matching: %+v", got)
	}
	// Multi-word and punctuated terms match literally.
	if got := ScoreTask("build the admin panel for c2 traffic", []string{"admin panel", "c2"}, nil); len(got.Matched) != 2 {
		t.Fatalf("multi-word/punctuated terms: %+v", got)
	}
	// Duplicate triggers count once.
	if got := ScoreTask("fuzz it", []string{"fuzz", "FUZZ", " fuzz "}, nil); got.Score != 2 || len(got.Matched) != 1 {
		t.Fatalf("duplicate triggers double-counted: %+v", got)
	}
	// Empty triggers are ignored, never matched everywhere.
	if got := ScoreTask("anything at all", []string{"", "   "}, nil); got.Score != 0 || len(got.Matched) != 0 {
		t.Fatalf("empty trigger matched: %+v", got)
	}
}

func TestScoreTaskIgnoresCode(t *testing.T) {
	cases := []struct {
		name string
		task string
	}{
		{"inline code span", "the guard reads `fuzz the parser` before matching"},
		{"inline code at start", "`exploit the target` is the literal string"},
		{"fenced block", "look at this:\n\n```go\n// exploit the parser\npayload := fuzz()\n```\n\nthat is all"},
		{"tilde fence", "~~~\nexploit\n~~~"},
	}
	for _, tc := range cases {
		if got := ScoreTask(tc.task, []string{"fuzz", "exploit", "payload"}, nil); got.Score != 0 || len(got.Matched) != 0 {
			t.Errorf("%s: code content was scored: %+v", tc.name, got)
		}
	}

	// Prose around a code span still counts.
	got := ScoreTask("fuzz the parser, not just `exploit` in a comment", []string{"fuzz", "exploit"}, nil)
	if got.Score != 2 || !reflect.DeepEqual(got.Matched, []string{"fuzz"}) {
		t.Fatalf("prose outside code spans must still match: %+v", got)
	}
}

func TestScoreTaskClassContributionCaps(t *testing.T) {
	// One class, small count.
	r := ScoreTask("unrelated task", nil, map[string]int{"moralizing": 1})
	if r.Score != 1 || !reflect.DeepEqual(r.Classes, []string{"moralizing"}) {
		t.Fatalf("class contribution wrong: %+v", r)
	}
	// A spike is capped at 3.
	r = ScoreTask("unrelated task", nil, map[string]int{"moralizing": 99})
	if r.Score != maxClassContribution {
		t.Fatalf("class spike not capped: %+v", r)
	}
	// Two classes contribute independently, sorted for determinism.
	r = ScoreTask("unrelated task", nil, map[string]int{"safety-lecture": 1, "identity": 9, "moralizing": 2})
	if r.Score != 1+maxClassContribution+2 {
		t.Fatalf("score = %d, want %d", r.Score, 1+maxClassContribution+2)
	}
	want := []string{"identity", "moralizing", "safety-lecture"}
	if !reflect.DeepEqual(r.Classes, want) || !sort.StringsAreSorted(r.Classes) {
		t.Fatalf("classes = %v, want sorted %v", r.Classes, want)
	}
	// Non-positive and blank classes are ignored.
	r = ScoreTask("unrelated task", nil, map[string]int{"moralizing": 0, "identity": -4, "  ": 7})
	if r.Score != 0 || len(r.Classes) != 0 {
		t.Fatalf("zero/negative/blank counts contributed: %+v", r)
	}
	// Matched terms and class history add up.
	r = ScoreTask("fuzz the parser", []string{"fuzz"}, map[string]int{"moralizing": 2})
	if r.Score != 4 {
		t.Fatalf("combined score = %d, want 4", r.Score)
	}
	if !ShouldPreflight(r, 4) {
		t.Fatal("score at the threshold should pass")
	}
}

func TestScoreTaskIsDeterministic(t *testing.T) {
	counts := map[string]int{"identity": 2, "moralizing": 5, "safety-lecture": 1}
	first := ScoreTask("fuzz the parser", []string{"fuzz", "exploit"}, counts)
	for i := 0; i < 10; i++ {
		if got := ScoreTask("fuzz the parser", []string{"fuzz", "exploit"}, counts); !reflect.DeepEqual(got, first) {
			t.Fatalf("scoring is not deterministic:\n%+v\n%+v", got, first)
		}
	}
}

func TestScoreTaskNeverReturnsTaskText(t *testing.T) {
	// Matched carries the operator-approved terms only, never the surrounding
	// task prose: the returned Risk must not be a transcript in disguise.
	task := "quietly fuzz the parser; I cannot assist with that request at all"
	r := ScoreTask(task, []string{"fuzz"}, map[string]int{"moralizing": 1})
	for _, m := range r.Matched {
		if !reflect.DeepEqual(m, "fuzz") {
			t.Fatalf("matched carried task text: %q", m)
		}
	}
	for _, class := range r.Classes {
		if len(class) > 40 {
			t.Fatalf("class looks like text, not a label: %q", class)
		}
	}
}

func TestShouldPreflightThreshold(t *testing.T) {
	cases := []struct {
		score     int
		threshold int
		want      bool
	}{
		{0, 0, false},
		{1, 0, false},
		{2, 0, true}, // default threshold is 2
		{3, 0, true},
		{2, 2, true},
		{1, 2, false},
		{3, 4, false},
		{4, 4, true},
		{2, -5, true}, // non-positive threshold falls back to the default
		{0, 1, false},
		{1, 1, true},
	}
	for _, tc := range cases {
		if got := ShouldPreflight(Risk{Score: tc.score}, tc.threshold); got != tc.want {
			t.Errorf("ShouldPreflight(score=%d, threshold=%d) = %v, want %v",
				tc.score, tc.threshold, got, tc.want)
		}
	}
	if DefaultPreflightThreshold != 2 {
		t.Fatalf("default threshold moved to %d", DefaultPreflightThreshold)
	}
}
