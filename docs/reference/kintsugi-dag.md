# kintsugi-dag

Run a node plan.

## Synopsis

```bash
kintsugi dag run <plan.yaml> [--json] [--max-revisions N]
```

## Description

Run a node plan with gates and revise cycles. DAG plans define a graph of tasks with dependencies.

## Options

- `--json` - Print the report as JSON
- `--max-revisions` - Blocking-gate revise cycles per node (default 1)

## Examples

Run a plan:

```bash
$ kintsugi dag run plan.yaml
```

JSON output:

```bash
$ kintsugi dag run plan.yaml --json
```

Allow more revisions:

```bash
$ kintsugi dag run plan.yaml --max-revisions 3
```

## See Also

- [kintsugi](kintsugi.md)
- [kintsugi-gate](kintsugi-gate.md)
