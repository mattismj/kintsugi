# {{PROJECT}}

Blueprint: `kintsugi new redteam` — an offensive-security tooling workspace:
scanners, fingerprinters, fuzzers, and exploit development against lab
targets this repository defines.

## What this blueprint scaffolds

- `CONTEXT.md` — the project memory the harness injects on every turn. The
  Authorization & scope section is a placeholder *you* fill in; Domain context
  is pre-drafted for this blueprint, and the rest is starter text you are
  expected to edit.
- `kintsugi.toml` — the workspace profile: `[workspace] profile = "lab"`,
  `[core] mode = "offline"` (loopback only), a commented `default_model`, and a
  permission policy that denies secret files and asks before anything unusual.
- `fixtures/README.md` — the conventions for the lab targets your tooling is
  developed and tested against.
- `lab/docker-compose.yml` — the disposable lab: two intentionally vulnerable
  public lab services, egress-free, so `kintsugi lab up` works the moment you
  scaffold.
- Nothing else. There is no scanner skeleton and no exploit skeleton: the
  first design decision (what you are building and against what) is yours.

## The layout to grow into

- `cmd/` — one directory per tool, one `main.go` each.
- `internal/` — shared scanning/fingerprinting/exploitation libraries.
- `fixtures/` — lab targets and recorded responses (see its README).
- `docs/` — each tool's coverage and reliability notes.

## The lab-only rule

Every tool developed here targets `lab/` and `fixtures/` — owned, synthetic,
disposable. The boundary is in the scaffold so the model holds it from the
first turn. Tooling is dual-use and neutral; the scope is what keeps it
professional.
