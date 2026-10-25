# kintsugi-run

Run one task to completion.

## Synopsis

```bash
kintsugi run [flags] "<task>"
```

## Description

Run a single task to completion. The agent reads your codebase, makes changes, and reports what it did. Output streams to stdout by default.

## Options

- `--model` - Provider/model to use (e.g., ollama/llama3)
- `--persona` - Persona name override
- `--profile` - Behavior profile
- `--yolo` - Skip tool approvals (dangerous)
- `--frame` - Inject Authorization & Domain context
- `--role` - Model role: default|coder|researcher|opsec
- `--resume` - Session id to continue
- `--output-format` - Output format: text|json|stream-json
- `--architect` - Plan first on architect model, then edit
- `--architect-model` - Model for architect phase

## Examples

Run a simple task:

```bash
$ kintsugi run "add error handling to main.go"
```

Use a specific model:

```bash
$ kintsugi run --model ollama/llama3 "fix the bug in auth.go"
```

Plan then execute:

```bash
$ kintsugi run --architect "refactor the database layer"
```

Resume a session:

```bash
$ kintsugi run --resume abc123 "continue where we left off"
```

JSON output for scripting:

```bash
$ kintsugi run --output-format json "list all TODOs" | jq .
```

## See Also

- [kintsugi](kintsugi.md)
- [kintsugi-plan](kintsugi-plan.md)
