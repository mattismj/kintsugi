# fixtures — {{PROJECT}}

Starter notes for the synthetic lab hosts this scan utility is allowed to see.
Nothing here is a real target: a fixture is a stand-in this repository defines,
so a run is reproducible and its footprint is stable.

## Rules for this directory

- **Synthetic only.** No executable malware, no real malware samples, no
  weaponized payloads, and nothing copied out of a real environment. Host
  descriptions, service lists, and the expected records are the intended
  contents.
- **Synthetic addressing only.** `127.0.0.1`, `10.11.x.x` lab ranges, and
  `*.lab.example` names. Never a real hostname, IP, domain, account, or
  credential — not even an internal one.
- **The scope file is the contract.** A file such as `scope.toml` listing the
  in-scope hosts and ranges is what the tool reads. A target that is not in it
  is out of scope: skip it, do not widen.
- **Expected output lives next to the input.** Each fixture records what a scan
  should emit for it, so a test can compare without reaching the network.

## Suggested shape

- `scope.toml` — in-scope synthetic hosts and ranges, and the explicit
  out-of-scope list.
- `hosts/` — one file per synthetic host: name, lab address, services, and the
  records a probe should produce.
- `expected/` — the JSONL records a run over the fixtures must produce, used as
  golden output in tests.
- `detections/` — what the blue team should see for each probe: the log line,
  the alert, or the deliberate absence of one.

Add the files you actually need and delete what you do not; this blueprint only
commits the notes.

## Scope guard

Every fixture is also the definition of what a run may reach. If a host is not
in the scope file, the harness has no reason to probe it — and neither does the
model working on the project.
