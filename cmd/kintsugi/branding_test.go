package main

// Tests for `kintsugi branding`. The command is derived art, so what is checked
// here is the derivation: every token reaches the palette, the export carries
// the real row count, and the plain profiles carry no escapes.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kintsugi/internal/ui"
)

// TestBrandingPaletteNamesEveryToken checks that the palette prints each token
// with its hex value — the table is the source, so a token missing from the
// output is a token missing from the kit.
func TestBrandingPaletteNamesEveryToken(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	out := captureStdout(t, func() {
		if err := brandingRun(t.TempDir(), []string{"palette"}); err != nil {
			t.Fatalf("branding palette: %v", err)
		}
	})
	for _, tok := range ui.Palette() {
		if !strings.Contains(out, string(tok)) {
			t.Errorf("the palette does not name token %q:\n%s", tok, out)
		}
		if hex := ui.Hex(tok); !strings.Contains(out, hex) {
			t.Errorf("the palette does not carry %s's hex %s:\n%s", tok, hex, out)
		}
		if fb := ui.Fallback256(tok); !strings.Contains(out, fb) {
			t.Errorf("the palette does not carry %s's 256 fallback %s:\n%s", tok, fb, out)
		}
	}
}

// TestBrandingExportWritesTheKit checks the three assets exist, are SVG, and
// that the wordmark's row count is the real wordmark's — derived, not asserted
// against a number that would drift.
func TestBrandingExportWritesTheKit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "assets")
	out := captureStdout(t, func() {
		if err := brandingRun(t.TempDir(), []string{"export", dir}); err != nil {
			t.Fatalf("branding export: %v", err)
		}
	})

	for _, name := range []string{"wordmark.svg", "sigil.svg", "palette.svg"} {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s was not written: %v", name, err)
		}
		body := string(data)
		if len(body) == 0 {
			t.Fatalf("%s is empty", name)
		}
		if !strings.HasPrefix(body, "<svg") {
			t.Errorf("%s does not start with <svg: %.40q", name, body)
		}
		if !strings.Contains(body, "viewBox") {
			t.Errorf("%s has no viewBox", name)
		}
		if !strings.Contains(out, path) {
			t.Errorf("the command did not report %s:\n%s", path, out)
		}
	}

	data, err := os.ReadFile(filepath.Join(dir, "wordmark.svg"))
	if err != nil {
		t.Fatal(err)
	}
	rows := len(ui.WordmarkRows())
	if got := strings.Count(string(data), `<g id="row-`); got != rows {
		t.Fatalf("wordmark.svg draws %d rows, want %d (the real wordmark)", got, rows)
	}

	// The other two assets carry the colour tables, not hardcoded hexes.
	palette, err := os.ReadFile(filepath.Join(dir, "palette.svg"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range ui.Palette() {
		if !strings.Contains(string(palette), ui.Hex(tok)) {
			t.Errorf("palette.svg is missing %s (%s)", tok, ui.Hex(tok))
		}
	}
	sigil, err := os.ReadFile(filepath.Join(dir, "sigil.svg"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sigil), ui.Hex(ui.Gold)) {
		t.Errorf("sigil.svg is missing the accent %s", ui.Hex(ui.Gold))
	}
}

// TestBrandingPlainProfilesCarryNoEscapes checks the honest fallback: under
// NO_COLOR — and on a pipe, which is what the capture is — the palette is plain
// text that still names every token.
func TestBrandingPlainProfilesCarryNoEscapes(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	out := captureStdout(t, func() {
		if err := brandingRun(t.TempDir(), []string{"palette"}); err != nil {
			t.Fatalf("branding palette: %v", err)
		}
	})
	if strings.ContainsRune(out, 0x1b) {
		t.Fatalf("NO_COLOR palette carries an escape byte:\n%q", out)
	}
	if !strings.Contains(out, "mono") {
		t.Errorf("the palette does not say it is rendering mono:\n%s", out)
	}
	for _, tok := range ui.Palette() {
		if !strings.Contains(out, string(tok)) {
			t.Errorf("the plain palette drops token %q:\n%s", tok, out)
		}
	}
}

// TestBrandingArtIsPlainUnderMono checks the two art subcommands: the marks
// print, and they print without escapes when colour is off.
func TestBrandingArtIsPlainUnderMono(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	wordmark := captureStdout(t, func() {
		if err := brandingRun(t.TempDir(), []string{"wordmark"}); err != nil {
			t.Fatalf("branding wordmark: %v", err)
		}
	})
	if strings.ContainsRune(wordmark, 0x1b) {
		t.Fatalf("mono wordmark carries an escape byte:\n%q", wordmark)
	}
	if got := strings.Count(strings.TrimRight(wordmark, "\n"), "\n") + 1; got != len(ui.WordmarkRows()) {
		t.Fatalf("the wordmark printed %d rows, want %d", got, len(ui.WordmarkRows()))
	}

	sigil := captureStdout(t, func() {
		if err := brandingRun(t.TempDir(), []string{"sigil"}); err != nil {
			t.Fatalf("branding sigil: %v", err)
		}
	})
	if strings.ContainsRune(sigil, 0x1b) {
		t.Fatalf("mono sigil carries an escape byte:\n%q", sigil)
	}
	if got := strings.Count(strings.TrimRight(sigil, "\n"), "\n") + 1; got != 1 {
		t.Fatalf("the sigil printed %d rows, want 1 (the seam)", got)
	}
	if !strings.Contains(sigil, "✦") {
		t.Fatalf("the sigil lost its gold star:\n%s", sigil)
	}
}

// TestBrandingUnknownSubcommandNamesTheValidOnes keeps the failure usable: an
// error that does not list the subcommands sends the operator to the manual.
func TestBrandingUnknownSubcommandNamesTheValidOnes(t *testing.T) {
	err := brandingRun(t.TempDir(), []string{"banner"})
	if err == nil {
		t.Fatal("an unknown subcommand was accepted")
	}
	msg := err.Error()
	if !strings.Contains(msg, "banner") {
		t.Errorf("the error does not name what was asked for: %s", msg)
	}
	for _, sub := range []string{"wordmark", "sigil", "palette", "export"} {
		if !strings.Contains(msg, sub) {
			t.Errorf("the error does not list %q: %s", sub, msg)
		}
	}
}

// TestBrandingUsageNamesEverySubcommand pins the help text to the dispatch.
func TestBrandingUsageNamesEverySubcommand(t *testing.T) {
	for _, want := range []string{
		"kintsugi branding wordmark", "kintsugi branding sigil",
		"kintsugi branding palette", "kintsugi branding export <dir>",
	} {
		if !strings.Contains(brandingUsage, want) {
			t.Errorf("the usage text does not mention %q:\n%s", want, brandingUsage)
		}
	}
}
