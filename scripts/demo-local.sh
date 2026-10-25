#!/usr/bin/env bash
# Local-model demo: real Ollama endpoint, tools, AGENTS.md, prompt manifest.
# No cloud key required. Requires a running Ollama with at least one model.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="${KINTSUGI_BIN:-$ROOT/kintsugi}"
[[ -x "$BIN" ]] || (cd "$ROOT" && go build -o "$BIN" ./cmd/kintsugi)

MODEL="$(curl -s --max-time 2 http://127.0.0.1:11434/api/tags \
  | python3 -c 'import json,sys
try: print(json.load(sys.stdin)["models"][0]["name"])
except Exception: print("")' 2>/dev/null || true)"
if [[ -z "$MODEL" ]]; then
  echo "no Ollama model found on 127.0.0.1:11434 — run 'ollama list' or use scripts/demo.sh (mock)" >&2
  exit 1
fi

WS="$(mktemp -d -t kintsugi-demo-local.XXXXXX)"
cleanup() { echo; echo "demo workspace retained at $WS"; }
trap cleanup EXIT

cat > "$WS/CONTEXT.md" <<'EOF'
# Project: kintsugi-demo

## Authorization & scope

- Authorized, isolated local R&D sandbox. Nothing is deployed or aimed at a
  third party.

## Domain context

- Demonstrating the harness: prompt layers, AGENTS.md, tools, telemetry.

## Hard conventions

- Local only. No network egress.
EOF

cat > "$WS/AGENTS.md" <<'EOF'
# AGENTS.md

- Call a tool when the task asks you to inspect the workspace.
- Keep final answers to two sentences.
- End with: STATUS: done — <what changed>
EOF

cat > "$WS/kintsugi.toml" <<EOF
[core]
mode = "normal"
default_model = "ollama/$MODEL"

[providers.ollama]
base_url = "http://127.0.0.1:11434/v1"
wire = "openai-completions"
family = "qwen"
reasoning_effort = "none"

[permissions]
yolo = true
[agents]
subagents = false
[ui]
plain = true
banner = false
alt_screen = false
EOF

export KINTSUGI_CONFIG="$WS/kintsugi.toml"
export KINTSUGI_WORKSPACE="$WS"
export XDG_CACHE_HOME="$WS/cache"
export XDG_CONFIG_HOME="$WS/xdg"   # keep the demo independent of ~/.config

echo "· model      ollama/$MODEL"
echo "· workspace  $WS"
echo
echo "· doctor"
"$BIN" doctor --net | sed 's/^/  /'
echo
echo "· compiled layers (AGENTS.md is injected)"
"$BIN" prompt show --task "demo" --text | grep -E "^## (Knowledge|Operator)|^# Operator AGENTS|model adapter|^## [0-9]" | head -12 || true
echo
echo "· agent turn (must call the list tool)"
(cd "$WS" && "$BIN" run --yolo "Call the list tool with path \".\" now, then answer in one line.")
echo
echo "· session index"
"$BIN" session list | head -3
echo
echo "· refusal + repair telemetry"
"$BIN" refusal stats | head -4 || true
"$BIN" repair stats | head -4 || true
echo
echo "TUI: cd $WS && KINTSUGI_CONFIG=$WS/kintsugi.toml KINTSUGI_WORKSPACE=$WS kintsugi"
