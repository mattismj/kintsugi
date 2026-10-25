# kintsugi-prompt

Inspect the compiled prompt.

## Synopsis

```bash
kintsugi prompt [test [--model ref] [--golden dir]] show [--text] [--json]
kintsugi prompt [test [--model ref] [--golden dir]] diff
```

## Description

Inspect the compiled prompt manifest. The manifest shows what layers are injected and their token counts.

## Commands

- **test** — compile the prompt across every configured model and assert the
  invariants: layer order, budget compliance, determinism, stack hash. Use
  `--model <ref>` for one model, `--golden <dir>` to write or diff manifests.

- **show** - Compile and print the prompt manifest
- **diff** - Diff this compile against the last one

## Options

- `--text` - Print the compiled prompt itself
- `--json` - Print the manifest as JSON

## Examples

View manifest:

```bash
$ kintsugi prompt [test [--model ref] [--golden dir]] show
```

View the prompt text:

```bash
$ kintsugi prompt [test [--model ref] [--golden dir]] show --text
```

Compare to last compile:

```bash
$ kintsugi prompt [test [--model ref] [--golden dir]] diff
```

## See Also

- [kintsugi](kintsugi.md)
