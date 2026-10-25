# kintsugi

Privacy-first agent harness for open-weights models.

## Synopsis

```bash
kintsugi [command] [flags]
```

## Description

Kintsugi is a local-first agent harness that runs open-weights models against your codebase. It keeps your data on disk, refuses to upload telemetry, and gives you explicit control over every tool the agent can call.

Run `kintsugi` with no arguments to start the interactive TUI session.

## Commands

- **tui** - Interactive session (default)
- **run** - Run one task to completion
- **plan** - Read-only planning turn
- **hooks** - Manage workspace hooks
- **audit** - View permission decisions
- **prompt** - Inspect the compiled prompt
- **models** - List providers and models
- **pack** - Persona pack management
- **skills** - Workspace skills
- **context** - Manage CONTEXT.md
- **ledger** - Accepted work ledger
- **taste** - Operator taste preferences
- **recall** - Search prior accepted work
- **persona** - Persona triggers
- **memory** - Lessons and skills search
- **session** - Session management
- **doctor** - Environment readiness check
- **repair** - Tool repair statistics
- **refusal** - Refusal telemetry
- **gate** - Opsec review gate
- **dag** - Run a node plan
- **rules** - Rule file management
- **quota** - Token usage vs limits
- **report** - Findings report
- **init** - Scaffold a workspace
- **connect** - Verify and store API keys
- **setup** - Install persona pack
- **new** - Scaffold a project
- **validate** - Provider validation
- **checkpoint** - Session snapshots
- **rewind** - Rewind to a checkpoint
- **kb** - Knowledge base management
- **lab** - Compose lab management
- **mods** - Mod manifests
- **docs** - Show man pages
- **version** - Print version
- **help** - Show usage

## Options

- `--no-alt-screen` - Keep output in scrollback
- `--no-mouse` - Disable mouse-wheel scrolling
- `--plain` - Force line-based REPL

## Environment

- `KINTSUGI_MODEL` - Override the default model
- `KINTSUGI_MODE` - Override the core mode (online/offline)
- `KINTSUGI_CONFIG` - Path to config file
- `KINTSUGI_WORKSPACE` - Override the workspace directory

## Examples

Start an interactive session:

```bash
$ kintsugi
```

Run a task headlessly:

```bash
$ kintsugi run "add error handling to main.go"
```

Check environment readiness:

```bash
$ kintsugi doctor
```

List configured models:

```bash
$ kintsugi models
```

## See Also

- [kintsugi-run](kintsugi-run.md)
- [kintsugi-plan](kintsugi-plan.md)
- [kintsugi-connect](kintsugi-connect.md)
- [kintsugi-doctor](kintsugi-doctor.md)
