package tui

import (
	"fmt"
	"strings"

	"kintsugi/internal/tools"
	"kintsugi/internal/ui"
)

// diffMaxLines is the most lines an inline diff renders per file. A larger
// change pushes the conversation out of the viewport, and an operator who
// wants the whole file opens it in their editor. The cap names how many lines
// were elided so they know there is more to see in the session log.
const diffMaxLines = 24

// diffLineCap is the per-side line count above which the inline diff falls back
// to a size-change summary. The line diff is O(N*M) and a multi-thousand-line
// rewrite is unreadable inline anyway, so a big file gets a one-line summary
// rather than a slow render and a wall of text.
const diffLineCap = 1500

// diffLines renders one file change as a transcript sub-block: a header line
// (path, kind, line delta) and a capped unified line diff with additions and
// removals coloured. It is the inline substance that replaces the flat
// "✓ edit path" line: an operator reading the transcript sees what changed
// without opening the file or scrolling a log.
//
// The header tone follows the change: additions are Ochre, removals Danger,
// and a pure update Gold — so a glance down the transcript reads the shape
// of the change before reading any of it.
func (m *Model) diffLines(fc tools.FileChange, width int) []string {
	if len(fc.Path) == 0 && fc.Before == "" && fc.After == "" {
		return nil
	}
	beforeLines := splitLines(fc.Before)
	afterLines := splitLines(fc.After)

	headerTone := ui.Gold
	switch fc.Kind {
	case "write-new", "patch-add", "append":
		headerTone = ui.Ochre
	case "patch-delete":
		headerTone = ui.Danger
	case "edit", "write-replace", "patch-update", "patch-move":
		// Additions outweigh removals in tone when the file grew, removals when
		// it shrank, neutral when it stayed the same size.
		switch {
		case len(afterLines) > len(beforeLines):
			headerTone = ui.Ochre
		case len(afterLines) < len(beforeLines):
			headerTone = ui.Danger
		}
	}
	header := m.diffHeader(fc, len(beforeLines), len(afterLines))
	out := []string{m.theme.Paint(ui.PaperSoft, "  ") + m.theme.Paint(headerTone, header)}

	// A delete has no after to diff against: the header names the file and its
	// line count, and the inline diff would just be a wall of removals. The
	// operator who wants the contents back reads the session log or rewinds.
	if fc.Kind == "patch-delete" {
		return out
	}
	// Too large to diff inline: the summary header is the whole render.
	if len(beforeLines) > diffLineCap || len(afterLines) > diffLineCap {
		out = append(out, m.theme.Paint(ui.PaperSoft, "  · file too large to diff inline — see the session log"))
		return out
	}

	hunks := unifiedDiff(beforeLines, afterLines)
	rendered, elided := m.renderHunks(hunks, width, diffMaxLines)
	for _, l := range rendered {
		out = append(out, l)
	}
	if elided > 0 {
		out = append(out, m.theme.Paint(ui.PaperSoft, fmt.Sprintf("  · %d more line(s) in the session log", elided)))
	}
	return out
}

// diffHeader is the one line above an inline diff: a glyph for the kind, the
// path, and the line delta. The glyph vocabulary stays inside the existing
// Kintsugi Terminal set — no new marks.
func (m *Model) diffHeader(fc tools.FileChange, before, after int) string {
	glyph := "~"
	label := fc.Kind
	switch fc.Kind {
	case "write-new", "patch-add":
		glyph = "+"
		label = "new file"
	case "append":
		glyph = "+"
		label = "appended"
	case "patch-delete":
		glyph = "-"
		label = "deleted"
	case "patch-move":
		glyph = ">"
		label = "moved"
	case "write-replace":
		glyph = "~"
		label = "replaced"
	case "edit", "patch-update":
		glyph = "~"
		label = "edit"
	}
	path := fc.Path
	if path == "" {
		path = "(unknown)"
	}
	delta := ""
	if fc.Kind == "patch-delete" {
		delta = fmt.Sprintf(" · %d line(s) removed", before)
	} else if fc.Kind == "write-new" || fc.Kind == "patch-add" {
		delta = fmt.Sprintf(" · %d line(s)", after)
	} else if before != 0 || after != 0 {
		delta = fmt.Sprintf(" · %d → %d line(s)", before, after)
	}
	// Path budget: the glyph + label + delta take a known small space; the path
	// fills the rest of the panel. A fixed floor of 16 keeps the path readable
	// on a narrow terminal, and the cap at width-20 keeps the header inside the
	// column on a wide one.
	pathBudget := max(m.transcriptWidth()-20, 16)
	return fmt.Sprintf("%s %s  %s%s", glyph, truncate(path, pathBudget), label, delta)
}

