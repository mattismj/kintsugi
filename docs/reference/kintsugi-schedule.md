# kintsugi-schedule

Scheduled one-shot and recurring jobs from a cron expression.

## Synopsis

```bash
kintsugi schedule add "<cron>" "<task>"
kintsugi schedule list
kintsugi schedule trigger <id>
kintsugi schedule pause|resume|remove <id>
kintsugi schedule run
```

## Description

Store a cron expression and a task, and fire the task when the expression comes
due. Jobs live in `.kintsugi/schedules.json` in the workspace, written
atomically at 0600. A malformed file is an error, never a file to be silently
truncated: the operator's schedules are the only copy.

The five cron fields are minute, hour, day-of-month, month and day-of-week. Each
supports `*`, a number, a range (`a-b`), a step (`*/n` or `a-b/n`) and
comma-separated lists; month and day-of-week also accept their three-letter
names. `@hourly @daily @midnight @weekly @monthly` are accepted as exact aliases.
Anything else is rejected with the reason: a schedule that silently never fires
is worse than a refusal.

`run` is the foreground scheduler. It fires due jobs, records each outcome, and
waits for the next window until interrupted. A firing is a background run in the
session hub, so it appears in the TUI roster and can be killed there. Delivery is
local: the answer is printed and one terminal notification is raised when a
backend is configured in `[notify]`.

## Commands

- **add** `<cron>` `<task>` — store a job; the cron expression and the injection
  guard are both checked here
- **list** — jobs with state, next firing, last firing and the last error
- **trigger** `<id>` — fire one job now; the cron is untouched and the next
  window is recomputed from it
- **pause** `<id>` / **resume** `<id>` — suspend and restart firing; the job and
  its window are kept
- **remove** `<id>` — delete the job
- **run** — the foreground scheduler until ctrl-c

## Options

- `--every 30s` — for `run`: how often to look for due jobs
- `--model provider/model`, `--role default|coder|researcher|opsec`

## Injection safety

A scheduled prompt is data, never authority. Three gates enforce it: at write
time (`add`), at fire time (the payload is re-checked before every firing, so a
hand-edited or agent-edited jobs file cannot smuggle one in), and inside the
runner before the prompt is built. A payload that tries to grant a permission,
widen the declared scope, or write authorization/scope context is rejected, and
the refusal is logged as a class and a rule label — never as the text that tried.

A scheduled turn cannot change the permission matrix, the `[scope]` block, the
sandbox posture, the attestation or a hook: none of those are read from task text
anywhere in the harness. The task text becomes a user turn and nothing else.

## Examples

```bash
# Ask for a nightly summary
$ kintsugi schedule add "0 3 * * *" "summarise yesterday's commits into NOTES.md"

# What is scheduled?
$ kintsugi schedule list

# Test a job without waiting for its window
$ kintsugi schedule trigger job-1

# Run the scheduler in a terminal
$ kintsugi schedule run --every 30s
```

## See Also

- [kintsugi](kintsugi.md)
- [kintsugi-loop](kintsugi-loop.md)
- [kintsugi-run](kintsugi-run.md)