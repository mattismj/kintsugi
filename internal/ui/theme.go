// Package ui is the Kintsugi Terminal design language: sharp frames only, an
// amber heat ramp (Ash → Umber → Gold → Ochre → Hikari) as the single
// brand family, a glyph status vocabulary, and a capability ladder from
// truecolor down to mono. Colour is never decoration — Danger only fails,
// OK only passes. See DESIGN §7.4.
package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

type Token string

const (
	Ink         Token = "ink"
	Surface     Token = "surface"
	SurfaceSoft Token = "surface-soft"
	Line        Token = "line"
	LineStrong  Token = "line-strong"
	Paper       Token = "paper"
	PaperDim    Token = "paper-dim"
	PaperSoft   Token = "paper-soft"
	Gold        Token = "accent"
	Umber       Token = "accent-deep"
	// Ochre is the hottest amber: the working/streaming state and the mascot's
	// live core.
	Ochre Token = "ember"
	// Hikari is the highlight step above Gold, used for the wordmark and the
	// peak of a heat ramp. A terminal cannot bloom, so "glow" is a lighter
	// amber, never a blur.
	Hikari Token = "glow"
	// Bronze is cooling steel: the coolest end of the live heat ramp.
	Bronze Token = "cherry"
	// Ash is cooled metal: idle, disabled, gutters — heat that has gone out.
	Ash    Token = "ash"
	Danger Token = "danger"
	Warn   Token = "warn-amber"
	OK     Token = "ok-green"
	Info   Token = "info-blue"
)

type Profile string

const (
	TrueColor Profile = "truecolor"
	ANSI256   Profile = "256"
	ANSI16    Profile = "16"
	Mono      Profile = "mono"
)

type Theme struct {
	Profile  Profile
	Ornament bool
}

var hex = map[Token]string{
	Ink: "#0a0a0a", Surface: "#141414", SurfaceSoft: "#1a1a1a",
	Line: "#2a2a2a", LineStrong: "#3a3a3a",
	Paper: "#f5f1e8", PaperDim: "#a8a39a", PaperSoft: "#6b675f",
	// The gold ramp, as a kintsugi seam actually reads: old bronze at the
	// cool end, through ochre and gold, to hikari (light) at the burnish.
	// This is the signature — the single accent family on urushi black.
	Bronze: "#8a6a2f", // the coolest live gold, edges and pressed lines
	Ochre:  "#b8860b", // the working body of the seam
	Gold:   "#d4a017", // the brand gold: running, CTA, numbers
	Umber:  "#9c7a1c", // deep gold, pressed / edge
	Hikari: "#f0d264", // burnished light, the hottest highlight
	Ash:    "#4a423a", // cooled, idle, gutters
	Danger: "#ff4d8d", Warn: "#e0823f", OK: "#5b9d59", Info: "#6ea8fe",
}

var ansi256 = map[Token]string{
	Ink: "232", Surface: "235", SurfaceSoft: "237",
	Line: "238", LineStrong: "240",
	Paper: "230", PaperDim: "246", PaperSoft: "243",
	Bronze: "94", Ochre: "136", Gold: "178", Umber: "100",
	Hikari: "221", Ash: "238",
	Danger: "212", Warn: "208", OK: "71", Info: "111",
}

var ansi16 = map[Token]string{
	Ink: "0", Surface: "0", SurfaceSoft: "8",
	Line: "8", LineStrong: "8",
	Paper: "7", PaperDim: "7", PaperSoft: "8",
	Bronze: "1", Ochre: "9", Gold: "3", Umber: "1",
	Hikari: "11", Ash: "8",
	Danger: "1", Warn: "3", OK: "2", Info: "4",
}

