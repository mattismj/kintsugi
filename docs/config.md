# Configuration reference

One TOML file, merged in this order (later wins): built-in defaults
(`internal/config/config.go`) → `~/.config/kintsugi/config.toml` (or
`$XDG_CONFIG_HOME/kintsugi/config.toml`) → `<workspace>/kintsugi.toml` →
environment (`KINTSUGI_MODE`, `KINTSUGI_MODEL`, `KINTSUGI_CONFIG`).

**Secrets never live in this file.** A provider entry names an environment
variable (`api_key_env`); the key comes from the environment or from the 0600
credential store `kintsugi connect` writes. See "Provider sign-in" in the README.

`config.example.toml` is the annotated file this page explains, section by
section.

---

## `[core]`

- `mode = "offline" | "normal"` — **the most important knob.** `offline` (the
  built-in default) refuses every non-loopback provider endpoint at request
  time and limits `webfetch` to loopback; `normal` allows cloud endpoints.
- `default_model = "provider/model"` — model compiled when nothing else is
  specified.
- `catalog_path` — merged model catalog; empty means
  `~/.config/kintsugi/catalog.json`.

## `[persona_pack]`

- `dir` — pack directory (`~/.config/kintsugi/agents/global`).
- `fallback = "_base"` — persona used when nothing else matches.
- `match_order = ["explicit", "id_glob", "family", "capability", "fallback"]` —
  the selection chain, first match wins (see `docs/personas.md`).

## `[context]`

- `enabled` — inject the project context at all.
- `path = "CONTEXT.md"` — workspace-relative context file.
- `inject_budget_tokens` — hard cap on the injected context block.
- `curator_model = "provider/model"` — the model that proposes changes; empty
  disables curation.
- `curator_on_session_end` — run the curator after a session ends. Advisory: a
  curator failure never fails the run.
- `auto_accept_classes` — proposal classes written without review (`state`,
  `convention`, `lesson`).
- `gated_classes = ["authorization", "scope"]` — **never auto-written.** The
  model may not author these; only the operator does.

## `[behavior]`, `[advisor]`, `[agents]`, `[skills]`, `[gates]`, `[roles]`, `[quota.*]`, `[retention]`

Short sections, one line each.

| Section | Keys | What it does |
|---|---|---|
| `[behavior]` | `dir`, `default_profile` | Behavior-profile directory (default `~/.config/kintsugi/agents/behavior`) and the profile used when none is chosen (empty = none). |
| `[advisor]` | `enabled`, `model`, `max_tokens` | The during-the-work secondary reviewer. `enabled` unset means the workspace profile decides (lab = on, plain = off); `model` empty = the worker's model; `max_tokens` is an approximate session cap. |
| `[agents]` | `subagents`, `max_subagents`, `worktree_dir`, `timeout_s` | Subagent fan-out. `subagents` exposes the `task` tool (needs a git workspace); `worktree_dir` empty = system temp; `timeout_s` is per worker. |
| `[skills]` | `dir` | Progressive disclosure: only names/descriptions enter the prompt, bodies load through the `skill` tool. Default `.kintsugi/skills`. |
| `[gates]` | `model`, `blocking`, `max_revisions` | Opsec review of diffs. `blocking` makes a failed gate stop the DAG node; `max_revisions` is how many revise cycles a gate may run. |
| `[roles]` | `default`, `coder`, `researcher`, `opsec` | Model roles as `provider/model` refs; empty = `core.default_model`. |
| `[roles.personas]` | per role | Force a persona for a role (`coder = "qwen-coder"`); empty = the pack's normal matching. |
| `[quota.<provider>]` | `limit_tokens`, `reset`, `note`, `live` | The offline quota ledger. `reset` is `daily`/`weekly`/`monthly`/`none`; omit the provider for unlimited; `live` adds a best-effort probe that never invalidates the ledger. |
| `[retention]` | `max_sessions`, `max_age_days` | Keep the newest N sessions (0 = unlimited) and delete sessions older than N days (0 = keep forever). `kintsugi doctor --retention` reports, `--apply` deletes. |

## `[harness]`

- `tool_call_repair` — repair malformed tool calls instead of failing the turn.
- `tool_call_repair_max` — repair attempts per call.
- `one_tool_per_turn` — serialize tool calls (per-adapter override).
- `max_tool_loops` — ceiling on tool rounds in one turn.
- `request_timeout_s` — per-request timeout.

## `[budget]`

- `max_prompt_tokens` — total budget for the compiled layers (0–5).
- `task_reserve_tokens` — tokens reserved for the task and the conversation.

## `[providers.<name>]`

The registry is data: `internal/config/providers.json` ships 34 endpoints, and
a block here **replaces** the built-in entry for that provider. Any
OpenAI-compatible endpoint can be added the same way.

