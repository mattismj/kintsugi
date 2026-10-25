# kintsugi-recall

Search prior accepted work.

## Synopsis

```bash
kintsugi recall "<query>" [--top N]
```

## Description

Search prior accepted work for a query. Returns the most relevant entries from the ledger.

## Options

- `--top` - Number of results (default 5)

## Examples

Search for prior work:

```bash
$ kintsugi recall "authentication refactor"
```

Get more results:

```bash
$ kintsugi recall "database migration" --top 10
```

## See Also

- [kintsugi](kintsugi.md)
- [kintsugi-ledger](kintsugi-ledger.md)
- [kintsugi-memory](kintsugi-memory.md)
