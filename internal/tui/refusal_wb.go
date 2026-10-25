package tui

// The refusal workbench (`/refusal`): the lexicon editor inside the TUI.
//
// The CLI half (`kintsugi refusal list|add|test|disable|restore`) works on the
// same file; this is the same surface for the operator who never leaves the
// transcript. One screen answers three questions: what will fire (the class
// and pattern list, with hit counts), what would fire on this text (the test
// composer), and what did I teach it (operator patterns, marked).
//
// Teaching the lexicon is a session-live operation: after an add or an edit,
// the running loop's interceptor is rebuilt from the file, so a pattern added
// mid-session catches the very next refusal — no restart.

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbletea"

	"kintsugi/internal/intercept"
	"kintsugi/internal/session"
	"kintsugi/internal/ui"
)

// refusalMode is where the workbench is: the list, or one of the two
// composers.
type refusalMode int

const (
	refusalList refusalMode = iota
	refusalTest
	refusalAdd
)

// refusalView is the whole modal.
type refusalView struct {
	mode    refusalMode
	scroll  int
	draft   string // the composer field (test sample or "class | regex")
	note    string
	result  string // the test composer's rendered verdict
	entries []refusalEntry
	path    string // the override file the editor opens
	w, h    int
}

// refusalEntry is one pattern row.
type refusalEntry struct {
	Class  string
	Src    string
	Origin string // default | operator
}

// openRefusalWorkbench snapshots the effective lexicon.
func (m *Model) openRefusalWorkbench() {
	v := &refusalView{mode: refusalList}
	if m.app != nil {
		v.path = m.app.EffectiveLexiconPath()
		if lx, err := m.app.RefusalLexicon(); err == nil {
			for _, p := range lx.Patterns {
				v.entries = append(v.entries, refusalEntry{Class: p.Class, Src: p.Src, Origin: p.Origin})
			}
		} else {
			v.note = "✗ " + err.Error()
		}
	}
	m.refusal = v
}

// refusalRows is how many list rows the panel can show.
func refusalRows(screenH int) int {
	return max(screenH-1-2-3-1, 1) // header + panel frame + column header
}

func (m *Model) refusalRows() int { return refusalRows(max(m.h, 8)) }

// classHits reads the refusal telemetry and rolls it up per class — counters
// only, the same boundary the CLI list respects.
func (m *Model) classHits() map[string]int {
	out := map[string]int{}
	dir, err := session.Dir()
	if err != nil {
		return out
	}
	stats, err := session.RefusalStats(dir)
	if err != nil {
		return out
	}
	for _, s := range stats {
		if c := strings.TrimSpace(s.Class); c != "" {
			out[c] += s.Count
		}
	}
	return out
}

// ---------- input ----------

// handleRefusalKey owns the keyboard while the workbench is open.
func (m *Model) handleRefusalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	v := m.refusal
	if v == nil {
		return m, nil
	}
	if msg.Type == tea.KeyCtrlC {
		m.refusal = nil
		return m, tea.Quit
	}

	switch v.mode {
	case refusalList:
		switch msg.String() {
		case "esc", "q":
			m.refusal = nil
		case "up", "k":
			v.scroll = max(v.scroll-1, 0)
		case "down", "j":
			v.scroll++
		case "pgup":
			v.scroll = max(v.scroll-m.refusalRows(), 0)
		case "pgdown":
			v.scroll += m.refusalRows()
		case "e":
			// Edit the override file; create it with the header first so the
			// editor opens something usable, never a dead end.
			if v.path != "" {
				if _, err := os.Stat(v.path); os.IsNotExist(err) {
					_ = intercept.WriteOverride(v.path, map[string][]string{})
				}
				return m, m.editFile("refusal", v.path)
			}
		case "t":
			v.mode, v.draft, v.result, v.note = refusalTest, "", "", ""
		case "a":
			v.mode, v.draft, v.result, v.note = refusalAdd, "", "", ""
		}

	case refusalTest, refusalAdd:
		switch msg.Type {
		case tea.KeyEsc:
			v.mode, v.draft, v.result = refusalList, "", ""
		case tea.KeyEnter:
			return m.refusalSubmit(v)
		case tea.KeyBackspace:
			if r := []rune(v.draft); len(r) > 0 {
				v.draft = string(r[:len(r)-1])
			}
		case tea.KeyCtrlU:
			v.draft = ""
		case tea.KeyRunes:
			v.draft += string(msg.Runes)
		case tea.KeySpace:
			v.draft += " "
		}
	}
	return m, nil
}

// refusalSubmit is enter in a composer: run the test, or validate and write
// the add.
func (m *Model) refusalSubmit(v *refusalView) (tea.Model, tea.Cmd) {
	if v.mode == refusalTest {
		return m.refusalRunTest(v)
	}
	return m.refusalRunAdd(v)
}

