package tui

// The changes view (`/changes`): every file the agent touched this session,
// the current proposed state of each, on one screen.
//
// This is the surface Cline's proposed-changes panel answers: "what did the
// agent actually do to my workspace?" — not by scrolling tool-call cards,
// but as one reviewable list: per-file headers with line deltas, capped
// inline diffs in the gold seam style, and the aggregate at the top.
//
// The view holds the LATEST change per path (a file edited five times shows
// its final state against the session's first-seen content), with the total
// touch count — the answer to "what changed" wants current state, not
// history; history is the transcript and the session log.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbletea"

	"kintsugi/internal/tools"
	"kintsugi/internal/ui"
)

// changeRecord is the latest change to one path plus how many times it was
// touched: the view shows the final state, and the touch count keeps the
// history honest.
type changeRecord struct {
	change  tools.FileChange
	touches int
}

// changesView is the whole modal.
type changesView struct {
	scroll int
	w, h   int
}

// foldChanges folds one tool result's diff into the session's change set:
// the latest state per path wins, the touch count accumulates.
func (m *Model) foldChanges(diff []tools.FileChange) {
	if m.changeSet == nil {
		m.changeSet = map[string]*changeRecord{}
	}
	for _, fc := range diff {
		if fc.Path == "" {
			continue
		}
		if rec, ok := m.changeSet[fc.Path]; ok {
			// The latest state supersedes; the count keeps the history.
			rec.change = fc
			rec.touches++
			continue
		}
		m.changeSet[fc.Path] = &changeRecord{change: fc, touches: 1}
	}
}

// changeLines is the ordered set of per-file render lines: the aggregate
// header, then each file's header and capped diff.
func (m *Model) changeLines(width int) []string {
	if len(m.changeSet) == 0 {
		return []string{m.theme.Paint(ui.PaperSoft, "· nothing changed yet — file edits and patches appear here")}
	}
	paths := make([]string, 0, len(m.changeSet))
	for p := range m.changeSet {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var lines []string
	lines = append(lines, m.changeHeader(paths))
	for _, p := range paths {
		rec := m.changeSet[p]
		for _, l := range m.diffLines(rec.change, width) {
			lines = append(lines, l)
		}
		if rec.touches > 1 {
			lines = append(lines, m.theme.Paint(ui.PaperSoft,
				fmt.Sprintf("  · touched %d times this session — showing the latest", rec.touches)))
		}
		lines = append(lines, "")
	}
	return lines
}

// changeHeader is the aggregate line: files, total line delta, touches.
func (m *Model) changeHeader(paths []string) string {
	adds, dels, touches := 0, 0, 0
	for _, p := range paths {
		rec := m.changeSet[p]
		before := strings.Count(rec.change.Before, "\n") + 1
		after := strings.Count(rec.change.After, "\n") + 1
		if rec.change.Before == "" {
			before = 0
		}
		if rec.change.After == "" {
			after = 0
		}
		adds += after
		dels += before
		touches += rec.touches
	}
	verb := "changed"
	if dels > adds {
		verb = "removed"
	} else if adds > dels {
		verb = "added"
	}
	return m.theme.Paint(ui.Gold, fmt.Sprintf(
		"%d file(s) %s · %d → %d line(s) · %d touch(es)", len(paths), verb, dels, adds, touches))
}

// openChanges opens the view.
func (m *Model) openChanges() {
	m.changes = &changesView{}
}

// ---------- input ----------

// handleChangesKey owns the keyboard while the view is open.
func (m *Model) handleChangesKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	v := m.changes
	if v == nil {
		return m, nil
	}
	if msg.Type == tea.KeyCtrlC {
		m.changes = nil
		return m, tea.Quit
	}
	switch msg.String() {
	case "esc", "q":
		m.changes = nil
	case "up", "k":
		v.scroll = max(v.scroll-1, 0)
	case "down", "j":
		v.scroll++
	case "pgup":
		v.scroll = max(v.scroll-changesRows(max(m.h, 8)), 0)
	case "pgdown":
		v.scroll += changesRows(max(m.h, 8))
	case "y":
		// Copy the whole change set as text: a review is a thing an operator
		// pastes into a ticket, and OSC 52 is the only copy that survives SSH.
		m.copyChanges()
	}
	return m, nil
}

// handleChangesMouse implements wheel scrolling.
func (m *Model) handleChangesMouse(msg tea.MouseMsg) {
	v := m.changes
	if v == nil || msg.Action != tea.MouseActionPress {
		return
	}
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		v.scroll = max(v.scroll-3, 0)
	case tea.MouseButtonWheelDown:
		v.scroll += 3
	}
}

// changesRows is how many lines the panel body shows.
func changesRows(screenH int) int { return max(screenH-1-2-3, 1) }

// ---------- rendering ----------

// renderChanges draws the whole surface: the panel plus its footer rows.
func (m *Model) renderChanges(width, height int, theme ui.Theme) string {
	v := m.changes
	if width < 24 {
		width = 24
	}
	if height < 8 {
		height = 8
	}
	v.w, v.h = width, height

	footer := m.changesFooter(width, theme)
	panelH := height - len(footer)
	if panelH < 5 {
		panelH = 5
	}
	bodyH := panelH - 3
	lines := m.changeLines(max(width-2, 8))
	v.scroll = clampInt(v.scroll, 0, max(len(lines)-bodyH, 0))
	end := minInt(v.scroll+bodyH, len(lines))
	visible := lines[v.scroll:end]
	for len(visible) < bodyH {
		visible = append(visible, "")
	}
	panel := theme.Panel("// changes · this session", strings.Join(visible, "\n"), width, panelH)
	out := strings.Split(panel, "\n")
	out = append(out, footer...)
	if len(out) > height {
		out = out[:height]
	}
	return strings.Join(out, "\n")
}

// changesFooter is the key legend.
func (m *Model) changesFooter(width int, theme ui.Theme) []string {
	keys := "[↑/↓] scroll · [y] copy · [esc] close"
	hint := "latest state per file · the transcript and session log hold the history"
	return []string{
		theme.Paint(ui.PaperSoft, clipShorten(keys, width)),
		theme.Paint(ui.PaperSoft, clipShorten(hint, width)),
	}
}

// copyChanges copies the whole change set as plain text through the OSC 52
// path, best effort: a review that cannot be pasted into a ticket is half a
// review.
func (m *Model) copyChanges() {
	var b strings.Builder
	for _, l := range m.changeLines(400) {
		b.WriteString(stripSGR(l))
		b.WriteString("\n")
	}
	m.sendToClipboard(strings.TrimRight(b.String(), "\n"), "changes")
}

// stripSGR removes SGR escape sequences from one styled line, for the
// clipboard payload: a ticket wants the text, not the gold.
func stripSGR(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			if n := ansiSequenceLen(s[i:]); n > 0 {
				i += n
				continue
			}
			i++
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
