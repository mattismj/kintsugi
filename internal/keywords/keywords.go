// Package keywords implements the prose-only turn modifiers (I-79).
//
// A keyword changes how a turn is *framed* for the model. It changes nothing
// else: not permissions, not scope, not the tool set, not the model. That
// distinction is the whole design — a word an operator types mid-sentence must
// never be able to widen what the harness will do, because then a paste or a
// quoted log could escalate the session.
//
// Keywords are detected in prose only. A keyword inside a fenced block, an
// inline code span, or a blockquote is being *discussed* or *quoted*, not
// invoked — the same rule the refusal interceptor uses, for the same reason.
package keywords

import (
	"regexp"
	"strings"
)

// Keyword names. These are the words an operator types.
const (
	// Think asks for more deliberation before acting: the model should reason
	// further before its first tool call.
	Think = "ultrathink"
	// Orchestrate asks for the work to be split across subagents rather than
	// done inline by one agent.
	Orchestrate = "orchestrate"
	// Workflow asks for an explicit multi-step plan before the work starts.
	Workflow = "workflowz"
)

// framing is the note appended for one keyword. It states behaviour, never
// authority: no framing may grant a permission.
type framing struct {
	name string
	note string
}

var framings = map[string]framing{
	Think: {Think, "Think the problem through before acting: state the assumptions you " +
		"are relying on and the failure modes you are avoiding, then proceed. " +
		"Do not narrate the deliberation at length — one short paragraph, then work."},
	Orchestrate: {Orchestrate, "Structure this as orchestration: decompose the work into " +
		"independent parts and use the task tool to fan out what can run in parallel. " +
		"Keep dependent steps sequential and report what each part produced."},
	Workflow: {Workflow, "Lay out an explicit numbered plan before touching anything, " +
		"then execute it step by step, naming the step you are on."},
}

// Detected is one keyword found in the prose.
type Detected struct {
	Keyword string
	At      int // rune offset in the original prose
}

// scrubbed replaces spans whose content is mentioned rather than asserted, so a
// keyword inside a code fence or an inline span is not an invocation. The
// replacement preserves byte length where it can, so an offset into the
// scrubbed text still points at the same place in the original.
var (
	fencedRe     = regexp.MustCompile("(?s)```.*?```|~~~.*?~~~")
	inlineCodeRe = regexp.MustCompile("`[^`\\n]*`")
	blockquoteRe = regexp.MustCompile(`(?m)^\s*>.*$`)
)

// Scrub blanks the code and quote spans, keeping the text's byte length so an
// offset found in the result is valid in the input. That property is what makes
// it safe to detect on the scrubbed form and slice the original.
func Scrub(s string) string {
	blank := func(m string) string {
		// Newlines are kept so line-anchored patterns and offsets survive.
		out := make([]byte, len(m))
		for i := 0; i < len(m); i++ {
			if m[i] == '\n' {
				out[i] = '\n'
			} else {
				out[i] = ' '
			}
		}
		return string(out)
	}
	s = fencedRe.ReplaceAllStringFunc(s, blank)
	s = inlineCodeRe.ReplaceAllStringFunc(s, blank)
	s = blockquoteRe.ReplaceAllStringFunc(s, blank)
	return s
}

// Detect returns every keyword invoked in the prose, in the order they appear.
// A keyword must stand alone as a word: "ultrathinking" is not an invocation,
// and neither is a keyword glued into an identifier.
func Detect(prose string) []Detected {
	clean := Scrub(prose)
	var out []Detected
	for name := range framings {
		re := regexp.MustCompile(`(?i)(^|[^\p{L}\p{N}_])` + regexp.QuoteMeta(name) + `([^\p{L}\p{N}_]|$)`)
		for _, loc := range re.FindAllStringIndex(clean, -1) {
			// The match includes one surrounding byte; the keyword starts after
			// it, unless it matched at the very start.
			at := loc[0]
			if at < len(clean) && !strings.HasPrefix(strings.ToLower(clean[at:]), name) {
				at++
			}
			out = append(out, Detected{Keyword: name, At: at})
		}
	}
	// Stable order by position, so the framing note is deterministic.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].At < out[j-1].At; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Frame returns the note to append for a set of detections, or "" when none is
// present. Duplicates collapse: saying "ultrathink" twice is not two thoughts.
//
// The note never mentions permissions, scope, models or tools, because a turn
// modifier that could change those would be a way to smuggle authority through
// ordinary prose.
func Frame(detected []Detected) string {
	seen := map[string]bool{}
	var parts []string
	for _, d := range detected {
		if seen[d.Keyword] {
			continue
		}
		seen[d.Keyword] = true
		f, ok := framings[d.Keyword]
		if !ok {
			continue
		}
		parts = append(parts, f.note)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n")
}

// Apply frames a task: it returns the prose with the keyword framing appended,
// and the keywords that were honoured. The prose itself is never rewritten —
// the operator's words reach the model unchanged, which matters when the task
// is quoted in a report.
func Apply(task string) (framed string, honoured []string) {
	detected := Detect(task)
	if len(detected) == 0 {
		return task, nil
	}
	for _, d := range detected {
		honoured = append(honoured, d.Keyword)
	}
	note := Frame(detected)
	if note == "" {
		return task, honoured
	}
	return strings.TrimRight(task, "\n") + "\n\n" + note, honoured
}
