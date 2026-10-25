# TUI slash commands

Every command the interactive transcript accepts, grouped the way `/help`
groups them. A test derives the list from the dispatch switch, so a new
command cannot ship undocumented.

## Session

| Command | What it does |
|---|---|
| `/help [group]` | the command groups, or one group in detail |
| `/status` | session facts with the derived next action |
| `/clear` | drop the transcript blocks (session state survives) |
| `/quit` | exit |

## Context

| Command | What it does |
|---|---|
| `/context [edit\|diff\|refresh]` | the CONTEXT.md workbench (section table, tokens, gates) |
| `/frame` | inject the Authorization & Domain frame for the next turn |
| `/recall <query>` | search the context ledger for prior accepted work |
| `/ledger` | the accepted-work ledger |
| `/accept <id>` | accept a ledger proposal |
| `/guard` | the continuity drift guard's state |
| `/persona [edit\|base\|refresh\|use\|auto]` | the persona workbench |
| `/modelctx` | the per-model AGENTS.md surface (view, edit, create, force) |
| `/agents` | the AGENTS.md tiers and the markdown agent roster |
| `/refusal` | the refusal lexicon workbench (list, test, teach, edit) |

## Models

| Command | What it does |
|---|---|
| `/model [provider/model]` | the model picker: live lists, filter, switch; or a typed ref |
| `/role <name>` | switch the session's model role |
| `/connect [provider]` | the provider sign-in modal (key stored 0600) |

## Permissions

| Command | What it does |
|---|---|
| `/mode [name]` | the permission mode (default / accept-edits / plan / yolo) |

## Review

| Command | What it does |
|---|---|
| `/plan [name\|review]` | the plan workbench (read, comment, review rounds, verdict) |
| `/changes` | every file the agent touched this session, with diffs |
| `/rewind [id]` | list or restore a checkpoint |

## Other

| Command | What it does |
|---|---|
| `/commands` | the workspace's custom commands |
| `/skills` | the installed skills |
| `/kb [query]` | the configured knowledge bases, or a BM25 search |
| `/memory [query]` | the recorded lessons and skills |
| `/remember <lesson>` | append a lesson to learned.md |
| `/prompt` | the compiled prompt's manifest |
| `/scope` | the declared scope boundary the tool layer enforces |
| `/panel <pillar>` | expand one pillar (context / scope / refusal / privacy) |