// Detect picks a color profile from the environment. NO_COLOR and TERM=dumb
// win over everything, per the design language's degradation ladder.
func Detect(getenv func(string) string) Theme {
	t := Theme{Profile: Mono, Ornament: true}
	switch {
	case getenv("NO_COLOR") != "":
		return t
	case strings.EqualFold(getenv("TERM"), "dumb"):
		return t
	}
	ct := strings.ToLower(getenv("COLORTERM"))
	term := strings.ToLower(getenv("TERM"))
	switch {
	case strings.Contains(ct, "truecolor") || strings.Contains(ct, "24bit"):
		t.Profile = TrueColor
	case strings.Contains(term, "truecolor") || strings.Contains(term, "24bit"):
		t.Profile = TrueColor
	case strings.Contains(term, "256color"):
		t.Profile = ANSI256
	case strings.Contains(term, "color"), strings.Contains(term, "xterm"),
		strings.Contains(term, "screen"), strings.Contains(term, "tmux"),
		strings.Contains(term, "linux"):
		t.Profile = ANSI16
	}
	return t
}

// Token resolves one design token for the active profile.
func (t Theme) Token(tok Token) lipgloss.TerminalColor {
	switch t.Profile {
	case TrueColor:
		return lipgloss.Color(hex[tok])
	case ANSI256:
		return lipgloss.Color(ansi256[tok])
	case ANSI16:
		return lipgloss.Color(ansi16[tok])
	default:
		return lipgloss.NoColor{}
	}
}

// Style returns a style carrying the token color. Mono uses attributes only,
// so the heat ramp degrades to weight: cold (Ash) is faint, hot (Ochre/Hikari)
// is bold, and the glyphs still carry the state.
func (t Theme) Style(tok Token) lipgloss.Style {
	st := lipgloss.NewStyle()
	if t.Profile == Mono {
		switch tok {
		case PaperSoft, Line, LineStrong, Ash:
			return st.Faint(true)
		case Bronze, Gold, Umber, Ochre, Hikari:
			// The live heat ramp degrades to weight: anything carrying heat is
			// bold, anything cooled is faint. Bronze is the coolest live step, so
			// it stays bold — it is still working metal.
			return st.Bold(true)
		case Danger:
			return st.Bold(true)
		}
		return st
	}
	return st.Foreground(t.Token(tok))
}

func (t Theme) Paint(tok Token, s string) string { return t.Style(tok).Render(s) }

// heatRamp is the gold ladder — the kintsugi seam, cold to burnished — used by
// Heat and Hikari. One family, five steps — this is the whole brand palette.
var heatRamp = []Token{Ash, Bronze, Ochre, Gold, Hikari}

// Heat maps a 0..4 intensity onto the gold ramp: 0 is cooled ash, 4 is the
// burnish peak. Out-of-range values clamp, so a caller can pass a raw counter.
func (t Theme) Heat(n int) Token {
	if n < 0 {
		n = 0
	}
	if n >= len(heatRamp) {
		n = len(heatRamp) - 1
	}
	return heatRamp[n]
}

// Hikari renders text with a ramp-shaded heat: the first glyph is the hottest
// (Hikari) and the rest step down toward Ochre, so a wordmark reads as metal
// cooling left to right. In mono (and below truecolor) it returns the text
// unchanged — a terminal cannot bloom, so the honest fallback is plain type.
func (t Theme) Hikari(text string) string {
	if t.Profile == Mono || text == "" {
		return text
	}
	runes := []rune(text)
	var b strings.Builder
	for i, r := range runes {
		// Step the ramp down across the word, never below Ochre.
		step := 4 - i
		if step < 3 {
			step = 3
		}
		b.WriteString(t.Paint(t.Heat(step), string(r)))
	}
	return b.String()
}

// StatusTone is the single mapping from state to glyph and tone: one table, so
// adding a state is one edit here and never a per-view colour guess.
func StatusTone(state string) (glyph string, tok Token) {
	switch strings.ToLower(state) {
	case "running", "reviewing", "active":
		return "▸", Ochre
	case "done", "passed", "ok":
		return "✓", OK
	case "blocked", "escalated", "failed", "error":
		return "✸", Danger
	case "waiting", "paused", "warning":
		return "▪", Warn
	case "killed":
		// Killed is an operator's own decision, not a failure: warn, not danger.
		return "×", Warn
	case "pending":
		return "▪", PaperDim
	case "queued", "idle", "empty":
		return "·", Ash
	case "destructive":
		return "×", Danger
	case "starred", "pinned":
		return "★", Gold
	default:
		return "·", PaperDim
	}
}

