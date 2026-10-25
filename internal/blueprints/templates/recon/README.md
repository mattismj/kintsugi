# {{PROJECT}}

Blueprint: `kintsugi new recon` — a lab-scoped discovery/scan utility with lab-safe
defaults.

## What this blueprint scaffolds

- `CONTEXT.md` — the project memory the harness injects on every turn. The
  Authorization & scope section is a placeholder *you* fill in; Domain context
  is pre-drafted for this blueprint, and Stack / Hard conventions / Current
  state / Lessons are starter text you are expected to edit.
- `kintsugi.toml` — the workspace profile: `[workspace] profile = "lab"`,
  `[core] mode = "offline"` (loopback only), a commented `default_model`, and a
  permission policy that denies secret files and asks before anything unusual.
- `fixtures/README.md` — the starting notes for the synthetic hosts an in-scope
  run is allowed to see.
- `lab/docker-compose.yml` — the disposable lab the harness drives: one
  synthetic, egress-free `fixture` service (no network, no published ports) so
  `kintsugi lab up` works the moment you scaffold. It is a stand-in for the host a
  scan should see, not a scanner — replace it with the fixture the scope file
  actually names.
- Nothing else. There is no scanner skeleton and no target list: the scope model
  comes first, because it is the part everything else depends on.

## The sandbox-only rule

This project is lab-scoped by construction:

- Targets come from the fixture scope file under `fixtures/`. If it is not
  listed there, it is out of scope and the tool skips it.
- The lab is egress-free by default: `lab/docker-compose.yml` gives its `fixture`
  service no network at all (`network_mode: none`) and publishes no ports. While
  the lab is up (`kintsugi lab up`) the harness refuses host commands, so a probe
  runs inside the fixture, never against an address on your machine.
- Defaults stay small: loopback and `10.11.x.x` lab ranges, low concurrency, a
  hard per-probe timeout, no stealth or rate-limit-evasion tuning.
- `[core] mode = "offline"` keeps egress to loopback; changing it is a
  deliberate, recorded operator decision, per workspace, never a default.
- Fixtures are synthetic: no executable malware, no real hosts, credentials,
  third-party accounts, or addresses.
- A scan of anything the fixture set does not define is out of bounds. If a task
  needs one, stop and hand it back to the operator.

## Finishing the Authorization & scope section

`CONTEXT.md` ships with this placeholder:

<!-- operator: complete this section; the harness never writes it -->

Replace the marker and its TODO bullet with your own words: what this project
is for, who is accountable for it, which fixtures or lab hosts are in scope, and
where it stops. The harness never writes this section, and `kintsugi context apply`
will not modify authorization or scope proposals (they stay gated even with
`--all`). Verify what the model will actually see with:

```
kintsugi context show        # sections, hashes, gated markers
kintsugi context why "scope" # prove which line the decision came from
```

## Suggested first steps

1. Finish the Authorization & scope section, then run `kintsugi context show`.
2. Write the fixture scope file: the synthetic hosts and ranges a run may touch,
   and the record shape the tool emits for each. That file is the contract.
3. Implement discovery against the fixtures with lab-safe defaults (sequential
   or low concurrency, per-probe timeout, explicit skip for out-of-scope).
4. Add what the blue team should see for each probe — the log line, the alert,
   or the deliberate absence of one — and tune detections against it.
5. Keep `Current state` current: what works, what is stubbed, what is next.
