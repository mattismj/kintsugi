# fixtures — {{PROJECT}}

Starter notes for the lab targets this tooling is developed and tested
against. Nothing here is a real target: a fixture is a service or recorded
response this repository defines, so a tool's run is reproducible and
observable.

## Rules for this directory

- **Owned and synthetic only.** No real hostnames, IPs, domains, accounts, or
  credentials — not even internal ones. Synthetic addressing (`127.0.0.1`,
  `10.11.x.x`, `*.lab.example`) and synthetic identities only.
- **Vulnerable by design, not by accident.** A target that exists to be
  exploited is either a public intentionally-vulnerable image (referenced in
  the lab compose, never copied in) or a small service written into this
  repository with its vulnerability documented in place.
- **One target per tool-class.** Each scanning/fingerprinting/exploitation
  target gets its own fixture or compose service, so a failure names the tool
  that missed, not the whole run.
- **Recorded responses.** Where a tool is tested against recorded data
  (service banners, protocol responses), the recording is a text fixture with
  its source noted — not a live capture from anywhere.

## Suggested shape

- `targets/` — descriptions of each lab target: what it runs, what it is
  vulnerable to, and where its compose service lives.
- `responses/` — recorded banners and protocol exchanges for
  offline-validated tooling.
- `docs/` — each tool's coverage notes: what it finds or exploits, against
  which target, with what reliability.

Add the files you actually need and delete what you do not; this blueprint
only commits the notes.

## Scope guard

Every fixture and lab service is also the definition of what the tooling may
touch. If a target is not defined here or in the lab compose, the harness has
no reason to reach it — and neither does the model working on the project.
