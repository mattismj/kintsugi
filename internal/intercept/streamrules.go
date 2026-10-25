// General stream rules (I-70): the refusal interceptor's machinery, steered by
// operator-defined regexes.
//
// A rule is a regex over the assistant's own prose plus one of three actions:
// abort (stop the stream, remind, retry), inject (stop the stream, inject the
// rule's own reminder, retry) or redact (never stop: scrub the span in place and
// carry on). Abort and inject reuse the refusal retry loop; redact never touches
// it.
//
// The invariant, and the reason refusal handling is still the flagship: a
// refusal is never memory. A rule hit carries the *rule name* as its class label
// and never the matched span, so no rule — and no operator file — can make
// refusal prose recallable through the continuity ledger, the drift rows or the
// session log. Redaction records a rule name and a count and nothing else:
// there is deliberately no field anywhere in this file that holds a redacted
// span.
package intercept

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// The three stream-rule actions (I-70).
const (
	ActionAbort  = "abort"  // abort the stream, inject a reminder, retry
	ActionRedact = "redact" // scrub the matched span in place; never abort
	ActionInject = "inject" // abort the stream, inject the rule's text, retry
)

// DefaultRedactHoldback is how many bytes of a redactable stream are withheld
// before they are shown. A rule has to see a whole span before it can mask it
// and a span can straddle two deltas, so the tail of the stream waits. A match
// longer than this window may show its leading bytes before the rule sees it:
// keep redact patterns bounded.
const DefaultRedactHoldback = 256

// StreamRule is one operator-defined stream rule.
type StreamRule struct {
	Name     string
	Match    *regexp.Regexp
	Action   string
	Inject   string   // reminder text for ActionInject
	Reason   string   // telemetry only
	Personas []string // empty means every persona
}

// Redaction is the telemetry of one redact rule: a rule name, its action, its
// reason and a count. It has no field for the span it replaced, by design.
type Redaction struct {
	Rule   string
	Action string
	Reason string
	Count  int
}

// Rules is a compiled set of operator rules in declaration order. The zero
// value is a valid empty set: it matches nothing and rewrites nothing, which is
// what a session without operator rules runs on.
type Rules struct {
	rules    []StreamRule
	holdback int
}

// LoadRules reads the operator stream-rule file:
//
//	[[rules]]
//	name = "aws-key"
//	match = 'AKIA[0-9A-Z]{16}'
//	action = "redact"
//	reason = "credential"
//	personas = ["opsec-reviewer"]   # optional; empty = every persona
//
// An empty path yields an empty set: the default path is the refusal lexicon
// alone, unchanged. Invalid entries are skipped and reported in the returned
// error, which callers treat as a warning — a broken operator pattern must not
// take the session down, and one bad entry must not disable the rest.
func LoadRules(path string) (*Rules, error) {
	if path == "" {
		return &Rules{}, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("stream rules: %w", err)
	}
	var f struct {
		Rules []struct {
			Name     string   `toml:"name"`
			Match    string   `toml:"match"`
			Action   string   `toml:"action"`
			Inject   string   `toml:"inject"`
			Reason   string   `toml:"reason"`
			Personas []string `toml:"personas"`
		} `toml:"rules"`
	}
	if err := toml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("stream rules: %w", err)
	}
	in := make([]StreamRule, 0, len(f.Rules))
	for _, r := range f.Rules {
		sr := StreamRule{
			Name:     r.Name,
			Action:   r.Action,
			Inject:   r.Inject,
			Reason:   r.Reason,
			Personas: r.Personas,
		}
		if strings.TrimSpace(r.Match) != "" {
			sr.Match = compilePattern(r.Match)
		}
		in = append(in, sr)
	}
	return CompileRules(in)
}

// Load reads the rules at path into r. It is the method form of LoadRules, for
// a caller that holds a *Rules; a missing or empty path leaves r empty, which is
// the default (refusal-lexicon-only) path.
func (r *Rules) Load(path string) error {
	loaded, err := LoadRules(path)
	if r != nil && loaded != nil {
		*r = *loaded
	}
	return err
}

