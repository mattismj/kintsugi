# kintsugi-session

Session management.

## Synopsis

```bash
kintsugi session list
kintsugi session show <id>
kintsugi session rename <id> "<title>"
kintsugi session search <query>
kintsugi session fork <id>
kintsugi session export <id>
kintsugi session ab <id> --model-a <p/m> --model-b <p/m> [--at <checkpoint>] [--turn "task"] [--json]
```

## Description

Manage sessions: list, view, rename, search, fork, and export.

## Commands

- **ab** — fork the session at a checkpoint and replay the same turn on two models, read-only (plan-mode tool policy), each in its own throwaway session. Prints a summary table (elapsed, tokens, refusals) and both outputs with a differ/identical verdict. The measurement tool for "which model codes better on this task".

- **list** - Index sessions by title/model/persona/turns
- **show** - Replay a session log
- **rename** - Set a session's title
- **search** - Search transcripts
- **fork** - Copy a session into a new branch
- **export** - Write a session as legacy JSONL

## Examples

List sessions:

```bash
$ kintsugi session list
```

View a session:

```bash
$ kintsugi session show abc123
```

Rename a session:

```bash
$ kintsugi session rename abc123 "refactor auth"
```

Search sessions:

```bash
$ kintsugi session search "authentication bug"
```

Fork a session:

```bash
$ kintsugi session fork abc123
```

Export as JSONL:

```bash
$ kintsugi session export abc123
```

## See Also

- [kintsugi](kintsugi.md)
- [kintsugi-run](kintsugi-run.md)
