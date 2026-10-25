# kintsugi-new

Scaffold a project from a blueprint.

## Synopsis

```bash
kintsugi new [<blueprint>] <dir> [--force]
```

## Description

Scaffold a new project from a blueprint. Blueprints provide templates for different project types.

## Options

- `--force` - Overwrite existing directory

## Examples

Create a new project:

```bash
$ kintsugi new myproject
```

Use a specific blueprint:

```bash
$ kintsugi new c2 myproject
$ kintsugi new detection my-detector
$ kintsugi new redteam my-tooling
```

Force overwrite:

```bash
$ kintsugi new --force myproject
```

## See Also

- [kintsugi](kintsugi.md)
- [kintsugi-init](kintsugi-init.md)
