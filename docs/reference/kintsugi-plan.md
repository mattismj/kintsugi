# kintsugi-plan

Read-only planning turn.

## Synopsis

```bash
kintsugi plan "<task>"
kintsugi plan list
kintsugi plan show <name>
```

## Description

Run a read-only planning turn. The agent analyzes the task and writes a plan to `.kintsugi/plans/` without making any code changes.

## Options

- `--model` - Provider/model to use
- `--persona` - Persona name override

## Examples

Create a plan:

```bash
$ kintsugi plan "refactor the authentication module"
```

List saved plans:

```bash
$ kintsugi plan list
```

View a specific plan:

```bash
$ kintsugi plan show refactor-auth
```

## See Also

- [kintsugi](kintsugi.md)
- [kintsugi-run](kintsugi-run.md)
