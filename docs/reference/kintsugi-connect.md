# kintsugi-connect

Verify and store provider API keys.

## Synopsis

```bash
kintsugi connect --list
kintsugi connect <provider> [--no-verify] [--key-stdin] [--delete]
```

## Description

Verify and store provider API keys securely. Keys are stored with 0600 permissions in the credential store. Environment variables take precedence over stored keys.

## Options

- `--list` - List configured providers and key status
- `--no-verify` - Save without pinging the provider first
- `--key-stdin` - Read the key from one line on stdin
- `--delete` - Remove the stored key for the provider

## Examples

List all providers:

```bash
$ kintsugi connect --list
```

Add a key interactively:

```bash
$ kintsugi connect ollama
```

Add a key from stdin:

```bash
$ echo "sk-..." | kintsugi connect openai --key-stdin
```

Skip verification:

```bash
$ kintsugi connect custom-provider --no-verify
```

Remove a stored key:

```bash
$ kintsugi connect ollama --delete
```

## See Also

- [kintsugi](kintsugi.md)
- [kintsugi-models](kintsugi-models.md)