// Status renders a glyph + label in the state's tone.
func (t Theme) Status(state, label string) string {
	g, tok := StatusTone(state)
	return t.Paint(tok, g) + " " + t.Paint(PaperDim, label)
}

// Card draws a sharp frame with manga corner-cut brackets in the tone color.
func (t Theme) Card(title, body string, width int, tone Token) string {
	width = maxInt(width, 8)
	inner := width - 2
	var b strings.Builder
	b.WriteString(t.Paint(tone, "┌") + t.Paint(Line, strings.Repeat("─", inner)) + t.Paint(Line, "┐") + "\n")
	if title != "" {
		b.WriteString(t.Paint(Line, "│") + fit(t.Paint(tone, title), inner) + t.Paint(Line, "│") + "\n")
	}
	if body != "" {
		for _, line := range strings.Split(body, "\n") {
			b.WriteString(t.Paint(Line, "│") + fit(line, inner) + t.Paint(Line, "│") + "\n")
		}
	}
	b.WriteString(t.Paint(Line, "└") + t.Paint(Line, strings.Repeat("─", inner)) + t.Paint(tone, "┘"))
	return b.String()
}

// Panel is a column surface: a slim eyebrow, the body, and a hairline rule at
// the bottom. It deliberately has no top/side borders — three boxed columns
// read as a debug frame, and the whitespace is what carries the layout. The
// bottom rule separates the column from the footer without boxing the whole
// thing.
func (t Theme) Panel(title, body string, width, height int) string {
	width = maxInt(width, 6)
	height = maxInt(height, 3)
	innerW := width

	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if body == "" {
		lines = nil
	}
	// Fill any leftover height with blank rows so every column stays the same
	// height and the bottom rule lands in the same place.
	maxBody := height - 2
	for len(lines) < maxBody {
		lines = append(lines, strings.Repeat(" ", innerW))
	}
	if len(lines) > maxBody {
		lines = lines[len(lines)-maxBody:]
	}

	var b strings.Builder
	b.WriteString(fit(t.Paint(PaperSoft, title), innerW) + "\n")
	for _, line := range lines {
		b.WriteString(fit(line, innerW) + "\n")
	}
	// Trailing newline stripped; the bottom rule is its own line.
	out := strings.TrimRight(b.String(), "\n")
	return out + "\n" + t.Paint(Line, strings.Repeat("─", innerW))
}

// fit truncates or pads one line to exactly width terminal cells.
func fit(s string, width int) string {
	if width <= 0 {
		return ""
	}
	w := lipgloss.Width(s)
	if w > width {
		return lipgloss.NewStyle().MaxWidth(width).Render(s)
	}
	if w < width {
		return s + strings.Repeat(" ", width-w)
	}
	return s
}

// Progress renders a square-marker bar; the filled portion uses the accent.
func (t Theme) Progress(frac float64, width int) string {
	if width <= 0 {
		width = 8
	}
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(frac*float64(width) + 0.5)
	// The filled portion runs hot (Gold) and the empty track is cooled ash.
	return t.Paint(Gold, strings.Repeat("▓", filled)) +
		t.Paint(Ash, strings.Repeat("░", width-filled))
}

// Sigil is a small per-family identity glyph (DESIGN §7.3), glyph-only.
func Sigil(family string) string {
	switch strings.ToLower(family) {
	case "deepseek":
		return "◤◢"
	case "qwen":
		return "◇◆"
	case "glm":
		return "▚▞"
	case "kimi":
		return "▞▚"
	default:
		return "▸▸"
	}
}

// wordmarkRows is the kintsugi wordmark: the word with the seam in it. The
// gold star is the repair set into the name itself, and the second row is
// the seam — a line broken and filled. Nine columns, two rows: a wordmark
// an operator can read at any terminal width.
var wordmarkRows = []string{
	"kin✦tsugi",
	"── ✦ ──",
}

