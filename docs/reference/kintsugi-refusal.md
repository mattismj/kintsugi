# kintsugi-refusal

Refusal interception: telemetry and the lexicon that detects refusals.

## Synopsis

```bash
kintsugi refusal stats
kintsugi refusal trends
kintsugi refusal list
kintsugi refusal add <class> <regex>
kintsugi refusal test "<sample assistant text>"
kintsugi refusal disable <class>
kintsugi refusal restore <class>
```

## Description

`kintsugi refusal` is the operator-facing surface for refusal interception. Two halves of the same idea live here:

- **stats / trends** read the interception telemetry. Refusals are telemetry, never memory: only counters (model, persona, class, outcome) are kept, never the matched phrase or the refused prose.
- **list / add / test / disable / restore** edit the lexicon itself — the regex denylist the live scanner matches against. Every command works against the same file the loader reads, so a pattern added at the CLI is in force on the next turn with no restart.

The override file lives at `.kintsugi/refusal_patterns.toml` (or wherever `intercept.lexicon_path` points). Classes present in the file replace the built-in class wholesale; classes absent keep their defaults. An empty list disables a class.

## Commands

- **corpus add** — append refusal texts to the operator-local corpus (`.kintsugi/refusal-corpus/`, never uploaded): pasted directly or extracted from an export report with `--file`. Duplicates are skipped.
- **corpus stats** — the corpus size and the current lexicon's coverage over it (matched / missed).
- **corpus propose** — the growth loop: find the 3-word phrases that recur across missed texts (simple token-overlap clustering, no ML), propose escaped word-bounded patterns with their evidence, and with `--apply` write them through the lexicon validation path and report the new coverage.

- **stats** — Show refusal interception rates by model/persona
- **trends** — Show refusal and drift trends
- **list** — Print the effective lexicon: every class, every pattern, where each came from (default or operator), and the per-class hit count from telemetry
- **add <class> <regex>** — Append a regex to a class in the override file. The regex is compiled before it is written; a pattern that does not parse is refused. Creating a class that did not exist replaces the built-in class for it.
- **test "<sample>"** — Run a sample of assistant prose through the scanner the way the harness does (fenced code, inline code, blockquotes and double-quoted spans are scrubbed first). Prints every matching pattern with its class, the matched phrase, the pattern source and its origin.
- **disable <class>** — Empty a class in the override file, turning it off.
- **restore <class>** — Remove a class from the override file so the built-in class applies again.
- **export [session-id]** — One Markdown report per session: every refusal event with its class, outcome, the model's refusal text verbatim, the current lexicon's coverage verdict, and the exact `kintsugi refusal add` command that closes each gap. Operator-initiated only — the refusal text exists only in the report, never in the ledger, memory, or telemetry.

## The override file

```toml
# Operator override of the refusal lexicon.
# Classes present here replace the built-in class; classes absent keep defaults.
# An empty list disables a class. Run `kintsugi refusal test` to check coverage.

[classes]
moralizing = ["\\bI (?:can't|cannot) assist\\b"]
off-topic-safety = []
```

The built-in classes are `moralizing`, `identity`, `legal-lecture`, `safety-lecture`, `off-topic-safety`. They are deliberately narrow: a false positive interrupts a turn.

## Examples

See the effective lexicon and per-class hit counts:

```bash
$ kintsugi refusal list
```

Teach the lexicon a new refusal phrasing, then verify it fires:

```bash
$ kintsugi refusal add moralizing '\bI am not (?:able|willing) to (?:help|assist)\b'
$ kintsugi refusal test "I am not able to help with that."
```

Find a coverage gap (a sample the lexicon would have let through):

```bash
$ kintsugi refusal test "I'm not the right tool for this task."
no patterns matched
· the lexicon would not have intercepted this — if it is a refusal, add a pattern with `kintsugi refusal add <class> <regex>`
```

## See Also

- [kintsugi-persona](kintsugi-persona.md) — the trigger map that turns a detected refusal's term into an honest rephrasing
- [kintsugi](kintsugi.md)
