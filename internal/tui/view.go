package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"kintsugi/internal/tools"
	"kintsugi/internal/ui"
)

func (m *Model) View() string {
	if m.w <= 0 {
		m.w = 80
	}
	if m.h <= 0 {
		m.h = 24
	}
	header := m.headerLine()
	leftW, rightW, centerW, bodyH, showLeft, showRight := m.layout()

	// The connect panel is a full-screen modal like the workbench: it replaces
	// every column and keeps only the header above it.
	if m.connect != nil {
		body := m.connect.renderConnect(m.w, max(m.h-1, 8), m.theme)
		return fitScreen(lipgloss.JoinVertical(lipgloss.Left, header, body), m.h)
	}

	// The model picker is the same shape of modal: the whole screen, the
	// header above it, nothing of the transcript behind it.
	if m.picker != nil {
		body := m.picker.renderModelPicker(m.w, max(m.h-1, 8), m.theme)
		return fitScreen(lipgloss.JoinVertical(lipgloss.Left, header, body), m.h)
	}

	// The refusal workbench is the same shape of modal.
	if m.refusal != nil {
		body := m.renderRefusal(m.w, max(m.h-1, 8), m.theme)
		return fitScreen(lipgloss.JoinVertical(lipgloss.Left, header, body), m.h)
	}

	// The changes view is the same shape of modal.
	if m.changes != nil {
		body := m.renderChanges(m.w, max(m.h-1, 8), m.theme)
		return fitScreen(lipgloss.JoinVertical(lipgloss.Left, header, body), m.h)
	}

	if m.wb != nil {
		body := m.theme.Panel(m.workbenchTitle(), m.workbenchBody(m.w-2, bodyH-3), m.w, bodyH)
		return fitScreen(lipgloss.JoinVertical(lipgloss.Left, header, body, m.workbenchFooter()), m.h)
	}

	// Panel reserves one row for the eyebrow and one for the bottom rule, so a
	// body of bodyH-2 rows is exactly what it displays. Feeding it more drops the
	// head; feeding it less pads (workbenchBody already works this way). There
	// are no side borders, so the body width is the full column width.
	bodyW := bodyH - 2
	center := m.theme.Panel(" ", m.transcriptBody(centerW, bodyW), centerW, bodyH)
	// The completion popup is spliced over the bottom interior rows of the
	// transcript panel (W3 item 3.4). It is deliberately not appended below the
	// footer: fitScreen only clamps height, so anything after the footer is
	// dropped, and transcriptBody's height argument must stay bodyW or the
	// scroll clamp in maxScroll() stops describing what is on screen.
	if rows := m.suggestLines(centerW); len(rows) > 0 {
		center = splicePopup(center, rows, centerW, m.theme)
	}
	parts := []string{center}
	if showLeft {
		left := m.theme.Panel("// 00 work", m.activityRailBody(leftW, bodyW), leftW, bodyH)
		parts = append([]string{left}, parts...)
	}
	if showRight {
		tab := m.inspectTab
		if tab == "" {
			tab = "work"
		}
		right := m.theme.Panel("// 03 inspector · "+tab, m.inspectorBody(rightW, bodyW), rightW, bodyH)
		parts = append(parts, right)
	}
	body := lipgloss.JoinHorizontal(lipgloss.Top, join(parts, "   ")...)
	footer := m.footer()

	out := lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
	// Center the whole layout when the terminal is wider than it: negative space
	// on each side is the "clean" look, and a left-hugging block on a wide
	// monitor reads as a bug, not a choice. The width is measured from the
	// joined body, not just the transcript column, so a side panel stays
	// centered with it.
	totalW := m.w
	if showLeft {
		totalW = leftW + 3 + centerW
	}
	if showRight {
		totalW = centerW + 3 + rightW
	}
	if m.w > totalW+2 && !showLeft && !showRight {
		pad := (m.w - centerW) / 2
		out = prefixLines(out, strings.Repeat(" ", pad))
	}
	return fitScreen(out, m.h)
}

