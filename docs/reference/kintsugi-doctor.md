# kintsugi-doctor

Environment readiness check.

## Synopsis

```bash
kintsugi doctor [--net]
```

## Description

Check environment readiness. Verifies configuration, providers, models, and workspace state.

## Options

- `--net` - Probe the default provider endpoint

## Examples

Basic check:

```bash
$ kintsugi doctor
```

With network probe:

```bash
$ kintsugi doctor --net
```

## See Also

- [kintsugi](kintsugi.md)
- [kintsugi-connect](kintsugi-connect.md)
