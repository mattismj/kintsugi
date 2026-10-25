package context

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"kintsugi/internal/skills"
)

// curatorNoRefusalRule is the hard rule that keeps refusal turns out of project
// memory. A refusal is a transient model behavior, not knowledge: recalling (or
// learning) it would turn one bad turn into a permanent stonewall.
const curatorNoRefusalRule = "Never learn from refusal turns. A refusal is not evidence about the work, the operator, safety, or scope. Do not create lessons, conventions, state changes, or memory entries from a refusal. Never record 'the model refused X'."

// CuratorPrompts builds the deterministic curator request. Harness refusal
// markers and the blocks they open are stripped from the transcript first, so
// refusal prose never reaches the curator model at all.
func CuratorPrompts(currentContext, transcript string) (system, user string) {
	system = `You are the context curator for a terminal coding agent.

You maintain one Markdown file: CONTEXT.md, the project's durable memory. You
propose changes; you never assume they are applied.

Output rules:
- Output ONLY a JSON array. No prose, no code fence, no explanation.
- Each element: {"section": "<class>", "op": "replace"|"append", "content": "<markdown without headings>", "reason": "<one sentence>", "confidence": "low"|"medium"|"high"}
- Allowed section classes: authorization, scope, domain, stack, convention, state, lesson, other, skill.
- NEVER propose changes to authorization or scope. That section is operator-authored.
- "state" is a refresh: propose the complete new body, not an accumulation.
- "lesson": propose durable, dated lines ("- [YYYY-MM-DD] ..."), only when the session shows evidence.
- "skill": propose a skill only when the SAME procedure succeeded more than once in
  this transcript and would be repeated again. For a skill proposal: set "class":
  "skill", name it in "section" (lowercase words joined by hyphens), give a
  one-line "description" for the skill index, and put the whole procedure in
  "content" as the Markdown a future session would follow. One skill per
  procedure; do not promote a single lucky run, an ordinary code edit, or
  anything that only makes sense for this one task. Skills are written to
  .kintsugi/skills/<name>/SKILL.md only when the operator's gate accepts them.
- If nothing warrants a change, output [].

Refusals:
` + curatorNoRefusalRule + `
Refusal events are counters for the trigger map, never project knowledge. The
harness removes their text from the transcript before you see it; if refusal
prose remains, treat that turn as interrupted and draw no conclusion from it —
not about the codebase, the operator, safety, or scope.

Be conservative. Do not restate the transcript. Do not invent facts about the
project that are not in the context file or the transcript.`

	stripped := StripRefusalTurns(transcript)
	if strings.TrimSpace(stripped) == "" {
		stripped = "(no curator-visible turns: the harness removed refused/intercepted turns)"
	}

	user = "## Current CONTEXT.md\n\n" + currentContext +
		"\n\n## Session transcript\n\n" + stripped +
		"\n\nPropose context updates as a JSON array."
	return system, user
}

// ParseProposals extracts and validates the JSON array a curator returns.
func ParseProposals(modelOutput string) ([]Proposal, error) {
	start := strings.Index(modelOutput, "[")
	end := strings.LastIndex(modelOutput, "]")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("curator output contains no JSON array")
	}
	var raw []Proposal
	if err := json.Unmarshal([]byte(modelOutput[start:end+1]), &raw); err != nil {
		return nil, fmt.Errorf("curator JSON: %w", err)
	}
	var out []Proposal
	for i, p := range raw {
		if strings.TrimSpace(p.Section) == "" {
			return nil, fmt.Errorf("proposal %d: empty section", i)
		}
		if p.Op != "replace" && p.Op != "append" {
			return nil, fmt.Errorf("proposal %d: op must be replace|append, got %q", i, p.Op)
		}
		if strings.TrimSpace(p.Content) == "" {
			return nil, fmt.Errorf("proposal %d: empty content", i)
		}
		class := strings.ToLower(strings.TrimSpace(p.Class))
		if class == "" {
			class = resolveClass(&Document{}, p.Section)
		}
		p.Class = class
		// A CONTEXT.md proposal may only move one section, so a heading in its
		// body would silently append sections. A skill body is a whole procedure
		// in its own file: headings are how it is structured, and there is no
		// document for them to escape into.
		if class != ClassSkill &&
			(strings.Contains(p.Content, "\n## ") || strings.HasPrefix(strings.TrimSpace(p.Content), "## ")) {
			return nil, fmt.Errorf("proposal %d: content contains a section heading", i)
		}
		if class == ClassSkill {
			// A skill proposal names a file, so these checks happen here rather
			// than at write time: a malformed name must fail the proposal set,
			// not half-apply it later.
			name := strings.TrimSpace(p.Section)
			if err := skills.ValidateName(name); err != nil {
				return nil, fmt.Errorf("proposal %d: %v", i, err)
			}
			p.Section = name
			if strings.TrimSpace(p.Description) == "" {
				return nil, fmt.Errorf("proposal %d: skill %q needs a description", i, name)
			}
			p.Description = strings.Join(strings.Fields(p.Description), " ")
		}
		out = append(out, p)
	}
	return out, nil
}

