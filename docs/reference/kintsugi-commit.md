# kintsugi-commit

Split the working tree into atomic commits.

## Synopsis

```bash
kintsugi commit [--apply] [--json] [--yes]
```

## Description

Read the working tree, split unrelated changes into atomic commit groups ordered
by dependency (source before the tests and docs that describe it), and generate a
message per group from the diff.

Dry-run is the default and needs no trust: nothing is staged, nothing is
committed, and git is not written to. `--apply` is the deliberate step; it asks
for confirmation first. Nothing here ever pushes — publishing stays the
operator's own decision.

## Options

- **--apply** - Run git commit per group after confirmation
- **--json** - Print the plan as JSON instead of human-readable text
- **--yes** - Skip the confirmation prompt (only meaningful with `--apply`)

## Examples

Preview the commit groups:

```bash
$ kintsugi commit
```

Commit them:

```bash
$ kintsugi commit --apply
```

## See Also

- [kintsugi](kintsugi.md)
- [kintsugi-review](kintsugi-review.md)