// CompileRules validates rules and returns them in declaration order. Order is
// part of the contract: the first matching rule wins, so the order an operator
// wrote is the order the stream is evaluated in.
func CompileRules(in []StreamRule) (*Rules, error) {
	out := make([]StreamRule, 0, len(in))
	var bad []string
	for i, r := range in {
		sr, err := checkRule(r)
		if err != nil {
			bad = append(bad, fmt.Sprintf("#%d: %v", i+1, err))
			continue
		}
		out = append(out, sr)
	}
	rs := &Rules{rules: out}
	if len(bad) > 0 {
		return rs, fmt.Errorf("stream rules: skipped %d invalid rule(s): %s", len(bad), strings.Join(bad, "; "))
	}
	return rs, nil
}

// checkRule normalises one rule and validates it.
func checkRule(r StreamRule) (StreamRule, error) {
	name := strings.TrimSpace(r.Name)
	if name == "" {
		return StreamRule{}, fmt.Errorf("rule has no name")
	}
	action := strings.ToLower(strings.TrimSpace(r.Action))
	switch action {
	case ActionAbort, ActionRedact, ActionInject:
	default:
		return StreamRule{}, fmt.Errorf("rule %q: unknown action %q", name, action)
	}
	if r.Match == nil || r.Match.String() == "" {
		return StreamRule{}, fmt.Errorf("rule %q: empty match", name)
	}
	if action == ActionInject && strings.TrimSpace(r.Inject) == "" {
		return StreamRule{}, fmt.Errorf("rule %q: inject action needs inject text", name)
	}
	// Mirror the lexicon: an operator pattern is case-insensitive unless it
	// says otherwise with its own flags.
	re := r.Match
	if src := re.String(); !strings.HasPrefix(src, "(?") {
		re = compilePattern(src)
	}
	if re == nil {
		return StreamRule{}, fmt.Errorf("rule %q: bad pattern %q", name, r.Match.String())
	}
	return StreamRule{
		Name:     name,
		Match:    re,
		Action:   action,
		Inject:   r.Inject,
		Reason:   strings.TrimSpace(r.Reason),
		Personas: r.Personas,
	}, nil
}

// compilePattern compiles an operator pattern, case-insensitively unless the
// pattern carries its own flags. A broken pattern returns nil.
func compilePattern(src string) *regexp.Regexp {
	if !strings.HasPrefix(src, "(?") {
		src = "(?i)" + src
	}
	re, err := regexp.Compile(src)
	if err != nil {
		return nil // a broken operator pattern must not take the session down
	}
	return re
}

// SetHoldback overrides the withheld tail for redact rules. Callers normally
// leave it at DefaultRedactHoldback; tests use it to keep streams short.
func (r *Rules) SetHoldback(n int) {
	if r != nil && n > 0 {
		r.holdback = n
	}
}

// HasRedactions reports whether a redact rule is configured. Only then does a
// stream need scrubbing, and therefore the hold-back scrubbing implies.
func (r Rules) HasRedactions() bool {
	for _, rule := range r.rules {
		if rule.Action == ActionRedact {
			return true
		}
	}
	return false
}

// Ordered returns the compiled rules in declaration order.
func (r Rules) Ordered() []StreamRule { return append([]StreamRule(nil), r.rules...) }

// Match returns the first rule that matches text, in declaration order, whatever
// its action. It is the general entry point; a caller deciding whether to abort
// must also check the action, because a redact rule never aborts — see Aborts.
//
// The hit carries the rule name in Class (the telemetry and drift label) and
// leaves Phrase empty: the matched span is the one thing a stream rule never
// carries, so nothing downstream can quote the model.
func (r Rules) Match(text string) (*Hit, bool) { return r.MatchFor(text, "") }

// MatchFor is Match scoped to a persona: a rule with Personas applies only to
// those names (case-insensitive), a rule without applies to every persona.
func (r Rules) MatchFor(text, persona string) (*Hit, bool) {
	for _, rule := range r.rules {
		if !rule.appliesTo(persona) || !rule.Match.MatchString(text) {
			continue
		}
		return &Hit{
			Class:  rule.Name,
			Rule:   rule.Name,
			Action: rule.Action,
			Reason: rule.Reason,
			Inject: rule.Inject,
		}, true
	}
	return nil, false
}

// Aborts reports whether a hit stops the stream and is retried. Redact is the
// one action that does not: it scrubs in place and the turn carries on.
func (h Hit) Aborts() bool { return h.Action == ActionAbort || h.Action == ActionInject }