- `base_url` — endpoint root.
- `api_key_env` — the environment variable that holds the key. The credential
  store is keyed by the same name, so `kintsugi connect <provider>` satisfies it
  without anything being written here.
- `family` — the family personas match on (`qwen`, `deepseek`, `glm`, `kimi`…).
- `wire` — `openai-completions` or `anthropic-messages`.
- `offline` — treat this endpoint as local (never blocked by `core.mode`).
- `reasoning_effort` — e.g. `"none"` to disable thinking on local models.
- `models` — ids or tables (`id`, `family`, `wire`, `context`, `reasoning`,
  `capabilities`).

## `[permissions]`

**The tool-permission layer.** The model never changes it, and it is never
part of a prompt.

- `allow_paths` — workspace-relative globs that may be touched. **Omit the
  list to allow everything that is not denied.**
- `deny_paths` — globs refused outright. The built-in floor always applies
  (`.env`, `id_rsa`, `id_ed25519`, `*.pem`, `.git/**`), even with an empty
  config.
- `allow_commands` — command **prefixes** that run without asking. Empty means
  every `bash` call asks.
- `deny_commands` — regular expressions refused outright; a malformed rule
  fails closed.
- `yolo = false` — **no approvals at all.** Deliberate opt-in only: it is the
  one switch that removes the operator from the loop entirely.

## `[intercept]`

- `enabled` — detect a refusal mid-stream, inject the Authorization/Domain
  frame, resume. Disable for local abliterated models if you want raw streams.
- `max_retries` — how many times one turn may be resumed before the refusal is
  handed back to the operator.
- `lexicon_path` — TOML override of the built-in refusal lexicon.

## `[ui]`

- `texture` — halftone/scanline texture in empty regions (truecolor only).
- `banner` — the nameplate on session start.
- `alt_screen` — use the alternate screen; `false` keeps output in scrollback.
- `ornament` — the footer ornament.
- `plain` — force the no-TTY line REPL.
- `mouse = true` — mouse-wheel scrolling (capture is **scroll-only**;
  `kintsugi tui --no-mouse` turns it off for one run).
- `boot = true` — the skippable boot nameplate reveal (TTY + color only;
  `kintsugi tui --no-boot` skips it).

## `[mcp]`

Local stdio Model Context Protocol servers. Each tool is exposed as
`mcp__<server>__<tool>` and passes through the normal permission gate.

- `enabled` — master switch for the MCP bridge.
- `[mcp.servers.<name>]` — `command`, `args`, `env`, `cwd`, `timeout_s`. Keep
  secrets in the parent environment; do not put them in `env`.

## `[continuity]`

- `enabled` — the continuity layer at all.
- `prior_work_top_k` — entries recalled per task (keep it small; ≤5).
- `prior_work_budget_tokens` — hard cap on the rendered prior-work block.
- `preflight` — score the first task locally and inject the frame before a
  high-risk request.
- `guard_on_refusal` — re-inject prior accepted work when a turn drifts.
- `ledger = ".kintsugi/ledger"` — the ledger directory; `ledger.md` is the
  source of truth.

**A refusal is never memory.** These settings only ever move positive,
operator-accepted work; refusal text is telemetry and is never written to the
ledger, `learned.md`, or skills.

---

## Also supported (not in the example file)

- `[chains.<name>] providers = ["provider/model", ...]` — ordered fallback
  list; slot 0 is the primary, later slots are tried when a slot fails
  retryably (429, 5xx, quota wall).
- `[kb.<name>] path / strategy / top_k / must_include / roles` — binds a
  knowledge base to a role; `strategy` is `retrieval`, `always`, `on-demand`,
  or `summary`.
- `[workspace] profile` — the engagement profile (`lab` turns the advisor on
  by default). `network` is reserved: the effective network switch today is
  `core.mode`.

## Not here yet

- **Sandbox.** OS-level isolation (bubblewrap/landlock), scope enforcement and
  the append-only audit log are M11. Until then the boundary is `core.mode` +
  `[permissions]` + the tool gate.
- **Permissions v2.** The current matrix is the coarse `yolo` /
  `allow_commands` model; the per-tool `ask|allow|deny` matrix with rule
  grammar is M9.

## A locked-down starting point

```toml
[core]
mode = "offline"          # or "normal" with a stored key

[permissions]
yolo = false
deny_paths = ["**/.env", "**/.env.*", "**/id_rsa", "**/id_ed25519", "**/*.pem", "**/.git/**"]
deny_commands = ["rm\\s+-[a-zA-Z]*[rf][a-zA-Z]*\\s+/(\\s|$)", "\\bmkfs(\\.|\\s)", "\\bdd\\s+if="]
allow_commands = ["go test", "go build", "gofmt", "git status", "git diff", "git log"]

[context]
gated_classes = ["authorization", "scope"]

[continuity]
enabled = true
guard_on_refusal = true
```
