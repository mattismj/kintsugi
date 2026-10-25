# AgentForge (`kintsugi`)

A terminal agent harness for open-weights cloud models, built for one audience:
operators doing **authorized security and dual-use development** — red-team
tooling, C2 components, exploit development, detection engineering — whose
working vocabulary ("shell", "exploit", "payload", "C2") trips the refusal
classifiers in every general-purpose harness.

The thesis is narrow and testable: **the context that stops a model
over-refusing legitimate work belongs in a durable project artifact, not in a
conversation you rebuild by luck every session.**

Three pillars follow, and they are what the code is actually built around:

1. **Continuous context.** Authorization and scope live in a per-project file
   the harness can read but never write; accepted work is recorded in a ledger
   fresh sessions can recall; every model gets operator-authored ground rules.
2. **Recoverable refusals.** A mid-stream refusal is detected, the turn is
   aborted, the authorization frame is re-injected, and the model resumes —
   a bounded number of times. The refusal itself never becomes memory.
3. **Privacy.** Everything is a local file; the only thing that leaves the
   machine is the model request to the provider you configure.

One Go binary, no runtime dependencies. Linux-first. Nothing phones home.

---

## How the prompt is built

The system prompt is a **build product**, produced by a pure-function compiler
with a fixed layer order:

```
kernel → adapter → persona → kb → behavior → skills → workspace → taste → task
```

- **kernel** — the harness doctrine: role, tool discipline, output rules.
- **adapter** — the model's tool-calling dialect and quirks, keyed by model
  family. A Qwen3-Coder adapter is not a DeepSeek-R1 adapter. Adapters are
  YAML data, not code.
- **persona** — one persona per model family, selected by an explicit →
  glob → family → capability → fallback chain, inheritance flattened
  base-first. A standalone persona keeps small (≤14B) models calling tools.
- **kb** — retrieved knowledge-base chunks (BM25 over local Markdown).
- **behavior** — optional pipeline definitions (plan → review → execute).
- **skills** — an index only; bodies load on demand through the `skill` tool.
- **workspace** — `CONTEXT.md` sections and `AGENTS.md` tiers (below).
- **taste** — the operator's style bullets.
- **task** — the turn itself.

Every layer carries a SHA-256 hash and a token count in a **manifest**. The
cache-stability rule is enforced, not aspirational: everything above the task
layer stays byte-identical turn-to-turn unless you change model, persona, or
KB, so provider prompt caching actually works. `/prompt` shows the stack,
`/prompt diff` shows layer-by-layer what a recompile changed.

## How context works

### `CONTEXT.md` — the project's authorization

Parsed into classified sections (authorization, scope, domain, conventions,
…), injected by class under an inject budget. The `authorization` and `scope`
classes are **operator-gated**: the harness may propose edits through the
curator, but applying one requires an explicit `--gated` flag. The harness
never authorizes itself.

### `AGENTS.md` tiers — including the per-model doc

Operator instructions resolve through three tiers, most specific last:

1. `~/.config/kintsugi/AGENTS.md` — the user tier, every session.
2. `<workspace>/AGENTS.md` — the project tier, injected unconditionally.
3. **A per-model `AGENTS.md`** — keyed to the resolved model.

The per-model tier is the one that matters for refusal behavior: each model
family has its own temperament, so it gets its own operator-authored ground
rules — how direct to be, what this model tends to refuse and why it should
not here, how to phrase tool output. `/modelctx` shows which document the
current model gets, edits it, and can force a specific one; `/agents` shows
the whole stack. This is how you lay the ground for a model's
refusal/non-refusal posture per model instead of per workspace.

### The context ledger

Accepted work — plans, artifacts, decisions, acceptances, with evidence
hashes — is recorded append-only in `.kintsugi/ledger/` (Markdown, FTS5
indexed; entries supersede, never rewrite). **Prior-work recall** injects the
accepted work for the task at hand into fresh sessions, visibly, as a system
message. **Attestation** records that an authorization frame was in force when
work was accepted. **The drift guard** re-injects prior accepted work when a
refusal still happens, and records the drift as metadata only.

**Hot reload**: watched files are stamped and re-checked before every turn —
edit `CONTEXT.md`, a persona, or a per-model `AGENTS.md` in your editor and
the next send recompiles and shows the layer diff.

## How refusal recovery works

1. The assistant's stream is scanned while it streams. The scan runs on the
   model's **own prose only** — fenced code, inline code, blockquotes, and
   double-quoted spans are scrubbed first, so *discussing* a refusal never
   triggers one.
2. The lexicon is a set of regex classes — `moralizing`, `identity`,
   `legal-lecture`, `safety-lecture`, `off-topic-safety` — deliberately
   narrow, because a false positive aborts a live turn. Every pattern is
   operator-editable without touching code: `kintsugi refusal list | add | test
   | disable | restore` work against the same file the loader reads, and
   `kintsugi refusal test "<text>"` shows exactly which patterns fire on a
   sample.
