# kintsugi-loop

Self-paced loops: run once now, re-run on a cadence, stop when a check cannot
help.

## Synopsis

```bash
kintsugi loop [interval] "<task>"
```

## Description

Run the task once immediately, then again every interval. The loop ends when
another check cannot help: a pass that failed will fail the same way, and two
consecutive passes with the same answer have nothing left to report. It also
ends at `--max` checks or on interrupt.

An interval is a duration (`90s`, `5m`, `1h`) or a bare number of seconds. The
default is 5m, the floor is 1s, the default ceiling is 10 checks.

Every pass is a background run in the session hub — the same registry
`task(background=true)` and `agent_output` use — so it appears in the TUI roster
and can be killed there. The cadence logic lives in `internal/loop`; the CLI
supplies only the runner (one ordinary agent turn) and the printing.

## Options

- `--max N` — stop after N checks (default 10)
- `--keep-going` — continue after a failed check instead of stopping
- `--quiet` — one line per check instead of the full answer
- `--model provider/model`, `--persona name`, `--role default|coder|researcher|opsec`

## Injection safety

A loop's task text is data, never authority. A payload that tries to grant a
permission, widen the declared scope, or write authorization/scope context is
refused when the loop starts, and the same check runs again before every pass and
once more inside the runner. The refusal is logged as a class and a rule label;
the matched text is never logged, so an injection attempt cannot be replayed into
a prompt. A pass runs under the configured permission matrix, scope and sandbox
exactly as a typed turn does, and carries a sentence telling the model the same
rule.

## Examples

```bash
# Watch a deploy every two minutes, up to eight checks
$ kintsugi loop 2m "check whether the staging deploy is healthy"

# Poll a test suite every 30 seconds
$ kintsugi loop --max 20 30s "run the unit tests and report failures"

# One line per pass, in a script
$ kintsugi loop --quiet 1m "report the queue depth"
```

## See Also

- [kintsugi](kintsugi.md)
- [kintsugi-schedule](kintsugi-schedule.md)
- [kintsugi-run](kintsugi-run.md)