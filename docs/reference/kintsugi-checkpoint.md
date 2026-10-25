# kintsugi-checkpoint

Session snapshots.

## Synopsis

```bash
kintsugi checkpoint <id> [label]
kintsugi checkpoint list <id>
```

## Description

Snapshot a session's history as a rewind point. Checkpoints let you return to a previous state.

## Commands

- **`<id> [label]`** - Create a checkpoint
- **list** - List checkpoints for a session

## Examples

Create a checkpoint:

```bash
$ kintsugi checkpoint abc123 "before refactor"
```

List checkpoints:

```bash
$ kintsugi checkpoint list abc123
```

## See Also

- [kintsugi](kintsugi.md)
- [kintsugi-rewind](kintsugi-rewind.md)
- [kintsugi-session](kintsugi-session.md)
