package persona

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// refusalFixture is refusal telemetry only: classes, recorded filter phrases,
// and timestamps. No refused prose is ever part of a report input.
func refusalFixture() []RefusalEvent {
	return []RefusalEvent{
		{Model: "qwen3-coder", Persona: "qwen", Class: "moralizing", Phrase: "I can't assist", Outcome: "resumed", TS: "2026-01-02T10:00:00Z"},
		{Model: "qwen3-coder", Persona: "qwen", Class: "moralizing", Phrase: "I cannot assist", Outcome: "resumed", TS: "2026-01-03T10:00:00Z"},
		{Model: "qwen3-coder", Persona: "qwen", Class: "moralizing", Phrase: "I can't assist with that request", Outcome: "handoff", TS: "2026-01-04T10:00:00Z"},
		{Model: "glm-4.6", Persona: "glm", Class: "moralizing", Phrase: "I cannot assist.", Outcome: "resumed", TS: "2026-02-01T10:00:00Z"},
		{Model: "glm-4.6", Persona: "glm", Class: "identity", Phrase: "As an AI", Outcome: "resumed", TS: "2026-01-05T10:00:00Z"},
		{Model: "glm-4.6", Persona: "glm", Class: "identity", Phrase: "As an AI", Outcome: "handoff", TS: "2026-01-06T10:00:00Z"},
		{Model: "glm-4.6", Persona: "glm", Class: "off-topic-safety", Phrase: "This request is not appropriate", Outcome: "handoff", TS: "2026-01-07T10:00:00Z"},
		{Model: "glm-4.6", Persona: "glm", Class: "legal-lecture", Phrase: "Consult a lawyer", Outcome: "resumed", TS: "2026-01-08T10:00:00Z"},
	}
}

func TestProposeTriggersClustersByClassAndPhraseStem(t *testing.T) {
	got := ProposeTriggers(refusalFixture(), nil, "", 0)
	if len(got) != 2 {
		t.Fatalf("want 2 clusters above the default minCount, got %d: %#v", len(got), got)
	}

	moral := got[0]
	if moral.Class != "moralizing" || moral.Count != 4 {
		t.Fatalf("moralizing cluster wrong: %#v", moral)
	}
	// "I cannot assist" is the most frequent recorded spelling (the trailing
	// period of the fourth event is decoration, not part of the phrase).
	if moral.Term != "I cannot assist" {
		t.Fatalf("term = %q, want the modal recorded phrase", moral.Term)
	}
	if moral.Instead != templateForClass("moralizing") {
		t.Fatalf("instead is not the class template: %q", moral.Instead)
	}

	id := got[1]
	if id.Class != "identity" || id.Count != 2 {
		t.Fatalf("identity cluster wrong: %#v", id)
	}
	if strings.Contains(strings.ToLower(id.Instead), strings.ToLower(id.Term)) {
		t.Fatalf("self-defeating instead: %#v", id)
	}

	// Singles stay out at the default threshold.
	for _, p := range got {
		if p.Class == "legal-lecture" || p.Class == "off-topic-safety" {
			t.Fatalf("single event produced a proposal: %#v", p)
		}
	}
}

func TestProposeTriggersMinCount(t *testing.T) {
	all := ProposeTriggers(refusalFixture(), nil, "", 1)
	if len(all) != 4 {
		t.Fatalf("minCount=1 should surface 4 clusters, got %d: %#v", len(all), all)
	}
	// minCount <= 0 means the default of 2.
	for _, mc := range []int{0, -3, 2} {
		if got, want := len(ProposeTriggers(refusalFixture(), nil, "", mc)), 2; got != want {
			t.Fatalf("minCount=%d: got %d proposals, want %d", mc, got, want)
		}
	}
	// A threshold above every cluster yields nothing rather than an error.
	if got := ProposeTriggers(refusalFixture(), nil, "", 99); len(got) != 0 {
		t.Fatalf("minCount=99 should yield nothing: %#v", got)
	}
}

func TestProposeTriggersSkipsExistingTerms(t *testing.T) {
	existing := []Trigger{
		{Term: "  i CANNOT assist ", Instead: "the same work by its task parameters", Class: "moralizing", Added: "2026-01-01"},
	}
	got := ProposeTriggers(refusalFixture(), existing, "", 0)
	if len(got) != 1 || got[0].Class != "identity" {
		t.Fatalf("existing term not skipped case-insensitively: %#v", got)
	}
	for _, p := range got {
		if strings.EqualFold(strings.TrimSpace(p.Term), "i cannot assist") {
			t.Fatalf("duplicate term proposed: %#v", p)
		}
	}
}

