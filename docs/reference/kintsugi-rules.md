# kintsugi-rules

Rule file management.

## Synopsis

```bash
kintsugi rules scan
kintsugi rules import [--apply]
```

## Description

Find and import rule files (AGENTS.md, .clinerules, .cursor rules).

## Commands

- **scan** - Find existing rule files
- **import** - Report only; `--apply` writes the inherited layer

## Options

- `--apply` - Write the inherited layer

## Examples

Scan for rules:

```bash
$ kintsugi rules scan
```

Import rules:

```bash
$ kintsugi rules import --apply
```

## See Also

- [kintsugi](kintsugi.md)
