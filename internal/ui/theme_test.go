package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDetectProfiles(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want Profile
	}{
		{"truecolor", map[string]string{"COLORTERM": "truecolor", "TERM": "xterm-256color"}, TrueColor},
		{"256", map[string]string{"TERM": "screen-256color"}, ANSI256},
		{"16", map[string]string{"TERM": "xterm"}, ANSI16},
		{"dumb", map[string]string{"TERM": "dumb"}, Mono},
		{"no-color", map[string]string{"NO_COLOR": "1", "COLORTERM": "truecolor"}, Mono},
		{"unknown", map[string]string{"TERM": ""}, Mono},
	}
	for _, c := range cases {
		if got := Detect(env(c.env)).Profile; got != c.want {
			t.Errorf("%s: profile = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestMonoUsesNoColor(t *testing.T) {
	th := Detect(env(map[string]string{"NO_COLOR": "1"}))
	out := th.Paint(Gold, "x")
	if strings.Contains(out, "\x1b[38") || strings.Contains(out, "\x1b[48") {
		t.Fatalf("mono emitted color escape: %q", out)
	}
	if th.Paint(Gold, "x") == "" {
		t.Fatal("paint dropped text")
	}
}

func TestStatusToneIsSingleMapping(t *testing.T) {
	cases := map[string]struct {
		glyph string
		tok   Token
	}{
		"running": {"▸", Ochre},
		"done":    {"✓", OK},
		"blocked": {"✸", Danger},
		"waiting": {"▪", Warn},
		"queued":  {"·", Ash},
		"pending": {"▪", PaperDim},
	}
	for state, want := range cases {
		g, tok := StatusTone(state)
		if g != want.glyph || tok != want.tok {
			t.Errorf("%s: got %s/%s, want %s/%s", state, g, tok, want.glyph, want.tok)
		}
	}
}

func TestPanelDimensions(t *testing.T) {
	th := Detect(env(map[string]string{"COLORTERM": "truecolor", "TERM": "xterm"}))
	for _, size := range [][2]int{{24, 8}, {40, 12}, {80, 20}} {
		out := th.Panel("// 01 tasks", "line one\nline two is much too long for this panel width", size[0], size[1])
		lines := strings.Split(out, "\n")
		if len(lines) != size[1] {
			t.Fatalf("%dx%d: %d lines", size[0], size[1], len(lines))
		}
		for i, line := range lines {
			if got := lipgloss.Width(line); got != size[0] {
				t.Errorf("%dx%d row %d width = %d, want %d: %q", size[0], size[1], i, got, size[0], line)
			}
		}
	}
}

func TestCardHasCornerBrackets(t *testing.T) {
	th := Detect(env(map[string]string{"NO_COLOR": "1"}))
	out := th.Card("// frame · injected", "reminder: scope ok", 30, Warn)
	lines := strings.Split(out, "\n")
	if !strings.Contains(lines[0], "┌") || !strings.Contains(lines[0], "┐") {
		t.Fatalf("top corners missing: %q", lines[0])
	}
	if !strings.Contains(lines[len(lines)-1], "┘") {
		t.Fatalf("bottom corner missing: %q", lines[len(lines)-1])
	}
	if !strings.Contains(out, "frame · injected") {
		t.Fatal("eyebrow missing")
	}
}

func TestProgress(t *testing.T) {
	th := Detect(env(map[string]string{"NO_COLOR": "1"}))
	if got := th.Progress(0, 8); strings.Count(got, "▓") != 0 || strings.Count(got, "░") != 8 {
		t.Fatalf("empty bar = %q", got)
	}
	if got := th.Progress(1, 8); strings.Count(got, "▓") != 8 {
		t.Fatalf("full bar = %q", got)
	}
	if got := th.Progress(0.5, 8); strings.Count(got, "▓") != 4 {
		t.Fatalf("half bar = %q", got)
	}
}

func TestSigilsAreGlyphsOnly(t *testing.T) {
	for _, fam := range []string{"deepseek", "qwen", "glm", "kimi", "other"} {
		s := Sigil(fam)
		if s == "" {
			t.Fatalf("%s: empty sigil", fam)
		}
		for _, r := range s {
			if r > 0x2FFF {
				t.Fatalf("%s: emoji-like rune %q", fam, r)
			}
		}
	}
}
