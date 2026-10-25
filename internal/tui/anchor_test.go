package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// anchoredModel is a scrolled model with a long transcript, so an anchor has
// something to hold on to.
func anchoredModel(w, h int) *Model {
	m := scrollableModel(w, h)
	m.scrollBy(20)
	if !m.anchor.valid {
		panic("scrollBy left no anchor")
	}
	return m
}

// streamLines appends n lines to the streaming assistant block, the way a
// token stream does.
func streamLines(m *Model, n int) {
	for i := 0; i < n; i++ {
		m.Update(streamDelta{s: fmt.Sprintf("streamed line %02d\n", i)})
	}
}

func TestAnchorHoldsTheViewportWhileStreaming(t *testing.T) {
	m := anchoredModel(100, 30)
	before := m.transcriptBody(m.transcriptWidth(), m.transcriptHeight())
	line := m.anchor.line

	streamLines(m, 40)

	after := m.transcriptBody(m.transcriptWidth(), m.transcriptHeight())
	if m.anchor.line != line {
		t.Fatalf("anchor moved from line %d to %d while streaming", line, m.anchor.line)
	}
	if after != before {
		t.Fatalf("the pinned viewport moved while 40 lines streamed in:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if m.newLinesBelow() == 0 {
		t.Error("no new output detected below the anchor")
	}
}

func TestAnchorClearedAtTheLiveEdge(t *testing.T) {
	m := scrollableModel(100, 30)
	if m.anchor.valid {
		t.Fatal("a fresh transcript should not be pinned")
	}
	m.scrollBy(10)
	if !m.anchor.valid {
		t.Fatal("scrolling up should pin")
	}
	m.scrollBy(-10)
	if m.anchor.valid {
		t.Fatal("returning to the live edge should clear the pin")
	}
}

func TestAnchorDroppedOnResize(t *testing.T) {
	m := anchoredModel(100, 30)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 34})
	if m.anchor.valid {
		t.Fatal("a width change should drop the anchor: every line has been renumbered")
	}
	if m.scroll > m.maxScroll() {
		t.Fatalf("scroll %d left past maxScroll %d", m.scroll, m.maxScroll())
	}
}

func TestEscClearsTheAnchor(t *testing.T) {
	m := anchoredModel(100, 30)
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.anchor.valid || m.scroll != 0 {
		t.Fatalf("esc left scroll=%d anchor=%v", m.scroll, m.anchor.valid)
	}
}

func TestAnchorNeverPointsInsideTheStreamingBlock(t *testing.T) {
	m := testModel(100, 30)
	addLines(m, 40)
	m.addBlock(block{kind: blockAssistant})
	m.stream = len(m.blocks) - 1
	m.busy = true
	// Scroll to the very top, then stream: the anchor must sit at or above the
	// streaming block's first line, never inside it.
	m.scrollBy(1000)

	streamLines(m, 30)

	width := m.transcriptWidth()
	lines := m.transcriptLines(width)
	start := m.firstLineOfLastBlock(width, len(lines))
	if m.anchor.line > start {
		t.Fatalf("anchor at line %d is inside the streaming block, which starts at %d", m.anchor.line, start)
	}
}

func TestFooterReportsNewOutputBelowThePin(t *testing.T) {
	m := anchoredModel(120, 34)
	streamLines(m, 20)
	out := plain(m.View())
	if !strings.Contains(out, "pinned") {
		t.Fatalf("footer did not report the pin:\n%s", out)
	}
	if !strings.Contains(out, "new") {
		t.Fatalf("footer did not report new output:\n%s", out)
	}
	assertFits(t, m)
}
