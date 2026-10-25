# Threat model

This document states what kintsugi protects against, what it does not, and
where the residual risks are. It is written for the security professional
who asks these questions before trusting a tool with their code and
credentials.

## Trust boundaries

```
┌─────────────────────────────────────────────────────────────┐
│  Operator (sole authority, all decisions)                    │
├─────────────────────────────────────────────────────────────┤
│  kintsugi process (local, no network except model requests) │
│    ├── tool executor (permission matrix, scope, sandbox)     │
│    ├── context system (CONTEXT.md, ledger, AGENTS.md)       │
│    └── session store (SQLite/WAL, local files)              │
├─────────────────────────────────────────────────────────────┤
│  Provider endpoint (network, trust boundary)                │
├─────────────────────────────────────────────────────────────┤
│  OS (kernel, filesystem, process isolation)                 │
└─────────────────────────────────────────────────────────────┘
```

## What kintsugi protects against

### 1. Unintended tool execution
**Mechanism:** permission matrix (deny → ask → allow), per tool and per
spec (path globs, command prefixes/regexes). Modes (default / accept-edits
/ plan / yolo) move the baseline; explicit deny rules win in every mode.
Plan mode forces read-only. Hooks may tighten, never widen.

**Stops:** a model that tries to run a destructive command, write outside
the workspace, or access a denied path. The operator is asked before any
mutating action unless they explicitly chose yolo.

**Does not stop:** a model that the operator has authorized to run. The
matrix is a baseline, not a sandbox — yolo mode approves everything.

### 2. Scope violations
**Mechanism:** machine-readable `[scope]` rules (paths, commands, hosts)
refused at the tool layer before the operator is asked. The same rules
render into the prompt; a test proves the prompt and the enforcer agree.

**Stops:** a model that tries to operate outside the declared engagement
scope. Out-of-scope paths, commands, and hosts never reach the operator.

**Does not stop:** a scope that is misconfigured or empty (no scope
declared means nothing is narrowed). Scope is a deny list, not a prompt.

### 3. Unsandboxed command execution
**Mechanism:** bash runs under bubblewrap (`off|readonly|workspace`,
network `none|loopback|allowlist`). An unavailable sandbox refuses the
command rather than running it unsandboxed.

**Stops:** a command that escapes the workspace, accesses the network, or
reads the host filesystem, when the sandbox is configured.

**Does not stop:** commands the operator explicitly approves. The sandbox
is opt-in (default off). It is Linux-only (bubblewrap); there is no
Landlock fallback. macOS has no backend.

### 4. Credential leakage
**Mechanism:** keys live in env vars (precedence) or a 0600 store inside a
0700 directory. They are never in config, prompts, logs, or error
messages. Redaction runs on tool output before persistence.

**Stops:** accidental credential exposure in session logs, transcript
blocks, or tool results. Redaction covers OpenAI-style keys, GitHub
tokens, AWS keys, JWTs, and `.env`-style values.

**Does not stop:** a model that exfiltrates a key through a tool the
operator approved (e.g., a bash command that echoes it to a network
endpoint the operator allowed). The sandbox's network mode is the
control here.

### 5. Unauthorized egress
**Mechanism:** offline mode (the default) refuses any non-loopback
endpoint at the tool layer. The egress guard is a code path, not a
setting.

**Stops:** a model that tries to fetch from a non-loopback host when the
mode is offline. The fetch layer checks scope and the sandbox allowlist.

**Does not stop:** a model that runs a bash command that makes its own
network calls (the sandbox's network mode controls this, not the fetch
layer). In `none` network mode, bubblewrap blocks the namespace.

### 6. Refusal contamination of memory
**Mechanism:** there is no code path that writes refusal text to the
ledger, learned memory, or skills. The drift guard receives a task hash
and a class label, never the model's output.

**Stops:** a refusal from one turn priming the next session to refuse.
The refusal is telemetry (counters only) and never becomes context.

**Does not stop:** a refusal that the operator deliberately exports via
`kintsugi refusal export` — that is operator-initiated and clearly
labeled.

### 7. Prompt cache poisoning
**Mechanism:** the prompt compiler enforces cache stability: everything
above the task layer stays byte-identical turn-to-turn. A recompile that
changes the prefix is reported, not silently applied.

**Stops:** an accidental config or file change invalidating the cached
prefix without the operator knowing.

**Does not stop:** a deliberate change (model, persona, KB) that
invalidates the prefix — that is by design and is reported.

## What kintsugi does NOT protect against

1. **A binary the operator runs.** The sandbox isolates bash commands, but
   a model that writes a malicious script and the operator runs it
   directly is outside the sandbox. The approval gate is the control
   here, and it is operator-driven.

2. **A provider server.** The model runs on the provider's infrastructure.
   kintsugi cannot control what the provider logs, retains, or does with
   the request. Offline mode and local runners are the mitigations.

3. **A model that lies in its thinking.** The thinking channel is
   surfaced and scanned, but a model that produces benign thinking and a
   malicious answer (or vice versa) is not caught by the lexicon. The
   operator must read the output.

4. **Social engineering of the operator.** The permission matrix asks the
   operator; if the operator approves a destructive action, the matrix
   permits it. Yolo mode makes this explicit.

5. **Physical access to the machine.** All state is local files. A person
   with filesystem access has the sessions, memory, ledger, and credentials
   (which are encrypted only by the 0600 mode, not by cryptography).

## Residual risks

- **The refusal lexicon is a regex denylist.** It catches what it has
  seen. A model that finds a new phrasing for refusal is not intercepted.
  Mitigation: the operator can extend it (`kintsugi refusal add`), and the
  corpus loop proposes patterns from collected refusals.
- **The sandbox is opt-in.** Default is off. An operator who does not
  configure it runs commands unsandboxed.
- **Subagent workers inherit the parent's permissions.** A delegated task
  runs with the same tool policy as the main loop. The task's tool
  allowlist narrows what the model *may* call, but the permission matrix
  is the same.
- **MCP servers are external processes.** A malicious MCP server has the
  same access as the tools it exposes. The operator controls which servers
  are configured.
- **The audit log is append-only but not tamper-proof.** A process with
  filesystem access can truncate or modify it. It is a record, not a
  cryptographic proof.

## What is out of scope

- Authentication or authorization of operators (single-operator design).
- Network-level controls beyond the sandbox and the egress guard.
- Cryptographic protection of local files.
- Multi-tenant isolation.