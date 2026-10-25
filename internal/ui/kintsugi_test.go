package ui

import (
	"strings"
	"testing"
)

// The heat ramp is the brand: cold ash to hot glow, one family, monotonic.
func TestHeatRampIsMonotonic(t *testing.T) {
	th := Theme{Profile: TrueColor}
	// Ash is heat that has gone out; then cherry red, ember, orange, amber-white.
	want := []Token{Ash, Bronze, Ochre, Gold, Hikari}
	for i, w := range want {
		if got := th.Heat(i); got != w {
			t.Errorf("Heat(%d) = %q, want %q", i, got, w)
		}
	}
	if th.Heat(-1) != Ash {
		t.Error("negative heat must clamp to ash")
	}
	if th.Heat(99) != Hikari {
		t.Error("overflow heat must clamp to glow")
	}
}

// Hikari shades a wordmark across the ramp in truecolor and returns the text
// unchanged in mono — a terminal cannot bloom, so the honest fallback is type.
func TestGlowDegradesToPlainTextInMono(t *testing.T) {
	mono := Theme{Profile: Mono}
	if got := mono.Hikari("FORGE"); got != "FORGE" {
		t.Fatalf("mono Hikari = %q, want plain text", got)
	}
	if got := mono.Hikari(""); got != "" {
		t.Fatalf("mono Hikari(\"\") = %q", got)
	}
	// In colour the letters survive exactly once, in ramp order. (Whether the
	// escape sequences survive depends on the terminal, so the contract tested
	// here is the text, not the bytes.)
	full := Theme{Profile: TrueColor}
	got := full.Hikari("FORGE")
	for _, r := range "FORGE" {
		if strings.Count(got, string(r)) != 1 {
			t.Fatalf("Hikari must keep %q exactly once: %q", r, got)
		}
	}
	if len(stripANSI(got)) != len("FORGE") {
		t.Fatalf("Hikari changed the visible width: %q", stripANSI(got))
	}
}

// The wordmark is five block letters, ramp-shaded, with a stable geometry so
// the banner never shifts width between frames.
func TestWordmarkGeometry(t *testing.T) {
	th := Theme{Profile: TrueColor}
	rows := th.Wordmark()
	if len(rows) != len(wordmarkRows) {
		t.Fatalf("wordmark rows = %d, want %d", len(rows), len(wordmarkRows))
	}
	for i, row := range rows {
		plain := stripANSI(row)
		if w := len([]rune(plain)); w > 19 {
			t.Errorf("row %d width %d exceeds the 19-column mark: %q", i, w, plain)
		}
		if strings.HasSuffix(plain, " ") {
			t.Errorf("row %d has trailing space: %q", i, plain)
		}
	}
	mono := Theme{Profile: Mono}
	for i, row := range mono.Wordmark() {
		if strings.Contains(row, "\x1b") {
			t.Errorf("mono wordmark row %d carries ANSI: %q", i, row)
		}
		if strings.TrimSpace(row) == "" {
			t.Errorf("mono wordmark row %d is empty", i)
		}
	}
}

func TestBannersCarryNoLegacyBranding(t *testing.T) {
	for _, th := range []Theme{{Profile: TrueColor}, {Profile: Mono}} {
		for _, b := range []string{
			th.Banner("mock/deepseek-chat", "deepseek-flash41", "20261022-test"),
			th.CompactBanner("mock/deepseek-chat", "deepseek-flash41", "20261022-test"),
		} {
			lower := strings.ToLower(stripANSI(b))
			if strings.Contains(lower, "damage") {
				t.Errorf("banner still carries the legacy design-system name: %q", b)
			}
			// M13: the nameplate is one line of type — session and model only.
			// The product name is the header's job ("kintsugi · model"); a second
			// "kintsugi" in the transcript banner would state it twice.
			if strings.Contains(lower, "kintsugi") {
				t.Errorf("banner names the product, but the header already does: %q", b)
			}
			if !strings.Contains(b, "20261022-test") {
				t.Errorf("banner lost the session id: %q", b)
			}
			if !strings.Contains(b, "mock/deepseek-chat") {
				t.Errorf("banner lost the model: %q", b)
			}
		}
	}
}

func TestNewTokensResolveOnEveryProfile(t *testing.T) {
	for _, tok := range []Token{Ochre, Hikari, Ash} {
		for _, p := range []Profile{TrueColor, ANSI256, ANSI16} {
			th := Theme{Profile: p}
			if c := th.Token(tok); c == nil {
				t.Errorf("%s on %s resolved to nil", tok, p)
			}
		}
		if got := (Theme{Profile: Mono}).Paint(tok, "x"); got != "x" {
			t.Errorf("mono must not alter the text for %s: %q", tok, got)
		}
	}
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
