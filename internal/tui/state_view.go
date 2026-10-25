package tui

// Identity placement: the state machine and the indicator glyphs
// block pinned to the bottom of the inspector, and the busy verb. Everything
// here is derived from model state — no timers, no goroutines, no emoji, and no
// rounded corners. The art itself lives in internal/ui (mascot.go); this file
// only decides which state the session is in and where the mark is drawn.

import (
	"strings"
	"time"

	"kintsugi/internal/ui"
)

// lastNote is the most recent note block's text, or "" when the transcript has
// no notes. Notes already carry the outcome of a turn (✗/✸ on failure, "· turn
// complete" on success), so the mascot reads them instead of keeping a parallel
// flag that could drift out of sync.
func (m *Model) lastNote() string {
	for i := len(m.blocks) - 1; i >= 0; i-- {
		if m.blocks[i].kind == blockNote {
			return m.blocks[i].text
		}
	}
	return ""
}

// auditCardActive reports whether the newest card is a gate/opsec review. It is
// a heuristic for the auditing frame only; a card that is not a gate event
// leaves the mascot to the other states.
func (m *Model) auditCardActive() bool {
	for i := len(m.blocks) - 1; i >= 0; i-- {
		if m.blocks[i].kind != blockCard {
			continue
		}
		title := strings.ToLower(m.blocks[i].title)
		return strings.Contains(title, "gate") || strings.Contains(title, "opsec")
	}
	return false
}

// mascotState maps the session onto one identity state. Danger (✗/✸) wins over
// a still-busy turn because the last word in the transcript is a failure; then
// work, then an active gate review, then a completed turn, else idle.
func (m *Model) mascotState() ui.MascotState {
	last := m.lastNote()
	switch {
	case strings.HasPrefix(last, "✗") || strings.HasPrefix(last, "✸"):
		return ui.MascotError
	case m.busy:
		return ui.MascotWorking
	case m.auditCardActive():
		return ui.MascotAuditing
	case strings.HasPrefix(last, "· turn complete"):
		return ui.MascotDone
	}
	return ui.MascotIdle
}

// busyLabel is the mono word "working" or the phase-locked verb in color. Mono
// never sees the verb cycle: the glyphs and the plain word carry the meaning
// (§7.4.10) and a changing word would be noise without color to soften it.
func (m *Model) busyLabel() string {
	if m.theme.Profile == ui.Mono {
		return "working"
	}
	return ui.Verb(time.Since(m.start))
}

// mascotLines renders the inspector's identity block: the state mark, the
// state and its activity verb, one line. Glyph vocabulary only — the state
// mark is the whole identity now.
func (m *Model) mascotLines(width int) []string {
	return []string{m.identityRail()}
}

// stateLabel is the activity word for a state: the kintsugi repair verbs while
// a turn runs, the state's rest word otherwise.
func stateLabel(state ui.MascotState) string {
	switch state {
	case ui.MascotThinking:
		return "thinking"
	case ui.MascotWorking:
		return "mending"
	case ui.MascotAuditing:
		return "auditing"
	case ui.MascotDone:
		return "done"
	case ui.MascotError:
		return "cracked"
	default:
		return "resting"
	}
}

// identityRail is the one-line identity summary that opens the inspector:
// the state mark, the state name and its activity label — "◇ idle · resting".
// While a turn runs the label is the phase-locked verb, so the rail moves
// with the work; mono keeps the static word (§7.4.10). A label that would
// only repeat the state name ("done · done") is dropped instead.
func (m *Model) identityRail() string {
	state := m.mascotState()
	label := stateLabel(state)
	if state == ui.MascotWorking && m.theme.Profile != ui.Mono {
		label = ui.Verb(time.Since(m.start))
	}
	rail := m.theme.RenderIndicator(state) + " " + m.theme.Paint(ui.Paper, string(state))
	if label != "" && label != string(state) {
		rail += m.theme.Paint(ui.PaperSoft, " · "+label)
	}
	return rail
}

// cardTitle builds a card eyebrow with the identity glyph in front:
// "<indicator> // title". The glyph carries the state tone; the eyebrow text
// is painted in the card's own tone, so the frame never loses its color.
func (m *Model) cardTitle(state ui.MascotState, tone ui.Token, title string) string {
	return m.theme.RenderIndicator(state) + " " + m.theme.Paint(tone, title)
}

// harnessState maps a harness card title to the identity state its glyph
// shows. One table, so the same event always draws the same mark (§7.4.3):
// approvals and reviews are audits, injected material and prior work are the
// session working, a drift or a blocker is an error, and a gate that passed
// is done.
func harnessState(title string) (ui.MascotState, bool) {
	t := strings.ToLower(title)
	switch {
	case strings.Contains(t, "approval"):
		return ui.MascotAuditing, true
	case strings.Contains(t, "preflight"):
		return ui.MascotThinking, true
	case strings.Contains(t, "drift"):
		return ui.MascotError, true
	case strings.Contains(t, "continuity"):
		return ui.MascotWorking, true
	case strings.Contains(t, "blocker"):
		return ui.MascotError, true
	case strings.Contains(t, "advisor"):
		return ui.MascotAuditing, true
	case strings.Contains(t, "refusal"):
		return ui.MascotError, true
	case strings.Contains(t, "frame"):
		return ui.MascotWorking, true
	case strings.Contains(t, "gate") || strings.Contains(t, "opsec"):
		if strings.Contains(t, "passed") || strings.Contains(t, "ok") {
			return ui.MascotDone, true
		}
		return ui.MascotAuditing, true
	}
	return ui.MascotIdle, false
}

// cardState is the state a card's glyph shows: a harness card carries the
// state of the event it reports, and every other card carries the state the
// session is in right now.
func (m *Model) cardState(title string) ui.MascotState {
	if st, ok := harnessState(title); ok {
		return st
	}
	return m.mascotState()
}

// isRepairCard reports whether a card title names a repaired event: the
// harness's gold seams. A refusal intercepted and resumed, prior accepted
// work re-injected, a frame applied, a drift recorded as metadata — each is
// a break repaired, and the card for it carries the seam mark.
func isRepairCard(title string) bool {
	t := strings.ToLower(title)
	for _, mark := range []string{"refusal", "drift", "frame", "continuity", "prior"} {
		if strings.Contains(t, mark) {
			return true
		}
	}
	return false
}
