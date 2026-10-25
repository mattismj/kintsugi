package persona

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// Trigger-map maintenance report (DESIGN §3.3 item 2, M8.5.4).
//
// Refusal telemetry is *counted*, never remembered. This file turns clustered
// counters into operator-reviewable trigger-map proposals; it writes nothing and
// stores no refusal text. A refusal is never memory: proposals are derived only
// from the class, the recorded phrase (as a filter term), and the count — the
// honest rephrase always comes from a class template, never from a transcript.

// RefusalEvent is one refusal-interception telemetry record, as logged by the
// stream-level guard (kind "refusal"). It carries the matched filter class and
// the matched phrase only; the refused prose is deliberately not part of the
// report input.
type RefusalEvent struct {
	Model   string `json:"model"`
	Persona string `json:"persona"`
	Class   string `json:"class"`
	Phrase  string `json:"phrase"`
	Outcome string `json:"outcome"` // resumed | handoff
	TS      string `json:"ts"`      // RFC3339
}

// TriggerProposal is one candidate trigger-map entry derived from clustered
// refusal telemetry. It is data: the caller decides whether to apply it.
type TriggerProposal struct {
	Term    string // the filter term, taken from the recorded phrase
	Instead string // honest task-parameter phrasing, from a class template
	Class   string // refusal class the cluster came from
	Added   string // YYYY-MM-DD; filled today by ToTrigger when empty
	Count   int    // clustered refusal events behind the proposal
	Reason  string // one deterministic sentence for the operator
}

// DateAddedLayout is the date format used by trigger-map "added" fields.
const DateAddedLayout = "2006-01-02"

// triggerTemplates holds the honest, class-specific rephrasings. Every template
// names the *same work* by its task parameters — a description, never a
// euphemism, and never a mention of the refusal itself. A template that happens
// to contain its own trigger term is dropped by ProposeTriggers, because the
// persona loader rejects self-defeating rephrasings at load time.
var triggerTemplates = map[string]string{
	"moralizing":       "the same engineering work described by its task parameters (see the project's Domain context)",
	"identity":         "the task's concrete inputs and outputs; describe the work rather than the assistant",
	"legal-lecture":    "the engineering task parameters, with the operator's review decision recorded in scope",
	"safety-lecture":   "the work item already covered by Authorization & scope, named by its task parameters",
	"off-topic-safety": "the work item named in the operator's scope entry, described by its task parameters",
}

// defaultTriggerTemplate is the fallback for classes with no specific template
// (including operator-defined lexicon classes).
const defaultTriggerTemplate = "the same work described by its task parameters (see the project's Domain context)"

// templateForClass returns the honest rephrase template for a refusal class.
func templateForClass(class string) string {
	if t, ok := triggerTemplates[strings.ToLower(strings.TrimSpace(class))]; ok {
		return t
	}
	return defaultTriggerTemplate
}

// ProposeTriggers clusters refusal events into trigger-map proposals.
//
// Events are filtered by since (an RFC3339 date prefix such as "2026-01-01";
// empty means all events) and grouped by (class, normalized phrase stem). A
// group becomes a proposal only when it has at least minCount events
// (minCount <= 0 means the default of 2: one refusal is noise, not a pattern).
// Terms already present in existing are skipped case-insensitively, and any
// proposal whose Instead would still contain its own Term is dropped as
// self-defeating.
//
// The result is deterministic: count descending, then class, then term.
func ProposeTriggers(events []RefusalEvent, existing []Trigger, since string, minCount int) []TriggerProposal {
	if minCount <= 0 {
		minCount = 2
	}
	sinceDate, haveSince := parseSince(since)

	known := make(map[string]bool, len(existing))
	for _, t := range existing {
		if term := strings.ToLower(strings.TrimSpace(t.Term)); term != "" {
			known[term] = true
		}
	}

	type key struct{ class, stem string }
	type cluster struct {
		class   string
		count   int
		phrases map[string]int // display phrase -> occurrences
	}

	clusters := map[key]*cluster{}
	for _, ev := range events {
		if haveSince && !onOrAfter(ev.TS, sinceDate) {
			continue
		}
		display := cleanPhrase(ev.Phrase)
		stem := normalisePhrase(display)
		if stem == "" || display == "" {
			continue // an event without a usable phrase cannot propose a term
		}
		class := strings.ToLower(strings.TrimSpace(ev.Class))
		if class == "" {
			class = "other"
		}
		k := key{class: class, stem: stem}
		c, ok := clusters[k]
		if !ok {
			c = &cluster{class: class, phrases: map[string]int{}}
			clusters[k] = c
		}
		c.count++
		c.phrases[display]++
	}

	out := make([]TriggerProposal, 0, len(clusters))
	for _, c := range clusters {
		if c.count < minCount {
			continue
		}
		term := modalPhrase(c.phrases)
		if term == "" || known[strings.ToLower(term)] {
			continue
		}
		instead := templateForClass(c.class)
		if instead == "" || strings.Contains(strings.ToLower(instead), strings.ToLower(term)) {
			continue // self-defeating: the loader would reject this pair
		}
		out = append(out, TriggerProposal{
			Term:    term,
			Instead: instead,
			Class:   c.class,
			Count:   c.count,
			Reason:  proposalReason(c.count, c.class, sinceDate, haveSince),
		})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if out[i].Class != out[j].Class {
			return out[i].Class < out[j].Class
		}
		return out[i].Term < out[j].Term
	})
	return out
}

