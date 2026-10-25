package main

// `kintsugi branding`: the identity kit, as terminal text and as SVG assets
// (DESIGN §7.4). Everything here is derived from the tables the TUI draws from
// — `ui.Wordmark` for the block letters, `ui.MascotAt` for the anvil mark and
// the token tables for the palette — so a shipped asset can never disagree with
// what the terminal renders.
//
//	kintsugi branding wordmark          the block FORGE wordmark, ramp-shaded
//	kintsugi branding sigil             the compact anvil mark
//	kintsugi branding palette           every token: name, hex, 256/16 fallbacks, swatches
//	kintsugi branding export <dir>      wordmark.svg, sigil.svg, palette.svg

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mattn/go-isatty"

	"kintsugi/internal/ui"
)

// brandingUsage is the text printed by `kintsugi branding` with no arguments and by
// `kintsugi branding help`.
const brandingUsage = `kintsugi branding — the Kintsugi identity kit

  kintsugi branding wordmark     print the block FORGE wordmark, ramp-shaded
  kintsugi branding sigil        print the compact anvil mark (6 rows)
  kintsugi branding palette      every token: name, hex, 256-colour and 16-colour
                              fallbacks, plus a live swatch row
  kintsugi branding export <dir> write wordmark.svg, sigil.svg and palette.svg into dir

The palette is printed for the current profile: truecolor, 256, 16 or mono.
NO_COLOR and a non-terminal stdout both fall back to mono, with no escapes.`

// cmdBranding is the entry point for `kintsugi branding ...`.
func cmdBranding(ws string, args []string) {
	if err := brandingRun(ws, args); err != nil {
		fail("%v", err)
	}
}

// brandingRun dispatches a branding subcommand and returns an error rather than
// exiting, so the unknown-subcommand path is testable.
func brandingRun(ws string, args []string) error {
	if len(args) == 0 {
		fmt.Println(brandingUsage)
		return nil
	}
	switch args[0] {
	case "wordmark":
		brandingWordmark()
		return nil
	case "sigil":
		brandingSigil()
		return nil
	case "palette":
		brandingPalette()
		return nil
	case "export":
		return brandingExport(ws, args[1:])
	case "help", "-h", "--help":
		fmt.Println(brandingUsage)
		return nil
	default:
		return fmt.Errorf("kintsugi branding: unknown subcommand %q\nusage: kintsugi branding wordmark|sigil|palette|export <dir>", args[0])
	}
}

// brandingWordmark prints the wordmark at its native width: the word with the
// seam in it, ramp-shaded, never scaled art.
func brandingWordmark() {
	for _, row := range brandingTheme().Wordmark() {
		fmt.Println(row)
	}
}

// brandingSigil prints the seam — the brand mark: a rule broken in the middle
// and repaired with gold. It is the mark of a repaired event, and the whole
// identity in one line.
func brandingSigil() {
	fmt.Println(brandingTheme().Seam(31))
}

// brandingPalette prints every token with its hex and both fallbacks, then one
// swatch run in token order. In mono the swatch is plain blocks, and the line
// under it says so: the hex column is the colour then.
func brandingPalette() {
	th := brandingTheme()
	fmt.Println("kintsugi branding · palette")
	fmt.Printf("profile  %s\n\n", brandingProfile())
	fmt.Printf("%-12s %-8s %-4s %-2s\n", "token", "hex", "256", "16")
	for _, tok := range ui.Palette() {
		fmt.Printf("%-12s %-8s %-4s %-2s\n", tok, ui.Hex(tok), ui.Fallback256(tok), ui.Fallback16(tok))
	}
	fmt.Println()
	var swatch strings.Builder
	for _, tok := range ui.Palette() {
		swatch.WriteString(th.Paint(tok, "██") + " ")
	}
	fmt.Println("swatch  " + strings.TrimRight(swatch.String(), " "))
	if th.Profile == ui.Mono {
		fmt.Println("        (mono: the swatches carry no colour — read the hex column)")
	}
	fmt.Printf("\nramp    %s\n", brandingRamp(th))
}

// brandingRamp prints the heat ramp cold to hot, which is the one family the
// whole palette is built on.
func brandingRamp(th ui.Theme) string {
	var b strings.Builder
	for i, tok := range ui.HeatRamp() {
		if i > 0 {
			b.WriteString(" → ")
		}
		if th.Profile == ui.Mono {
			b.WriteString(string(tok))
			continue
		}
		b.WriteString(th.Paint(tok, string(tok)))
	}
	return b.String()
}

// brandingTheme is the theme branding output is drawn with: the detected
// profile, downgraded to mono when stdout cannot take escapes.
func brandingTheme() ui.Theme {
	th := ui.Detect(os.Getenv)
	if th.Profile != ui.Mono && !isatty.IsTerminal(os.Stdout.Fd()) {
		th.Profile = ui.Mono
	}
	return th
}

// brandingProfile says which profile is in use and why, so a plain palette is
// never mistaken for a broken one.
func brandingProfile() string {
	detected, inUse := ui.Detect(os.Getenv), brandingTheme()
	switch {
	case detected.Profile == ui.Mono && os.Getenv("NO_COLOR") != "":
		return "mono (NO_COLOR)"
	case detected.Profile == ui.Mono:
		return "mono (TERM=" + os.Getenv("TERM") + ")"
	case inUse.Profile != detected.Profile:
		return fmt.Sprintf("mono (detected %s; stdout is not a terminal, so escapes would be stripped)", detected.Profile)
	}
	return string(inUse.Profile)
}

