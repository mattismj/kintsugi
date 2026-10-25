# kintsugi-gate

Opsec review gate.

## Synopsis

```bash
kintsugi gate review --diff <file|-> [--blocking] [--json]
```

## Description

Run a read-only opsec review of a diff. The gate checks for security issues and policy violations.

## Options

- `--diff` - Diff file to review, or `-` for stdin (required)
- `--blocking` - Exit non-zero on a non-pass verdict
- `--json` - Print the typed review as JSON

## Examples

Review a diff file:

```bash
$ kintsugi gate review --diff changes.patch
```

Review from stdin:

```bash
$ git diff | kintsugi gate review --diff -
```

Blocking mode for CI:

```bash
$ kintsugi gate review --diff changes.patch --blocking
```

JSON output:

```bash
$ kintsugi gate review --diff changes.patch --json
```

## See Also

- [kintsugi](kintsugi.md)
- [kintsugi-dag](kintsugi-dag.md)