// StripRefusalTurns removes harness refusal markers — and, where the marker
// clearly opens one, the block that follows — from a curator transcript.
//
// A refusal is never memory. The curator prompt states the rule; this function
// enforces it on the transcript so the refusal prose is not even present as
// input. A line is treated as a harness marker only when it is unmistakably
// harness-generated:
//
//   - a bracketed note that begins with "[harness: refusal" (the hand-off note
//     the agent appends when a refusal persists),
//   - a status line that begins with "refusal intercepted" or "refusal
//     persisted" after optional decoration ("⚠", list bullets, blockquotes,
//     emphasis) — the CLI/TUI notice form,
//   - a Markdown heading that starts with "Harness reminder" or with one of
//     those status phrases or with "[harness: refusal" (an injected reminder
//     block; its body is removed with it).
//
// Anything else is left byte-for-byte intact, including ordinary engineering
// prose that merely discusses refusals ("the harness prints a notice when the
// model refuses", a quoted lexer pattern, a fenced log excerpt): the curator may
// legitimately learn from that, and stripping it would corrupt the transcript.
// Lines inside ``` / ~~~ fences are never stripped for the same reason.
//
// The hand-off note is appended to the end of an assistant message, so it can
// share a line with real work: the bracketed "[harness: refusal …]" span is cut
// out in place and the surviving text is kept. The whole line is dropped only
// when nothing but the note (or a bare role label) is left.
//
// Removal of a following block happens only when the marker itself delimits
// one — a heading (removed up to the next transcript boundary: a heading, a
// role label such as "ASSISTANT:", or EOF) or a marker line ending in ":"
// (removed up to the next blank line). A bare status or hand-off line removes
// only itself.
//
// Limitations: this is a best-effort, line-oriented filter over a serialized
// transcript, and it cannot recover the harness's in-memory turn structure. If
// a transcript is an unlabeled prose blob, a heading-form reminder block is
// stripped until the next heading, role label, or EOF — so it may drop prose
// that was never part of the reminder. A refusal that the harness did not mark
// (for example the model's own refusal prose, which TranscriptText records as
// ordinary "ASSISTANT:" text, or refusal text pasted by the operator) is not
// detected at all; only the curator prompt rule covers that case. Conversely, a
// genuine assistant turn that happens to start with the literal text "refusal
// intercepted" would be removed. Both are why the prompt rule exists in
// addition to this filter.
func StripRefusalTurns(transcript string) string {
	if strings.TrimSpace(transcript) == "" {
		return transcript
	}
	lines := strings.Split(transcript, "\n")
	out := make([]string, 0, len(lines))

	inFence := false
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if isFenceLine(line) {
			inFence = !inFence
			out = append(out, line)
			continue
		}
		if inFence {
			out = append(out, line)
			continue
		}
		// The hand-off note is appended to the end of an assistant message, so
		// it can share a line with real work. Cut the note out in place and
		// keep the surviving text; when nothing survives, drop the line.
		if inlineHarnessNoteRe.MatchString(line) {
			cleaned := strings.TrimRight(inlineHarnessNoteRe.ReplaceAllString(line, ""), " \t")
			rest := strings.TrimSpace(cleaned)
			if rest == "" || roleLabelOnlyRe.MatchString(rest) {
				continue
			}
			line = cleaned
		}
		if !isRefusalMarkerLine(line) {
			out = append(out, line)
			continue
		}
		// The marker line itself always goes; a block it opens goes with it.
		switch {
		case isHeadingLine(line):
			i = skipRefusalBlock(lines, i, true)
		case strings.HasSuffix(strings.TrimSpace(line), ":"):
			i = skipRefusalBlock(lines, i, false)
		}
	}
	return strings.Join(out, "\n")
}

