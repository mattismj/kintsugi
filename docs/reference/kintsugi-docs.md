# kintsugi-docs

Show man pages.

## Synopsis

```bash
kintsugi docs [<command>]
kintsugi docs list
```

## Description

Print a man page for a top-level command. Tries to render through the system `man(1)` when available, falls back to plain text otherwise.

## Commands

- **`<command>`** - Show the man page for a command
- **list** - List available man pages

## Examples

View kintsugi-run man page:

```bash
$ kintsugi docs run
```

List all man pages:

```bash
$ kintsugi docs list
```

View overview:

```bash
$ kintsugi docs
```

## See Also

- [kintsugi](kintsugi.md)