// refusalRunTest scans the draft the way the live harness does and renders
// every hit into the result block.
func (m *Model) refusalRunTest(v *refusalView) (tea.Model, tea.Cmd) {
	if m.app == nil {
		return m, nil
	}
	lx, err := m.app.RefusalLexicon()
	if err != nil {
		v.note = "✗ " + err.Error()
		return m, nil
	}
	hits := lx.ScanAll(strings.TrimSpace(v.draft))
	if len(hits) == 0 {
		v.result = "no patterns matched\n· the lexicon would not have intercepted this — if it is a refusal, add a pattern below"
		return m, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d pattern(s) matched:\n", len(hits))
	for _, h := range hits {
		origin := h.Origin
		if origin == "" {
			origin = "default"
		}
		fmt.Fprintf(&b, "class %s · phrase %q\n  %s (%s)\n", h.Class, h.Phrase, h.Pattern, origin)
	}
	v.result = b.String()
	return m, nil
}

// refusalRunAdd validates a "class | regex" draft, appends it to the override
// file, and rewires the running loop so the pattern is live this session.
func (m *Model) refusalRunAdd(v *refusalView) (tea.Model, tea.Cmd) {
	if m.app == nil {
		return m, nil
	}
	parts := strings.SplitN(v.draft, "|", 2)
	if len(parts) != 2 {
		v.note = "✗ form: class | regex · type it like  moralizing | \\bI am not permitted to\\b"
		return m, nil
	}
	class, pattern := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	if class == "" || pattern == "" {
		v.note = "✗ form: class | regex · type both halves, separated by the bar"
		return m, nil
	}
	compileSrc := pattern
	if !strings.HasPrefix(compileSrc, "(?") {
		compileSrc = "(?i)" + compileSrc
	}
	if err := intercept.ValidatePattern(compileSrc); err != nil {
		v.note = "✗ pattern does not compile: " + oneLine(err.Error())
		return m, nil
	}
	path := m.app.EffectiveLexiconPath()
	overrides, err := intercept.OverrideClasses(path)
	if err != nil {
		v.note = "✗ " + err.Error()
		return m, nil
	}
	overrides[class] = append(overrides[class], pattern)
	if err := intercept.WriteOverride(path, overrides); err != nil {
		v.note = "✗ " + err.Error()
		return m, nil
	}
	v.draft = ""
	v.note = fmt.Sprintf("added to %s in %s — live on the next turn", class, path)
	// Rewire the running loop so the pattern catches the next refusal without
	// a restart.
	if m.loop != nil && m.app.Cfg.Intercept.Enabled {
		if lx, err := m.app.RefusalLexicon(); err == nil {
			m.loop.Interceptor = lx
		}
	}
	// Refresh the list so the new pattern is visible immediately.
	m.openRefusalWorkbench()
	m.refusal.mode, m.refusal.note = refusalAdd, v.note
	m.refusal.draft = ""
	return m, nil
}

// handleRefusalMouse implements wheel scrolling over the list.
func (m *Model) handleRefusalMouse(msg tea.MouseMsg) {
	v := m.refusal
	if v == nil || msg.Action != tea.MouseActionPress || v.mode != refusalList {
		return
	}
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		v.scroll = max(v.scroll-3, 0)
	case tea.MouseButtonWheelDown:
		v.scroll += 3
	}
}

// afterRefusalEdit reloads the lexicon after the override file was edited in
// $EDITOR: the loop is rewired and the list refreshed.
func (m *Model) afterRefusalEdit() {
	if m.loop != nil && m.app != nil && m.app.Cfg.Intercept.Enabled {
		if _, err := m.app.RefusalLexicon(); err == nil {
			// Reload only when the file parses; a broken edit keeps the
			// previous lexicon running, and the note says so.
			if lx, err := m.app.RefusalLexicon(); err == nil {
				m.loop.Interceptor = lx
				m.addNote("· refusal lexicon reloaded — live on the next turn")
			}
		} else {
			m.addNote("✗ refusal lexicon did not parse — the previous patterns stay live: " + oneLine(err.Error()))
		}
	}
	m.openRefusalWorkbench()
}

// ---------- rendering ----------

// renderRefusal draws the whole surface: the panel plus its footer rows.
func (m *Model) renderRefusal(width, height int, theme ui.Theme) string {
	if width < 24 {
		width = 24
	}
	if height < 8 {
		height = 8
	}
	v := m.refusal
	v.w, v.h = width, height

	footer := v.footerLines(width, theme)
	panelH := height - len(footer)
	if panelH < 5 {
		panelH = 5
	}
	bodyH := panelH - 3
	title, body := m.refusalPanelBody(v, max(width-2, 8), bodyH, theme)

	lines := strings.Split(theme.Panel(title, body, width, panelH), "\n")
	lines = append(lines, footer...)
	if len(lines) > height {
		lines = lines[:height]
	}
	return strings.Join(lines, "\n")
}

// footerLines is the key legend for the mode.
func (v *refusalView) footerLines(width int, theme ui.Theme) []string {
	keys, hint := "[esc] close", "refusals are telemetry, never memory"
	switch v.mode {
	case refusalList:
		keys = "[t] test · [a] add · [e] edit · [↑/↓] scroll · [esc] close"
		hint = "classes in the file replace the built-in class; an empty list disables it"
	case refusalTest:
		keys = "[enter] scan · [esc] back"
		hint = "quoted spans and fenced code are scrubbed first, the way the live scanner works"
	case refusalAdd:
		keys = "[enter] add & go live · [esc] back"
		hint = "form: class | regex — the pattern is validated before it is written"
	}
	if v.note != "" {
		hint = v.note
	}
	return []string{
		theme.Paint(ui.PaperSoft, clipShorten(keys, width)),
		theme.Paint(ui.PaperSoft, clipShorten(hint, width)),
	}
}