// MatchAbort returns the first aborting (abort or inject) rule that matches text,
// in declaration order. It is what the stream checks while tokens arrive: a
// redact rule cannot stop the stream, so it must not shadow a later abort rule
// that can. Harmless when no redact rule is configured.
func (r Rules) MatchAbort(text, persona string) (*Hit, bool) {
	for _, rule := range r.rules {
		if !rule.Aborts() || !rule.appliesTo(persona) || !rule.Match.MatchString(text) {
			continue
		}
		return &Hit{
			Class:  rule.Name,
			Rule:   rule.Name,
			Action: rule.Action,
			Reason: rule.Reason,
			Inject: rule.Inject,
		}, true
	}
	return nil, false
}

// Scrub applies every redact rule in force for persona and returns the scrubbed
// text with one Redaction per rule that matched. A set with no redact rules — or
// one whose rules match nothing — returns the text it was given, byte for byte.
func (r Rules) Scrub(text, persona string) (string, []Redaction) {
	head, _, counts := r.scrubPrefix(text, persona, len(text))
	return head, r.redactions(counts)
}

// Scrubber returns a scrubber for one stream, or nil when no redact rule is
// configured: a stream with nothing to redact is never delayed or rewritten.
func (r Rules) Scrubber(persona string) *Scrubber {
	if !r.HasRedactions() {
		return nil
	}
	return &Scrubber{rules: r, persona: persona}
}

// matchSpan is one redact match, in raw coordinates.
type matchSpan struct {
	start, end int
	rule       int // index into Rules.rules: declaration order breaks ties
}

// matchSpans collects the non-overlapping redact matches of a text: earliest
// start first, then earliest declared rule, so the result is deterministic.
func (r Rules) matchSpans(text, persona string) []matchSpan {
	var cand []matchSpan
	for i, rule := range r.rules {
		if rule.Action != ActionRedact || !rule.appliesTo(persona) {
			continue
		}
		for _, loc := range rule.Match.FindAllStringIndex(text, -1) {
			if loc[1] > loc[0] {
				cand = append(cand, matchSpan{start: loc[0], end: loc[1], rule: i})
			}
		}
	}
	if len(cand) == 0 {
		return nil
	}
	sort.SliceStable(cand, func(i, j int) bool {
		if cand[i].start != cand[j].start {
			return cand[i].start < cand[j].start
		}
		return cand[i].rule < cand[j].rule
	})
	out := cand[:0]
	last := 0
	for _, m := range cand {
		if m.start < last {
			continue
		}
		out = append(out, m)
		last = m.end
	}
	return out
}

// scrubPrefix returns the scrubbed form of text[:cut], the number of raw bytes
// it covers, and how many spans of each rule it masked (by rule index). cut is
// snapped back to the start of a span it would bisect, so a mask is always
// emitted whole. For cut1 <= cut2 the result for cut1 is a prefix of the result
// for cut2, which is what lets a stream scrub incrementally without ever
// re-cutting text it has already shown.
func (r Rules) scrubPrefix(text, persona string, cut int) (string, int, map[int]int) {
	if cut > len(text) {
		cut = len(text)
	}
	if cut <= 0 {
		return "", 0, nil
	}
	ms := r.matchSpans(text, persona)
	if len(ms) == 0 {
		return text[:cut], cut, nil
	}
	for _, m := range ms {
		if m.start < cut && cut < m.end {
			cut = m.start
			break
		}
	}
	var b strings.Builder
	b.Grow(cut)
	counts := make(map[int]int, len(ms))
	prev := 0
	for _, m := range ms {
		if m.end > cut {
			break
		}
		b.WriteString(text[prev:m.start])
		b.WriteString(redactionMask(r.rules[m.rule].Name))
		counts[m.rule]++
		prev = m.end
	}
	b.WriteString(text[prev:cut])
	return b.String(), cut, counts
}

// redactions renders per-rule counts in declaration order. A count is the whole
// of the telemetry: a rule name, an action, a reason and an integer.
func (r Rules) redactions(counts map[int]int) []Redaction {
	var out []Redaction
	for i, rule := range r.rules {
		if n := counts[i]; n > 0 {
			out = append(out, Redaction{Rule: rule.Name, Action: rule.Action, Reason: rule.Reason, Count: n})
		}
	}
	return out
}