// brandingExport writes the three SVG assets into dir, creating it.
func brandingExport(ws string, args []string) error {
	var pos []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") && a != "-" {
			return fmt.Errorf("branding export: unknown flag %q\nusage: kintsugi branding export <dir>", a)
		}
		pos = append(pos, a)
	}
	if len(pos) == 0 {
		return fmt.Errorf("usage: kintsugi branding export <dir>")
	}
	if len(pos) > 1 {
		return fmt.Errorf("branding export: unexpected argument %q (quote a path with spaces)", pos[1])
	}
	dir := strings.TrimSpace(pos[0])
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(ws, dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("branding export: %v", err)
	}
	assets := []struct{ name, body string }{
		{"wordmark.svg", wordmarkSVG()},
		{"sigil.svg", sigilSVG()},
		{"palette.svg", paletteSVG()},
	}
	for _, a := range assets {
		path := filepath.Join(dir, a.name)
		if err := os.WriteFile(path, []byte(a.body), 0o644); err != nil {
			return fmt.Errorf("branding export: %v", err)
		}
		fmt.Printf("wrote %s\n", path)
	}
	return nil
}

// SVG geometry: one grid cell is one terminal cell, so a glyph block stays
// square-ish and the art keeps the proportions it has on screen.
const (
	svgCellW = 12
	svgCellH = 20
	svgPad   = 10
)

// svgSize is the canvas for a cols × rows grid.
func svgSize(cols, rows int) (int, int) {
	return svgPad*2 + cols*svgCellW, svgPad*2 + rows*svgCellH
}

// svgDoc wraps body in a self-contained document with a background.
func svgDoc(w, h int, title, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-label="%s">`, w, h, w, h, title)
	fmt.Fprintf(&b, "\n  <rect x=\"0\" y=\"0\" width=\"%d\" height=\"%d\" fill=\"%s\"/>", w, h, ui.Hex(ui.Ink))
	b.WriteString(body)
	b.WriteString("</svg>\n")
	return b.String()
}

// svgCells draws one group per row, one rect per filled cell. A row group is
// what makes the asset's row count readable, in the file and in the tests.
func svgCells(rows []string, fill func(row, col int) string) string {
	var b strings.Builder
	for r, row := range rows {
		cells := []rune(strings.TrimRight(row, " "))
		if len(cells) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n  <g id=\"row-%d\">", r)
		for c, ch := range cells {
			if ch == ' ' {
				continue
			}
			fmt.Fprintf(&b, "\n    <rect x=\"%d\" y=\"%d\" width=\"%d\" height=\"%d\" fill=\"%s\"/>",
				svgPad+c*svgCellW, svgPad+r*svgCellH, svgCellW, svgCellH, fill(r, c))
		}
		b.WriteString("\n  </g>")
	}
	return b.String()
}

// wordmarkSVG draws the real wordmark rows, each column at its real heat step.
func wordmarkSVG() string {
	rows, cols := ui.WordmarkRows(), ui.WordmarkWidth()
	body := svgCells(rows, func(_, col int) string {
		return ui.Hex(ui.HeatRamp()[ui.WordmarkHeatAt(col, cols)])
	})
	w, h := svgSize(cols, len(rows))
	return svgDoc(w, h, "kintsugi wordmark", body)
}

// sigilSVG draws the seam — a rule broken and repaired with gold.
func sigilSVG() string {
	rows := []string{brandingTheme().Seam(31)}
	body := svgCells(rows, func(_, col int) string {
		return ui.Hex(ui.Gold)
	})
	w, h := svgSize(31, 1)
	return svgDoc(w, h, "kintsugi sigil", body)
}

// paletteSVG draws every token as a named swatch, in table order.
func paletteSVG() string {
	const (
		swatchW = 48
		swatchH = 26
		rowH    = 34
		textX   = svgPad + swatchW + 12
	)
	w, h := svgPad*2+260, svgPad*2+len(ui.Palette())*rowH
	var b strings.Builder
	for i, tok := range ui.Palette() {
		y := svgPad + i*rowH
		fmt.Fprintf(&b, "\n  <g id=\"%s\">", tok)
		fmt.Fprintf(&b, "\n    <rect x=\"%d\" y=\"%d\" width=\"%d\" height=\"%d\" fill=\"%s\"/>",
			svgPad, y, swatchW, swatchH, ui.Hex(tok))
		fmt.Fprintf(&b, "\n    <text x=\"%d\" y=\"%d\" font-family=\"monospace\" font-size=\"12\" fill=\"%s\">%s</text>",
			textX, y+12, ui.Hex(ui.Paper), tok)
		fmt.Fprintf(&b, "\n    <text x=\"%d\" y=\"%d\" font-family=\"monospace\" font-size=\"11\" fill=\"%s\">%s · %s / %s</text>",
			textX, y+25, ui.Hex(ui.PaperSoft), ui.Hex(tok), ui.Fallback256(tok), ui.Fallback16(tok))
		b.WriteString("\n  </g>")
	}
	return svgDoc(w, h, "kintsugi palette", b.String())
}
