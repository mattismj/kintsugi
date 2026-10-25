package tui

import (
	"kintsugi/internal/ui"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// pasteMsg is a bracketed paste: bubbletea delivers the whole paste as one
// KeyMsg with Paste set, and String() wraps it in brackets on purpose.
func pasteMsg(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s), Paste: true}
}

func TestPasteInsertsRawText(t *testing.T) {
	m := testModel(100, 30)
	pasted := "10.0.0.0/8 is in scope"
	m.Update(pasteMsg(pasted))

	if m.input != pasted {
		t.Fatalf("paste produced %q, want %q", m.input, pasted)
	}
	if strings.ContainsAny(m.input, "[]") {
		t.Fatalf("paste kept bubbletea's shortcut-guard brackets: %q", m.input)
	}
	if m.inputCursor != len([]rune(pasted)) {
		t.Fatalf("caret at %d, want %d", m.inputCursor, len([]rune(pasted)))
	}
}

func TestPasteWithNewlinesDoesNotSubmit(t *testing.T) {
	m := testModel(100, 30)
	m.Update(pasteMsg("first line\nsecond line\nthird line"))

	if len(m.blocks) != 1 { // the banner only
		t.Fatalf("a multi-line paste submitted the turn: %d blocks", len(m.blocks))
	}
	if got := strings.Count(m.input, "\n"); got != 2 {
		t.Fatalf("newlines kept = %d, want 2 (input %q)", got, m.input)
	}
	if m.inputRows() != 3 {
		t.Fatalf("inputRows = %d, want 3", m.inputRows())
	}
}

func TestCtrlJAndAltEnterInsertNewlineEnterSubmits(t *testing.T) {
	srv := recordingProvider(t, new(string))
	t.Cleanup(srv.Close)
	m, _ := commandModel(t, mentionWorkspace(t, srv.URL+"/v1"))

	m.Update(keyRunes("line one"))
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlJ})
	m.Update(keyRunes("line two"))
	m.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	m.Update(keyRunes("line three"))

	want := "line one\nline two\nline three"
	if m.input != want {
		t.Fatalf("input = %q, want %q", m.input, want)
	}
	if len(m.blocks) != 1 {
		t.Fatal("a newline key submitted the turn")
	}

	m.input = "a task"
	m.Update(keyEnter())
	if m.input != "" {
		t.Fatalf("enter left %q in the buffer", m.input)
	}
	if len(m.blocks) == 1 {
		t.Fatal("enter did not submit")
	}
}

func TestCaretMovementAndInsertion(t *testing.T) {
	m := testModel(100, 30)
	m.Update(keyRunes("abcd"))
	m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m.Update(keyRunes("X"))
	if m.input != "abXcd" {
		t.Fatalf("insert at caret gave %q, want %q", m.input, "abXcd")
	}
	if m.inputCursor != 3 {
		t.Fatalf("caret at %d, want 3", m.inputCursor)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlA})
	m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if m.input != "abXcd" {
		t.Fatalf("backspace at the start of the buffer changed it: %q", m.input)
	}
}

func TestBackspaceAtTheStartOfALineJoinsIt(t *testing.T) {
	m := testModel(100, 30)
	m.Update(pasteMsg("alpha\nbeta"))
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlA})
	m.Update(tea.KeyMsg{Type: tea.KeyBackspace})

	if m.input != "alphabeta" {
		t.Fatalf("backspace did not join the lines: %q", m.input)
	}
	if m.inputRows() != 1 {
		t.Fatalf("inputRows = %d after the join, want 1", m.inputRows())
	}
}

func TestCtrlAAndCtrlEMoveToLineBounds(t *testing.T) {
	m := testModel(100, 30)
	m.Update(pasteMsg("alpha\nbeta"))
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlA})
	if m.inputCursor != 6 {
		t.Fatalf("ctrl+a left the caret at %d, want the start of the last line (6)", m.inputCursor)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlE})
	if m.inputCursor != 10 {
		t.Fatalf("ctrl+e left the caret at %d, want 10", m.inputCursor)
	}
}

