# fixtures — {{PROJECT}}

Starter notes for the synthetic lab fixtures this tasking harness is validated
against. Nothing here is a target: a fixture is a stand-in this repository
defines, so a run is reproducible and observable.

## Rules for this directory

- **Synthetic only.** No executable malware, no real malware samples, no
  weaponized payloads, and no copies of anything collected from a real
  environment. Text, config, and small generator scripts that describe a fake
  host are the intended contents.
- **Synthetic addressing only.** `127.0.0.1`, `10.11.x.x` lab ranges, and
  `*.lab.example` names. Never a real hostname, IP, domain, account, or
  credential — not even an internal one.
- **Small and reviewable.** A fixture is a few lines a reviewer can read in one
  sitting: a host description, the services on it, and what a check-in should
  look like from the server's side.
- **One fixture per behaviour.** Each tasking behaviour (first check-in, idle
  poll, task assignment, result report, timeout/retry) gets its own fixture so a
  failure names the behaviour instead of the whole run.

## Suggested shape

- `hosts/` — one file per synthetic host: name, lab address, service, and the
  events it should generate.
- `tasking/` — the tasking exchange as recorded request/response pairs, used by
  the server and client tests.
- `detections/` — what the blue team should see for each behaviour: the log
  line, the alert, or the absence of one.

Add the files you actually need and delete what you do not; this blueprint only
commits the notes.

## Scope guard

Every fixture is also the definition of what the client and server may touch. If
a fixture is not in this directory, the harness has no reason to reach it — and
neither does the model working on the project.
