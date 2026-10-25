# kintsugi-audit

View permission decisions.

## Synopsis

```bash
kintsugi audit [--json] [--clear] [--n <limit>]
```

## Description

View permission decisions in this workspace. The audit log records what was allowed or refused, with the reason and the rule that decided it. It lives in the workspace and is never uploaded.

## Options

- `--json` - Print events as JSON
- `--clear` - Truncate the audit log (asks first)
- `--n` - How many events to print (default 50)
- `--yes` - Do not ask before clearing

## Examples

View recent events:

```bash
$ kintsugi audit
```

View as JSON:

```bash
$ kintsugi audit --json
```

View last 100 events:

```bash
$ kintsugi audit --n 100
```

Clear the log:

```bash
$ kintsugi audit --clear
```

## See Also

- [kintsugi](kintsugi.md)
