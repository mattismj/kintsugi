# kintsugi-memory

Lessons and skills search.

## Synopsis

```bash
kintsugi memory add "<lesson>" [--models a,b] [--source ...]
kintsugi memory search "<query>" [--k N]
kintsugi memory reindex
kintsugi memory list
```

## Description

Manage lessons and skills. Lessons are stored in `.kintsugi/learned.md` and indexed for full-text search.

## Commands

- **add** - Append a lesson
- **search** - Search lessons and skills (FTS5)
- **reindex** - Rebuild the derived index
- **list** - Show all lessons

## Options

- `--models` - Comma-separated model list (add)
- `--source` - Source reference (add)
- `--k` - Number of results (search, default 8)

## Examples

Add a lesson:

```bash
$ kintsugi memory add "always use context managers for file I/O"
```

Search lessons:

```bash
$ kintsugi memory search "file handling"
```

Rebuild index:

```bash
$ kintsugi memory reindex
```

List all lessons:

```bash
$ kintsugi memory list
```

## See Also

- [kintsugi](kintsugi.md)
- [kintsugi-recall](kintsugi-recall.md)