// refusalPanelBody dispatches to the mode renderer.
func (m *Model) refusalPanelBody(v *refusalView, width, height int, theme ui.Theme) (string, string) {
	switch v.mode {
	case refusalTest:
		return v.testBody(width, height, theme)
	case refusalAdd:
		return v.addBody(width, height, theme)
	default:
		return m.refusalListBody(v, width, height, theme)
	}
}

// listBody is the lexicon: classes with hit counts, patterns with their
// origin, then the persona's rephrasings — the two halves of refusal
// handling on one screen.
func (m *Model) refusalListBody(v *refusalView, width, height int, theme ui.Theme) (string, string) {
	hits := m.classHits()
	var lines []string
	byClass := map[string][]refusalEntry{}
	order := []string{}
	for _, e := range v.entries {
		if _, ok := byClass[e.Class]; !ok {
			order = append(order, e.Class)
		}
		byClass[e.Class] = append(byClass[e.Class], e)
	}
	for _, class := range order {
		pats := byClass[class]
		origin := "default"
		for _, p := range pats {
			if p.Origin == "operator" {
				origin = "operator"
			}
		}
		lines = append(lines, theme.Paint(ui.Gold, clipShorten(
			fmt.Sprintf("%s  %d pattern(s) · %s · %d hit(s)", class, len(pats), origin, hits[class]), width)))
		for _, p := range pats {
			mark := "  "
			if p.Origin == "operator" {
				mark = "+ "
			}
			lines = append(lines, theme.Paint(ui.PaperDim, clipShorten(mark+p.Src, width)))
		}
	}
	if len(v.entries) == 0 {
		lines = append(lines, theme.Paint(ui.PaperSoft, "· no lexicon loaded"))
	}
	// The rephrasings: the persona trigger map that turns a refusal's term
	// into an honest replacement. One screen, both halves.
	if tr := m.rephrasings(); len(tr) > 0 {
		lines = append(lines, "", theme.Paint(ui.PaperSoft, clipShorten(
			fmt.Sprintf("rephrasings (%d, persona trigger map)", len(tr)), width)))
		for _, r := range tr {
			lines = append(lines, theme.Paint(ui.PaperDim, clipShorten(
				fmt.Sprintf("  %s → %s", r.term, r.instead), width)))
		}
	}
	// Scroll clamp and window.
	v.scroll = clampInt(v.scroll, 0, max(len(lines)-height, 0))
	end := minInt(v.scroll+height, len(lines))
	visible := lines[v.scroll:end]
	for len(visible) < height {
		visible = append(visible, "")
	}
	return "// refusal · lexicon", strings.Join(visible, "\n")
}

// testBody is the sample composer and its verdict.
func (v *refusalView) testBody(width, height int, theme ui.Theme) (string, string) {
	var lines []string
	field := max(width-8, 4)
	lines = append(lines, theme.Paint(ui.PaperSoft, "sample  ")+theme.Paint(ui.Paper, clipShorten(v.draft, field))+theme.Paint(ui.Gold, "▌"))
	if v.result != "" {
		lines = append(lines, "")
		for _, l := range strings.Split(v.result, "\n") {
			lines = append(lines, theme.Paint(ui.Paper, clipShorten(l, width)))
		}
	}
	return "// refusal · test", padLines(lines, height)
}

// addBody is the class|regex composer.
func (v *refusalView) addBody(width, height int, theme ui.Theme) (string, string) {
	var lines []string
	field := max(width-8, 4)
	lines = append(lines, theme.Paint(ui.PaperSoft, "add    ")+theme.Paint(ui.Paper, clipShorten(v.draft, field))+theme.Paint(ui.Gold, "▌"))
	lines = append(lines, "",
		theme.Paint(ui.PaperSoft, clipShorten("the class replaces the built-in one the moment it is named", width)),
		theme.Paint(ui.PaperSoft, clipShorten("e.g.  moralizing | \\bI am not permitted to\\b", width)))
	return "// refusal · add", padLines(lines, height)
}

// rephraseRow is one term→instead pair from the current persona's trigger
// map, for the workbench's last block.
type rephraseRow struct {
	term    string
	instead string
}

// rephrasings reads the trigger map of the persona the session compiled:
// detected refusals are rephrased by it, so the lexicon screen shows both
// halves of the machinery.
func (m *Model) rephrasings() []rephraseRow {
	if m.comp == nil || m.comp.Persona == nil || m.comp.Persona.Persona == nil {
		return nil
	}
	known := m.comp.Persona.Persona.Triggers.Known
	out := make([]rephraseRow, 0, len(known))
	for _, t := range known {
		out = append(out, rephraseRow{term: t.Term, instead: t.Instead})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].term < out[j].term })
	return out
}
