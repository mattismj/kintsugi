package ui

import "strings"

// paletteOrder is every design token, in the order the palette prints them: the
// surfaces, the paper tones, the heat ramp, then the semantic colours.
var paletteOrder = []Token{
	Ink, Surface, SurfaceSoft, Line, LineStrong,
	Paper, PaperDim, PaperSoft,
	Ash, Bronze, Ochre, Gold, Umber, Hikari,
	Danger, Warn, OK, Info,
}

// Palette is every design token, in palette order.
func Palette() []Token { return append([]Token(nil), paletteOrder...) }

// HeatRamp is the gold ladder, cold to white-hot.
func HeatRamp() []Token { return append([]Token(nil), heatRamp...) }

// Hex is the truecolor value of a token.
func Hex(tok Token) string { return hex[tok] }

// Fallback256 is the xterm-256 value a token degrades to.
func Fallback256(tok Token) string { return ansi256[tok] }

// Fallback16 is the ANSI-16 value a token degrades to.
func Fallback16(tok Token) string { return ansi16[tok] }

// WordmarkRows is the plain block-letter wordmark the TUI draws.
func WordmarkRows() []string { return append([]string(nil), wordmarkRows...) }

// WordmarkWidth is the column count the wordmark is shaded across.
func WordmarkWidth() int {
	width := 0
	for _, row := range wordmarkRows {
		if w := len([]rune(strings.TrimRight(row, " "))); w > width {
			width = w
		}
	}
	return width
}

// WordmarkHeatAt is the heat step a wordmark column is painted at.
func WordmarkHeatAt(col, width int) int { return wordmarkHeatAt(col, width) }
