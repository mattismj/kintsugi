#!/usr/bin/env bash
# AgentForge gauntlet: every check the harness must pass, in one command.
#
#   bash scripts/gauntlet.sh
#
# Stages, in order:
#
#   gofmt        gofmt -l . must be empty outside spike/
#   build        go build ./...
#   vet          go vet ./...
#   cgo-off      CGO_ENABLED=0 go build ./cmd/kintsugi (the release posture)
#   lab-flow     the blueprints → lab → turn → gate → report integration test
#   test         go test ./... -count=1  (the hermetic suite)
#   demo         scripts/demo.sh against the loopback mock provider (timed)
#   doctor       kintsugi doctor in a throwaway workspace
#
# Each stage prints one PASS/FAIL/SKIP line and the run exits non-zero if any
# stage failed. A stage whose prerequisite is genuinely absent (no python3, no
# `timeout`) is reported as SKIP with the reason, never as a pass: the gauntlet
# does not claim coverage it did not run.
#
# This mirrors .github/workflows/ci.yml and adds the end-to-end stages CI does
# not run. One CI job is deliberately absent: `go test -race`, because the race
# detector is built with cgo and would make a local gauntlet require a C
# compiler — the tree is pure Go on purpose. CI runs it on a machine that has
# one; `KINTSUGI_GAUNTLET_STAGES` cannot select it here.
#
# No stage needs the network or a secret: the demo and doctor stages use a
# loopback mock provider, and the suite's HTTP tests bind 127.0.0.1.
#
# Overrides:
#   KINTSUGI_GAUNTLET_DEMO_TIMEOUT  seconds for the demo stage (default 300)
#   KINTSUGI_GAUNTLET_STAGES        space-separated subset, e.g. "build test"
#   KINTSUGI_BIN                    reuse an existing kintsugi binary for the demo
#                                and doctor stages (the `build` stage still
#                                type-checks the whole module)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

DEMO_TIMEOUT="${KINTSUGI_GAUNTLET_DEMO_TIMEOUT:-300}"
STAGES_ALL=(gofmt build vet cgo-off lab-flow test demo doctor)
read -r -a STAGES <<<"${KINTSUGI_GAUNTLET_STAGES:-${STAGES_ALL[*]}}"

WORK="$(mktemp -d -t kintsugi-gauntlet.XXXXXX)"
LOGDIR="$WORK/logs"
mkdir -p "$LOGDIR"

declare -A STATUS REASON
FAILED=0

# enabled reports whether a stage is in the requested set.
enabled() {
  local want="$1" s
  for s in "${STAGES[@]}"; do
    [[ "$s" == "$want" ]] && return 0
  done
  return 1
}

# A selector that names an unknown stage is a typo, and a typo must not read as a
# pass: `KINTSUGI_GAUNTLET_STAGES="buld"` selects nothing, runs nothing and would
# otherwise exit 0 claiming success. Reject it before any stage runs.
for s in "${STAGES[@]}"; do
  known=0
  for all in "${STAGES_ALL[@]}"; do
    [[ "$s" == "$all" ]] && known=1
  done
  if ((!known)); then
    echo "gauntlet: unknown stage '$s' in KINTSUGI_GAUNTLET_STAGES" >&2
    echo "  known stages: ${STAGES_ALL[*]}" >&2
    exit 2
  fi
done
if ((${#STAGES[@]} == 0)); then
  echo "gauntlet: KINTSUGI_GAUNTLET_STAGES selected no stages; refusing to report a pass" >&2
  exit 2
fi

# stage runs one stage, records its outcome and prints one summary line.
# A stage's output goes to its own log; on failure the tail is echoed so the
# operator sees the reason without opening a file.
stage() {
  local name="$1" log="$LOGDIR/$1.log" rc=0

  if ! enabled "$name"; then
    STATUS[$name]="SKIP"
    REASON[$name]="not selected"
    printf '· %-9s SKIP  not selected\n' "$name"
    return 0
  fi

  printf '· %-9s ' "$name"
  set +e
  run_stage "$name" >"$log" 2>&1
  rc=$?
  set -e
  case $rc in
    0)
      STATUS[$name]="PASS"
      printf 'PASS  (%s)\n' "$log"
      ;;
    77)
      # A stage that skipped itself: 77 is this script's convention for
      # "prerequisite absent", with the reason as the stage's last line.
      STATUS[$name]="SKIP"
      REASON[$name]="$(tail -1 "$log")"
      printf 'SKIP  %s\n' "${REASON[$name]}"
      ;;
    *)
      STATUS[$name]="FAIL"
      printf 'FAIL  (%s)\n' "$log"
      tail_log "$log" | sed 's/^/      /'
      FAILED=1
      ;;
  esac
  return 0
}

# is_elf <file> — the four-byte ELF magic, so a stage cannot mistake a script or
# an error message on stdout for a built binary.
is_elf() {
  local magic
  magic="$(dd if="$1" bs=1 count=4 2>/dev/null | od -An -tx1 | tr -d ' \n')" || return 1
  [[ "$magic" == "7f454c46" ]]
}

# tail_log prints the last lines of a stage log.
tail_log() {
  local log="$1"
  [[ -f "$log" ]] || return 0
  tail -25 "$log"
}

# ---- stage implementations -------------------------------------------------

