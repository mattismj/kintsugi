package tui

// Tests for the live tool stream: the running tool's output renders while
// the tool runs, rolls at the cap, and is replaced by the final card.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbletea"

	"kintsugi/internal/provider"
	"kintsugi/internal/tools"
)

func streamEv(label, line string) toolStreamNotice {
	return toolStreamNotice{ev: tools.ToolOutputEvent{Tool: "bash", Label: label, Line: line}}
}

func TestToolStreamRendersTheLiveBlock(t *testing.T) {
	m := testModel(100, 30)
	m.ch = make(chan tea.Msg, 4)
	_, _ = m.Update(streamEv("nmap -sV lab", "PORT 22 STATE open"))
	_, _ = m.Update(streamEv("nmap -sV lab", "PORT 443 STATE open"))
	out := plain(m.View())
	for _, want := range []string{"bash", "nmap -sV lab", "PORT 22 STATE open", "PORT 443 STATE open"} {
		if !strings.Contains(out, want) {
			t.Errorf("the live block missing %q:\n%s", want, out)
		}
	}
	assertFits(t, m)
}

func TestToolStreamRollsAtTheCap(t *testing.T) {
	m := testModel(100, 40)
	m.ch = make(chan tea.Msg, 64)
	for i := 0; i < toolStreamMaxLines+5; i++ {
		_, _ = m.Update(streamEv("long-scan", fmt.Sprintf("line %d", i)))
	}
	out := plain(m.View())
	// The window holds the tail; the head rolled out and is counted.
	if strings.Contains(out, "line 0\n") || strings.Contains(out, "line 4 ") {
		t.Errorf("the window kept rolled-out lines:\n%s", out)
	}
	if !strings.Contains(out, fmt.Sprintf("line %d", toolStreamMaxLines+4)) {
		t.Errorf("the window lost the newest line:\n%s", out)
	}
	if !strings.Contains(out, "earlier line(s)") {
		t.Errorf("the rolled-out count is missing:\n%s", out)
	}
	assertFits(t, m)
}

func TestToolNoticeReplacesTheLiveBlock(t *testing.T) {
	m := testModel(100, 30)
	m.ch = make(chan tea.Msg, 4)
	_, _ = m.Update(streamEv("echo done", "intermediate"))
	_, _ = m.Update(toolNotice{
		call: providerCall("bash", `{"command":"echo done"}`),
		res:  tools.Result{Content: "done\n[exit 0]"},
	})
	if m.liveTool != nil {
		t.Fatal("the tool notice did not clear the live block")
	}
	out := plain(m.View())
	if !strings.Contains(out, "bash") {
		t.Errorf("the final tool card is missing:\n%s", out)
	}
	if strings.Contains(out, "intermediate") {
		t.Errorf("the live block outlived its replacement:\n%s", out)
	}
}

// TestToolStreamFingerprintInvalidates: a streamed line changes the rendered
// transcript, so the memo must not serve the pre-line window.
func TestToolStreamFingerprintInvalidates(t *testing.T) {
	m := testModel(100, 30)
	m.ch = make(chan tea.Msg, 4)
	before := plain(m.View())
	_, _ = m.Update(streamEv("scan", "a line that was not there"))
	if plain(m.View()) == before {
		t.Fatal("a streamed line did not change the rendered transcript")
	}
}

func providerCall(name, args string) provider.ToolCall {
	return provider.ToolCall{Type: "function", Function: provider.Function{Name: name, Arguments: args}}
}
