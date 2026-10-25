# kintsugi-taste

Operator taste preferences.

## Synopsis

```bash
kintsugi taste list [--json]
kintsugi taste open
kintsugi taste lint
```

## Description

Manage operator taste preferences. Taste is local-only: no remote sync, no model-authored policy. Every accept/reject/edit of a proposed change becomes a local signal.

## Commands

- **list** - Show taste bullets, project + global
- **open** - Print the taste file path and content
- **lint** - Reject bullets without a date or kind

## Options

- `--json` - Output as JSON

## Examples

List preferences:

```bash
$ kintsugi taste list
```

Open taste file:

```bash
$ kintsugi taste open
```

Validate taste file:

```bash
$ kintsugi taste lint
```

## See Also

- [kintsugi](kintsugi.md)