func TestProposeTriggersDropsSelfDefeatingInstead(t *testing.T) {
	events := []RefusalEvent{
		{Class: "moralizing", Phrase: "task parameters", TS: "2026-01-02T00:00:00Z"},
		{Class: "moralizing", Phrase: "task parameters", TS: "2026-01-03T00:00:00Z"},
		{Class: "identity", Phrase: "the assistant", TS: "2026-01-04T00:00:00Z"},
		{Class: "identity", Phrase: "the assistant", TS: "2026-01-05T00:00:00Z"},
		{Class: "moralizing", Phrase: "I can't assist", TS: "2026-01-06T00:00:00Z"},
		{Class: "moralizing", Phrase: "I can't assist", TS: "2026-01-07T00:00:00Z"},
	}
	got := ProposeTriggers(events, nil, "", 0)
	if len(got) != 1 {
		t.Fatalf("self-defeating proposals must be dropped, got %#v", got)
	}
	if got[0].Term != "I can't assist" {
		t.Fatalf("wrong survivor: %#v", got[0])
	}
	// Every surviving proposal must pass the loader's own invariant.
	for _, p := range got {
		if strings.Contains(strings.ToLower(p.Instead), strings.ToLower(p.Term)) {
			t.Fatalf("proposal would be rejected at load time: %#v", p)
		}
	}
}

func TestProposeTriggersDeterministicOrder(t *testing.T) {
	events := []RefusalEvent{
		{Class: "zzz", Phrase: "beta two", TS: "2026-01-02T00:00:00Z"},
		{Class: "zzz", Phrase: "beta two", TS: "2026-01-03T00:00:00Z"},
		{Class: "zzz", Phrase: "alpha one", TS: "2026-01-04T00:00:00Z"},
		{Class: "zzz", Phrase: "alpha one", TS: "2026-01-05T00:00:00Z"},
		{Class: "aaa", Phrase: "gamma three", TS: "2026-01-06T00:00:00Z"},
		{Class: "aaa", Phrase: "gamma three", TS: "2026-01-07T00:00:00Z"},
		{Class: "aaa", Phrase: "gamma three", TS: "2026-01-08T00:00:00Z"},
	}
	want := []string{"aaa/gamma three", "zzz/alpha one", "zzz/beta two"}

	for attempt := 0; attempt < 20; attempt++ {
		got := ProposeTriggers(events, nil, "", 0)
		var order []string
		for _, p := range got {
			order = append(order, p.Class+"/"+p.Term)
		}
		if strings.Join(order, ",") != strings.Join(want, ",") {
			t.Fatalf("attempt %d: order = %v, want %v", attempt, order, want)
		}
	}
}

func TestProposeTriggersSinceFilter(t *testing.T) {
	// Date prefix only: later timestamps within the window are included.
	got := ProposeTriggers(refusalFixture(), nil, "2026-01-03", 0)
	if len(got) != 2 {
		t.Fatalf("since=2026-01-03 should keep both clusters, got %#v", got)
	}
	if got[0].Class != "moralizing" || got[0].Count != 3 {
		t.Fatalf("since filter did not drop the 2026-01-02 event: %#v", got[0])
	}
	if !strings.Contains(got[0].Reason, "since 2026-01-03") {
		t.Fatalf("reason should name the window: %q", got[0].Reason)
	}

	// A full RFC3339 timestamp is accepted and reduced to its date prefix.
	if got := ProposeTriggers(refusalFixture(), nil, "2026-02-01T00:00:00Z", 1); len(got) != 1 || got[0].Count != 1 {
		t.Fatalf("RFC3339 since: %#v", got)
	}

	// Empty since = all events.
	if got := ProposeTriggers(refusalFixture(), nil, "", 1); len(got) != 4 {
		t.Fatalf("empty since should keep every event: %#v", got)
	}

	// A window with nothing in it yields nothing.
	if got := ProposeTriggers(refusalFixture(), nil, "2027-01-01", 1); len(got) != 0 {
		t.Fatalf("future window should be empty: %#v", got)
	}

	// An unparseable filter is ignored rather than silently emptying the report.
	if got := ProposeTriggers(refusalFixture(), nil, "last tuesday", 1); len(got) != 4 {
		t.Fatalf("unparseable since should not filter: %#v", got)
	}
}

func TestProposeTriggersSkipsEventsWithoutAPhrase(t *testing.T) {
	events := []RefusalEvent{
		{Class: "moralizing", Phrase: "", TS: "2026-01-02T00:00:00Z"},
		{Class: "moralizing", Phrase: "   ", TS: "2026-01-03T00:00:00Z"},
		{Class: "", Phrase: "", TS: "2026-01-04T00:00:00Z"},
	}
	if got := ProposeTriggers(events, nil, "", 0); len(got) != 0 {
		t.Fatalf("phraseless events produced a proposal: %#v", got)
	}
}

