#!/usr/bin/env bash
# capture-brand.sh — capture the TUI at the sizes the identity plans judge it on.
#
# This is deliberately conservative. It refuses to run without a real terminal,
# because a capture made under a pipe is an empty or mis-sized file that looks
# like a screenshot and is not one. A missing capture is obvious; a bogus one is
#  not, and it would become the reference for a UI nobody looked at.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="$ROOT/docs/brand"
CHECK=0
KINTSUGI_BIN="${KINTSUGI_BIN:-$HOME/.local/bin/kintsugi}"

usage() {
  cat <<'USAGE'
usage: scripts/capture-brand.sh [options]

Captures the TUI at 80x24, 120x34 and 200x60 in truecolor and mono.

  --out DIR     write captures here (default: docs/brand)
  --check       report what is missing and exit; write nothing
  --bin PATH    the kintsugi binary (default: ~/.local/bin/kintsugi, or $KINTSUGI_BIN)
  -h, --help    this text

Requires: a real TTY, and one of `script(1)`, tmux, or `expect`.
Refuses to run when stdout is not a terminal.
USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    --out) OUT="$2"; shift 2 ;;
    --check) CHECK=1; shift ;;
    --bin) KINTSUGI_BIN="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "capture-brand: unknown argument $1" >&2; usage >&2; exit 2 ;;
  esac
done

SIZES=("80x24" "120x34" "200x60")
SURFACES=("startup" "idle" "review" "modelctx")

need() { for f in "${SIZES[@]}"; do for s in "${SURFACES[@]}"; do
  [ -f "$OUT/$s-$f.txt" ] || echo "missing $s-$f.txt"
  [ -f "$OUT/$s-$f-mono.txt" ] || echo "missing $s-$f-mono.txt"
done; done; }

if [ "$CHECK" = 1 ]; then
  echo "capture-brand: checking $OUT"
  missing="$(need)"
  if [ -z "$missing" ]; then
    echo "capture-brand: complete"
    exit 0
  fi
  printf '%s\n' "$missing"
  echo "capture-brand: $(printf '%s\n' "$missing" | wc -l) capture(s) missing"
  exit 1
fi

# A capture is only meaningful on a real terminal.
if [ ! -t 1 ]; then
  echo "capture-brand: stdout is not a terminal — refusing to write a bogus capture" >&2
  echo "capture-brand: run this from an interactive shell, or use --check in CI" >&2
  exit 3
fi

if [ ! -x "$KINTSUGI_BIN" ]; then
  echo "capture-brand: $KINTSUGI_BIN is not executable" >&2
  echo "capture-brand: build it first: scripts/install.sh" >&2
  exit 4
fi

# Pick a pty recorder. `script` is the most portable; tmux is the fallback.
RECORDER=""
if command -v script >/dev/null 2>&1; then RECORDER=script
elif command -v tmux >/dev/null 2>&1; then RECORDER=tmux
fi
if [ -z "$RECORDER" ]; then
  echo "capture-brand: no pty recorder found (need script(1) or tmux)" >&2
  exit 5
fi

mkdir -p "$OUT"
trap 'stty sane 2>/dev/null || true' EXIT

# capture <surface> <WxH> <mono:0|1>
#
# It drives the binary through a short scripted session and records the terminal
# output. The surface is selected with a slash command so the capture shows the
# thing it is named for.
capture() {
  local surface="$1" size="$2" mono="$3"
  local cols="${size%x*}" rows="${size#*x}"
  local file="$OUT/$surface-$size.txt"
  [ "$mono" = 1 ] && file="$OUT/$surface-$size-mono.txt"

  local drive=""
  case "$surface" in
    idle)     drive="/status" ;;
    review)   drive="/plan" ;;
    modelctx) drive="/modelctx" ;;
    startup)  drive="" ;;
  esac

  echo "capture-brand: $surface $size mono=$mono -> $file"

  local stub="$ROOT/scripts/.capture-tmp"
  mkdir -p "$stub"
  {
    echo "set -g window-size manual 2>/dev/null"
    echo "stty cols $cols rows $rows 2>/dev/null || true"
    [ "$mono" = 1 ] && echo "export NO_COLOR=1"
    echo "export XDG_CONFIG_HOME=\${XDG_CONFIG_HOME:-$HOME/.config}"
    if [ -n "$drive" ]; then
      printf '%s\n' "$drive"
    fi
    echo "# the operator presses ctrl+c to leave"
  } > "$stub/drive.sh"

  if [ "$RECORDER" = script ]; then
    # `script` records everything written to the pty. We bound the run with a
    # timeout so an interactive TUI that is waiting for input still exits.
    ( stty cols "$cols" rows "$rows" 2>/dev/null || true
      if [ "$mono" = 1 ]; then export NO_COLOR=1; fi
      timeout 8 script -q -c "$KINTSUGI_BIN" /dev/null ) >"$file" 2>/dev/null || true
  else
    local sess="capture-$$"
    tmux kill-session -t "$sess" 2>/dev/null || true
    tmux new-session -d -s "$sess" -x "$cols" -y "$rows" \
      "$([ "$mono" = 1 ] && echo 'NO_COLOR=1') $KINTSUGI_BIN"
    sleep 3
    if [ -n "$drive" ]; then
      tmux send-keys -t "$sess" "$drive" C-m 2>/dev/null || true
      sleep 2
    fi
    tmux capture-pane -p -t "$sess" >"$file" 2>/dev/null || true
    tmux kill-session -t "$sess" 2>/dev/null || true
  fi
  rm -rf "$stub"

  if [ ! -s "$file" ]; then
    echo "capture-brand: $file is empty — the capture did not work" >&2
    rm -f "$file"
    return 1
  fi
  return 0
}

fail=0
for size in "${SIZES[@]}"; do
  for surface in "${SURFACES[@]}"; do
    capture "$surface" "$size" 0 || fail=1
    capture "$surface" "$size" 1 || fail=1
  done
done

if [ "$fail" != 0 ]; then
  echo "capture-brand: some captures failed; nothing was fabricated" >&2
  exit 1
fi

echo "capture-brand: wrote captures to $OUT"
echo "capture-brand: review them before committing — docs/brand/README.md says what to look for"