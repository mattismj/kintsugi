package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbletea"

	"kintsugi/internal/config"
	"kintsugi/internal/hub"
	"kintsugi/internal/notify"
	"kintsugi/internal/ui"
)

// rosterMinWidth is the minimum transcript width at which the roster section
// appears in the left panel. Below this the panel stays with the simpler
// tasks/agents view.
const rosterMinWidth = 100

// rosterMinHeight is the minimum body height at which the roster section
// appears.
const rosterMinHeight = 30

// notifyFor builds the session's notifier from config. An unset or unknown
// backend yields nil, which every call site treats as "raise nothing": the
// default posture is silence.
func notifyFor(cfg config.Notify) *notify.Notifier {
	backend := notify.Backend(strings.ToLower(strings.TrimSpace(cfg.Backend)))
	switch backend {
	case notify.BackendOSC9, notify.BackendOSC777, notify.BackendBell, notify.BackendCommand:
		return notify.New(notify.Config{Backend: backend, Command: cfg.Command})
	default:
		return nil
	}
}

// raise emits one terminal notification, if the operator configured a backend.
// The message is a single line of the operator's own session detail — a tool
// name, a session id, a percentage — and never the model's content. A failed
// notification is dropped: the work must not stop because a bell did not carry.
func (m *Model) raise(kind notify.Kind, message string) {
	if m.notify == nil {
		return
	}
	_ = m.notify.Notify(kind, message)
}

// rosterBody renders the background subagent roster section for the left panel.
// It returns the rendered lines (without trailing padding).
func (m *Model) rosterBody(width int) []string {
	if m.hub == nil {
		return nil
	}
	runs := m.hub.List()
	if len(runs) == 0 {
		return nil
	}

	var lines []string
	lines = append(lines, m.theme.Paint(ui.PaperSoft, "roster"))

	for _, run := range runs {
		glyph, tone := ui.StatusTone(rosterState(run.State))
		elapsed := formatElapsed(run.Elapsed())
		tokens := ""
		if run.Tokens > 0 {
			tokens = fmt.Sprintf(" %dtok", run.Tokens)
		}
		// Build the row: glyph + id + role + model + state + elapsed + tokens
		id := run.ID
		role := run.Role
		model := run.Model
		if model == "" {
			model = "-"
		}
		state := rosterState(run.State)

		// Compact form: id role state elapsed tokens
		// At wider widths: id role model state elapsed tokens
		if width >= 30 {
			line := fmt.Sprintf("%-8s %-8s %-10s %-7s %s%s",
				truncate(id, 8), truncate(role, 8), truncate(model, 10),
				state, elapsed, tokens)
			lines = append(lines, m.theme.Paint(tone, glyph)+" "+m.theme.Paint(ui.PaperDim, line))
		} else {
			line := fmt.Sprintf("%-8s %-7s %s%s",
				truncate(id, 8), state, elapsed, tokens)
			lines = append(lines, m.theme.Paint(tone, glyph)+" "+m.theme.Paint(ui.PaperDim, line))
		}
	}

	lines = append(lines, m.theme.Paint(ui.PaperSoft, "keys: o=open  k=kill"))
	return lines
}

// rosterFocusedKeys handles keys while the roster owns the keyboard (ctrl+r).
// It reports whether the key was consumed; anything else leaves focus and falls
// through to the input line, so the roster can never eat the operator's typing.
func (m *Model) rosterFocusedKeys(msg tea.KeyMsg) bool {
	runs := m.rosterIDs()
	if len(runs) == 0 {
		m.rosterFocus = false
		return false
	}
	switch msg.Type {
	case tea.KeyUp:
		m.rosterMove(-1)
	case tea.KeyDown:
		m.rosterMove(1)
	case tea.KeyRunes:
		switch string(msg.Runes) {
		case "o":
			m.openRosterRun()
		case "k":
			m.killRosterRun()
		default:
			return false
		}
	case tea.KeyEsc:
		m.rosterFocus = false
		return true
	default:
		return false
	}
	return true
}

// rosterMove moves the selection, clamped to the list.
func (m *Model) rosterMove(delta int) {
	n := len(m.rosterIDs())
	if n == 0 {
		return
	}
	m.rosterCursor += delta
	if m.rosterCursor < 0 {
		m.rosterCursor = 0
	}
	if m.rosterCursor > n-1 {
		m.rosterCursor = n - 1
	}
}

func (m *Model) openRosterRun() {
	id := m.rosterSelected()
	if id == "" {
		return
	}
	m.addNote("· run " + id + " — collect it with agent_output, or read it in the log")
}

func (m *Model) killRosterRun() {
	id := m.rosterSelected()
	if id == "" {
		return
	}
	if err := m.hub.Kill(id); err != nil {
		m.addNote("✗ kill " + id + ": " + err.Error() + " · o opens the run, then read its log")
		return
	}
	m.addNote("· killed run " + id)
}

// rosterState maps a hub run state to the state vocabulary ui.StatusTone
// understands. The strings pass through unchanged: the hub names its states
// after what they are, and the tone table is the single place that decides how
// a state looks.
func rosterState(s hub.RunState) string {
	return string(s)
}

// formatElapsed renders a duration in a compact form.
func formatElapsed(d time.Duration) string {
	if d < time.Second {
		return "<1s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

// rosterSelected returns the ID of the currently selected roster entry, or ""
// if none is selected.
func (m *Model) rosterSelected() string {
	runs := m.rosterIDs()
	if m.rosterCursor < 0 || m.rosterCursor >= len(runs) {
		return ""
	}
	return runs[m.rosterCursor]
}

// rosterIDs returns the IDs of all runs in the roster, in display order.
func (m *Model) rosterIDs() []string {
	if m.hub == nil {
		return nil
	}
	runs := m.hub.List()
	ids := make([]string, len(runs))
	for i, r := range runs {
		ids[i] = r.ID
	}
	return ids
}

// rosterVisible reports whether the roster section should be shown.
func (m *Model) rosterVisible() bool {
	return m.w >= rosterMinWidth && m.h >= rosterMinHeight && m.hub != nil && len(m.hub.List()) > 0
}