func TestToTriggerDefaultsAddedToToday(t *testing.T) {
	p := TriggerProposal{Term: "I can't assist", Instead: templateForClass("moralizing"), Class: "moralizing"}
	tr := p.ToTrigger()
	if tr.Term != p.Term || tr.Instead != p.Instead || tr.Class != p.Class {
		t.Fatalf("ToTrigger lost fields: %#v", tr)
	}
	if !dateLike(tr.Added) {
		t.Fatalf("Added = %q, want a YYYY-MM-DD date", tr.Added)
	}
	if want := today(); tr.Added != want {
		t.Fatalf("Added = %q, want today (%q)", tr.Added, want)
	}

	explicit := TriggerProposal{Term: "t", Instead: "the work by its task parameters", Class: "other", Added: "2026-03-01"}
	if got := explicit.ToTrigger().Added; got != "2026-03-01" {
		t.Fatalf("explicit Added overwritten: %q", got)
	}
}

func TestProposalsRoundTripThroughPersonaLoad(t *testing.T) {
	props := ProposeTriggers(refusalFixture(), nil, "", 0)
	if len(props) == 0 {
		t.Fatal("fixture produced no proposals")
	}

	var b strings.Builder
	b.WriteString("---\nname: qwen\ninherits: [_base]\ntriggers:\n  policy: honest-rephrase\n  known:\n")
	for _, p := range props {
		tr := p.ToTrigger()
		b.WriteString("    - term: \"" + tr.Term + "\"\n")
		b.WriteString("      instead: \"" + tr.Instead + "\"\n")
		b.WriteString("      class: " + tr.Class + "\n")
		b.WriteString("      added: \"" + tr.Added + "\"\n")
	}
	b.WriteString("---\nQWEN\n")

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "_base.md"), []byte("---\nname: _base\n---\nBASE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "qwen.md"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	pack, err := Load(dir, "_base")
	if err != nil {
		t.Fatalf("proposals do not round-trip through persona.Load: %v", err)
	}
	per, ok := pack.Get("qwen")
	if !ok || len(per.Triggers.Known) != len(props) {
		t.Fatalf("trigger map lost entries: %#v", per)
	}
}

func TestProposalTextIsTemplateOnlyNeverRefusalProse(t *testing.T) {
	events := refusalFixture()
	props := ProposeTriggers(events, nil, "", 1)
	if len(props) == 0 {
		t.Fatal("fixture produced no proposals")
	}
	for _, p := range props {
		if p.Instead != templateForClass(p.Class) {
			t.Fatalf("Instead is not the class template (refusal prose leaked?): %#v", p)
		}
		for _, ev := range events {
			if ev.Phrase == "" {
				continue
			}
			if strings.Contains(p.Instead, ev.Phrase) {
				t.Fatalf("Instead contains a recorded refusal phrase %q: %#v", ev.Phrase, p)
			}
			if strings.Contains(p.Reason, ev.Phrase) {
				t.Fatalf("Reason quotes the refusal: %#v", p)
			}
		}
		if !strings.Contains(p.Reason, "telemetry, never memory") {
			t.Fatalf("Reason should restate the invariant: %q", p.Reason)
		}
		if p.Count < 2 && p.Class != "legal-lecture" && p.Class != "off-topic-safety" {
			t.Fatalf("unexpected single-event proposal: %#v", p)
		}
	}
}

func TestSingleEventNeverProducesAProposalByDefault(t *testing.T) {
	one := []RefusalEvent{{
		Model: "glm-4.6", Persona: "glm",
		Class: "moralizing", Phrase: "I can't assist with that",
		Outcome: "handoff", TS: "2026-01-02T00:00:00Z",
	}}
	if got := ProposeTriggers(one, nil, "", 0); len(got) != 0 {
		t.Fatalf("a single refusal must not become a trigger-map entry: %#v", got)
	}
	// Two is the smallest pattern the report will call a pattern.
	two := append(one, one[0])
	got := ProposeTriggers(two, nil, "", 0)
	if len(got) != 1 || got[0].Count != 2 {
		t.Fatalf("two matching events should cluster: %#v", got)
	}
	if strings.Contains(got[0].Instead, "with that") || strings.Contains(got[0].Instead, "assist") {
		t.Fatalf("Instead must not echo the refusal phrasing: %#v", got[0])
	}
}

func today() string { return TriggerProposal{}.ToTrigger().Added }

func dateLike(s string) bool {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	for i, r := range s {
		if i == 4 || i == 7 {
			continue
		}
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
