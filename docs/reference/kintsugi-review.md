# kintsugi-review

Read-only code review of a diff.

## Synopsis

```bash
kintsugi review [ref] [--json] [--ship-check]
```

## Description

Read the change, ask the gate model what is wrong with it, and print findings by
severity with a ship/no-ship verdict.

With no `ref` the working tree is judged; with one, the diff against that ref is
(a branch name or a commit both work).

The reviewer is read-only by construction: it is handed a diff, it returns
findings, and it holds no tool access. Nothing here writes, stages or commits.

Findings are graded P0-P3. A P0 or P1 drives a no-ship verdict:

- **P0** - security vulnerabilities, data loss, crashes, correctness bugs
- **P1** - performance issues, resource leaks, race conditions
- **P2** - code quality, maintainability, missing error handling
- **P3** - style, naming, documentation

## Options

- **--json** - Print the typed review as JSON
- **--ship-check** - Exit non-zero when the verdict is no-ship, for use in a script

## Examples

Review the working tree:

```bash
$ kintsugi review
```

Review a branch:

```bash
$ kintsugi review main
```

Fail a script on a no-ship verdict:

```bash
$ kintsugi review --ship-check
```

## See Also

- [kintsugi](kintsugi.md)
- [kintsugi-gate](kintsugi-gate.md)
- [kintsugi-commit](kintsugi-commit.md)
