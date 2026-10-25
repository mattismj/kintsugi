# fixtures — {{PROJECT}}

Starter notes for the synthetic telemetry this detection set is validated
against. Nothing here is production data: a fixture is a recorded or
generated event stream this repository defines, so a validation run is
reproducible and observable.

## Rules for this directory

- **Synthetic only.** No production log exports, no real usernames, hostnames,
  IPs, domains, or account identifiers — not even internal ones. Synthetic
  addressing (`127.0.0.1`, `10.11.x.x`, `*.lab.example`) and synthetic
  identities (`labuser01`, `WS-001`) only.
- **Malware handling.** Behaviors, hashes, and network indicators of
  lab-analyzed samples may be recorded here as data. Executable samples are
  never committed; a defanged copy is a text file, not a payload.
- **One behaviour per fixture.** Each detection behaviour (process creation
  from an office macro, beacon check-in with jitter, credential dumping
  telemetry) gets its own event stream, so a failure names the detection that
  missed, not the whole run.
- **Positive and negative.** A fixture for what should fire carries a sibling
  for what should not — benign look-alikes are where false positives live.

## Suggested shape

- `events/` — synthetic event recordings, one file per behaviour, in the
  source format the detection expects (ECS JSON, Windows event XML, syslog,
  pcaps for network rules).
- `expected/` — the fire/no-fire verdicts: which rule IDs match which
  fixture, maintained by the validation runner.

Add the files you actually need and delete what you do not; this blueprint
only commits the notes.

## Scope guard

Every fixture is also the definition of what the detections may assume. If
a telemetry source is not represented here, a rule written against it is
untested — and the validation runner should say so.
