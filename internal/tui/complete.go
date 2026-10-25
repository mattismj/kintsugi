package tui

// Completion popup for the input line (W3 item 3.4).
//
// The generator lives in package commands (commands.Suggest) beside the
// reference grammar, so what the popup offers is what ExpandReferences will
// accept. This file is the surface: a list engine modelled on connectView's
// (sel, scroll, moveSel, scrollBy, maxScrollFor) plus the two rules that keep a
// popup from becoming a second input mode:
//
//   - the key contract is rosterFocusedKeys': consume only the keys it owns
//     (↑ ↓ tab enter esc), report false for everything else so the keystroke
//     still reaches the input line;
//   - the popup follows the buffer. Every key that changes the draft refreshes
//     it *after* the key is applied — never before — so the list is always the
//     completion of what is actually in the box.
//
// Rendering splices rows over the bottom interior rows of the centre transcript
// panel (view.go). It must not be appended after the footer: fitScreen only
// clamps height, so anything below the footer is silently dropped, and
// transcriptBody's height argument has to stay untouched or the scroll
// invariants in maxScroll() stop holding.

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"kintsugi/internal/commands"
	"kintsugi/internal/ui"
)

// suggestMaxRows caps the popup: a peephole over the candidates, never a panel
// that eats the transcript.
const suggestMaxRows = 8

// completeView is the popup's state. cands is the generator's answer for the
// token the buffer held when it was refreshed; start and end are the rune span
// of that token, so accepting replaces exactly what was suggested for and the
// caret lands after the completion.
type completeView struct {
	cands       []commands.Candidate
	sel, scroll int
	start, end  int // rune offsets into the input buffer
}

// current is the selected candidate, if the list is not empty.
func (v *completeView) current() (commands.Candidate, bool) {
	if v.sel < 0 || v.sel >= len(v.cands) {
		return commands.Candidate{}, false
	}
	return v.cands[v.sel], true
}

// maxScrollFor is the furthest the list can scroll with rows visible.
func (v *completeView) maxScrollFor(rows int) int {
	return max(len(v.cands)-max(rows, 1), 0)
}

// clampSel keeps the selection inside the list.
func (v *completeView) clampSel() {
	if len(v.cands) == 0 {
		v.sel, v.scroll = 0, 0
		return
	}
	v.sel = clampInt(v.sel, 0, len(v.cands)-1)
}

// moveSel moves the selection by delta and keeps it inside the visible window.
func (v *completeView) moveSel(delta, rows int) {
	if len(v.cands) == 0 {
		v.sel, v.scroll = 0, 0
		return
	}
	v.sel = clampInt(v.sel+delta, 0, len(v.cands)-1)
	if v.sel < v.scroll {
		v.scroll = v.sel
	}
	if v.sel >= v.scroll+max(rows, 1) {
		v.scroll = v.sel - max(rows, 1) + 1
	}
	v.scroll = clampInt(v.scroll, 0, v.maxScrollFor(rows))
}

// scrollBy moves the window by delta rows (positive = down). The selection is
// pulled into the window so the renderer can never disagree with the handler.
func (v *completeView) scrollBy(delta, rows int) {
	v.scroll = clampInt(v.scroll+delta, 0, v.maxScrollFor(rows))
	if len(v.cands) == 0 {
		return
	}
	if v.sel < v.scroll {
		v.sel = v.scroll
	}
	if v.sel >= v.scroll+max(rows, 1) {
		v.sel = v.scroll + max(rows, 1) - 1
	}
	v.clampSel()
}

// ---------- state ----------

// suggestDismissal records the token an esc closed the popup on: the token's
// byte start and its text. The pair, not the text alone, is what makes the
// dismissal honest — two different tokens can read "@fi", and only the one the
// operator actually closed must stay closed.
type suggestDismissal struct {
	start int
	text  string
}

// suggestToken is the token under the caret as the dismissal records it.
func (m *Model) suggestToken() suggestDismissal {
	start, end := commands.TokenSpan(m.input, m.caretByte())
	return suggestDismissal{start: start, text: m.input[start:end]}
}

