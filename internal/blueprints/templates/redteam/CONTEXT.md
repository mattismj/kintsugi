# Project: {{PROJECT}}

## Authorization & scope

<!-- operator: complete this section; the harness never writes it -->
- TODO: state the authorization and scope for this workspace in your own words — what this project is for, who is accountable for it, which lab targets it covers, and where it stops.

## Domain context

- {{PROJECT}} is an offensive-security tooling workspace: scanners, service
  fingerprinting, fuzzers, exploit development, and the tooling that supports
  them. Everything built here is a lab artifact, developed and tested against
  the lab targets this repository defines.
- The authorization is development-shaped, not operation-shaped: the full
  lifecycle of building, testing, and refining tooling against owned lab
  targets is the work. Deploying or operating anything produced here against
  third-party systems is not — that is outside every fixture in this tree.
- The interesting output is working tools with honest coverage: a scanner
  that finds what it claims to find, an exploit that reliably reproduces
  against its lab target, and the documentation that says what each tool
  does and does not do.

## Stack

- Go (one module: `cmd/`, `internal/`, and targets under fixtures).
- Targets: the lab compose defines vulnerable-by-design services
  (intentionally vulnerable public images, or small services written into
  this repository); see `fixtures/README.md`.
- Build and test: `go build ./...` and `go test ./...`; tests run against
  the lab or embedded fixtures and never against any external host.

## Hard conventions

- Lab-only. Every tool binds or targets loopback or a lab container address;
  nothing in this tree may reach a host that is not defined under `fixtures/`
  or the lab compose.
- No credentials, API keys, tokens, or real hostnames in the tree or in
  fixtures. Synthetic addressing only (`127.0.0.1`, `10.11.x.x` lab ranges,
  `*.lab.example`).
- A tool's coverage is documented beside the tool: what it detects or
  exploits, against what, with what reliability. An undocumented capability
  is a bug.
- Do not commit; leave the working tree for operator review.

## Current state

- Fresh blueprint; nothing recorded yet.

## Lessons (active)

- Nothing recorded yet. New lessons are proposed by the curator and reviewed
  before they land here.