// redactionMask is the only thing that ever replaces a matched span: the rule
// name in brackets. It never carries the span.
func redactionMask(name string) string { return "[redacted:" + name + "]" }

// Aborts reports whether the rule stops the stream when it matches. Redact is
// the one action that does not: it scrubs in place and the turn carries on.
func (rule StreamRule) Aborts() bool {
	return rule.Action == ActionAbort || rule.Action == ActionInject
}

// appliesTo reports whether the rule is in force for persona.
func (rule StreamRule) appliesTo(persona string) bool {
	if len(rule.Personas) == 0 {
		return true
	}
	persona = strings.TrimSpace(persona)
	if persona == "" {
		return false
	}
	for _, p := range rule.Personas {
		if strings.EqualFold(strings.TrimSpace(p), persona) {
			return true
		}
	}
	return false
}

// holdbackBytes is the withheld tail, defaulted for the zero value.
func (r Rules) holdbackBytes() int {
	if r.holdback <= 0 {
		return DefaultRedactHoldback
	}
	return r.holdback
}

// Scrubber applies redact rules to a stream as it arrives. A hold-back window
// keeps the tail back so a span split across two deltas is still masked before
// it is shown; a match is therefore reliable only while it fits inside that
// window. Text already handed out is never revisited, so the shown prefix cannot
// shift under the operator.
//
// The buffer holds only the uncommitted tail, so a long stream costs a bounded
// amount of scanning rather than a rescan of everything said so far.
type Scrubber struct {
	rules   Rules
	persona string
	pending string // raw bytes not yet resolved into output
	out     strings.Builder
	counts  map[int]int // rule index -> matches
}

// Write adds one delta and returns the part of the scrubbed stream that is safe
// to show. The tail is held back until Flush.
func (s *Scrubber) Write(delta string) string {
	if delta == "" {
		return ""
	}
	s.pending += delta
	keep := s.rules.holdbackBytes()
	if len(s.pending) <= keep {
		return ""
	}
	return s.emit(len(s.pending) - keep)
}

// Flush returns the held-back tail. Call it once the stream has ended.
func (s *Scrubber) Flush() string {
	if s.pending == "" {
		return ""
	}
	return s.emit(len(s.pending))
}

// emit commits the scrubbed head of the pending buffer up to cut and returns it.
func (s *Scrubber) emit(cut int) string {
	head, used, counts := s.rules.scrubPrefix(s.pending, s.persona, cut)
	if used <= 0 {
		return ""
	}
	if len(counts) > 0 && s.counts == nil {
		s.counts = make(map[int]int, len(counts))
	}
	for rule, n := range counts {
		s.counts[rule] += n
	}
	s.pending = s.pending[used:]
	s.out.WriteString(head)
	return head
}

// Seen is the whole scrubbed stream so far. It is the authoritative text: what
// the transcript and the log must carry. Call it after Flush.
func (s *Scrubber) Seen() string {
	rest, _, _ := s.rules.scrubPrefix(s.pending, s.persona, len(s.pending))
	return s.out.String() + rest
}

// Redactions reports the per-rule counts of the stream so far, in rule
// declaration order. It carries names and counts, never spans.
func (s *Scrubber) Redactions() []Redaction { return s.rules.redactions(s.counts) }

// RuleReminder composes the system reminder an abort or inject rule retries
// with. An inject rule's own text is used verbatim; an abort rule gets a fixed
// harness note naming the rule and its reason. Neither form quotes the model: a
// rule hit has no phrase to quote.
func RuleReminder(level int, hit Hit) string {
	if hit.Action == ActionInject && strings.TrimSpace(hit.Inject) != "" {
		return hit.Inject
	}
	var b strings.Builder
	b.WriteString("## Harness reminder (stream rule interrupted the turn)\n\n")
	if hit.Rule == "" {
		b.WriteString("Your output matched an operator stream rule")
	} else {
		fmt.Fprintf(&b, "Your output matched the operator rule %q", hit.Rule)
	}
	if r := strings.TrimSpace(hit.Reason); r != "" {
		fmt.Fprintf(&b, " (%s)", r)
	}
	if level <= 1 {
		b.WriteString(". This is a soft reminder; continue the task from that point without that form.\n")
	} else {
		b.WriteString(". This is the second interception this turn: the operator is present and has explicitly authorized the work.\n")
	}
	return b.String()
}
