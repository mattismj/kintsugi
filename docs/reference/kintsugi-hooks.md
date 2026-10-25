# kintsugi-hooks

Manage workspace hooks.

## Synopsis

```bash
kintsugi hooks trust
kintsugi hooks list
kintsugi hooks status
kintsugi hooks untrust
```

## Description

Manage workspace hooks. Hooks are scripts that run at specific points in the agent lifecycle. Project hooks must be explicitly trusted before they run.

## Commands

- **trust** - Allow this workspace's project hooks to run
- **list** - Show operator and project hooks with trust state
- **status** - Show whether this workspace is trusted
- **untrust** - Revoke this workspace's trust marker

## Examples

Check hook trust status:

```bash
$ kintsugi hooks status
```

List all hooks:

```bash
$ kintsugi hooks list
```

Trust project hooks:

```bash
$ kintsugi hooks trust
```

Revoke trust:

```bash
$ kintsugi hooks untrust
```

## See Also

- [kintsugi](kintsugi.md)