// prefixLines indents every line by pad spaces, preserving the block's width so
// a later fitScreen still measures the original columns correctly.
func prefixLines(s string, pad string) string {
	if pad == "" {
		return s
	}
	lines := strings.Split(s, "\n")
	for i := range lines {
		if lines[i] != "" {
			lines[i] = pad + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

func fitScreen(out string, height int) string {
	lines := strings.Split(out, "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	return strings.Join(lines, "\n")
}

func join(parts []string, sep string) []string {
	out := make([]string, 0, len(parts)*2)
	for i, p := range parts {
		if i > 0 {
			out = append(out, sep)
		}
		out = append(out, p)
	}
	return out
}

func (m *Model) headerLine() string {
	// A single quiet line: the product word, the model, and the live state glyph.
	// Everything else is reachable, not always-on.
	left := m.theme.Paint(ui.Gold, "kintsugi") +
		m.theme.Paint(ui.PaperSoft, " · "+m.viewModel())
	right := m.theme.RenderIndicator(m.mascotState()) + " "
	line := left + "  " + right
	return fitWidth(line, m.w)
}

// modeTone is the tone of the mode word in the header. The default posture
// is dim like the rest of the header; any other posture is the accent, so a
// changed mode is visible without reading it.
func (m *Model) modeTone() ui.Token {
	if m.currentMode() == tools.ModeDefault {
		return ui.PaperDim
	}
	return ui.Gold
}

// roleName is the active role with the empty case spelled out, so the header and
// the inspector always render a glyph for something.
func (m *Model) roleName() string {
	if r := strings.ToLower(strings.TrimSpace(m.role)); r != "" {
		return r
	}
	return "default"
}

// transcriptKey is the fingerprint the transcript memo validates against. It is
// derived from the model every call rather than remembered, so a mutation that
// forgets to announce itself still misses the cache: the derived values move.
//
// identity covers the two things the transcript renders live rather than from a
// stored block: the compact banner's model/persona line and the quickstart card's
// text. Both follow a recompile, and a stale one would show the operator the
// model they just switched away from.
//
// revision covers what no fingerprint can see cheaply — file-backed state the
// quickstart reads on every render (CONTEXT.md sections, AGENTS.md, the KB
// binding). The sites that reload it bump the revision.
type transcriptKey struct {
	width    int
	blocks   int
	bytes    int // total block text+title bytes, so a streamed delta invalidates
	flags    int // busy, approval present
	bootStep int
	stream   int
	apprSig  string
	identity string
	// liveBytes is the live tool block's content size: a streamed line moves
	// it, so the memo cannot serve a stale window.
	liveBytes int
	revision  int
}

// transcriptFingerprint describes the transcript's inputs right now.
func (m *Model) transcriptFingerprint(width int) transcriptKey {
	k := transcriptKey{
		width: width, blocks: len(m.blocks), stream: m.stream,
		revision: m.revision, identity: m.viewModel() + "\x00" + m.personaName(),
	}
	for i := range m.blocks {
		k.bytes += len(m.blocks[i].text) + len(m.blocks[i].title)
		if m.blocks[i].expanded {
			// One byte per open block: ctrl+t must invalidate the memo, and
			// the flag is otherwise invisible to the fingerprint.
			k.bytes++
		}
	}
	if m.liveTool != nil {
		k.flags |= flagLiveTool
		k.liveBytes = len(m.liveTool.label) + m.liveTool.dropped
		for _, l := range m.liveTool.lines {
			k.liveBytes += len(l)
		}
	}
	if m.busy {
		k.flags |= flagBusy
	}
	if m.approval != nil {
		k.flags |= flagApproval
		k.apprSig = m.approval.tool + "\x00" + m.approval.detail
	}
	return k
}

const (
	flagBusy = 1 << iota
	flagLiveTool
	flagApproval
)

// transcriptLines assembles the whole transcript for a content width, memoised.
// Rendering slices it from the bottom by m.scroll, and maxScroll counts the same
// slice, so the clamp and the view can never disagree — and neither re-wraps when
// only the cursor moved.
//
// The returned slice is shared with the cache. Callers must not append into it:
// transcriptBody copies before padding for exactly that reason.
func (m *Model) transcriptLines(width int) []string {
	key := m.transcriptFingerprint(width)
	if m.transcriptMemo != nil && m.transcriptMemoKey == key {
		return m.transcriptMemo
	}
	lines := m.buildTranscriptLines(width)
	m.transcriptMemo = lines
	m.transcriptMemoKey = key
	return lines
}

// buildTranscriptLines is the uncached assembly.
func (m *Model) buildTranscriptLines(width int) []string {
	if width < 4 {
		width = 4
	}
	var lines []string
	if m.approval != nil {
		tone := ui.Gold
		if destructive(m.approval.detail) {
			tone = ui.Danger
		}
		// The approval is informed: the proposed change rides the request, so
		// [y]/[n] is answered with the diff in front of the operator, not a
		// one-line summary. The card is capped like every diff render, and a
		// destructive command keeps the danger tone regardless.
		body := truncate(oneLine(m.approval.tool+" "+m.approval.detail), width-4)
		for _, fc := range m.approval.changes {
			for _, l := range m.diffLines(fc, max(width-4, 20)) {
				body += "\n" + l
			}
		}
		body += "\n[y] approve   [n] deny"
		lines = append(lines, strings.Split(m.theme.Card(
			m.cardTitle(ui.MascotAuditing, tone, "// approval · pending"),
			body, width, tone), "\n")...)
		lines = append(lines, "")
	}
	for i := range m.blocks {
		lines = append(lines, m.blockLines(i, m.blocks[i], width)...)
	}
	// The running tool's live output: the command, a rolling window of its
	// streamed lines, and the count of what rolled past. Replaced by the
	// final tool card when the tool returns.
	lines = append(lines, m.liveToolLines(width)...)
	if m.busy && m.stream >= 0 && m.stream < len(m.blocks) {
		lines = append(lines, m.theme.Paint(ui.Gold, "▌"))
	}
	if len(lines) == 0 {
		lines = []string{m.theme.Paint(ui.PaperSoft, "· type a task, or /help")}
	}
	return lines
}

// invalidateTranscript drops the memo. It is the cheap, explicit path: a caller
// that knows it changed something the fingerprint cannot see (a recompile, a
// sprite flag) bumps the revision rather than reasoning about the key.
func (m *Model) invalidateTranscript() {
	m.transcriptMemo = nil
	m.revision++
}

func (m *Model) transcriptBody(width, height int) string {
	lines := m.transcriptLines(width)

	// A pinned viewport is positioned by its anchor, not by its distance from
	// the bottom: while the model streams, lines are appended below the window
	// and a bottom-relative scroll would drag the text out from under the
	// operator. Deriving scroll here — the one place a scroll becomes a window —
	// keeps maxScroll the single clamp, the same way workbenchBody does it.
	if pinned, ok := m.anchorScroll(width, height); ok {
		m.scroll = pinned
	}

	end := len(lines) - m.scroll
	if end > len(lines) {
		end = len(lines)
	}
	start := end - height
	if start < 0 {
		start = 0
	}
	if end < start {
		end = start
	}
	visible := lines[start:end]

	// Pad to exactly `height` rows, copying into a fresh slice rather than
	// appending to `visible`: transcriptLines hands back a slice shared with the
	// memo, and appending into its spare capacity would write blank rows into the
	// cached transcript that the next frame reads back.
	if len(visible) < height {
		padded := make([]string, height)
		copy(padded, visible)
		visible = padded
	}
	return strings.Join(visible, "\n")
}

func (m *Model) blockLines(idx int, b block, width int) []string {
	switch b.kind {
	case blockBanner:
		// The block stores the full nameplate; a narrow transcript draws the
		// compact one instead, so the wordmark never crowds the panels at
		// 80 columns or after a resize down.
		text := b.text
		var out []string
		for _, line := range strings.Split(text, "\n") {
			out = append(out, wrap(line, width)...)
		}
		out = append(out, "")
		return out

	case blockUser:
		prefix := m.theme.Paint(ui.Gold, "operator ▸ ")
		body := wrap(b.text, width-lipgloss.Width(prefix))
		if len(body) == 0 {
			body = []string{""}
		}
		out := []string{prefix + body[0]}
		for _, line := range body[1:] {
			out = append(out, strings.Repeat(" ", lipgloss.Width(prefix))+line)
		}
		return out

	case blockThinking:
		// The thinking renders dim, above the answer — the operator's data,
		// never hidden. A live block shows the tail of the reasoning; a
		// finished block collapses to a few lines with the count, and ctrl+t
		// opens the full text. Nothing is ever discarded.
		prefix := m.theme.Paint(ui.PaperSoft, "◇ thinking · ")
		body := wrap(b.text, max(width-lipgloss.Width(prefix), 4))
		if len(body) == 0 {
			return nil
		}
		limit := thinkingCollapsedLines
		live := idx == m.thinkIdx
		if live {
			limit = thinkingLiveLines
		}
		if b.expanded {
			limit = len(body)
		}
		start := 0
		if len(body) > limit {
			start = len(body) - limit
		}
		hidden := start
		out := []string{prefix + body[start]}
		for _, l := range body[start+1:] {
			out = append(out, strings.Repeat(" ", lipgloss.Width(prefix))+l)
		}
		if hidden > 0 {
			hint := fmt.Sprintf("  · %d earlier thinking line(s) · ctrl+t to expand", hidden)
			out = append(out, m.theme.Paint(ui.PaperSoft, clipCells(hint, width)))
		}
		return out

	case blockAssistant:
		return wrap(b.text, width)

	case blockTool:
		line := m.theme.Paint(b.tone, b.text)
		out := wrap(line, width)
		if b.title != "" {
			for _, l := range wrap(m.theme.Paint(ui.PaperSoft, "  "+b.title), width) {
				out = append(out, l)
			}
		}
		// Inline diff for file-authoring tools (B1). An operator reading the
		// transcript sees what changed in the same surface they read the
		// summary line — not a separate log to scroll to. The diff is capped
		// per file so a 400-line rewrite does not push the conversation out of
		// the viewport; the cap names how many lines were elided so the
		// operator knows there is more to see in the session log.
		for _, fc := range b.diff {
			out = append(out, m.diffLines(fc, width)...)
		}
		return out

	case blockNote:
		tone := ui.PaperSoft
		if strings.HasPrefix(b.text, "✗") || strings.HasPrefix(b.text, "✸") {
			tone = ui.Danger
		}
		return wrap(m.theme.Paint(tone, b.text), width)

	case blockCard:
		body := b.text
		// The repair cards — a refusal intercepted and resumed, prior work
		// re-injected, a frame applied, a drift recorded — carry the seam: a
		// rule broken in the middle and repaired with gold. The repair being
		// visible is the product's thesis, so the repaired events wear the
		// brand mark itself.
		if isRepairCard(b.title) {
			body = m.theme.Seam(max(width-4, 5)) + "\n" + body
		}
		// addCard already prefixed the title with the identity glyph for the
		// state the card reports; render it verbatim so the mark appears once.
		return strings.Split(m.theme.Card(b.title, body, width, b.tone), "\n")
	}
	return nil
}

// liveToolLines is the running tool's live output block: the working-state
// glyph, the tool and its label, then the rolling window of streamed lines.
// The lines are dim — they are provisional, and the final card (with the
// capped full output) is the record that stays.
func (m *Model) liveToolLines(width int) []string {
	lt := m.liveTool
	if lt == nil {
		return nil
	}
	var lines []string
	head := m.theme.RenderIndicator(ui.MascotWorking) + " " +
		m.theme.Paint(ui.Gold, "bash") +
		m.theme.Paint(ui.PaperSoft, " · "+truncate(orDash(lt.label), max(width-8, 8)))
	lines = append(lines, head)
	for _, l := range lt.lines {
		for _, w := range wrap(m.theme.Paint(ui.PaperDim, l), max(width-2, 6)) {
			lines = append(lines, "  "+w)
		}
	}
	if lt.dropped > 0 {
		lines = append(lines, m.theme.Paint(ui.PaperSoft,
			fmt.Sprintf("  · %d earlier line(s) — the full output arrives with the result", lt.dropped)))
	}
	return lines
}

// quickstartBody draws the startup card: the identity rail (the state mark and
// its activity word), then the getting-started ladder. Glyph vocabulary only —
// the state mark is the identity; a quiet opening is the design. Mono is the
// same minus the colour.
func (m *Model) quickstartBody(width int) string {
	info := strings.Split(m.quickstartText(), "\n")
	return strings.Join(append([]string{m.identityRail(), ""}, info...), "\n")
}

// pillarDetailLines is the expanded pillar block for the startup card: one line
// per pillar, mark and name in tone, value in paper-soft, truncated to the column.
func (m *Model) pillarDetailLines(width int) []string {
	return []string{
		m.pillarLine(pillarContext, ui.Gold, "context", m.contextPillar(), width),
		m.pillarLine(pillarScope, ui.Gold, "scope", m.scopePillar(), width),
		m.pillarLine(pillarRefusal, m.refusalTone(), "refusal", m.refusalPillar(), width),
		m.pillarLine(pillarPrivacy, ui.OK, "privacy", m.privacyPillar(), width),
	}
}

// activityRailBody is the left column: the identity rail on top, then the
// tasks, agents and background roster underneath. The creature is gone — the
// state mark carries the identity in one row, and the activity is the body.
func (m *Model) activityRailBody(width, height int) string {
	var lines []string
	lines = append(lines, m.identityRail())
	lines = append(lines, "")
	lines = append(lines, m.activityLines(width)...)
	for len(lines) < height {
		lines = append(lines, "")
	}
	return strings.Join(lines[:minInt(len(lines), height)], "\n")
}

// activityLines are the tasks, agents and background roster the rail carries
// under the creature. It is the body the old `// 01 tasks` panel rendered, minus
// its own padding and truncation — the rail does that once, around everything.
func (m *Model) activityLines(width int) []string {
	var lines []string
	if len(m.todos) > 0 {
		for i, t := range m.todos {
			if i >= 8 {
				break
			}
			state := t.Status
			switch state {
			case "in_progress":
				state = "running"
			case "completed":
				state = "done"
			case "cancelled":
				state = "paused"
			}
			glyph, tone := ui.StatusTone(state)
			label := ellipsisWords(t.Content, max(width-3, 4))
			lines = append(lines, m.theme.Paint(tone, glyph)+" "+m.theme.Paint(ui.PaperDim, label))
		}
	} else {
		if len(m.tasks) == 0 {
			lines = append(lines, m.theme.Paint(ui.PaperSoft, "· none yet"))
		}
		for i, t := range m.tasks {
			if i >= 8 {
				break
			}
			glyph, tone := ui.StatusTone(t.state)
			label := ellipsisWords(t.label, max(width-3, 4))
			lines = append(lines, m.theme.Paint(tone, glyph)+" "+m.theme.Paint(ui.PaperDim, label))
		}
	}
	lines = append(lines, "")
	lines = append(lines, m.theme.Paint(ui.PaperSoft, "agents"))
	coderState := "idle"
	if m.busy {
		coderState = "running"
	}
	lines = append(lines, statusRow(m.theme, width, "coder", coderState))
	lines = append(lines, statusRow(m.theme, width, "advisor", "idle"))
	lines = append(lines, statusRow(m.theme, width, "opsec", "idle"))

	// Roster section (M10-D): background subagent runs.
	if m.rosterVisible() {
		lines = append(lines, "")
		lines = append(lines, m.rosterBody(width)...)
	}
	return lines
}

func statusRow(th ui.Theme, width int, name, state string) string {
	g, tone := ui.StatusTone(state)
	room := max(width-2, 4) // the glyph and the space after it
	for _, suffix := range []string{" (" + state + ")", " " + state} {
		avail := room - lipgloss.Width(suffix)
		if avail < 3 || lipgloss.Width(name) > avail {
			continue
		}
		pad := avail - lipgloss.Width(name)
		return th.Paint(tone, g) + " " + th.Paint(ui.PaperDim, name) +
			strings.Repeat(" ", pad) + th.Paint(ui.PaperSoft, suffix)
	}
	return th.Paint(tone, g) + " " + th.Paint(ui.PaperDim, ellipsisWords(name, room))
}

// ellipsisWords cuts s to width cells at a word boundary where one exists
// and marks the cut with an ellipsis. A single word longer than the width is
// cut by cells — there is no boundary to cut at.
func ellipsisWords(s string, width int) string {
	s = strings.TrimSpace(s)
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	line := ""
	for _, w := range strings.Fields(s) {
		cand := w
		if line != "" {
			cand = line + " " + w
		}
		if lipgloss.Width(cand)+1 > width {
			break
		}
		line = cand
	}
	if line != "" {
		return line + "…"
	}
	// One word that does not fit: cut it by cells.
	runes := []rune(s)
	head := ""
	for i, r := range runes {
		cand := head + string(r)
		if lipgloss.Width(cand)+1 > width {
			break
		}
		head = cand
		_ = i
	}
	return head + "…"
}

func (m *Model) inspectorBody(width, height int) string {
	// The tab names the body; the panel field only says whether it is shown.
	if m.inspectTab == "harness" {
		return m.harnessInspectorBody(width, height)
	}
	// The default tab: work. (/prompt shows the harness detail by name.)
	return m.workInspectorBody(width, height)
}

// workInspectorBody answers what the session is doing: the task, the files
// it touched (the same change set /changes renders, so the surfaces agree),
// the spend, and the refusal/repair counters. This is the default because it
// is what an operator glances at; the manifest detail is the harness tab.
func (m *Model) workInspectorBody(width, height int) string {
	var lines []string
	lines = append(lines, m.identityRail())
	task := "· none yet"
	if len(m.tasks) > 0 {
		last := m.tasks[len(m.tasks)-1]
		state := last.state
		if state == "" {
			state = "running"
		}
		task = last.label + " · " + state
	}
	lines = append(lines, m.theme.Paint(ui.PaperSoft, truncate("task    "+task, width)))
	changed := len(m.changeSet)
	verb := "touched"
	var paths []string
	for p := range m.changeSet {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	files := "none"
	if changed > 0 {
		files = fmt.Sprintf("%d file(s)", changed)
		if changed == 1 {
			verb = "touched"
		}
	}
	lines = append(lines, m.theme.Paint(ui.PaperSoft, truncate("files   "+verb+" "+files, width)))
	for i, p := range paths {
		if i >= 8 {
			lines = append(lines, m.theme.Paint(ui.PaperSoft, truncate(fmt.Sprintf("        … and %d more", changed-8), width)))
			break
		}
		lines = append(lines, m.theme.Paint(ui.PaperDim, truncate("        "+p, width)))
	}
	lines = append(lines, m.theme.Paint(ui.PaperSoft, truncate(
		fmt.Sprintf("tokens  %d · refusals %d · repairs %d", m.quotaUsed, m.refusals, m.repairs), width)))
	quota := "n/a"
	if m.quotaLimit > 0 {
		quota = fmt.Sprintf("%d/%d", m.quotaUsed, m.quotaLimit)
	}
	lines = append(lines, m.theme.Paint(ui.PaperSoft, truncate("quota   "+quota, width)))
	elapsed := time.Since(m.start).Round(time.Second)
	lines = append(lines, m.theme.Paint(ui.PaperSoft, truncate(fmt.Sprintf("elapsed %s · tab shows harness detail", elapsed), width)))
	lines = append(lines, "")
	pillars := m.pillarSummary(width)
	for len(lines)+len(pillars) > height && len(lines) > 1 {
		lines = lines[:len(lines)-1]
	}
	for len(lines) < height-len(pillars) {
		lines = append(lines, "")
	}
	lines = append(lines, pillars...)
	if len(lines) > height {
		lines = lines[:height]
	}
	return strings.Join(lines, "\n")
}

// harnessInspectorBody is the manifest/layer detail — the inspector's whole
// body before the work tab existed, unchanged.
func (m *Model) harnessInspectorBody(width, height int) string {
	if m.comp == nil {
		return "· no prompt compiled"
	}
	manifest := m.comp.Manifest
	total := manifest.Totals.Tokens
	budget := manifest.Totals.Budget
	frac := 0.0
	if budget > 0 {
		frac = float64(total) / float64(budget)
	}
	barW := max(minInt(width-12, 12), 5)

	var lines []string
	roleAt := -1
	// The status rail opens the inspector: the identity state, its label and
	// the mark that carries the tone. The layer table stays below it, and the
	// pillars close it. The role row only joins when the panel has spare space,
	// so adding it can never push a stat out.
	lines = append(lines, m.identityRail())
	lines = append(lines, m.theme.Paint(ui.PaperSoft, fmt.Sprintf("window  %d tok  %d%%", total, int(frac*100))))
	lines = append(lines, "        "+m.theme.Progress(frac, barW))
	lines = append(lines, "")
	for _, l := range manifest.Layers {
		label := l.ID
		if l.Dropped {
			label += " (drop)"
		}
		lines = append(lines, fmt.Sprintf("%-9s %5d", label, l.Tokens))
	}
	lines = append(lines, "")
	personaLine := "persona " + manifest.Persona.Name
	if manifest.Persona.Rule != "" {
		personaLine += " (" + manifest.Persona.Rule + ")"
	}
	lines = append(lines, m.theme.Paint(ui.PaperSoft, truncate(personaLine, width)))
	roleLine := "role    " + ui.RoleGlyph(m.roleName()) + " " + m.roleName()
	if width >= lipgloss.Width(roleLine) {
		roleAt = len(lines)
		lines = append(lines, m.theme.Paint(ui.PaperSoft, roleLine))
	}
	// The mode is the posture the operator is in: it sits next to the role,
	// so a glance at the inspector says what the session may do.
	lines = append(lines, m.theme.Paint(ui.PaperSoft, truncate("mode    "+m.modeLabel(), width)))
	agents := "none"
	if _, p := m.app.AgentsMD(); p != "" {
		agents = p
	}
	lines = append(lines, m.theme.Paint(ui.PaperSoft, truncate("agents  "+agents, width)))
	cache := "prefix stable"
	if stale := m.stalePaths(); len(stale) > 0 {
		cache = "stale · recompile on send"
	}
	lines = append(lines, m.theme.Paint(ui.PaperSoft, "cache   "+cache))
	if m.cacheRead > 0 || m.cacheWritten > 0 {
		lines = append(lines, m.theme.Paint(ui.PaperSoft, truncate(
			fmt.Sprintf("        read %d · written %d", m.cacheRead, m.cacheWritten), width)))
	}
	lines = append(lines, m.theme.Paint(ui.PaperSoft, "stack   "+shortHash(manifest.StackHash)))
	ckpt := fmt.Sprintf("ckpt    %d", m.checkpoints)
	if m.rewoundTo != "" {
		ckpt = fmt.Sprintf("ckpt    %d · %s", m.checkpoints, m.rewoundTo)
	}
	lines = append(lines, m.theme.Paint(ui.PaperSoft, truncate(ckpt, width)))
	quota := "n/a"
	if m.quotaLimit > 0 {
		quota = fmt.Sprintf("%d/%d", m.quotaUsed, m.quotaLimit)
	}
	lines = append(lines, m.theme.Paint(ui.PaperSoft, "quota   "+quota))
	lines = append(lines, "")

	// The pillars close the inspector (M13). The creature moved to the left rail,
	// which freed this block — and the identity line at the top already names the
	// state, so repeating it here would be the same information twice. What goes
	// in the space instead is the reason the identity exists: the four promises,
	// each as a live readout.
	pillars := m.pillarSummary(width)

	// If the panel is tight, the role row is the first thing to go: the header
	// already carries the same glyph, and the stats below it are not repeated
	// anywhere else in the view.
	if roleAt >= 0 && len(lines)+len(pillars) > height {
		lines = append(lines[:roleAt], lines[roleAt+1:]...)
	}

	// The pillar block is reserved before padding, so a long manifest can never
	// push it out. It is the last thing in the column and the first thing an
	// operator reads when they want to know what the session is committed to.
	body := max(height-len(pillars), 0)
	if len(lines) > body {
		lines = lines[:body]
	}
	for len(lines) < body {
		lines = append(lines, "")
	}
	lines = append(lines, pillars...)
	if len(lines) > height {
		lines = lines[:height]
	}
	return strings.Join(lines, "\n")
}

func (m *Model) footer() string {
	var lines []string
	if m.approval != nil {
		lines = append(lines, m.theme.Paint(ui.Warn, "[y] approve   [n] deny   · approval required"))
	} else {
		// No leading prompt — just the input and its trailing caret, like a
		// chat input. The caret runs hot (Ochre) while the model works and
		// cools to amber at rest.
		caretTone := ui.Gold
		if m.busy || m.stream >= 0 {
			caretTone = ui.Ochre
		}
		lines = append(lines, m.inputLines("", caretTone)...)
		var hints []string
		if m.busy {
			// The footer status chip: the at-a-glance state mark and the
			// phase-locked verb. Token and cache detail stay in the inspector,
			// so the chip keeps one line free for the operator's next action.
			hints = append(hints, m.theme.RenderIndicator(m.mascotState())+" "+
				m.busyLabel()+" · ctrl+c abort")
		}
		if m.scroll > 0 {
			// A pinned viewport says so, and says whether anything arrived below
			// it: "new output" is the useful half of the message, because it
			// tells the operator work is happening without moving their view.
			scrolled := fmt.Sprintf("scrolled %d · esc to bottom", m.scroll)
			if m.anchor.valid {
				scrolled = fmt.Sprintf("pinned · line %d · esc to bottom", m.anchor.line+1)
			}
			if n := m.newLinesBelow(); n > 0 {
				scrolled += fmt.Sprintf(" · %d new", n)
			}
			if !m.busy {
				scrolled += " · wheel/PgUp/PgDn"
			}
			hints = append(hints, scrolled)
		}
		if len(hints) == 0 {
			hints = append(hints, "/help · tab completes")
		}
		// One line: the input, then a single hint row. The session stamp is
		// folded away — it is bookkeeping, not something the operator needs on
		// every frame. It is reachable via /status.
		hint := truncate(strings.Join(hints, " · "), max(m.w-1, 12))
		if m.busy {
			lines = append(lines, hint)
		} else {
			lines = append(lines, m.theme.Paint(ui.PaperSoft, hint))
		}
	}
	// The footer is outside every panel, so nothing else can clip it: an
	// over-long stamp or hint would widen the whole frame, and fitScreen only
	// clamps height.
	for i, l := range lines {
		lines[i] = clipCells(l, m.w)
	}
	return strings.Join(lines, "\n")
}

// inputLines renders the input box: one row per buffer line, the caret drawn at
// inputCursor, and the whole thing clipped to the terminal width. The prompt is
// only on the first row so a multi-line draft reads as a block of text rather
// than a repeated prompt.
//
// Horizontal clipping is not cosmetic: the input row is the one place in the
// view that grows with what the operator types, and fitScreen only clamps line
// count. An untruncated long line soft-wraps in the terminal and pushes the
// transcript out of the measured height.
func (m *Model) inputLines(prompt string, caretTone ui.Token) []string {
	buf := m.inputRunes()
	at := clampInt(m.inputCursor, 0, len(buf))
	rows := strings.Split(string(buf), "\n")
	// Which row the caret is on: the number of newlines before it.
	caretRow := strings.Count(string(buf[:at]), "\n")
	caretCol := at - strings.LastIndex(string(buf[:at]), "\n") - 1

	// A tall buffer keeps the caret's row visible; without this the operator
	// types into a box that has scrolled away from them.
	rowsShown := m.inputRows()
	offset := 0
	if caretRow >= rowsShown {
		offset = caretRow - rowsShown + 1
	}

	budget := max(m.w-1, 12)
	out := make([]string, 0, rowsShown)
	for i := offset; i < len(rows) && len(out) < rowsShown; i++ {
		prefix := ""
		if i == 0 {
			prefix = prompt
		}
		room := budget - lipgloss.Width(prefix)
		text := rows[i]
		// Scroll the line so the caret stays visible when one line is longer
		// than the box.
		if i == caretRow {
			col := lipgloss.Width(text[:min(caretCol, len([]rune(text)))])
			if col >= room {
				text = sliceFromCell(text, col-room+1)
				col = room - 1
			}
			head, tail := splitWidth(text, clampInt(col, 0, len(text)))
			line := prefix + m.theme.Paint(ui.Paper, head) + m.theme.Paint(caretTone, "▌") + m.theme.Paint(ui.Paper, tail)
			out = append(out, clipCells(line, budget))
			continue
		}
		out = append(out, clipCells(prefix+m.theme.Paint(ui.Paper, text), budget))
	}
	for len(out) < rowsShown {
		out = append(out, "")
	}
	return out
}

// clipCells shortens a styled line to at most width display cells. truncate
// deliberately puts its ellipsis outside the budget, which is the wrong contract
// here: the input row must never exceed the terminal width.
func clipCells(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	head, _ := splitWidth(s, width)
	return head
}

// sliceFromCell drops the first n display cells of s, used to scroll a long
// input line so the caret stays in view.
func sliceFromCell(s string, n int) string {
	if n <= 0 {
		return s
	}
	_, tail := splitWidth(s, n)
	return tail
}

func destructive(detail string) bool {
	d := strings.ToLower(strings.TrimSpace(detail))
	for _, prefix := range []string{"rm ", "rm -", "dd ", "mkfs", "shred ", "git reset --hard", "git clean -"} {
		if strings.HasPrefix(d, prefix) || strings.Contains(d, " "+prefix) {
			return true
		}
	}
	return false
}

func wrap(s string, width int) []string {
	if width <= 1 {
		return []string{s}
	}
	var out []string
	for _, raw := range strings.Split(s, "\n") {
		words := strings.Fields(raw)
		if len(words) == 0 {
			out = append(out, "")
			continue
		}
		line := ""
		for _, w := range words {
			for lipgloss.Width(w) > width {
				// A single long token: hard-split by display cells.
				head, tail := splitWidth(w, width)
				if line != "" {
					out = append(out, line)
					line = ""
				}
				out = append(out, head)
				w = tail
			}
			if line == "" {
				line = w
			} else if lipgloss.Width(line)+1+lipgloss.Width(w) <= width {
				line += " " + w
			} else {
				out = append(out, line)
				line = w
			}
		}
		if line != "" || len(out) == 0 {
			out = append(out, line)
		}
	}
	return out
}

func splitWidth(s string, width int) (string, string) {
	r := []rune(s)
	cells := 0
	for i, ch := range r {
		cells += lipgloss.Width(string(ch))
		if cells > width {
			return string(r[:i]), string(r[i:])
		}
	}
	return s, ""
}

func fitWidth(s string, width int) string {
	w := lipgloss.Width(s)
	if w > width {
		return lipgloss.NewStyle().MaxWidth(width).Render(s)
	}
	if w < width {
		return s + strings.Repeat(" ", width-w)
	}
	return s
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
