# kintsugi-branding

The Kintsugi identity kit.

## Synopsis

```bash
kintsugi branding wordmark
kintsugi branding sigil
kintsugi branding palette
kintsugi branding export <dir>
```

## Description

Print the Kintsugi identity in the terminal, or export it as SVG assets. The art
and the colours come from the same tables the TUI draws from — the real wordmark
rows, the rail-scale anvil and the token palette — so an exported asset cannot
disagree with what the terminal renders.

## Commands

- **wordmark** - Print the block KINTSUGI wordmark at its native 19 columns, shaded across the heat ramp from glow at the left to accent-deep at the right.
- **sigil** - Print the compact anvil mark: 6 rows by 13 columns, spark above the anvil.
- **palette** - Print every design token with its name, hex value, 256-colour fallback and 16-colour fallback, plus a live swatch row.
- **export &lt;dir&gt;** - Write `wordmark.svg`, `sigil.svg` and `palette.svg` into the directory, creating it if needed.

The palette is honest about the active profile. Under `NO_COLOR`, `TERM=dumb` or
a non-terminal stdout it renders plain, with no escape sequences, and says which
profile it is in.

## Examples

Draw the wordmark:

```bash
$ kintsugi branding wordmark
```

Inspect the palette for this terminal:

```bash
$ kintsugi branding palette
```

Write the asset kit:

```bash
$ kintsugi branding export branding/
```

## See Also

- [kintsugi](kintsugi.md)