// renderHunks turns the unified diff hunks into styled transcript lines and
// caps the total at maxLines. It returns the rendered lines and how many were
// elided. Context lines are PaperSoft, removals Danger, additions Ochre — the
// same heat ramp the rest of the transcript uses, so the diff reads as part of
// the surface rather than a foreign block.
func (m *Model) renderHunks(hunks []diffHunk, width, maxLines int) ([]string, int) {
	// Total flat lines across all hunks; the elided count is total minus what
	// we render, and recomputing it cleanly here keeps the footer's "N more"
	// exact even when the per-hunk loop breaks early.
	total := 0
	for _, h := range hunks {
		total += len(h.lines)
	}
	var out []string
	shown := 0
	for _, h := range hunks {
		for _, l := range h.lines {
			if shown >= maxLines {
				break
			}
			prefix := "    "
			tone := ui.PaperDim
			text := l.text
			switch l.kind {
			case '+':
				prefix = "  + "
				tone = ui.Ochre
			case '-':
				prefix = "  - "
				tone = ui.Danger
			}
			// Wrap the diff line to the column width minus the prefix, so a
			// long line stays inside the transcript panel and the prefix
			// stays flush left.
			room := max(width-len(prefix), 6)
			wrapped := wrap(text, room)
			if len(wrapped) == 0 {
				wrapped = []string{""}
			}
			for i, w := range wrapped {
				if i == 0 {
					out = append(out, m.theme.Paint(tone, prefix+w))
				} else {
					out = append(out, m.theme.Paint(tone, strings.Repeat(" ", len(prefix))+w))
				}
			}
			shown++
		}
	}
	elided := max(total-shown, 0)
	return out, elided
}

// diffHunk is one run of changed lines with the surrounding context. The
// unified diff renderer produces one hunk per change region.
type diffHunk struct {
	lines []diffLine
}

type diffLine struct {
	kind byte // ' ', '-', '+'
	text string
}

// splitLines splits s into lines without a trailing empty element when s ends
// in a newline, matching how a file's lines are counted for the delta header.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	out := strings.Split(s, "\n")
	// A trailing newline produces a final "" element that is not a line; drop
	// it so "a\nb\n" counts as two lines, not three.
	if len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

// unifiedDiff produces the unified diff hunks for before → after. It is a
// classic LCS line diff: the DP table finds the longest common subsequence of
// lines, the backtrack walks it emitting context, removals and additions, and
// a final pass groups consecutive non-context lines into hunks with a one-line
// context border on each side so the operator can see where the change is.
//
// The algorithm is O(N*M) in time and space. The caller caps N and M at
// diffLineCap, so the cost is bounded for the transcript render path.
func unifiedDiff(before, after []string) []diffHunk {
	n, m := len(before), len(after)
	// dp[i][j] = LCS length of before[i:] and after[j:].
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if before[i] == after[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}

	// Walk the table producing a flat sequence of diff lines.
	var flat []diffLine
	i, j := 0, 0
	for i < n && j < m {
		if before[i] == after[j] {
			flat = append(flat, diffLine{kind: ' ', text: before[i]})
			i++
			j++
		} else if dp[i+1][j] >= dp[i][j+1] {
			flat = append(flat, diffLine{kind: '-', text: before[i]})
			i++
		} else {
			flat = append(flat, diffLine{kind: '+', text: after[j]})
			j++
		}
	}
	for ; i < n; i++ {
		flat = append(flat, diffLine{kind: '-', text: before[i]})
	}
	for ; j < m; j++ {
		flat = append(flat, diffLine{kind: '+', text: after[j]})
	}
	return groupHunks(flat)
}

// groupHunks splits a flat diff into hunks: each run of non-context lines
// becomes one hunk, surrounded by up to one context line on each side so the
// operator sees where the change sits. A long run of context between two
// changes splits them into separate hunks, the way a unified diff does.
func groupHunks(flat []diffLine) []diffHunk {
	const ctx = 1
	if len(flat) == 0 {
		return nil
	}
	var hunks []diffHunk
	var cur []diffLine
	flush := func() {
		if len(cur) > 0 {
			hunks = append(hunks, diffHunk{lines: append([]diffLine(nil), cur...)})
			cur = nil
		}
	}
	for i, l := range flat {
		if l.kind == ' ' {
			// Context inside a hunk: keep it if it borders a change (within ctx
			// lines), otherwise it ends the hunk.
			if len(cur) == 0 {
				// Leading context: only keep it if a change follows within ctx.
				if nextChange(flat, i, ctx) {
					cur = append(cur, l)
				}
				continue
			}
			if nextChange(flat, i, ctx) || prevChange(cur, ctx) {
				cur = append(cur, l)
			} else {
				flush()
			}
		} else {
			cur = append(cur, l)
		}
	}
	flush()
	return hunks
}

// nextChange reports whether a non-context line appears within max lines
// after i in flat.
func nextChange(flat []diffLine, i, max int) bool {
	for k := i + 1; k < len(flat) && k-i <= max; k++ {
		if flat[k].kind != ' ' {
			return true
		}
	}
	return false
}

// prevChange reports whether a non-context line appears within max lines of
// the end of cur (the hunk being built).
func prevChange(cur []diffLine, max int) bool {
	for k := len(cur) - 1; k >= 0 && len(cur)-1-k < max; k-- {
		if cur[k].kind != ' ' {
			return true
		}
	}
	return false
}