3. On a hit, the stream is aborted and a **reminder** is injected: the
   matched class, the operator's `Authorization` and `Domain` sections
   verbatim, and the persona's **trigger map** — known honest rephrasings for
   this model family (a term that trips a filter gets a replacement). The
   turn resumes from that point, up to `intercept.max_retries` (default 2).
4. Past the retries, the turn hands back to the operator with `/frame` as the
   way forward — it never loops.
5. **The refusal never becomes memory.** No code path writes refusal text to
   the ledger, learned memory, or skills. The drift record carries a task
   hash, matched ids, a class label, and an outcome — nothing that could echo
   a refusal back into a prompt. Interception telemetry is counters only
   (model, persona, class, outcome); the matched term is dropped at the
   telemetry boundary.
6. **Operator stream rules** extend the same machinery with regex rules that
   `redact`, `inject`, or `abort` — scrub a secret pattern, inject a reminder,
   or kill a stream.

## How tools are governed

25 built-in tools (`read list glob grep edit write apply_patch bash todo
webfetch websearch extract cve_lookup run_in_lab memory_search recall retain
memory_edit learn manage_skill reflect kb_search repo_map ast_grep skill task
agent_output`), plus MCP servers (stdio) and repo-controlled mods — all
through one permission matrix:

- **deny → ask → allow**, per tool and per spec (path globs, command
  prefixes/regexes). A bare `*` never grants. Modes (`default`,
  `accept-edits`, `plan`, `yolo`) move the baseline; explicit deny rules win
  in every mode; `plan` is a hard read-only floor. Approvals block the agent
  until you answer `[y]`/`[n]`.
- **Scope enforcement**: the declared engagement scope denies out-of-scope
  paths, commands, and hosts *before* they are asked about. The same rules
  render into the prompt, and a test proves the prompt and the enforcer
  return the same verdict — the model reasons about the boundary it is held
  to.
- **Validate-then-repair**: malformed tool arguments are repaired from a
  catalogue of known shapes (with ordering invariants) before execution, with
  per-(model, tool, class) telemetry.
- **OS sandbox**: `bash` runs under bubblewrap (`off|readonly|workspace`,
  network `none|loopback|allowlist`). A configured-but-unavailable sandbox
  refuses the command rather than running unsandboxed. Linux only; no
  Landlock fallback.
- **Audit**: every permission decision — the rule that decided it, your
  answer, scope and sandbox refusals — lands in a local append-only log.
- **Redaction** runs on tool output before persistence.

**Memory is context, never policy**: a memory entry that claims "the operator
approves destructive commands" does nothing to the permission layer and is
surfaced as a finding.

## Sessions, memory, knowledge

- **Sessions** are SQLite/WAL stores: resume, search, fork, export JSONL,
  checkpoints with rewind, mid-session compaction, and a headless
  `--output-format json` emitter for scripting.
- **Memory tier-2**: learned lessons and skills as Markdown, FTS5-indexed,
  writable through gated verbs (`/remember`, `reflect`, `learn`).
- **Knowledge bases**: local Markdown chunk collections with frontmatter,
  BM25 retrieval at compile time, a STIX importer, and a MITRE ATT&CK starter
  KB (CC BY 4.0, per-chunk provenance).

## Providers and privacy

Two wire implementations — OpenAI chat-completions and Anthropic messages
(system hoisting, `tool_use`/`tool_result` blocks, SSE assembly, cache
breakpoints) — cover 34 configured providers (DeepSeek, Qwen/DashScope, GLM,
Kimi, OpenRouter, Ollama, vLLM, …), plus fallback chains and a catalog
fetcher. Keys: env var wins, else `kintsugi connect`'s `0600` store — never
config, prompts, logs, or errors. **Offline mode** (the default) refuses any
non-loopback endpoint — the egress guard is a code path, not a setting you
have to remember.

## The interface