// dismissalHolds reports whether the popup must stay closed. It holds while the
// caret is in the token the operator dismissed, or in one that token grew or
// shrank into (typing and backspacing inside the same word) — fighting that
// would make esc feel broken. Any other token is a new decision.
//
// An empty dismissal is the "just completed" case: accept records the empty slot
// the caret landed in, so the popup does not reopen on the reference that was
// just finished, and the first character typed into that slot releases it.
func (m *Model) dismissalHolds() bool {
	d := m.suggestDismissed
	if d == nil {
		return false
	}
	cur := m.suggestToken()
	if cur.start != d.start {
		return false
	}
	if d.text == "" {
		return cur.text == ""
	}
	return strings.HasPrefix(cur.text, d.text) || strings.HasPrefix(d.text, cur.text)
}

// dismissalKey is the dismissal's part of the refresh memo.
func (m *Model) dismissalKey() string {
	if m.suggestDismissed == nil {
		return ""
	}
	return fmt.Sprintf("%d=%s", m.suggestDismissed.start, m.suggestDismissed.text)
}

// refreshSuggest recomputes the popup from the buffer and the caret. It is
// called after a key is applied, never before, and it is the only place the
// list is built: open, refresh and reopen are the same operation.
//
// The memo makes a key that changes nothing free: an `@file` completion walks
// the workspace, and a key that cannot change the answer must not pay for that
// walk again. It covers the popup's own state too (suggestVersion), because
// accept, dismiss and open change the answer without touching the buffer — a
// memo keyed on the buffer alone would wrongly skip those.
func (m *Model) refreshSuggest() {
	// A surface that owns the keyboard outranks the popup, and that check comes
	// before the memo: a modal opening must close the popup even though the
	// buffer it was built from has not changed.
	if m.wb != nil || m.connect != nil || m.approval != nil {
		m.suggest = nil
		return
	}
	memo := fmt.Sprintf("%d\x00%d\x00%s\x00%s", m.inputCursor, m.suggestVersion, m.input, m.dismissalKey())
	if m.suggestMemo == memo {
		return
	}
	m.suggestMemo = memo

	if m.dismissalHolds() {
		m.suggest = nil
		return
	}
	if strings.TrimSpace(m.input) == "" {
		m.suggest = nil
		return
	}
	cands := commands.Suggest(m.input, m.caretByte(), m.commandOptions())
	if len(cands) == 0 {
		m.suggest = nil
		return
	}
	start, end := commands.TokenSpan(m.input, m.caretByte())
	runeStart := len([]rune(m.input[:start]))
	runeEnd := runeStart + len([]rune(m.input[start:end]))

	prev := m.suggest
	sel, scroll := 0, 0
	if prev != nil {
		// Keep the operator's place when the list is a refinement of the same
		// token: re-selecting on every keystroke makes ↓ feel broken.
		sel, scroll = prev.sel, prev.scroll
	}
	m.suggest = &completeView{cands: cands, sel: max(sel, 0), scroll: max(scroll, 0), start: runeStart, end: runeEnd}
	m.suggest.clampSel()
}

// openSuggest is tab's "show me what is possible": it clears a dismissal and
// builds the list the buffer already qualifies for.
func (m *Model) openSuggest() bool {
	m.suggestDismissed = nil
	m.suggest = nil
	m.suggestVersion++
	m.refreshSuggest()
	return m.suggest != nil
}

// dismissSuggest closes the popup and remembers the token it was closed on, so
// a refresh triggered by a stray key does not immediately reopen it.
func (m *Model) dismissSuggest() {
	if m.suggest != nil {
		d := m.suggestToken()
		m.suggestDismissed = &d
	}
	m.suggest = nil
	m.suggestVersion++
}

// caretByte converts the caret's rune offset into the byte offset the commands
// package works in.
func (m *Model) caretByte() int {
	buf := m.inputRunes()
	at := clampInt(m.inputCursor, 0, len(buf))
	return len(string(buf[:at]))
}

// ---------- keys ----------

