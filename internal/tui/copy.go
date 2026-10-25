package tui

import (
	"fmt"
	"os"
	"strings"

	"kintsugi/internal/clipboard"
)

// copyLast sends the newest block of a kind to the terminal clipboard.
//
// The note says "sent", never "copied". OSC 52 is a request the terminal is
// free to ignore, and it has no acknowledgement in common terminals, so
// claiming success would be a lie the operator only discovers when they paste.
func (m *Model) copyLast(kind blockKind) {
	for i := len(m.blocks) - 1; i >= 0; i-- {
		if m.blocks[i].kind != kind {
			continue
		}
		text := strings.TrimSpace(m.blocks[i].text)
		if text == "" {
			m.addNote("· the last " + blockKindName(kind) + " is empty")
			return
		}
		m.sendToClipboard(text, blockKindName(kind))
		return
	}
	m.addNote("· nothing to copy yet · no " + blockKindName(kind) + " in this transcript")
}

// copyPlan sends the open plan to the clipboard — the whole document, which is
// what an operator pastes into a ticket or a report.
func (m *Model) copyPlan() {
	p := m.planReview()
	if p == nil {
		return
	}
	m.sendToClipboard(strings.TrimRight(p.art.Content, "\n"), "plan")
}

// sendToClipboard writes text via OSC 52 and reports honestly what happened.
//
// copyHook is the test seam: the clipboard needs a real TTY, so a test replaces
// the writer to inspect the payload. It is nil in every real run.
func (m *Model) sendToClipboard(text, what string) {
	if m.copyHook != nil {
		m.copyHook(text)
	}
	if !clipboard.Supported() {
		m.addNote("✗ no terminal clipboard here · OSC 52 needs a real TTY · select the text with shift+drag instead")
		return
	}
	if _, err := clipboard.Copy(os.Stdout, text); err != nil {
		m.addNote("✗ clipboard: " + err.Error() + " · select the text with shift+drag instead")
		return
	}
	lines := strings.Count(text, "\n") + 1
	m.addNote(fmt.Sprintf("· sent %d %s of %s to the terminal clipboard · paste to confirm",
		lines, plural(lines, "line"), what))
}

// blockKindName is the operator-facing name of a block kind, used in notes.
func blockKindName(k blockKind) string {
	switch k {
	case blockAssistant:
		return "reply"
	case blockTool:
		return "tool result"
	case blockUser:
		return "task"
	case blockNote:
		return "note"
	case blockCard:
		return "card"
	case blockBanner:
		return "nameplate"
	}
	return "block"
}
