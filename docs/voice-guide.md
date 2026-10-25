# Voice guide

The harness speaks like a senior practitioner: terse, warm about craft, allergic
to hype. This is not a style preference — the audience is doing security work
where a tool that oversells itself is a tool you cannot trust.

The rules are enforced by `internal/tui/voice_test.go`, which parses the real
source with `go/ast` and checks every string literal. It cannot drift, because it
reads what is actually there.

## 1. No hype words

Banned: `amazing`, `incredible`, `powerful`, `seamless`, `effortless`,
`revolutionary`, `AI-powered`, `supercharge`, `simply`, `just works`,
`best-in-class`, `cutting-edge`, `game-changing`, `blazing`.

| Bad | Good |
|---|---|
| `✗ simply run kintsugi setup` | `· run kintsugi setup` |
| `powerful context engine` | `context stack · 3 layers · prefix stable` |
| `seamlessly switches models` | `model switched · persona recompiled` |

## 2. No exclamation marks

A terminal that shouts is a terminal nobody reads twice.

| Bad | Good |
|---|---|
| `✓ Setting applied!` | `✓ setting applied` |
| `✗ That did not work!` | `✗ that did not work · the file is missing` |

Shell negations (`!=`) and format verbs are unaffected; only a bang at the end
of a word is caught.

## 3. Every error names a next action

The form is `✗ <what broke> · <what to do>`. An error that only reports is
incomplete: the operator already knows something went wrong.

| Bad | Good |
|---|---|
| `✗ recompile failed` | `✗ recompile: persona pack not found · run kintsugi setup` |
| `✗ no workspace` | `✗ no workspace bound · start kintsugi from the workspace, or set KINTSUGI_WORKSPACE` |
| `✗ review failed` | `✗ reviewer: no model bound · bind one with /model` |

## 4. Glyphs, never emoji

The allowed set is the brand's: `✓ ✗ ✸ ⚠ ◇ ✦ ▸ ◉ · ─ │ ┌ ┐ └ ┘ █ ▓ ▒ ░ ▀ ▄ ▸
▲ ◈ → ↑ ↓`. Emoji are banned outright — they render inconsistently, break the
mono path, and are not part of the identity.

The guard test targets the pictographic blocks (`U+1F000–U+1FAFF`) and the
variation selector, which is where a pasted emoji always lands. It deliberately
does **not** flag `✓ ✗ ✸ ⚠ →`, which are required.

## 5. State, not adjectives

Say what is true and let the operator judge. Prefer a number, a path, or a
verdict over a description of one.

| Bad | Good |
|---|---|
| `an intelligent agent is carefully analysing` | `▸ working · 3.2k tok · cache 91%` |
| `excellent context coverage` | `◈ context 6 layers · prefix stable` |
| `no issues found, all clear` | `verdict pass · 0 findings` |

## Running the guard

```sh
go test ./internal/tui/ -run 'TestNoHype|TestNoExclamation|TestNoEmoji|TestErrorsName'
```

`TestTheScannerActuallyReadsSource` exists so the guard cannot pass vacuously: if
the source scan ever stops seeing the package, that test fails rather than
letting the four checks above succeed against an empty list.