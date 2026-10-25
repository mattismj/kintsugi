# kintsugi-validate

Provider validation.

## Synopsis

```bash
kintsugi validate [--provider <name>] [--model <id>] [--timeout <duration>]
```

## Description

Run a live validation battery for a provider/model.

## Options

- `--provider` - Provider name
- `--model` - Model id
- `--timeout` - Validation timeout

## Examples

Validate default provider:

```bash
$ kintsugi validate
```

Validate specific provider:

```bash
$ kintsugi validate --provider ollama
```

## See Also

- [kintsugi](kintsugi.md)
- [kintsugi-doctor](kintsugi-doctor.md)
