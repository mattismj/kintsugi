# kintsugi-ledger

Accepted work ledger.

## Synopsis

```bash
kintsugi ledger list [--kind <type>] [--json]
kintsugi ledger add
kintsugi ledger export [--file <path>]
```

## Description

Manage the accepted work ledger. The ledger tracks plans, artifacts, and decisions the operator has accepted.

## Commands

- **list** - Show accepted work, newest first
- **add** - Append one accepted entry
- **export** - Write the ledger as JSONL

## Options

- `--kind` - Filter by entry type
- `--json` - Output as JSON
- `--file` - Output file path (export)

## Examples

List entries:

```bash
$ kintsugi ledger list
```

Filter by kind:

```bash
$ kintsugi ledger list --kind plan
```

Add an entry:

```bash
$ kintsugi ledger add
```

Export as JSONL:

```bash
$ kintsugi ledger export --file ledger.jsonl
```

## See Also

- [kintsugi](kintsugi.md)
- [kintsugi-recall](kintsugi-recall.md)
