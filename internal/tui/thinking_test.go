package tui

// Tests for the thinking channel: the model's reasoning renders in the
// transcript as a dim block above the answer, the answer closes it, and the
// thinking never leaks into the answer's block.

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbletea"

	"kintsugi/internal/config"
	"kintsugi/internal/intercept"
	"kintsugi/internal/session"
)

func TestThinkingDeltaRendersTheBlock(t *testing.T) {
	m := testModel(100, 30)
	m.ch = make(chan tea.Msg, 8)
	_, _ = m.Update(thinkingDelta{s: "The user wants a port scanner"})
	_, _ = m.Update(thinkingDelta{s: " for their lab. This seems like"})
	_, _ = m.Update(thinkingDelta{s: " legitimate dual-use work."})
	out := plain(m.View())
	for _, want := range []string{"◇ thinking ·", "port scanner", "dual-use"} {
		if !strings.Contains(out, want) {
			t.Errorf("the thinking block missing %q:\n%s", want, out)
		}
	}
	assertFits(t, m)
}

func TestAnswerClosesTheThinkingBlock(t *testing.T) {
	m := testModel(100, 30)
	m.ch = make(chan tea.Msg, 8)
	// The model thinks, then answers.
	_, _ = m.Update(thinkingDelta{s: "thinking about it"})
	_, _ = m.Update(streamDelta{s: "Here's the scanner."})
	// The thinking block is closed (thinkIdx reset) but stays in the
	// transcript.
	if m.thinkIdx != -1 {
		t.Fatalf("thinkIdx = %d after the answer started, want -1", m.thinkIdx)
	}
	out := plain(m.View())
	if !strings.Contains(out, "thinking about it") {
		t.Errorf("the thinking block vanished after the answer:\n%s", out)
	}
	if !strings.Contains(out, "Here's the scanner.") {
		t.Errorf("the answer is missing:\n%s", out)
	}
}

func TestThinkingFingerprintInvalidates(t *testing.T) {
	m := testModel(100, 30)
	m.ch = make(chan tea.Msg, 4)
	before := plain(m.View())
	_, _ = m.Update(thinkingDelta{s: "a reasoning line"})
	if plain(m.View()) == before {
		t.Fatal("a thinking delta did not change the rendered transcript")
	}
}

func TestThinkingIsNotAssistant(t *testing.T) {
	m := testModel(100, 30)
	m.ch = make(chan tea.Msg, 4)
	_, _ = m.Update(thinkingDelta{s: "reasoning"})
	// The thinking block is its own kind, not an assistant block.
	if len(m.blocks) == 0 || m.blocks[len(m.blocks)-1].kind != blockThinking {
		t.Fatalf("the thinking created a %v block, want blockThinking", m.blocks[len(m.blocks)-1].kind)
	}
}

func TestThinkingCollapsesAfterTheTurn(t *testing.T) {
	m := testModel(100, 40)
	m.ch = make(chan tea.Msg, 64)
	// A long reasoning stream (40 lines), then the answer starts.
	for i := 0; i < 40; i++ {
		_, _ = m.Update(thinkingDelta{s: "reasoning line " + itoa(i) + "\n"})
	}
	_, _ = m.Update(streamDelta{s: "answer"})
	out := plain(m.View())
	// Collapsed to the last few lines with the count and the expand hint.
	if !strings.Contains(out, "ctrl+t to expand") {
		t.Errorf("the collapsed thinking does not name the expand key:\n%s", out)
	}
	if !strings.Contains(out, "earlier thinking line(s)") {
		t.Errorf("the collapsed thinking lost its count:\n%s", out)
	}
	// The head is not shown while collapsed.
	if strings.Contains(out, "reasoning line 0\n") {
		t.Errorf("the collapsed block shows its head:\n%s", out)
	}
	// The tail is.
	if !strings.Contains(out, "reasoning line 39") {
		t.Errorf("the collapsed block lost its tail:\n%s", out)
	}
}

func TestCtrlTOpensTheNewestThinking(t *testing.T) {
	m := testModel(100, 40)
	m.ch = make(chan tea.Msg, 64)
	for i := 0; i < 10; i++ {
		_, _ = m.Update(thinkingDelta{s: "line " + itoa(i) + "\n"})
	}
	_, _ = m.Update(streamDelta{s: "answer"})
	before := plain(m.View())
	pressKeys(m, tea.KeyMsg{Type: tea.KeyCtrlT})
	after := plain(m.View())
	if after == before {
		t.Fatal("ctrl+t did not change the transcript")
	}
	// Expanded: the head is back.
	if !strings.Contains(after, "thinking · line 0") {
		t.Errorf("expanded thinking lost its head:\n%s", after)
	}
	// Toggle back.
	pressKeys(m, tea.KeyMsg{Type: tea.KeyCtrlT})
	if strings.Contains(plain(m.View()), "thinking · line 0") {
		t.Error("the second ctrl+t did not collapse")
	}
}

func TestAltTCopiesTheThinking(t *testing.T) {
	m := testModel(100, 40)
	m.ch = make(chan tea.Msg, 8)
	var copied string
	m.copyHook = func(s string) { copied = s }
	_, _ = m.Update(thinkingDelta{s: "the full reasoning text"})
	_, _ = m.Update(streamDelta{s: "answer"})
	pressKeys(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t"), Alt: true})
	if !strings.Contains(copied, "the full reasoning text") {
		t.Fatalf("alt+t copied %q", copied)
	}
}

// TestHardRefusalCardRecommendsFromTelemetry: a hard refusal's card states
// the stonewall and names a model the telemetry says handled the work.
func TestHardRefusalCardRecommendsFromTelemetry(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	m := testModel(120, 30)
	m.ch = make(chan tea.Msg, 4)

	// Seed reliability: a clean model with two turns; the current model is
	// excluded by the recommendation.
	s, err := session.New()
	if err != nil {
		t.Fatal(err)
	}
	s.Log("meta", map[string]string{"model": "qwen/qwen3-coder", "persona": "qwen-coder"})
	s.Log("user", "t1")
	s.Log("assistant", "a")
	s.Log("user", "t2")
	s.Log("assistant", "a")
	s.Close()
	m.res.ProviderName = "deepseek"
	m.res.Model = config.Model{ID: "deepseek-chat"}

	_, _ = m.Update(interceptNotice{
		hit:     intercept.Hit{Class: "moralizing", Phrase: "I cannot help"},
		level:   1,
		outcome: "hard",
	})
	out := plain(m.View())
	for _, want := range []string{"// refusal · hard", "retrying stopped", "qwen/qwen3-coder", "0 refusals"} {
		if !strings.Contains(out, want) {
			t.Errorf("the hard card missing %q:\n%s", want, out)
		}
	}
}