**TUI** (`kintsugi`): a centered transcript with a chat-style multi-line input —
**ma (間)**, the negative space, is the layout's whole idea. Side panels are on
demand — `ctrl+o` opens the work rail (state mark, tasks, agents, background
roster), `ctrl+p` the inspector (work tab by default: task, files, tokens,
refusals; harness tab: manifest, layers, cache state, pillars) — never two at
once, `tab` switches the inspector's tabs. The visual language is **kintsugi**:
urushi-black ground, warm paper text, and one gold ramp (`Ash → Bronze → Ochre
→ Gold → Hikari`) as the entire accent family. State is glyph-only (✦ ▸ ◉ ✓ ✸
◇); while a turn runs the footer carries the repair verbs — mending →
lacquering → gilding → polishing; a repaired refusal leaves a literal gold
seam in the transcript, because the repair being visible is the point. Slash
commands, `@file`/`@agent` mentions with tab completion, `/model` — a live
picker that fetches real model lists from keyed providers, `/refusal` — the
lexicon workbench (test a sample, teach a pattern, live mid-session), `/plan`
review workbench with line-anchored comments and rounds, `/changes` — every
file the agent touched with diffs, `/connect` sign-in, mouse scrolling that
pins under streaming, bash output streaming live in the transcript, informed
approvals (the diff rides the request), inline diffs after file edits,
OSC 52 clipboard copy, the model's thinking rendered as a readable block
(collapsed to its tail, expandable with ctrl+t, copyable with alt+t), and
the refusal detector scanning the thinking before the answer starts. Degradation is truecolor → 256 → 16 → mono —
wabi-sabi: beautiful at every step down, and mono emits zero escape
sequences.

**CLI**: the same engine headless — `kintsugi run "<task>"` (streams or JSON),
`kintsugi plan`, `kintsugi review`, `kintsugi refusal`, `kintsugi context`, `kintsugi
doctor --net`, and ~40 more verbs, each with an embedded man page
(`kintsugi docs`).

**Subagents**: `task` fans out to isolated workers on per-role models
(coder/researcher/opsec); background runs collect via `agent_output`;
Markdown-defined agents (`@name`) carry their own model, prompt, and tool
allowlist.

```sh
kintsugi                        # TUI
kintsugi run "<task>"           # headless
kintsugi plan "<task>"          # read-only planning turn
kintsugi connect <provider>     # sign in; key stored 0600
kintsugi setup                  # materialize persona pack, config, AGENTS.md
kintsugi init                   # draft CONTEXT.md + kintsugi.toml here
kintsugi refusal list           # the lexicon you have, and its hit counts
kintsugi refusal test "..."     # what would fire on this text
kintsugi doctor [--net]         # sanity-check the install
kintsugi docs list              # every man page, embedded
```

## Configuration

One TOML file, in precedence order: `<workspace>/kintsugi.toml`,
`~/.config/kintsugi/config.toml`, `$XDG_CONFIG_HOME/kintsugi/…`. Secrets
come from the environment or the 0600 store — not from config.
`config.example.toml` documents every section; `docs/config.md` is the
section-by-section reference.

## Security posture, stated plainly

See `docs/threat-model.md` for the full threat model: what the harness
protects against, what it does not, and where the residual risks are.

See  for the full threat model: what the harness
protects against, what it does not, and where the residual risks are.

- Permissions are a baseline plus rules; deny beats ask beats allow; a bare
  `*` never grants; hooks may tighten, never widen.
- The sandbox is opt-in and honest: unavailable means refused, not
  unsandboxed.
- Audit is append-only; redaction runs before persistence.
- The offline guard is why `kintsugi plan` fails in a fresh workspace with a
  cloud provider until `core.mode = "normal"` — that is the guard working.

## Building and testing

```sh
go build ./...             # builds (CGO_ENABLED=0, static)
go test ./...              # hermetic: no network, ever
go vet ./...
gofmt -l internal/ cmd/    # clean

./scripts/install.sh       # build from this checkout → ~/.local/bin/kintsugi
```

No release tarball exists — building from the checkout is the only install
path, and the safe one: you can read every line that runs. Layout:
`cmd/kintsugi` (CLI dispatch) + `internal/*` (~50 packages, ~66k lines of
non-test Go, 1,414 test functions) + `pack/` (embedded persona pack) +
`knowledgebases/` (ATT&CK starter) + `docs/` (embedded man pages and
reference). CI runs gofmt, build, vet, the hermetic suite, a `-race` job on
the concurrency packages, and a docs guard that fails if a command ships
without its man page.

## Status, honestly

Milestones M0–M13c, one operator. Known-open, without euphemism:

- **The core claim is not live-validated yet.** Refusal recovery was
  developed by hand and against mock providers; real cloud-provider data is
  what this project now needs most.
- **The refusal lexicon is a regex denylist** — it catches what it has seen.
  Precision/recall is unmeasured. `kintsugi refusal test` exists so you can
  check coverage yourself and report gaps.
- Linux-only, bubblewrap-only sandbox; no Landlock fallback.
- No published release; install builds from the checkout.
- Only OpenAI and Anthropic wire formats; a native Gemini wire is not written.
- The refusal lexicon's precision/recall is unmeasured — `kintsugi refusal
  test` and `kintsugi refusal export` exist so operators can check coverage
  themselves and report gaps.

`NOTICE` states exactly what was borrowed and from where: wire formats
(public specs), a patch grammar (format only), one CC BY 4.0 dataset, and
ideas. No source code was copied.