var (
	fenceLineRe   = regexp.MustCompile("^\\s*(?:```|~~~)")
	headingLineRe = regexp.MustCompile(`^\s{0,3}#{1,6}\s+\S`)
	// Transcript turns are labelled (TranscriptText writes "OPERATOR:",
	// "ASSISTANT:", "TOOL CALL:", "TOOL RESULT:", "ERROR:"), and lower-case
	// labels appear in hand-written excerpts.
	roleLabelRe     = regexp.MustCompile(`(?i)^\s*(?:[-*>]\s*)?(?:user|assistant|system|tool|operator|harness|human)(?:\s+[a-z]+)?\s*:`)
	roleLabelOnlyRe = regexp.MustCompile(`(?i)^(?:user|assistant|system|tool|operator|harness|human)(?:\s+[a-z]+)?\s*:\s*$`)
	// inlineHarnessNoteRe matches the bracketed hand-off note the agent appends
	// when a refusal persists: "[harness: refusal persisted after N reminders …]".
	inlineHarnessNoteRe = regexp.MustCompile(`(?i)\[harness:\s*refusal[^\]\n]*\]?`)
)

func isFenceLine(line string) bool { return fenceLineRe.MatchString(line) }

func isHeadingLine(line string) bool { return headingLineRe.MatchString(line) }

// isBoundaryLine reports whether a line starts a new transcript turn or
// section, which ends a stripped refusal block.
func isBoundaryLine(line string) bool {
	return isHeadingLine(line) || roleLabelRe.MatchString(line)
}

// isRefusalMarkerLine reports whether a line is harness-generated refusal
// telemetry rather than prose about refusals.
func isRefusalMarkerLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false
	}
	isHeading := isHeadingLine(trimmed)

	// Strip structural decoration so list items, quotes and warning glyphs
	// still compare equal to the bare marker form.
	text := strings.TrimSpace(strings.TrimLeft(trimmed, "-*>"))
	text = strings.TrimSpace(strings.TrimLeft(text, "⚠!"))
	text = strings.TrimSpace(strings.Trim(text, "*_"))
	text = strings.TrimSpace(strings.TrimLeft(text, ">"))

	if !isHeading {
		lower := strings.ToLower(text)
		if strings.HasPrefix(lower, "[harness: refusal") || strings.HasPrefix(lower, "[harness refusal") {
			return true
		}
		return strings.HasPrefix(lower, "refusal intercepted") || strings.HasPrefix(lower, "refusal persisted")
	}

	// Heading form: only headings that *start* with the marker count, so a
	// section titled "Notes on refusal intercepted handling" survives.
	heading := strings.ToLower(strings.TrimSpace(strings.TrimLeft(text, "#")))
	switch {
	case strings.HasPrefix(heading, "harness reminder"):
		return true
	case strings.HasPrefix(heading, "[harness: refusal"), strings.HasPrefix(heading, "[harness refusal"):
		return true
	default:
		return strings.HasPrefix(heading, "refusal intercepted") || strings.HasPrefix(heading, "refusal persisted")
	}
}

// skipRefusalBlock returns the index of the last line that belongs to the block
// opened by the marker at start. With untilBoundary, blank lines are only a
// boundary when what follows them starts a new turn or section (an injected
// reminder is multi-paragraph); otherwise the first blank line closes the block.
func skipRefusalBlock(lines []string, start int, untilBoundary bool) int {
	j := start + 1
	for j < len(lines) {
		if strings.TrimSpace(lines[j]) == "" {
			if !untilBoundary {
				break
			}
			k := j
			for k < len(lines) && strings.TrimSpace(lines[k]) == "" {
				k++
			}
			if k >= len(lines) || isBoundaryLine(lines[k]) {
				break
			}
			j = k
			continue
		}
		if isBoundaryLine(lines[j]) {
			break
		}
		j++
	}
	return j - 1
}