run_stage() {
  case "$1" in
    gofmt)
      # spike/ is excluded on purpose: those are historical probes kept exactly
      # as they were written, not code anyone maintains. Everything else must be
      # gofmt-clean, with no allowlist — the four internal/ files that once
      # needed one were reformatted instead.
      local unformatted
      unformatted="$(gofmt -l . | grep -v '^spike/' || true)"
      if [[ -n "$unformatted" ]]; then
        echo "gofmt reports unformatted files (spike/ is excluded by design):"
        echo "$unformatted"
        echo "fix with: gofmt -w <file>"
        return 1
      fi
      echo "gofmt: clean"
      ;;
    build)
      go build ./...
      if [[ -n "${KINTSUGI_BIN:-}" && -x "${KINTSUGI_BIN}" ]]; then
        echo "reusing KINTSUGI_BIN=$KINTSUGI_BIN for the later stages"
      else
        go build -o "$WORK/kintsugi" ./cmd/kintsugi
      fi
      ;;
    vet)
      go vet ./...
      ;;
    cgo-off)
      # The release posture: modernc.org/sqlite is the pure-Go driver, so the
      # binary must link with cgo off. If this stage ever fails, a cgo
      # dependency has crept into the tree and .goreleaser.yaml's CGO_ENABLED=0
      # would produce a broken release.
      CGO_ENABLED=0 go build -trimpath -o "$WORK/kintsugi-cgo0" ./cmd/kintsugi
      is_elf "$WORK/kintsugi-cgo0" || { echo "the CGO_ENABLED=0 output is not an ELF binary"; return 1; }
      if command -v ldd >/dev/null 2>&1 && ldd "$WORK/kintsugi-cgo0" 2>&1 | grep -q 'not a dynamic executable'; then
        echo "CGO_ENABLED=0 build is statically linked"
      else
        echo "CGO_ENABLED=0 build succeeded"
      fi
      ;;
    lab-flow)
      # The M8-A flow, runnable on its own: blueprints ship the lab compose
      # file, the lab harness resolves and drives it, and the end-to-end
      # blueprints → lab → turn → gate → report test passes.
      go test ./internal/blueprints/ ./internal/kintsugi/ \
        -run 'TestRenderShipsTheLabComposeFile|TestLabFromConfigResolvesTheBlueprintComposeFile|TestLabUpAndDownOnARenderedWorkspace|TestLabFlowBlueprintToReport' \
        -count=1 -v
      ;;
    test)
      go test ./... -count=1
      ;;
    demo)
      stage_demo
      ;;
    doctor)
      stage_doctor
      ;;
    *)
      echo "unknown stage $1" >&2
      return 2
      ;;
  esac
}

# stage_demo runs the mock-provider demo under a hard timeout. It needs go and
# python3; the demo itself refuses to start without them.
stage_demo() {
  command -v python3 >/dev/null 2>&1 || { echo "python3 is not installed (scripts/mock_provider.py needs it)"; return 77; }
  command -v timeout >/dev/null 2>&1 || { echo "the timeout(1) utility is missing; refusing to run the demo unbounded"; return 77; }
  [[ -x "$ROOT/scripts/demo.sh" ]] || { echo "scripts/demo.sh is missing or not executable"; return 2; }
  echo "timeout ${DEMO_TIMEOUT}s scripts/demo.sh"
  timeout "$DEMO_TIMEOUT" "$ROOT/scripts/demo.sh"
}

# stage_doctor builds (or reuses) the binary and runs `kintsugi doctor` in a
# throwaway workspace with an isolated XDG tree and an offline loopback
# provider, so the check never needs a key or the operator's own config.
stage_doctor() {
  local bin="$WORK/kintsugi" ws="$WORK/doctor-ws"
  if [[ -n "${KINTSUGI_BIN:-}" && -x "${KINTSUGI_BIN}" ]]; then
    bin="$KINTSUGI_BIN"
  elif [[ ! -x "$bin" ]]; then
    go build -o "$bin" ./cmd/kintsugi
  fi
  mkdir -p "$ws"
  cat >"$ws/kintsugi.toml" <<'EOF'
[core]
mode = "offline"
default_model = "mock/deepseek-chat"

[providers.mock]
family = "deepseek"
base_url = "http://127.0.0.1:9/v1"
wire = "openai-completions"
offline = true
models = ["deepseek-chat"]
EOF
  cat >"$ws/CONTEXT.md" <<'EOF'
# Project: gauntlet-doctor

## Domain context

- Throwaway workspace for `kintsugi doctor` in the gauntlet.
EOF

  KINTSUGI_WORKSPACE="$ws" \
  KINTSUGI_CONFIG="" \
  XDG_CONFIG_HOME="$ws/xdg" \
  XDG_CACHE_HOME="$ws/cache" \
  XDG_DATA_HOME="$ws/data" \
    "$bin" doctor </dev/null
}

# ---- run -------------------------------------------------------------------

echo "kintsugi gauntlet · $ROOT"
echo "logs · $LOGDIR"
echo

for name in "${STAGES_ALL[@]}"; do
  stage "$name"
done

echo
echo "── summary ──"
for name in "${STAGES_ALL[@]}"; do
  printf '%-5s %-9s' "${STATUS[$name]:-SKIP}" "$name"
  if [[ -n "${REASON[$name]:-}" ]]; then
    printf '%s' "${REASON[$name]}"
  fi
  printf '\n'
done

if ((FAILED)); then
  echo
  echo "gauntlet: FAIL (logs in $LOGDIR)"
  exit 1
fi
rm -rf "$WORK"
echo
echo "gauntlet: PASS (stage logs cleaned up; a failure keeps them)"
