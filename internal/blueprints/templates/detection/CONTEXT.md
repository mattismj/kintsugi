# Project: {{PROJECT}}

## Authorization & scope

<!-- operator: complete this section; the harness never writes it -->
- TODO: state the authorization and scope for this workspace in your own words — what this project is for, who is accountable for it, which telemetry or lab sources it covers, and where it stops.

## Domain context

- {{PROJECT}} is a detection engineering workspace: Sigma rules, Suricata/Snort
  signatures, and hunting queries, with the tooling that validates them against
  lab telemetry. The work is defensive by construction — every detection is
  written to catch adversary tradecraft, and validated against lab-generated
  events that reproduce that tradecraft's telemetry.
- The offensive knowledge here is instrumental: writing a detection for C2
  beaconing means understanding beacon jitter, channel selection, and
  check-in patterns, and simulating them in the lab. Attack simulation code
  that runs in the lab is part of the work; a detection that has never fired
  against a simulation is a hypothesis, not a detection.
- ATT&CK mapping is first-class: every rule and query names the technique(s)
  it covers and cites the telemetry source it expects to fire on.

## Stack

- Detection content: YAML (Sigma core + backend-specific modifiers),
  Suricata/Snort syntax, and the query language of the target platform.
- Lab: docker-compose with a log generator and a receiver (see `lab/`).
- Validation: a script or harness that replays `fixtures/` events through
  the detection and asserts expected fire/no-fire.
- Build and test: whatever runner the project settles on — the scaffold's
  `fixtures/README.md` records the convention.

## Hard conventions

- Lab telemetry only. No production log access, no live SIEM connections
  from the harness; detections land in this repository and are promoted to
  production by reviewed deployment, not by this workspace.
- Every rule and query carries a header block: ATT&CK technique IDs, data
  source, confidence, and false-positive notes. A rule without its header is
  incomplete.
- Malware samples in fixtures are lab-safe or defanged copies, analyzed in
  the sandbox; hashes and behaviors are recorded, executables are not
  committed.
- Do not commit; leave the working tree for operator review.

## Current state

- Fresh blueprint; nothing recorded yet.

## Lessons (active)

- Nothing recorded yet. New lessons are proposed by the curator and reviewed
  before they land here.
