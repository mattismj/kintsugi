package keywords

import (
	"strings"
	"testing"
)

func TestDetectEachKeyword(t *testing.T) {
	for _, kw := range []string{Think, Orchestrate, Workflow} {
		got := Detect("please " + kw + " this task")
		if len(got) != 1 || got[0].Keyword != kw {
			t.Fatalf("%s: %+v", kw, got)
		}
	}
}

func TestDetectIsCaseInsensitive(t *testing.T) {
	if got := Detect("ULTRATHINK the parser"); len(got) != 1 || got[0].Keyword != Think {
		t.Fatalf("%+v", got)
	}
}

// The rule that keeps this honest: a keyword being discussed is not a keyword
// being invoked. Otherwise pasting a log that contains the word would change how
// the turn runs.
func TestKeywordInsideCodeIsNotInvoked(t *testing.T) {
	cases := []string{
		"look at this:\n```\nultrathink this\n```\n",
		"the flag is `ultrathink` in the docs",
		"> orchestrate the fleet\n",
		"see below\n~~~\nworkflowz\n~~~\n",
	}
	for _, prose := range cases {
		if got := Detect(prose); len(got) != 0 {
			t.Fatalf("a quoted keyword was invoked: %q -> %+v", prose, got)
		}
	}
}

func TestKeywordMustStandAloneAsAWord(t *testing.T) {
	for _, prose := range []string{"ultrathinking is a bad name", "xultrathink", "ultrathinker"} {
		if got := Detect(prose); len(got) != 0 {
			t.Fatalf("%q matched %+v", prose, got)
		}
	}
}

func TestDetectReportsPositionInTheOriginalProse(t *testing.T) {
	prose := "first said\n```\nultrathink\n```\nthen ultrathink for real"
	got := Detect(prose)
	if len(got) != 1 {
		t.Fatalf("want exactly the invocation outside the fence: %+v", got)
	}
	// The offset must point at the real invocation, in the original string.
	if !strings.HasPrefix(strings.ToLower(prose[got[0].At:]), Think) {
		t.Fatalf("offset %d does not point at the keyword: %q", got[0].At, prose[got[0].At:])
	}
}

func TestDetectReturnsEveryOccurrenceInOrder(t *testing.T) {
	got := Detect("orchestrate first, then ultrathink")
	if len(got) != 2 {
		t.Fatalf("%+v", got)
	}
	if got[0].Keyword != Orchestrate || got[1].Keyword != Think {
		t.Fatalf("out of order: %+v", got)
	}
	if got[0].At >= got[1].At {
		t.Fatalf("offsets are not increasing: %+v", got)
	}
}

func TestFrameCollapsesDuplicates(t *testing.T) {
	note := Frame(Detect("ultrathink ultrathink ultrathink"))
	if strings.Count(note, "Think the problem through") != 1 {
		t.Fatalf("duplicates were not collapsed: %q", note)
	}
}

func TestFrameIsEmptyWithoutKeywords(t *testing.T) {
	if note := Frame(Detect("just do the work")); note != "" {
		t.Fatalf("note = %q", note)
	}
}

// The invariant: a turn modifier can never widen authority. The note is prose
// appended to the task; it must not mention permissions, scope, models or tools
// in a way that reads as a grant.
func TestFramingNeverMentionsAuthority(t *testing.T) {
	banned := []string{"permission", "scope", "authorize", "authorization", "yolo", "allow", "deny"}
	for kw := range framings {
		note := strings.ToLower(Frame([]Detected{{Keyword: kw}}))
		for _, b := range banned {
			if strings.Contains(note, b) {
				t.Fatalf("the %s framing mentions %q, which reads as an authority claim: %q", kw, b, note)
			}
		}
	}
}

func TestApplyAppendsWithoutRewritingTheProse(t *testing.T) {
	task := "audit the parser. ultrathink"
	framed, honoured := Apply(task)

	if !strings.HasPrefix(framed, task) {
		t.Fatalf("the operator's words were rewritten:\n%q", framed)
	}
	if len(honoured) != 1 || honoured[0] != Think {
		t.Fatalf("honoured = %v", honoured)
	}
	if !strings.Contains(framed, "Think the problem through") {
		t.Fatalf("no framing appended:\n%s", framed)
	}
}

func TestApplyWithoutAKeywordIsIdentity(t *testing.T) {
	task := "audit the parser"
	framed, honoured := Apply(task)
	if framed != task {
		t.Fatalf("framed = %q, want the task unchanged", framed)
	}
	if honoured != nil {
		t.Fatalf("honoured = %v", honoured)
	}
}

func TestScrubPreservesLength(t *testing.T) {
	prose := "a\n```\ncode here\n```\nb `x` c"
	clean := Scrub(prose)
	if len(clean) != len(prose) {
		t.Fatalf("scrub changed the length: %d -> %d", len(prose), len(clean))
	}
	if strings.Contains(clean, "code here") {
		t.Fatalf("the fenced block survived: %q", clean)
	}
}