func TestArrowsMoveInsideAMultiLineBufferThenReachHistory(t *testing.T) {
	m := testModel(100, 30)
	m.history = []string{"earlier task"}
	m.histPos = 1
	m.Update(pasteMsg("top\nbottom"))

	// The caret starts on the last row, so up must move within the buffer.
	m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if m.caretRow() != 0 {
		t.Fatalf("up did not move within the buffer: row %d", m.caretRow())
	}
	if m.input != "top\nbottom" {
		t.Fatalf("up replaced the draft with history: %q", m.input)
	}
	// At the top row, up is history.
	m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if m.input != "earlier task" {
		t.Fatalf("up at the top row did not walk history: %q", m.input)
	}
	if m.scroll != 0 {
		t.Fatalf("arrows scrolled the transcript to %d", m.scroll)
	}
}

func TestFooterHeightFollowsTheBufferAndKeepsTheScrollMathHonest(t *testing.T) {
	m := testModel(100, 30)
	base := m.transcriptHeight()
	m.Update(pasteMsg("a\nb\nc"))

	if got := m.footerHeight(); got != 5 {
		t.Fatalf("footerHeight = %d, want 5 (3 rows + 2)", got)
	}
	if m.transcriptHeight() >= base {
		t.Fatalf("transcript did not shrink for a taller input: %d -> %d", base, m.transcriptHeight())
	}
	if m.scroll > m.maxScroll() {
		t.Fatalf("scroll %d past maxScroll %d", m.scroll, m.maxScroll())
	}
	assertFits(t, m)
}

func TestInputBoxCapsItsRowsAndScrollsInsideThem(t *testing.T) {
	m := testModel(100, 40)
	m.Update(pasteMsg("1\n2\n3\n4\n5\n6\n7\n8\n9"))

	if m.inputRows() != inputRowsMax {
		t.Fatalf("inputRows = %d, want the cap %d", m.inputRows(), inputRowsMax)
	}
	if got := len(m.inputLines("> ", ui.Gold)); got != inputRowsMax {
		t.Fatalf("inputLines drew %d rows, want %d", got, inputRowsMax)
	}
	assertFits(t, m)
}

func TestLongInputLineDoesNotOverflow(t *testing.T) {
	m := testModel(60, 24)
	m.Update(pasteMsg(strings.Repeat("exfiltrate ", 40)))

	rows := m.inputLines("operator@kintsugi:~$ ", ui.Gold)
	if len(rows) != 1 {
		t.Fatalf("one logical line drew %d rows", len(rows))
	}
	if w := len([]rune(rows[0])); w > m.w {
		t.Fatalf("input row is %d chars wide, terminal is %d: %q", w, m.w, rows[0])
	}
	assertFits(t, m)
}

func TestCaretStaysVisibleInALongLine(t *testing.T) {
	m := testModel(40, 24)
	m.Update(pasteMsg(strings.Repeat("x", 200)))

	rows := m.inputLines("", ui.Gold)
	if len(rows) != 1 {
		t.Fatalf("drew %d rows", len(rows))
	}
	for _, r := range rows {
		if w := len([]rune(r)); w > m.w {
			t.Fatalf("row overflows: %d > %d", w, m.w)
		}
	}
}

func TestCtrlUClearsTheBufferAndTheCaret(t *testing.T) {
	m := testModel(100, 30)
	m.Update(pasteMsg("some text\nmore text"))
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	if m.input != "" || m.inputCursor != 0 {
		t.Fatalf("ctrl+u left input=%q cursor=%d", m.input, m.inputCursor)
	}
	if m.inputRows() != 1 {
		t.Fatalf("inputRows = %d after clear, want 1", m.inputRows())
	}
}
