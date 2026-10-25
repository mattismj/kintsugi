#!/usr/bin/env bash
# checksums.sh — sha256 manifest for a build output tree (M12-B).
#
# Writes dist/SHA256SUMS covering every ELF binary and every archive under the
# dist directory, with paths relative to that directory so the manifest can be
# verified from inside it:
#
#   scripts/checksums.sh              # → dist/SHA256SUMS
#   scripts/checksums.sh out/         # → out/SHA256SUMS
#   scripts/checksums.sh --check      # verify dist/SHA256SUMS
#
# This is the no-goreleaser path: `goreleaser release` writes its own
# dist/SHA256SUMS from the `checksum:` block of .goreleaser.yaml, and this
# script produces the same file name and format for a hand-built tree
# (go build -o dist/kintsugi_linux_amd64_v1/kintsugi ./cmd/kintsugi, …).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST="$ROOT/dist"
MODE="write"

usage() {
  cat <<'EOF'
usage: scripts/checksums.sh [--check] [dist-dir]

  --check      verify an existing SHA256SUMS instead of writing one
  dist-dir     build output directory (default: <repo>/dist)
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --check) MODE="check"; shift ;;
    -h|--help) usage; exit 0 ;;
    --) shift; break ;;
    -*) echo "checksums.sh: unknown flag $1" >&2; usage >&2; exit 2 ;;
    *) DIST="$1"; shift ;;
  esac
done
if [[ $# -gt 0 ]]; then
  DIST="$1"
fi

# Resolve DIST to an absolute path without requiring it to exist yet.
case "$DIST" in
  /*) ;;
  *) DIST="$PWD/$DIST" ;;
esac

if [[ ! -d "$DIST" ]]; then
  echo "checksums.sh: no build directory at $DIST" >&2
  echo "  build first, e.g.: CGO_ENABLED=0 go build -trimpath -o $DIST/kintsugi ./cmd/kintsugi" >&2
  exit 1
fi

# sha256 is GNU coreutils' name; shasum (perl) is the macOS/BSD fallback.
sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$@"
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$@"
  else
    echo "checksums.sh: neither sha256sum nor shasum is on PATH" >&2
    exit 1
  fi
}

MANIFEST="$DIST/SHA256SUMS"

if [[ "$MODE" == "check" ]]; then
  if [[ ! -f "$MANIFEST" ]]; then
    echo "checksums.sh: no manifest at $MANIFEST (run without --check first)" >&2
    exit 1
  fi
  (cd "$DIST" && sha256 --check SHA256SUMS)
  exit 0
fi

# is_elf <file> — the four-byte ELF magic, so scripts and text files in the
# tree are never mistaken for binaries.
is_elf() {
  local magic
  magic="$(dd if="$1" bs=1 count=4 2>/dev/null | od -An -tx1 | tr -d ' \n')" || return 1
  [[ "$magic" == "7f454c46" ]]
}

# Collect artifacts: ELF binaries and release archives, skipping the manifest
# itself. Sorted so two runs over the same tree produce identical bytes.
artifacts=()
while IFS= read -r -d '' f; do
  rel="${f#"$DIST"/}"
  [[ "$rel" == "SHA256SUMS" ]] && continue
  case "$rel" in
    *.tar.gz|*.tgz|*.zip) artifacts+=("$rel"); continue ;;
  esac
  if is_elf "$f"; then
    artifacts+=("$rel")
  fi
done < <(find "$DIST" -type f -print0 | LC_ALL=C sort -z)

if [[ ${#artifacts[@]} -eq 0 ]]; then
  echo "checksums.sh: no binaries or archives under $DIST" >&2
  echo "  expected an ELF binary (e.g. kintsugi) or a *.tar.gz archive" >&2
  exit 1
fi

tmp="$(mktemp "$DIST/.SHA256SUMS.XXXXXX")"
trap 'rm -f "$tmp"' EXIT
(cd "$DIST" && sha256 "${artifacts[@]}" > "$tmp")
mv "$tmp" "$MANIFEST"
trap - EXIT
chmod 0644 "$MANIFEST"

echo "wrote $MANIFEST (${#artifacts[@]} artifact(s))"
sed 's/^/  /' "$MANIFEST"
