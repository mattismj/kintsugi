#!/usr/bin/env bash
# Server readiness check: verifies kintsugi works without a TTY and, with --live,
# completes a real headless turn. Safe for SSH / systemd / CI.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="${KINTSUGI_BIN:-$ROOT/kintsugi}"
[[ -x "$BIN" ]] || (cd "$ROOT" && go build -o "$BIN" ./cmd/kintsugi)

WS="$(mktemp -d -t kintsugi-server-check.XXXXXX)"
trap 'rm -rf "$WS"' EXIT
export KINTSUGI_WORKSPACE="$WS"
export XDG_CACHE_HOME="$WS/cache"

cat > "$WS/CONTEXT.md" <<'EOF'
# Project: server-check

## Domain context

- Headless readiness check.
EOF
printf '# AGENTS.md\n\n- Answer briefly.\n' > "$WS/AGENTS.md"

echo "== kintsugi doctor (stdin not a TTY) =="
"$BIN" doctor --net </dev/null | tee "$WS/doctor.txt"
grep -q "kintsugi doctor: clean" "$WS/doctor.txt" || {
  echo "FAIL: doctor reported issues" >&2
  exit 1
}

echo
echo "== headless run (piped stdout) =="
if [[ "${1:-}" == "--live" ]]; then
  "$BIN" run --yolo "Use the list tool on \".\" and answer in one sentence." </dev/null | tail -8
else
  echo "skipped (pass --live to run a real model turn)"
fi

echo
echo "PASS: server readiness checks completed"
