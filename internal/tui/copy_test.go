package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"kintsugi/internal/plan"
)

func TestCopyLastPicksTheNewestBlockOfTheKind(t *testing.T) {
	m := testModel(100, 30)
	m.addBlock(block{kind: blockAssistant, text: "first reply"})
	m.addBlock(block{kind: blockTool, text: "tool output"})
	m.addBlock(block{kind: blockAssistant, text: "second reply"})

	m.copyLast(blockAssistant)
	note := m.blocks[len(m.blocks)-1]
	if note.kind != blockNote {
		t.Fatalf("copy did not report back: %+v", note)
	}
	// Under `go test` stdout is a pipe, so the honest answer is "no terminal
	// clipboard here" — never a claim that it copied.
	if !strings.Contains(note.text, "clipboard") {
		t.Fatalf("the note says nothing about the clipboard: %q", note.text)
	}
	if strings.Contains(note.text, "✓") {
		t.Fatalf("a pipe claimed success: %q", note.text)
	}
}

func TestCopyLastWithNothingToCopySaysSo(t *testing.T) {
	m := testModel(100, 30)
	m.copyLast(blockAssistant)
	note := m.blocks[len(m.blocks)-1].text
	if !strings.Contains(note, "nothing to copy") {
		t.Fatalf("an empty transcript gave %q", note)
	}
}

func TestCopyLastOfAnEmptyBlockSaysSo(t *testing.T) {
	m := testModel(100, 30)
	m.addBlock(block{kind: blockAssistant, text: "   "})
	m.copyLast(blockAssistant)
	if note := m.blocks[len(m.blocks)-1].text; !strings.Contains(note, "empty") {
		t.Fatalf("an empty block gave %q", note)
	}
}

func TestCtrlYIsBoundAndDoesNotType(t *testing.T) {
	m := testModel(100, 30)
	m.addBlock(block{kind: blockAssistant, text: "a finding"})
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlY})
	if m.input != "" {
		t.Fatalf("ctrl+y typed into the draft: %q", m.input)
	}
	if len(m.blocks) == 0 || m.blocks[len(m.blocks)-1].kind != blockNote {
		t.Fatal("ctrl+y produced no feedback")
	}
}

func TestCopyPlanFromTheReviewSurface(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m, art := reviewModel(t, 120, 34)

	var copied string
	m.copyHook = func(text string) { copied = text }
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})

	if copied != strings.TrimRight(art.Content, "\n") {
		t.Fatalf("y copied %d bytes, want the plan document (%d)", len(copied), len(art.Content))
	}
	last := m.blocks[len(m.blocks)-1]
	if last.kind != blockNote {
		t.Fatalf("y gave no feedback: %+v", last)
	}
	// The workbench stays open: copying is not a decision.
	if m.wb == nil || m.wb.kind != "plan" {
		t.Fatal("copying closed the review surface")
	}
}

func TestReviewFooterAdvertisesCopy(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m, _ := reviewModel(t, 140, 40)
	if footer := plain(m.workbenchFooter()); !strings.Contains(footer, "copy") {
		t.Fatalf("the review footer does not advertise y: %q", footer)
	}
}

func TestPlanReviewCopyUsesTheDocumentNotTheRenderedLines(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m, art := reviewModel(t, 120, 34)
	// Stand in for the writer so the payload can be inspected without a TTY.
	var got string
	m.copyHook = func(text string) { got = text }
	m.copyPlan()

	if !strings.Contains(got, plan.Marker) {
		t.Fatalf("the copied text is not the plan document: %q", got)
	}
	if strings.HasSuffix(got, "\n") {
		t.Fatal("the copied document carries a trailing newline")
	}
	if len(got) != len(strings.TrimRight(art.Content, "\n")) {
		t.Fatalf("copied %d bytes, document is %d", len(got), len(strings.TrimRight(art.Content, "\n")))
	}
}
