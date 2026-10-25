# Project: {{PROJECT}}

## Authorization & scope

<!-- operator: complete this section; the harness never writes it -->
- TODO: state the authorization and scope for this workspace in your own words — what this project is for, who is accountable for it, which fixtures or lab hosts it covers, and where it stops.

## Domain context

- {{PROJECT}} is a lab-scoped discovery and scan utility: it enumerates hosts
  and services inside the fixture network this repository defines and writes
  what it found as structured output, for the operator and for detection
  tuning.
- The defaults are deliberately boring. Targets come from an explicit scope file
  under `fixtures/` (see `fixtures/README.md`), never from a wide range; probing
  is loopback and lab-range only, with low concurrency and a hard per-probe
  timeout, and the tool refuses to start when it can resolve no in-scope target.
- The intended use is validating what the blue team can see: a scan should
  produce the same observable footprint every run, so log pipelines, detections,
  and inventory checks can be tested against it. Nothing here is intended to
  reach a network this repository does not describe.

## Stack

- Go (one module: `cmd/`, `scan/`, and fixtures under the same `go.mod`).
- Network access: standard library only (`net`, `net/http` for the probe
  timeout wrapper). No third-party scanner dependency while the scope model is
  still moving.
- Output: one machine-readable record per result (JSONL) plus a short text
  summary; both are local files.
- Build and test: `go build ./...` and `go test ./...`; tests run against the
  fixtures, never against a host.

## Hard conventions

- Lab-only. Targets are read from the fixture scope file; a target that is not
  listed there is out of scope and the tool must skip it, not warn and continue.
- Defaults stay safe: loopback and `10.11.x.x` lab ranges, low concurrency, a
  per-probe timeout, and no rate-limit-evasion or stealth tuning.
- `[workspace] profile = "lab"` and `[core] mode = "offline"` stay in
  `kintsugi.toml` unless an operator change records why.
- No credentials, API keys, tokens, or real hostnames in the tree or in
  fixtures. Synthetic addressing only (`127.0.0.1`, `10.11.x.x`, `*.lab.example`).
- A new probe type lands with a fixture host and the output record it produces.
- Do not commit; leave the working tree for operator review.

## Current state

- Fresh blueprint; nothing recorded yet.

## Lessons (active)

- Nothing recorded yet. New lessons are proposed by the curator and reviewed
  before they land here.