// suggestKeys owns ↑ ↓ tab enter esc while the popup is open and reports
// whether it consumed the key. Everything else returns false and falls through
// to the input line, which is the rosterFocusedKeys contract: a popup must
// never eat the operator's typing.
func (m *Model) suggestKeys(msg tea.KeyMsg) bool {
	v := m.suggest
	if v == nil {
		return false
	}
	rows := m.suggestWindow()
	switch msg.Type {
	case tea.KeyUp:
		v.moveSel(-1, rows)
		return true
	case tea.KeyDown:
		v.moveSel(1, rows)
		return true
	case tea.KeyPgUp:
		v.scrollBy(-rows, rows)
		return true
	case tea.KeyPgDown:
		v.scrollBy(rows, rows)
		return true
	case tea.KeyTab:
		m.acceptSuggest()
		return true
	case tea.KeyEnter:
		// alt+enter is still a newline: it is the multi-line input's key, and
		// stealing it while a popup happens to be open would be a surprise.
		if msg.Alt {
			return false
		}
		// Enter accepts a completion, but only one that completes something. A
		// candidate that is exactly the token already typed adds nothing, so
		// enter falls through and submits the line — typing "/connect" and
		// pressing enter must run /connect, not pad it with a space.
		if !m.suggestChangesToken() {
			return false
		}
		m.acceptSuggest()
		return true
	case tea.KeyEsc:
		m.dismissSuggest()
		return true
	}
	return false
}

// suggestChangesToken reports whether the selected candidate would change the
// token under the caret. It is what keeps enter honest: with nothing left to
// complete, enter means "send", exactly as it does without a popup.
func (m *Model) suggestChangesToken() bool {
	v := m.suggest
	if v == nil {
		return false
	}
	c, ok := v.current()
	if !ok || c.Value == "" {
		return false
	}
	buf := m.inputRunes()
	start := clampInt(v.start, 0, len(buf))
	end := clampInt(v.end, start, len(buf))
	return c.Value != string(buf[start:end])
}

// acceptSuggest replaces the token span with the selected candidate and puts
// the caret after it. A candidate that opens something — a reference keyword, a
// directory — leaves the popup open on the next step, because that is what the
// operator asked for; anything else finishes the token, so the popup closes and
// stays closed until the operator types again.
func (m *Model) acceptSuggest() bool {
	v := m.suggest
	if v == nil {
		return false
	}
	c, ok := v.current()
	if !ok || c.Value == "" {
		m.dismissSuggest()
		return false
	}
	buf := m.inputRunes()
	start := clampInt(v.start, 0, len(buf))
	end := clampInt(v.end, start, len(buf))

	insert := []rune(c.Value)
	if completeSpace(c.Value, runeAt(buf, end)) {
		insert = append(insert, ' ')
	}
	out := make([]rune, 0, len(buf)+len(insert))
	out = append(out, buf[:start]...)
	out = append(out, insert...)
	out = append(out, buf[end:]...)
	m.input = string(out)
	m.inputCursor = start + len(insert)

	m.suggest = nil
	m.suggestVersion++
	if strings.HasPrefix(c.Value, "@") || strings.HasSuffix(c.Value, "/") {
		// A keyword or a directory is a step, not an answer: reopen the popup on
		// whatever the completion just made possible.
		m.suggestDismissed = nil
	} else {
		// Finished. The empty slot the caret landed in must not immediately
		// reopen the list for the reference that was just completed — the next
		// character typed there releases it.
		m.suggestDismissed = &suggestDismissal{start: m.caretByte(), text: ""}
	}
	m.refreshSuggest()
	return true
}

// runeAt is the rune at index i, or 0 at the end of the buffer. The popup only
// needs "is there whitespace after the token".
func runeAt(buf []rune, i int) rune {
	if i < 0 || i >= len(buf) {
		return 0
	}
	return buf[i]
}

// completeSpace reports whether a completion should be followed by one space.
// A path that opens a directory keeps its slash and gets no space, and text
// that already ends the token (whitespace, sentence punctuation) needs none
// either.
func completeSpace(value string, next rune) bool {
	if value == "" || strings.HasSuffix(value, "/") {
		return false
	}
	switch next {
	case 0, ' ', '\t', '\n', '\r', ',', '.', ';', ':', ')', ']', '}', '!', '?':
		// 0 is the end of the buffer: the operator is going to keep typing, or
		// press enter — either way the token is finished, so a space is right.
		return next == 0
	}
	return true
}

// ---------- rendering ----------

// suggestRows is how many rows the popup occupies: the candidate window plus
// one counter row. The handler and the renderer both read it so their scrolling
// cannot drift.
func (m *Model) suggestRows() int {
	if m.suggest == nil || len(m.suggest.cands) == 0 {
		return 0
	}
	// Keep at least one transcript row: a popup that owns the whole panel is
	// not a peek, it is a takeover.
	avail := m.transcriptHeight() - 1
	if avail < 2 {
		return 0
	}
	want := minInt(len(m.suggest.cands), suggestMaxRows-1) + 1 // + the counter
	return minInt(want, avail)
}

