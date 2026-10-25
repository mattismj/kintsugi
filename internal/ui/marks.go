package ui

import (
	"strings"
	"time"
)

// The state system: glyph vocabulary only. There is no creature, no sprite,
// no animation — every state is one deterministic character in a tone, so the
// identity survives every terminal, including mono.
//
// The states themselves are the harness's phases of work. Kintsugi names the
// two that matter: idle is "resting" (an unbroken vessel), error is "cracked"
// (the break), and the working states are the repair.

// MascotState is the harness's live state. The name is historical; the marks
// are what it is now — one glyph per state, deterministic, no art.
type MascotState string

const (
	MascotIdle     MascotState = "idle"
	MascotThinking MascotState = "thinking"
	MascotWorking  MascotState = "working"
	MascotAuditing MascotState = "auditing"
	MascotDone     MascotState = "done"
	MascotError    MascotState = "error"
)

// Indicator is the state sigil: one glyph, deterministic, sized for task rows,
// agent lists and notifications. · is never used here — idle is a hollow
// diamond so an idle agent still reads as present. The glyphs are the whole
// identity now; nothing animated sits behind them.
func Indicator(state MascotState) string {
	switch normalizeMascotState(state) {
	case MascotThinking:
		return "✦"
	case MascotWorking:
		return "▸"
	case MascotAuditing:
		return "◉"
	case MascotDone:
		return "✓"
	case MascotError:
		return "✸"
	default: // idle, and anything unknown normalized into it
		return "◇"
	}
}

// RenderIndicator paints the state sigil in the state's tone.
func (t Theme) RenderIndicator(state MascotState) string {
	return t.Paint(stateTone(state), Indicator(state))
}

// RoleGlyph is the per-agent-role mark. Unknown roles get the neutral
// middot, never a guess at a family glyph.
func RoleGlyph(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "coder":
		return "▣"
	case "researcher":
		return "◇"
	case "opsec":
		return "▲"
	case "orchestrator":
		return "◈"
	default:
		return "·"
	}
}

// verbPhases are the busy verb cycle, named for the stages of a kintsugi
// repair: the crack is filled with urushi lacquer (mending), the lacquer
// cures (lacquering), gold dust is applied (gilding), and the seam is
// burnished (polishing). Eight seconds, one full repair, wrap.
var verbPhases = [...]string{"mending", "lacquering", "gilding", "polishing"}

// Verb returns the phase-locked activity verb for an elapsed duration: four
// 2s phases (mending → lacquering → gilding → polishing) that wrap every 8s.
func Verb(elapsed time.Duration) string {
	const phase = 2 * time.Second
	if elapsed < 0 {
		elapsed = 0
	}
	return verbPhases[int(elapsed/phase)%len(verbPhases)]
}

// stateTone is the single state→tone mapping for the mark. Danger is reserved
// for the error state — the break; gold is reserved for the repair's working
// states; success is functional, never decorative.
func stateTone(state MascotState) Token {
	switch normalizeMascotState(state) {
	case MascotThinking, MascotWorking:
		return Gold
	case MascotAuditing:
		return Warn
	case MascotDone:
		return OK
	case MascotError:
		return Danger
	default:
		return PaperSoft
	}
}

// StateTone is the exported tone for a state, for callers that paint the
// glyph themselves.
func StateTone(state MascotState) Token { return stateTone(state) }

// normalizeMascotState canonicalizes a state string and degrades unknown
// input to idle, so callers never render an empty mark.
func normalizeMascotState(state MascotState) MascotState {
	s := MascotState(strings.ToLower(strings.TrimSpace(string(state))))
	switch s {
	case MascotThinking, MascotWorking, MascotAuditing, MascotDone, MascotError, MascotIdle:
		return s
	}
	return MascotIdle
}
