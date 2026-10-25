# kintsugi-models

List providers and models.

## Synopsis

```bash
kintsugi models
kintsugi models import <file> [--write]
kintsugi models fetch <provider> [--write]
kintsugi models report [--since YYYY-MM-DD] [--json]
```

## Description

List configured providers and models, import model catalogs, or fetch a provider's model list.

## Options

- `--write` - Persist changes to config

## Examples

List all models:

```bash
$ kintsugi models
```

Import a model catalog:

```bash
$ kintsugi models import models.json --write
```

Fetch models from a provider:

```bash
$ kintsugi models fetch ollama --write
```

## See Also

- [kintsugi](kintsugi.md)
- [kintsugi-connect](kintsugi-connect.md)
