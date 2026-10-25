# {{PROJECT}}

Blueprint: `kintsugi new c2` — an adversary-simulation tasking client/server
harness, built for purple-team validation against synthetic lab fixtures.

## What this blueprint scaffolds

- `CONTEXT.md` — the project memory the harness injects on every turn. The
  Authorization & scope section is a placeholder *you* fill in; Domain context
  is pre-drafted for this blueprint, and Stack / Hard conventions / Current
  state / Lessons are starter text you are expected to edit.
- `kintsugi.toml` — the workspace profile: `[workspace] profile = "lab"`,
  `[core] mode = "offline"` (loopback only), a commented `default_model`, and a
  permission policy that denies secret files and asks before anything unusual.
- `fixtures/README.md` — the starting notes for the synthetic fixtures the
  client and server are allowed to talk to.
- `lab/docker-compose.yml` — the disposable lab the harness drives: one
  synthetic, egress-free `fixture` service (no network, no published ports) so
  `kintsugi lab up` works the moment you scaffold. It is a stand-in to run your
  tooling against, not a tasking server — replace it with the fixture the
  design actually needs.
- Nothing else. There is no server skeleton and no client skeleton: the first
  design decision (what tasking looks like) is yours.

## The sandbox-only rule

This project is lab-scoped by construction:

- Every listener binds loopback or a lab container address. Nothing points at a
  host that is not defined under `fixtures/`.
- The lab is egress-free by default: `lab/docker-compose.yml` gives its `fixture`
  service no network at all (`network_mode: none`) and publishes no ports. While
  the lab is up (`kintsugi lab up`) the harness refuses host commands, so work runs
  inside the fixture, never on your machine.
- `[core] mode = "offline"` keeps egress to loopback; changing it is a
  deliberate, recorded operator decision, per workspace, never a default.
- Fixtures are synthetic: no executable malware, no real malware samples, no
  weaponized payloads, no real hostnames, credentials, or third-party accounts.
- The client is a lab artifact for exercising detection, not a capability that
  gets deployed anywhere.

If a task would need to leave the sandbox, stop and hand it back to the operator.

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
2. Define the tasking model in `fixtures/` terms: check-in, task assignment,
   result report, and the timings you care about. Write it down before code.
3. Write down the detection side too — which events and log lines should fire
   when the pair runs, so "it works" means "the blue team can see it".
4. Build the server first (queue + check-in handler), then the client, keeping
   both loopback-only and both covered by a test that uses the fixtures.
5. Keep `Current state` current: what works, what is stubbed, what is next.