// Wordmark renders the wordmark, ramp-shaded: the letters step up the gold
// ramp toward the seam, the seam's star is Hikari (the burnish), and the
// dashes are Bronze. In mono it returns the plain rows: no colour, no
// attributes — the shape alone still reads.
func (t Theme) Wordmark() []string {
	out := make([]string, 0, len(wordmarkRows))
	for i, row := range wordmarkRows {
		r := []rune(strings.TrimRight(row, " "))
		if t.Profile == Mono {
			out = append(out, string(r))
			continue
		}
		var b strings.Builder
		for j, ch := range r {
			tok := t.wordmarkTone(i, j, ch, len(r))
			b.WriteString(t.Paint(tok, string(ch)))
		}
		out = append(out, b.String())
	}
	return out
}

// wordmarkTone is the tone of one wordmark cell. Row 0 climbs the gold ramp
// toward the seam (the ✦ itself is Hikari); row 1 is the seam — dashes in
// Bronze, the repair in Hikari. One escape per run is not worth the
// complexity at this size; the rows are nine cells.
func (t Theme) wordmarkTone(row, col int, ch rune, width int) Token {
	if ch == '✦' {
		return Hikari
	}
	if row == 1 {
		return Bronze
	}
	// Row 0: Ash at the k, warming right through the gold ramp to Hikari at
	// the seam. The word literally heats up to the repair.
	return t.Heat(wordmarkHeatAt(col, width))
}

// wordmarkHeatAt maps a column onto the 0..4 gold ramp: left is cooled ash,
// right is the burnish, so the word heats up to the seam.
func wordmarkHeatAt(col, width int) int {
	if width <= 1 {
		return 4
	}
	if col < 0 {
		col = 0
	}
	if col > width-1 {
		col = width - 1
	}
	// 0 → 4 across the word: every column maps to a step of the ramp.
	return col * 4 / (width - 1)
}

// Seam renders the kintsugi seam: a rule broken in the middle and repaired
// with gold. It is the mark of a repaired event — a refusal intercepted and
// resumed, prior work re-injected, a frame applied — because the repair being
// visible is the product's whole thesis. The dashes are Bronze (the cooled
// gold at a seam's edges), the star is Hikari (the burnish at the centre).
func (t Theme) Seam(width int) string {
	if width < 5 {
		return t.Paint(Bronze, strings.Repeat("─", maxInt(width, 0)))
	}
	dashes := width - 3
	left := dashes / 2
	right := dashes - left
	return t.Paint(Bronze, strings.Repeat("─", left)) + " " +
		t.Paint(Hikari, "✦") + " " +
		t.Paint(Bronze, strings.Repeat("─", right))
}

// Banner is the nameplate shown once per session. It is a nameplate, not a claim
// that work is happening — no fake scanning, no animation.
//
// It deliberately carries no block-letter wordmark. A five-row glyph lockup at
// the top of a transcript competes with the ember creature for the same
// attention, reads as noise at any width where the panels are already tight,
// and is the first thing to break when a line is clipped. The identity in the
// transcript is the creature and the heat ramp; the nameplate is one framed
// line of type. `kintsugi branding wordmark` still prints the block lockup for
// anything that genuinely wants a logo (a README, a banner image).
func (t Theme) Banner(model, persona, sessionID string) string {
	// One line, and only what an operator needs to confirm a session is live.
	// Session/model/persona live in the status footer and the on-demand
	// detail panel, not as three lines of nameplate at the top of every
	// conversation.
	return t.Paint(PaperDim, sessionID) + t.Paint(PaperSoft, " · ") + t.Paint(Paper, model)
}

// CompactBanner is the narrow-terminal nameplate: one ramp-shaded word, one
// facts line. Used below 100 columns, where the block letters would crowd the
// transcript.
func (t Theme) CompactBanner(model, persona, sessionID string) string {
	return t.Paint(PaperDim, sessionID) + t.Paint(PaperSoft, " · ") + t.Paint(Paper, model)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
