# kintsugi-context

Manage CONTEXT.md.

## Synopsis

```bash
kintsugi context init [--force] [--template general|redteam|c2|detection|webapp|research]
kintsugi context show
kintsugi context why "<question>"
kintsugi context diff
kintsugi context apply [--all] [--gated]
kintsugi context curate
kintsugi context refresh [--apply]
kintsugi context attest
```

## Description

Manage CONTEXT.md, the operator-authored document that defines authorization, domain, and scope. It is injected on every turn.

## Commands

- **init** - Create CONTEXT.md from template
- **show** - Show sections, hashes, gated markers
- **why** - Which injected context line says this
- **diff** - Review curator proposals
- **apply** - Apply auto-accepted proposals
- **curate** - Run the curator over a session
- **refresh** - Rebuild Current state from signals
- **attest** - Record operator review of Authorization & scope

## Options

- `--force` - Overwrite existing file (init)
- `--all` - Apply every non-gated proposal
- `--gated` - Include authorization/scope proposals
- `--apply` - Write changes (refresh)

## Examples

Initialize CONTEXT.md. On a TTY the authorization template is picked from a
menu; `--template` takes the choice directly (non-interactive defaults to
general). Every template ships a worked Authorization & scope example marked
for replacement with the operator's own words:

```bash
$ kintsugi context init
authorization template — what kind of work is this workspace for?
  1. general    dual-use tooling / red-team R&D (the default)
  2. redteam    offensive security tooling against lab targets
  ...
pick [1]: 2

$ kintsugi context init --template c2 --force
```

Show context structure:

```bash
$ kintsugi context show
```

Find context for a question:

```bash
$ kintsugi context why "what databases can I access?"
```

Review curator proposals:

```bash
$ kintsugi context diff
```

Apply proposals:

```bash
$ kintsugi context apply --all
```

Attest authorization:

```bash
$ kintsugi context attest
```

## See Also

- [kintsugi](kintsugi.md)
