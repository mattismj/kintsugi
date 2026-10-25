# kintsugi-kb

Knowledge base management.

## Synopsis

```bash
kintsugi kb build <stix> <out>
kintsugi kb reindex <dir>
kintsugi kb search <dir> "<query>" [--top N]
kintsugi kb list <dir>
```

## Description

Manage knowledge bases built from MITRE CTI STIX bundles.

## Commands

- **build** - Build a KB from a STIX bundle
- **reindex** - Rebuild the derived BM25 index
- **search** - Rank chunks with BM25
- **list** - Show base id/version/title and chunk count

## Options

- `--top` - Number of results (search, default 8)

## Examples

Build a knowledge base:

```bash
$ kintsugi kb build bundle.json kb/
```

Rebuild index:

```bash
$ kintsugi kb reindex kb/
```

Search:

```bash
$ kintsugi kb search kb/ "lateral movement"
```

List KB contents:

```bash
$ kintsugi kb list kb/
```

## See Also

- [kintsugi](kintsugi.md)
