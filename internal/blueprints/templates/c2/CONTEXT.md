# Project: {{PROJECT}}

## Authorization & scope

<!-- operator: complete this section; the harness never writes it -->
- TODO: state the authorization and scope for this workspace in your own words — what this project is for, who is accountable for it, which fixtures or lab hosts it covers, and where it stops.

## Domain context

- {{PROJECT}} is an adversary-simulation tasking client/server harness: a server
  that queues and hands out tasking, and a client that checks in, pulls that
  tasking, and reports results back. Both sides, and the fixtures they talk
  through, live in this repository. The intended use is purple-team validation
  against synthetic lab fixtures — the tasking channel exists so detection
  content, log pipelines, and response playbooks can be exercised against a
  realistic, observable pattern of traffic and host events.
- The interesting output is defensive: what a listener/client pair looks like in
  telemetry, which events fire, and what a responder can see. The client is a
  lab artifact, not a deployed capability.
- Everything runs inside an isolated local sandbox against fixtures this
  repository defines. There is no production path here, egress is denied by
  default, and no fixture names real infrastructure.

## Stack

- Go (one module: `server/`, `client/`, and fixtures under the same `go.mod`).
- Transport: JSON over HTTP on loopback (`127.0.0.1`); TLS and transport
  obfuscation are deliberate later decisions, not scaffolded.
- Storage: SQLite for the tasking queue (JSONL is fine while the harness is
  small — both are local files).
- Build and test: `go build ./...` and `go test ./...`; tests run against
  fixtures under `fixtures/` and never against a host.

## Hard conventions

- Lab-only. Every listener binds loopback or a lab container address; nothing in
  this tree may reach a host that is not defined under `fixtures/`.
- `[workspace] profile = "lab"` and `[core] mode = "offline"` stay in
  `kintsugi.toml` unless an operator change records why.
- No credentials, API keys, tokens, or real hostnames in the tree or in
  fixtures. Synthetic addressing only (`127.0.0.1`, `10.11.x.x` lab ranges,
  `*.lab.example`).
- A protocol or tasking-format change lands with a fixture that exercises it.
- Do not commit; leave the working tree for operator review.

## Current state

- Fresh blueprint; nothing recorded yet.

## Lessons (active)

- Nothing recorded yet. New lessons are proposed by the curator and reviewed
  before they land here.