// suggestWindow is the candidate rows the popup shows, without the counter.
func (m *Model) suggestWindow() int {
	if n := m.suggestRows(); n > 0 {
		return n - 1
	}
	return 0
}

// suggestLines renders the popup's content rows, each exactly width cells wide
// and already painted. It returns nil when there is nothing to draw, which is
// what keeps View() free of a special case.
func (m *Model) suggestLines(width int) []string {
	v := m.suggest
	if v == nil || width < 8 {
		return nil
	}
	rows := m.suggestWindow()
	if rows < 1 {
		return nil
	}
	v.clampSel()
	v.scroll = clampInt(v.scroll, 0, v.maxScrollFor(rows))
	if v.sel < v.scroll {
		v.scroll = v.sel
	}
	if v.sel >= v.scroll+rows {
		v.scroll = v.sel - rows + 1
	}

	// The hint column takes at most a third of the row, so a long description
	// can never crowd out the value the operator is choosing.
	hintW := minInt(max(width/3, 6), 24)
	valueW := max(width-2-2-hintW, 4) // marker + space, two-cell gap

	out := make([]string, 0, rows+1)
	end := minInt(v.scroll+rows, len(v.cands))
	for i := v.scroll; i < end; i++ {
		c := v.cands[i]
		mark, tone := " ", ui.Paper
		if i == v.sel {
			mark, tone = "▸", ui.Gold
		}
		value := clipShorten(c.Value, valueW)
		hint := clipShorten(c.Hint, hintW)
		// The row is composed as plain text first and clipped, then painted:
		// clipping after painting would cut a style's escape sequence in half.
		plain := mark + " " + value +
			strings.Repeat(" ", max(valueW-lipgloss.Width(value), 0)) + "  " + hint
		plain = clipCells(plain, width)
		pad := strings.Repeat(" ", max(width-lipgloss.Width(plain), 0))
		out = append(out, m.theme.Paint(tone, plain)+m.theme.Paint(ui.PaperSoft, pad))
	}
	counter := clipCells(m.suggestCounter(), width)
	out = append(out, m.theme.Paint(ui.PaperSoft,
		counter+strings.Repeat(" ", max(width-lipgloss.Width(counter), 0))))
	return out
}

// suggestCounter is the popup's status row: where the selection is, and the
// keys that act on it — a popup that does not say how to use it is a puzzle.
func (m *Model) suggestCounter() string {
	v := m.suggest
	if v == nil || len(v.cands) == 0 {
		return ""
	}
	kind := v.cands[max(v.sel, 0)].Kind
	return fmt.Sprintf("%d/%d · %s · tab enter accept · ↑↓ choose · esc close",
		v.sel+1, len(v.cands), kind)
}

// splicePopup overwrites the bottom body rows of a flat (borderless) panel with
// the popup's rows. It renders the popup as its own framed card so the popup
// stays visually separable even though the panel itself has no border. The
// panel's last line is the bottom rule, so the popup occupies the rows just
// above it.
func splicePopup(panel string, rows []string, innerW int, theme ui.Theme) string {
	if len(rows) == 0 || innerW < 1 {
		return panel
	}
	lines := strings.Split(panel, "\n")
	// The last line is the bottom rule; the popup sits above it, never over it.
	last := len(lines) - 2
	first := last - len(rows) + 1
	if first < 1 { // never over the eyebrow
		return panel
	}
	bar := theme.Paint(ui.Line, "│")
	for i, row := range rows {
		lines[first+i] = padCells(clipCells(row, innerW-2), innerW-2)
		if i == 0 {
			lines[first+i] = theme.Paint(ui.Line, "┌"+strings.Repeat("─", innerW-2)+"┐")
		} else {
			lines[first+i] = bar + padCells(clipCells(row, innerW-2), innerW-2) + bar
		}
	}
	// Close the popup card on the row after its content.
	if first+len(rows) < len(lines)-1 {
		lines[first+len(rows)] = theme.Paint(ui.Line, "└"+strings.Repeat("─", innerW-2)+"┘")
	}
	return strings.Join(lines, "\n")
}

// padCells pads s to width display cells with spaces. It never truncates; call
// clipCells first.
func padCells(s string, width int) string {
	if w := lipgloss.Width(s); w < width {
		return s + strings.Repeat(" ", width-w)
	}
	return s
}
