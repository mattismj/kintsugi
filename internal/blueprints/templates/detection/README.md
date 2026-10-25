# {{PROJECT}}

Blueprint: `kintsugi new detection` — a detection engineering workspace for
Sigma rules, network signatures, and hunting queries, with the lab that
validates them.

## What this blueprint scaffolds

- `CONTEXT.md` — the project memory the harness injects on every turn. The
  Authorization & scope section is a placeholder *you* fill in; Domain context
  is pre-drafted for this blueprint, and the rest is starter text you are
  expected to edit.
- `kintsugi.toml` — the workspace profile: `[workspace] profile = "lab"`,
  `[core] mode = "offline"` (loopback only), a commented `default_model`, and a
  permission policy that denies secret files and asks before anything unusual.
- `fixtures/README.md` — the conventions for lab telemetry and event
  recordings: the synthetic event streams your detections are validated
  against.
- `lab/docker-compose.yml` — the disposable lab: one event-generator service
  and one receiver, egress-free, so `kintsugi lab up` works the moment you
  scaffold.
- Nothing else. There is no rules skeleton: the first design decision (which
  platform and telemetry you are targeting) is yours.

## The layout to grow into

- `rules/sigma/` — Sigma rules, one file per detection, headers carrying
  ATT&CK IDs and data source.
- `rules/suricata/` — network signatures, same header discipline.
- `hunts/` — hunting queries with the question they answer and the
  telemetry they need.
- `tests/` — the validation runner: replay fixtures, assert fire/no-fire.
- `fixtures/` — synthetic event recordings (see its README for the rules).

## The lab-only rule

Detections are developed and validated entirely against `lab/` and
`fixtures/`. Production telemetry never enters this workspace, and nothing
here deploys itself. The boundary is in the scaffold so the model holds it
from the first turn.