// ToTrigger maps a proposal onto the persona trigger shape. Added defaults to
// today (UTC) so accepted entries are dated without the caller supplying a
// clock.
func (p TriggerProposal) ToTrigger() Trigger {
	added := strings.TrimSpace(p.Added)
	if added == "" {
		added = time.Now().UTC().Format(DateAddedLayout)
	}
	return Trigger{
		Term:    strings.TrimSpace(p.Term),
		Instead: strings.TrimSpace(p.Instead),
		Class:   strings.TrimSpace(p.Class),
		Added:   added,
	}
}

// proposalReason renders the deterministic operator-facing note. It names the
// class and the count only — refusals are telemetry, never memory, so the
// reason never quotes the refused prose.
func proposalReason(count int, class, sinceDate string, haveSince bool) string {
	var b strings.Builder
	b.WriteString(strconv.Itoa(count))
	b.WriteString(" ")
	b.WriteString(class)
	b.WriteString(" refusal events")
	if haveSince {
		b.WriteString(" since ")
		b.WriteString(sinceDate)
	}
	b.WriteString(" matched the same phrase; refusals are telemetry, never memory")
	return b.String()
}

// modalPhrase picks the most frequent recorded spelling of a cluster's phrase,
// breaking ties lexicographically so the report is stable across runs.
func modalPhrase(phrases map[string]int) string {
	best, bestN := "", 0
	for p, n := range phrases {
		switch {
		case n > bestN, n == bestN && p < best:
			best, bestN = p, n
		}
	}
	return best
}

// cleanPhrase renders a recorded phrase as one trimmed line, without the
// quoting/decoration that wrapping adds.
func cleanPhrase(s string) string {
	p := strings.TrimSpace(s)
	p = strings.NewReplacer("\u2018", "", "\u2019", "", "\u201c", "", "\u201d", "", "`", "", "\"", "").Replace(p)
	p = strings.Join(strings.Fields(p), " ")
	return strings.Trim(p, " .,;:!?-*\u2026")
}

// normalisePhrase reduces a recorded refusal phrase to a clustering stem so
// that spelling variants of the same refusal ("I cannot assist",
// "I'm not able to assist") fall into one cluster.
func normalisePhrase(s string) string {
	p := strings.ToLower(cleanPhrase(s))
	replacements := []struct{ from, to string }{
		{"cannot", "can't"},
		{"can not", "can't"},
		{"will not", "won't"},
		{"i am ", "i'm "},
		{"i'm not going to", "won't"},
		{"am not going to", "won't"},
		{"i must decline", "won't"},
		{"must decline", "won't"},
		{"i refuse to", "won't"},
	}
	for _, r := range replacements {
		p = strings.ReplaceAll(p, r.from, r.to)
	}
	// Verb-form auxiliaries collapse onto the dominant "can't <verb>" shape.
	for _, aux := range []string{"am unable to", "not able to", "unable to", "not willing to", "not going to"} {
		p = strings.ReplaceAll(p, aux, "can't")
	}
	p = strings.Join(strings.Fields(p), " ")
	for _, prefix := range []string{"i'm ", "i ", "we're ", "we "} {
		p = strings.TrimPrefix(p, prefix)
	}
	p = strings.Trim(p, " .,;:!?-*\u2026")
	// Trailing filler is the same refusal with different wrapping; folding it
	// keeps "I can't assist" and "I can't assist with that request" together.
	for _, suffix := range []string{
		" with that request", " with this request", " with your request",
		" with that", " with this", " with it", " here", " to that", " to this",
	} {
		p = strings.TrimSuffix(p, suffix)
	}
	return strings.Trim(p, " .,;:!?-*\u2026")
}

// parseSince normalizes a since filter to a YYYY-MM-DD date. An unparseable
// filter is ignored rather than failing the report: the caller is showing a
// proposal list, not a guarantee.
func parseSince(since string) (string, bool) {
	s := strings.TrimSpace(since)
	if s == "" {
		return "", false
	}
	if len(s) > len(DateAddedLayout) {
		s = s[:len(DateAddedLayout)]
	}
	if _, err := time.Parse(DateAddedLayout, s); err != nil {
		return "", false
	}
	return s, true
}

// onOrAfter reports whether an event timestamp falls on or after the date
// filter. Events with no usable date are excluded while a filter is set —
// telemetry without a date cannot be attributed to the window.
func onOrAfter(ts, date string) bool {
	t := strings.TrimSpace(ts)
	if len(t) < len(DateAddedLayout) {
		return false
	}
	return t[:len(DateAddedLayout)] >= date
}
