# kintsugi-mods

Mod manifests.

## Synopsis

```bash
kintsugi mods list
kintsugi mods trust
kintsugi mods untrust
kintsugi mods call <mod> <tool> <json>
```

## Description

Manage mod manifests. Mods are external tools that extend kintsugi's capabilities. Project mods must be explicitly trusted before they run.

## Commands

- **list** - Show mod manifests with trust state
- **trust** - Allow this workspace's project mods to run
- **untrust** - Revoke this workspace's trust marker
- **call** - Start one mod and call one of its tools

## Examples

List mods:

```bash
$ kintsugi mods list
```

Trust project mods:

```bash
$ kintsugi mods trust
```

Call a mod tool:

```bash
$ kintsugi mods call mymod mytool '{"arg": "value"}'
```

## See Also

- [kintsugi](kintsugi.md)
- [kintsugi-hooks](kintsugi-hooks.md)
