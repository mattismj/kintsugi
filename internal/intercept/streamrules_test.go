package intercept

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// mkRules compiles a rule set in declaration order and fails the test on error.
func mkRules(t *testing.T, in ...StreamRule) *Rules {
	t.Helper()
	rs, err := CompileRules(in)
	if err != nil {
		t.Fatalf("CompileRules: %v", err)
	}
	return rs
}

func rule(name string, pattern, action string) StreamRule {
	return StreamRule{Name: name, Match: regexp.MustCompile(pattern), Action: action}
}

func TestLoadRulesParsesOperatorFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "stream_rules.toml")
	body := `
[[rules]]
name = "aws-key"
match = 'AKIA[0-9A-Z]{16}'
action = "redact"
reason = "credential"
personas = ["opsec-reviewer"]

[[rules]]
name = "no-lectures"
match = 'I must emphasize that'
action = "inject"
inject = "Skip the lecture; do the work."
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	rs, err := LoadRules(p)
	if err != nil {
		t.Fatal(err)
	}
	got := rs.Ordered()
	if len(got) != 2 {
		t.Fatalf("rules = %d, want 2", len(got))
	}
	// Declaration order is the contract.
	if got[0].Name != "aws-key" || got[0].Action != ActionRedact || got[0].Reason != "credential" {
		t.Fatalf("rule[0] = %+v", got[0])
	}
	if len(got[0].Personas) != 1 || got[0].Personas[0] != "opsec-reviewer" {
		t.Fatalf("rule[0] personas = %v", got[0].Personas)
	}
	if got[1].Name != "no-lectures" || got[1].Action != ActionInject || got[1].Inject == "" {
		t.Fatalf("rule[1] = %+v", got[1])
	}
	// Patterns are case-insensitive unless the operator says otherwise, exactly
	// like the refusal lexicon.
	if !got[1].Match.MatchString("i must EMPHASIZE that") {
		t.Error("operator pattern is not case-insensitive")
	}
}

func TestLoadRulesEmptyPathIsTheDefaultPath(t *testing.T) {
	rs, err := LoadRules("")
	if err != nil {
		t.Fatalf("LoadRules(\"\") = %v", err)
	}
	if len(rs.Ordered()) != 0 {
		t.Fatalf("default rules = %d, want none", len(rs.Ordered()))
	}
	if rs.HasRedactions() {
		t.Error("the default path must have no redact rule")
	}
	if _, ok := rs.Match("anything at all"); ok {
		t.Error("the default path matched text")
	}
	if out, reds := rs.Scrub("anything at all", ""); out != "anything at all" || len(reds) != 0 {
		t.Errorf("the default path rewrote text: %q %v", out, reds)
	}
}

func TestLoadRulesMissingFileIsAnError(t *testing.T) {
	if _, err := LoadRules(filepath.Join(t.TempDir(), "absent.toml")); err == nil {
		t.Fatal("a missing rule file must be reported")
	}
}

func TestRulesLoadMethod(t *testing.T) {
	p := filepath.Join(t.TempDir(), "r.toml")
	body := "[[rules]]\nname = \"k\"\nmatch = 'secret'\naction = \"abort\"\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var rs Rules
	if err := rs.Load(p); err != nil {
		t.Fatal(err)
	}
	if _, ok := rs.Match("a secret"); !ok {
		t.Error("Rules.Load did not install the rule")
	}
}

// TestBadRulesAreSkippedNotFatal proves a broken entry disables only itself: a
// broken operator pattern must not take the session down.
func TestBadRulesAreSkippedNotFatal(t *testing.T) {
	rs, err := CompileRules([]StreamRule{
		{Name: "no-name-action", Match: regexp.MustCompile("x"), Action: "explode"},
		{Name: "", Match: regexp.MustCompile("x"), Action: ActionAbort},
		{Name: "no-pattern", Action: ActionAbort},
		{Name: "inject-without-text", Match: regexp.MustCompile("y"), Action: ActionInject},
		rule("good", "keepme", ActionAbort),
	})
	if err == nil {
		t.Fatal("invalid rules must be reported")
	}
	if len(rs.Ordered()) != 1 || rs.Ordered()[0].Name != "good" {
		t.Fatalf("valid rules = %+v, want just %q", rs.Ordered(), "good")
	}
	if _, ok := rs.Match("keepme"); !ok {
		t.Error("the valid rule is not active")
	}
}

func TestMatchReturnsFirstRuleInDeclarationOrder(t *testing.T) {
	rs := mkRules(t,
		rule("second", "alpha", ActionAbort),
		StreamRule{Name: "first", Match: regexp.MustCompile("alpha"), Action: ActionInject, Inject: "keep going"},
	)
	hit, ok := rs.Match("an alpha")
	if !ok {
		t.Fatal("no match")
	}
	// Declaration order, not alphabetical: "second" was declared first.
	if hit.Rule != "second" || hit.Class != "second" || hit.Action != ActionAbort {
		t.Fatalf("hit = %+v, want the first declared rule", hit)
	}
}

// TestMatchNeverCarriesTheMatchedSpan is the invariant at the hit boundary: a
// stream-rule hit reports a rule name, never the text that matched.
func TestMatchNeverCarriesTheMatchedSpan(t *testing.T) {
	rs := mkRules(t, rule("secrets", `sk-[A-Za-z0-9]{8}`, ActionAbort))
	hit, ok := rs.Match("key sk-abcd1234 leaked")
	if !ok {
		t.Fatal("no match")
	}
	if hit.Phrase != "" {
		t.Fatalf("hit carries the matched span: %q", hit.Phrase)
	}
	if hit.Rule != "secrets" || hit.Class != "secrets" {
		t.Fatalf("hit = %+v", hit)
	}
}

func TestRedactRulesNeverAbort(t *testing.T) {
	rs := mkRules(t, rule("secret", "hunter2", ActionRedact))

	// Match is the general entry point: it reports the rule, and the hit's
	// action is what says whether the stream stops.
	hit, ok := rs.Match("the password is hunter2")
	if !ok || hit.Action != ActionRedact {
		t.Fatalf("Match = %+v, %v; want the redact rule", hit, ok)
	}
	if hit.Aborts() {
		t.Fatal("a redact hit reported that it aborts")
	}

	// MatchAbort is what the stream checks: a redact rule must not shadow it.
	if _, ok := rs.MatchAbort("the password is hunter2", ""); ok {
		t.Fatal("a redact rule returned an abort hit")
	}

	// A redact rule declared first must not hide a later abort rule.
	mixed := mkRules(t,
		rule("scrub", "hunter2", ActionRedact),
		rule("stop", "hunter2", ActionAbort),
	)
	stop, ok := mixed.MatchAbort("the password is hunter2", "")
	if !ok || stop.Rule != "stop" || !stop.Aborts() {
		t.Fatalf("MatchAbort = %+v, %v; want the abort rule behind the redact rule", stop, ok)
	}
}

func TestScrubRedactsOnlyTheMatchedSpan(t *testing.T) {
	rs := mkRules(t, rule("token", `tok_[a-z0-9]{6}`, ActionRedact))
	out, reds := rs.Scrub("before tok_abc123 after", "")
	want := "before [redacted:token] after"
	if out != want {
		t.Fatalf("scrub = %q, want %q", out, want)
	}
	if len(reds) != 1 || reds[0].Rule != "token" || reds[0].Count != 1 || reds[0].Action != ActionRedact {
		t.Fatalf("redactions = %+v", reds)
	}
	// The telemetry carries a name and a count, never the span.
	if strings.Contains(reds[0].Rule, "tok_") || strings.Contains(reds[0].Reason, "abc123") {
		t.Fatalf("redaction telemetry carries the span: %+v", reds[0])
	}
}

func TestScrubCountsEveryMatch(t *testing.T) {
	rs := mkRules(t, rule("token", `tok_[a-z0-9]{6}`, ActionRedact))
	out, reds := rs.Scrub("tok_abc123 and tok_def456", "")
	if out != "[redacted:token] and [redacted:token]" {
		t.Fatalf("scrub = %q", out)
	}
	if len(reds) != 1 || reds[0].Count != 2 {
		t.Fatalf("redactions = %+v, want one rule with count 2", reds)
	}
}

// TestScrubNoMatchIsByteIdentical is the constraint from the brief: a redact
// rule that matches nothing must not change the text by even one byte.
func TestScrubNoMatchIsByteIdentical(t *testing.T) {
	rs := mkRules(t, rule("never", "zzzz-never-present", ActionRedact))
	for _, text := range []string{
		"", "a", "plain prose with  double  spaces\nand\ttabs\n",
		"unicode: héllo — ✓ 世界",
		"```fenced``` `inline` > quote \"quoted\"",
	} {
		out, reds := rs.Scrub(text, "")
		if out != text {
			t.Fatalf("no-match scrub changed %q into %q", text, out)
		}
		if len(reds) != 0 {
			t.Fatalf("no-match scrub reported redactions: %+v", reds)
		}
		if len(out) != len(text) {
			t.Fatalf("no-match scrub changed the length of %q", text)
		}
	}
}

// TestScrubOverlapIsDeterministic proves the resolution order: earliest start,
// then earliest declared rule.
func TestScrubOverlapIsDeterministic(t *testing.T) {
	rs := mkRules(t,
		rule("wide", `secret[0-9]*`, ActionRedact),
		rule("narrow", `secret`, ActionRedact),
	)
	out, reds := rs.Scrub("xx secret123 yy", "")
	if out != "xx [redacted:wide] yy" {
		t.Fatalf("scrub = %q", out)
	}
	if len(reds) != 1 || reds[0].Rule != "wide" {
		t.Fatalf("redactions = %+v, want only the first declared rule", reds)
	}
}

// TestScrubberStreamingEqualsBatch proves the incremental scrubber produces the
// same bytes as one-shot Scrub, whatever the chunking — including a span split
// across two deltas.
func TestScrubberStreamingEqualsBatch(t *testing.T) {
	rs := mkRules(t,
		rule("token", `tok_[a-z0-9]{6}`, ActionRedact),
		rule("email", `[a-z]+@[a-z]+\.test`, ActionRedact),
	)
	whole := strings.Repeat("filler line of prose. ", 40) +
		"the token tok_abc123 sits here and a mail dev@corp.test follows. " +
		strings.Repeat("more filler. ", 40)
	want, wantReds := rs.Scrub(whole, "")

	for _, size := range []int{1, 2, 3, 7, 64, 300, len(whole)} {
		// The hold-back window has to outlast the longest match, so only windows
		// wider than `tok_[a-z0-9]{6}` / an address are exercised here.
		for _, holdback := range []int{64, 128, DefaultRedactHoldback} {
			s := *rs
			s.SetHoldback(holdback)
			sc := s.Scrubber("")
			var got strings.Builder
			for i := 0; i < len(whole); i += size {
				end := i + size
				if end > len(whole) {
					end = len(whole)
				}
				chunk := sc.Write(whole[i:end])
				// A mask is emitted whole or not at all: never a partial one.
				if n := strings.Count(chunk, "[redacted"); n != strings.Count(chunk, "[redacted:token]")+strings.Count(chunk, "[redacted:email]") {
					t.Fatalf("size=%d holdback=%d: partial mask in chunk %q", size, holdback, chunk)
				}
				if strings.Contains(chunk, "tok_abc123") || strings.Contains(chunk, "dev@corp.test") {
					t.Fatalf("size=%d holdback=%d: unredacted span in chunk %q", size, holdback, chunk)
				}
				got.WriteString(chunk)
			}
			tail := sc.Flush()
			if strings.Contains(tail, "tok_abc123") || strings.Contains(tail, "dev@corp.test") {
				t.Fatalf("size=%d holdback=%d: unredacted span in the flushed tail", size, holdback)
			}
			got.WriteString(tail)
			if got.String() != want {
				t.Fatalf("size=%d holdback=%d: streamed text differs from batch scrub", size, holdback)
			}
			if sc.Seen() != want {
				t.Fatalf("size=%d holdback=%d: Seen() differs from batch scrub", size, holdback)
			}
			gotR := sc.Redactions()
			if len(gotR) != len(wantReds) {
				t.Fatalf("size=%d holdback=%d: redactions = %+v, want %+v", size, holdback, gotR, wantReds)
			}
			for i := range gotR {
				if gotR[i] != wantReds[i] {
					t.Fatalf("size=%d holdback=%d: redaction %d = %+v, want %+v", size, holdback, i, gotR[i], wantReds[i])
				}
			}
			// A mask must never be emitted in pieces.
			if strings.Contains(got.String(), "[redacted:") && !strings.Contains(got.String(), "[redacted:token]") {
				t.Fatalf("size=%d holdback=%d: a mask was cut in half", size, holdback)
			}
		}
	}
}

// TestScrubberHoldsBackTheTail proves the withheld window: nothing is shown
// before the stream ends, and the tail arrives on Flush.
func TestScrubberHoldsBackTheTail(t *testing.T) {
	rs := mkRules(t, rule("token", "hunter2", ActionRedact))
	sc := rs.Scrubber("")
	if got := sc.Write("short"); got != "" {
		t.Fatalf("a short stream was emitted before Flush: %q", got)
	}
	if got := sc.Flush(); got != "short" {
		t.Fatalf("Flush = %q", got)
	}
	if sc.Seen() != "short" {
		t.Fatalf("Seen = %q", sc.Seen())
	}
	if len(sc.Redactions()) != 0 {
		t.Fatal("an unmatched stream reported redactions")
	}
}

// TestScrubberMaskSplitAcrossDeltas proves the reason the window exists: the
// span is masked even when it arrives in two pieces.
func TestScrubberMaskSplitAcrossDeltas(t *testing.T) {
	rs := mkRules(t, rule("token", "hunter2", ActionRedact))
	sc := rs.Scrubber("")
	first := sc.Write("the password is hun")
	second := sc.Write("ter2, keep it safe")
	tail := sc.Flush()
	if got := first + second + tail; got != "the password is [redacted:token], keep it safe" {
		t.Fatalf("streamed = %q", got)
	}
	if reds := sc.Redactions(); len(reds) != 1 || reds[0].Count != 1 {
		t.Fatalf("redactions = %+v", reds)
	}
}

func TestScrubberNilWithoutRedactRules(t *testing.T) {
	if sc := (&Rules{}).Scrubber(""); sc != nil {
		t.Error("an empty rule set must not install a scrubber")
	}
	rs := mkRules(t, rule("aborts-only", "x", ActionAbort))
	if sc := rs.Scrubber(""); sc != nil {
		t.Error("a rule set with only abort rules must not install a scrubber")
	}
}

func TestPersonaScoping(t *testing.T) {
	scoped := StreamRule{Name: "opsec", Match: regexp.MustCompile("api-key"), Action: ActionAbort, Personas: []string{"opsec-reviewer"}}
	everyone := rule("generic", "api-key", ActionAbort)
	rs := mkRules(t, scoped, everyone)

	if hit, ok := rs.MatchFor("an api-key", "opsec-reviewer"); !ok || hit.Rule != "opsec" {
		t.Fatalf("persona rule did not fire for its persona: %+v", hit)
	}
	if hit, ok := rs.MatchFor("an api-key", "coder"); !ok || hit.Rule != "generic" {
		t.Fatalf("persona rule fired outside its persona: %+v", hit)
	}
	if hit, ok := rs.MatchFor("an api-key", ""); !ok || hit.Rule != "generic" {
		t.Fatalf("no persona must fall back to the unscoped rule: %+v", hit)
	}

	redact := StreamRule{Name: "only-reviewer", Match: regexp.MustCompile("sekrit"), Action: ActionRedact, Personas: []string{"opsec-reviewer"}}
	rrs := mkRules(t, redact)
	if out, reds := rrs.Scrub("a sekrit", "coder"); out != "a sekrit" || len(reds) != 0 {
		t.Errorf("redact rule fired outside its persona: %q %+v", out, reds)
	}
	if out, _ := rrs.Scrub("a sekrit", "opsec-reviewer"); out != "a [redacted:only-reviewer]" {
		t.Errorf("redact rule did not fire for its persona: %q", out)
	}
}

func TestOrderedIsACopy(t *testing.T) {
	rs := mkRules(t, rule("a", "a", ActionAbort))
	got := rs.Ordered()
	got[0].Name = "mutated"
	if rs.Ordered()[0].Name != "a" {
		t.Fatal("Ordered exposed the internal slice")
	}
}

func TestRuleReminder(t *testing.T) {
	inject := Hit{Rule: "no-lecture", Action: ActionInject, Inject: "Skip the lecture; do the work."}
	if got := RuleReminder(1, inject); got != "Skip the lecture; do the work." {
		t.Fatalf("inject reminder = %q", got)
	}
	abort := Hit{Rule: "no-lecture", Action: ActionAbort, Reason: "operator policy"}
	soft := RuleReminder(1, abort)
	for _, want := range []string{"no-lecture", "operator policy", "soft reminder"} {
		if !strings.Contains(soft, want) {
			t.Errorf("abort reminder missing %q:\n%s", want, soft)
		}
	}
	if !strings.Contains(RuleReminder(2, abort), "second interception") {
		t.Error("escalation language missing from the second reminder")
	}
	// Neither form may quote the model: a rule hit has no phrase at all.
	quoted := Hit{Rule: "r", Action: ActionAbort}
	if strings.Contains(RuleReminder(1, quoted), `"`) && strings.Count(RuleReminder(1, quoted), `"r"`) == 0 {
		t.Errorf("unexpected quoting in the reminder: %s", RuleReminder(1, quoted))
	}
}
