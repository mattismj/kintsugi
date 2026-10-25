#!/usr/bin/env bash
# install.sh — build or download kintsugi and install it to ~/.local/bin (M12-B).
#
# Two ways in, in this order of preference:
#
#   1. inside a checkout (default when this script sits in one):
#        GOBIN=<staging> CGO_ENABLED=0 go install -trimpath ./cmd/kintsugi
#      then scripts/checksums.sh writes SHA256SUMS over the staging directory
#      and verifies it back before anything is installed. --version TAG stamps
#      the build with -X main.version=TAG.
#   2. anywhere else (or with --download/--tarball): a release tarball
#        <base>/v1.4.0/kintsugi_1.4.0_linux_amd64.tar.gz
#      plus its sibling SHA256SUMS; the archive is verified before it is
#      unpacked, so an unverified artifact is never extracted. The base URL is
#      a placeholder that must be set once releases exist — this script refuses
#      to guess a host.
#
# Idempotent: re-running installs the same bytes, and an already-installed
# kintsugi whose bytes match is reported and left alone. It never overwrites a
# binary at ~/.local/bin/kintsugi that does not identify itself as kintsugi, and it
# never executes a non-ELF file that happens to sit at that path.
#
# Usage:
#   scripts/install.sh                     # build from this checkout, install
#   scripts/install.sh --repo-dir DIR      # build from another checkout
#   scripts/install.sh --version v1.4.0    # stamp the local build 1.4.0
#   scripts/install.sh --download --version v1.4.0
#                                          # fetch that release instead
#   scripts/install.sh --tarball PATH|URL  # install a specific tarball
#   scripts/install.sh --checksum-file F   # verify against this sha256 file
#   scripts/install.sh --prefix DIR        # install somewhere else
#   scripts/install.sh --dry-run           # do everything except the install
#   scripts/install.sh --help
set -euo pipefail

BIN_NAME="kintsugi"
# Placeholder, on purpose. There is no release host yet: set this to the real
# GitHub/Gitea release base when the first tag is published, or pass --tarball.
DEFAULT_RELEASE_BASE_URL="${KINTSUGI_RELEASE_BASE_URL:-}"
RELEASE_URL_HINT="https://<release-host>/<owner>/kintsugi/releases/download"

SOURCE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

PREFIX="${KINTSUGI_INSTALL_DIR:-${HOME}/.local/bin}"
MODE="auto"              # auto | repo | tarball (--download forces tarball)
REPO_DIR=""
TARBALL=""
VERSION=""
CHECKSUM_FILE=""
RELEASE_BASE_URL="$DEFAULT_RELEASE_BASE_URL"
DRY_RUN=0
SKIP_CHECKSUM=0

log() { printf '· %s\n' "$*"; }
warn() { printf 'warn: %s\n' "$*" >&2; }
die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<EOF
usage: scripts/install.sh [options]

Builds or downloads kintsugi and installs it to $PREFIX.

  --repo-dir DIR      build from a kintsugi checkout (default: the one holding this script)
  --version TAG       stamp/select a version: v1.4.0 or 1.4.0
  --download          take the download path even inside a checkout
  --tarball PATH|URL  install a specific release tarball
  --checksum-file F   verify against this SHA256SUMS or .sha256 file
  --prefix DIR        install directory (default: ~/.local/bin, or \$KINTSUGI_INSTALL_DIR)
  --release-url URL   release download base URL (default: \$KINTSUGI_RELEASE_BASE_URL)
  --dry-run           build, verify, and stop before writing anything
  --skip-checksum     install without verification (not recommended)
  -h, --help          this text

Without --repo-dir/--version/--tarball the script prefers the checkout it lives
in (no network needed); otherwise it downloads. The release base URL is a
placeholder until a release host exists — set KINTSUGI_RELEASE_BASE_URL or pass
--release-url/--tarball.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo-dir) REPO_DIR="${2:?--repo-dir needs a directory}"; MODE="repo"; shift 2 ;;
    --repo-dir=*) REPO_DIR="${1#*=}"; MODE="repo"; shift ;;
    --tarball) TARBALL="${2:?--tarball needs a path or URL}"; MODE="tarball"; shift 2 ;;
    --tarball=*) TARBALL="${1#*=}"; MODE="tarball"; shift ;;
    # --version means "this version" in either mode: inside a checkout it is
    # the stamp passed to -X main.version, and with --download it selects the
    # release tarball. It does not by itself force a download.
    --version) VERSION="${2:?--version needs a tag}"; shift 2 ;;
    --version=*) VERSION="${1#*=}"; shift ;;
    --download) MODE="tarball"; shift ;;
    --checksum-file) CHECKSUM_FILE="${2:?--checksum-file needs a path}"; shift 2 ;;
    --checksum-file=*) CHECKSUM_FILE="${1#*=}"; shift ;;
    --prefix) PREFIX="${2:?--prefix needs a directory}"; shift 2 ;;
    --prefix=*) PREFIX="${1#*=}"; shift ;;
    --release-url) RELEASE_BASE_URL="${2:?--release-url needs a base URL}"; shift 2 ;;
    --release-url=*) RELEASE_BASE_URL="${1#*=}"; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    --skip-checksum) SKIP_CHECKSUM=1; shift ;;
    -h|--help) usage; exit 0 ;;
    --) shift; break ;;
    *) die "unknown argument $1 (try --help)" ;;
  esac
done
[[ $# -eq 0 ]] || die "unexpected trailing argument $1 (try --help)"

# ---------------------------------------------------------------- helpers ---

need() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required but not on PATH"
}

have() { command -v "$1" >/dev/null 2>&1; }

# sha256 <file> — the hex digest alone, GNU or perl flavour.
sha256() {
  if have sha256sum; then
    sha256sum "$1" | awk '{print $1}'
  elif have shasum; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    die "neither sha256sum nor shasum is on PATH"
  fi
}

host_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    *) die "unsupported architecture $(uname -m): releases are linux amd64/arm64" ;;
  esac
}

host_os() {
  case "$(uname -s)" in
    Linux) echo "linux" ;;
    *) die "unsupported OS $(uname -s): releases are linux amd64/arm64 only" ;;
  esac
}

fetch() {  # fetch <url> <dest>
  local url="$1" dest="$2" rc=0
  case "$url" in
    http://*) warn "downloading over plain HTTP: $url" ;;
    https://*) ;;
    *) die "not an http(s) URL: $url" ;;
  esac
  log "downloading $url"
  if have curl; then
    curl -fsSL --location --retry 2 --max-time 300 -o "$dest" "$url" || rc=$?
  elif have wget; then
    wget -q -O "$dest" "$url" || rc=$?
  else
    die "curl or wget is required to download $url"
  fi
  if [[ $rc -ne 0 ]]; then
    rm -f "$dest"
    die "download failed (exit $rc): $url"
  fi
}

is_elf() {
  local magic
  magic="$(dd if="$1" bs=1 count=4 2>/dev/null | od -An -tx1 | tr -d ' \n')" || return 1
  [[ "$magic" == "7f454c46" ]]
}

# ------------------------------------------------------------------- setup ---

STAGE="$(mktemp -d -t kintsugi-install.XXXXXX)"
trap 'rm -rf "$STAGE"' EXIT

# A repo checkout is what MODE=auto prefers: it needs no network and no
# release host, and it builds exactly this tree.
if [[ "$MODE" == "auto" ]]; then
  if [[ -f "$SOURCE_DIR/go.mod" && -d "$SOURCE_DIR/cmd/kintsugi" ]]; then
    MODE="repo"
    REPO_DIR="$SOURCE_DIR"
  else
    MODE="tarball"
  fi
fi

if [[ "$MODE" == "repo" ]]; then
  [[ -n "$REPO_DIR" ]] || REPO_DIR="$SOURCE_DIR"
  [[ -f "$REPO_DIR/go.mod" ]] || die "$REPO_DIR is not a kintsugi checkout (no go.mod)"
  [[ -d "$REPO_DIR/cmd/kintsugi" ]] || die "$REPO_DIR has no cmd/kintsugi"
  need go
fi

# ------------------------------------------------------------- build/fetch ---

CANDIDATE=""   # the staged binary that is about to be installed

build_from_repo() {
  log "building from $REPO_DIR (go install ./cmd/kintsugi)"
  # GOBIN keeps the artifact in the staging directory instead of the operator's
  # $(go env GOPATH)/bin, so what is checksummed is what gets installed.
  if [[ -n "$VERSION" ]]; then
    (cd "$REPO_DIR" && GOBIN="$STAGE" CGO_ENABLED=0 \
      go install -trimpath -ldflags "-X main.version=${VERSION#v}" ./cmd/kintsugi)
  else
    (cd "$REPO_DIR" && GOBIN="$STAGE" CGO_ENABLED=0 \
      go install -trimpath ./cmd/kintsugi)
  fi
  CANDIDATE="$STAGE/$BIN_NAME"
  [[ -f "$CANDIDATE" ]] || die "go install produced no $CANDIDATE"
}

fetch_tarball() {
  local url="$TARBALL" manifest=""
  if [[ -z "$url" ]]; then
    [[ -n "$VERSION" ]] || die "--version or --tarball is required outside a checkout"
    [[ -n "$RELEASE_BASE_URL" ]] || die "no release host configured: pass --tarball <path|url>, or set KINTSUGI_RELEASE_BASE_URL (placeholder shape: $RELEASE_URL_HINT)"
    local os arch base tag
    os="$(host_os)"; arch="$(host_arch)"
    tag="$VERSION"; base="${VERSION#v}"
    url="${RELEASE_BASE_URL%/}/${tag}/${BIN_NAME}_${base}_${os}_${arch}.tar.gz"
    manifest="${RELEASE_BASE_URL%/}/${tag}/SHA256SUMS"
  fi

  local archive="$STAGE/$(basename "$url")"
  case "$url" in
    /*|./*)
      [[ -f "$url" ]] || die "no such tarball: $url"
      archive="$url"
      ;;
    *)
      fetch "$url" "$archive"
      # The release manifest is fetched alongside the archive so a downloaded
      # build always has something to be verified against. A missing manifest
      # is not fatal here — it becomes fatal at verification, which is the
      # honest place to refuse an unverifiable download.
      if [[ -n "$manifest" ]]; then
        if ( fetch "$manifest" "$STAGE/SHA256SUMS" ) >/dev/null 2>&1; then
          CHECKSUM_FILE="$STAGE/SHA256SUMS"
          log "release manifest downloaded ($(basename "$manifest"))"
        else
          warn "no release manifest at $manifest — refusing later unless --checksum-file is given"
        fi
      fi
      ;;
  esac

  [[ -f "$archive" ]] || die "download produced no archive"

  # A manifest sitting next to the archive is the one that belongs to it:
  # SHA256SUMS (goreleaser + scripts/checksums.sh) or <archive>.sha256.
  if [[ -z "$CHECKSUM_FILE" ]]; then
    local dir sibling
    dir="$(dirname "$archive")"
    for sibling in "$dir/SHA256SUMS" "$dir/SHA256SUMS.txt" "$archive.sha256"; do
      if [[ -f "$sibling" ]]; then
        CHECKSUM_FILE="$sibling"
        log "verifying against $(basename "$sibling")"
        break
      fi
    done
  fi

  # Verify the archive before unpacking it: an unverified tarball is never
  # extracted, so nothing from it can run.
  if [[ "$SKIP_CHECKSUM" -eq 1 && -z "$CHECKSUM_FILE" ]]; then
    warn "--skip-checksum: installing $archive WITHOUT any checksum verification"
  else
    verify_against_file "$archive" "${CHECKSUM_FILE:-}" || exit 1
  fi

  need tar
  log "unpacking $(basename "$archive")"
  tar -xzf "$archive" -C "$STAGE"
  local found
  # goreleaser puts the binary at the archive root; allow one nesting level so a
  # hand-rolled tarball works too.
  found="$(find "$STAGE" -maxdepth 3 -type f -name "$BIN_NAME" ! -path "$STAGE/*.tar.gz" -print -quit)"
  [[ -n "$found" ]] || die "no $BIN_NAME binary in $(basename "$archive")"
  CANDIDATE="$found"
}

# verify_against_file <artifact> [manifest] — the artifact's sha256 must appear
# in the manifest, whose name may be a bare path, a basename, or absent (a
# single-hash .sha256 file). With no manifest the caller must have passed
# --skip-checksum, which this function refuses.
verify_against_file() {
  local artifact="$1" manifest="${2:-}"
  if [[ -z "$manifest" || ! -f "$manifest" ]]; then
    if [[ "$SKIP_CHECKSUM" -eq 1 ]]; then
      warn "no checksum file to verify against — skipping verification"
      return 0
    fi
    die "no checksum to verify against: pass --checksum-file <SHA256SUMS|.sha256> (or --skip-checksum to install unverified, not recommended)"
  fi

  local actual expected name
  actual="$(sha256 "$artifact")"
  name="$(basename "$artifact")"
  expected="$(awk -v name="$name" '
    function is_hex(s) { return s ~ /^[0-9a-fA-F]{64}$/ }
    {
      line = $0
      sub(/\r$/, "", line)
      gsub(/\*/, "", line)
      n = split(line, f, /[ \t]+/)
      if (n == 1 && is_hex(f[1])) { print tolower(f[1]); exit }
      if (n == 2 && is_hex(f[1])) {
        if (f[2] == name || f[2] ~ ("(^|/)" name "$")) { print tolower(f[1]); exit }
      } else if (n == 2 && is_hex(f[2])) {
        if (f[1] == name || f[1] ~ ("(^|/)" name "$")) { print tolower(f[2]); exit }
      }
    }' "$manifest")"

  if [[ -z "$expected" ]]; then
    die "$(basename "$manifest") has no sha256 entry for $name"
  fi
  if [[ "$actual" != "$expected" ]]; then
    die "checksum mismatch for $name
  expected $expected
  actual   $actual
refusing to install: the artifact is not what the manifest describes"
  fi
  log "sha256 verified: $actual ($name)"
}

# verify_via_checksums_script — the in-repo path: scripts/checksums.sh writes a
# manifest over the staging directory, then verifies it back. This proves the
# binary that is installed is byte-identical to the one that was built.
verify_via_checksums_script() {
  local script="$REPO_DIR/scripts/checksums.sh"
  [[ -f "$script" ]] || die "no scripts/checksums.sh in $REPO_DIR — pass --checksum-file <SHA256SUMS> or --skip-checksum"
  log "checksumming the build (scripts/checksums.sh)"
  bash "$script" "$STAGE" >/dev/null
  bash "$script" --check "$STAGE"
  log "sha256 verified: $(sha256 "$CANDIDATE") ($BIN_NAME)"
}

# ------------------------------------------------------------------ install ---

# preflight_target refuses a foreign occupant at the install path *before* the
# build or the download starts, so no work is wasted on a run that cannot
# finish. install_candidate re-checks, because a dry run and the real run must
# reach the same verdict.
preflight_target() {
  local target="$PREFIX/$BIN_NAME"
  if [[ -e "$target" || -L "$target" ]] && [[ -z "$(existing_identity "$target")" ]]; then
    refuse_foreign_binary "$target"
  fi
}

existing_identity() {  # existing_identity <path> → "kintsugi <version>" or ""
  local path="$1" out=""
  # A symlink is the operator's own arrangement and is never followed, let
  # alone replaced: it reports as unidentified and is refused. Only ELF
  # binaries are executed here, and only ones this user owns without a
  # setuid/setgid bit — an unidentified script, a data file, or someone else's
  # privileged binary at the target path is reported and refused, never run.
  if [[ -L "$path" ]]; then
    return 0
  fi
  if ! is_elf "$path" || ! safe_to_probe "$path"; then
    return 0
  fi
  out="$("$path" version 2>/dev/null || true)"
  case "$out" in
    kintsugi\ *) printf '%s' "${out#kintsugi }" ;;
  esac
}

# safe_to_probe <path> — is it acceptable to execute this file once, with the
# argument `version`, in order to ask it what it is? Refuses anything this user
# does not own and anything carrying a setuid/setgid bit.
safe_to_probe() {
  local path="$1" owner perms special
  owner="$(stat -c '%U' "$path" 2>/dev/null || stat -f '%Su' "$path" 2>/dev/null || echo "")"
  if [[ -n "$owner" && "$owner" != "$(id -un)" ]]; then
    return 1
  fi
  perms="$(stat -c '%a' "$path" 2>/dev/null || stat -f '%Lp' "$path" 2>/dev/null || echo "")"
  if [[ -n "$perms" ]]; then
    # Everything above the owner/triplet digits is the setuid/setgid/sticky
    # field: 0755 → "0", 4755 → "4", 2755 → "2". Any non-zero bit there means
    # the file is privileged and must not be run.
    special="$(printf '%s' "${perms:0:${#perms}-3}" | tr -d '0')"
    [[ -z "$special" ]] || return 1
  fi
  return 0
}

refuse_foreign_binary() {
  local target="$1" what=""
  if [[ -L "$target" ]]; then
    what="a symlink to $(readlink "$target")"
  elif ! is_elf "$target"; then
    what="a non-ELF file (not executed)"
  elif ! safe_to_probe "$target"; then
    what="an ELF binary that is not owned by $(id -un) or carries a setuid/setgid bit (not executed)"
  else
    what="an ELF binary that does not report itself as kintsugi"
  fi
  die "$target already exists and is $what.
install.sh will not overwrite something that is not kintsugi. Move it aside and re-run:
  mv '$target' '$target.bak' && scripts/install.sh"
}

install_candidate() {
  local target="$PREFIX/$BIN_NAME"

  [[ -f "$CANDIDATE" ]] || die "nothing staged to install"
  is_elf "$CANDIDATE" || die "$CANDIDATE is not an ELF binary"

  # Refuse a foreign occupant before touching anything.
  if [[ -e "$target" || -L "$target" ]]; then
    local existing
    existing="$(existing_identity "$target")"
    if [[ -z "$existing" ]]; then
      refuse_foreign_binary "$target"
    fi
    # Idempotent on *bytes*, not on the version string: a rebuild of the same
    # version is still installed, while re-running this script straight after a
    # successful install is a no-op that leaves the binary untouched.
    if [[ "$DRY_RUN" -eq 0 && "$(sha256 "$target")" == "$(sha256 "$CANDIDATE")" ]]; then
      log "$BIN_NAME $existing is already installed at $target — nothing to do"
      return 0
    fi
    log "replacing $BIN_NAME $existing"
  fi

  if [[ "$DRY_RUN" -eq 1 ]]; then
    log "dry run: would install $CANDIDATE → $target"
    return 0
  fi

  if ! mkdir -p "$PREFIX"; then
    die "could not create the install directory $PREFIX"
  fi
  chmod 0755 "$PREFIX" 2>/dev/null || warn "could not chmod $PREFIX"
  # Copy into the destination directory under a private name and rename over the
  # target: a partially written binary can never be what PATH resolves to, and
  # the rename is atomic on one filesystem.
  local tmp="$target.tmp.$$"
  rm -f "$tmp"
  if ! cp "$CANDIDATE" "$tmp"; then
    rm -f "$tmp"
    die "could not write $tmp"
  fi
  chmod 0755 "$tmp"
  if ! mv -f "$tmp" "$target"; then
    rm -f "$tmp"
    die "could not install $target"
  fi

  local installed
  installed="$("$target" version 2>/dev/null || true)"
  if [[ -z "$installed" ]]; then
    die "installed $target but it does not print a version"
  fi
  log "installed $target"
  log "$installed"

  case ":$PATH:" in
    *":$PREFIX:"*) ;;
    *) warn "$PREFIX is not on PATH — add it: export PATH=\"$PREFIX:\$PATH\"" ;;
  esac
}

# --------------------------------------------------------------------- main ---

# Refuse a foreign occupant before spending time on a build or a download.
preflight_target

case "$MODE" in
  repo) build_from_repo ;;
  tarball) fetch_tarball ;;
  *) die "internal: unknown mode $MODE" ;;
esac

# Repo builds are verified through scripts/checksums.sh unless the operator
# handed over an explicit manifest; downloaded tarballs were verified above.
if [[ "$MODE" == "repo" && -z "$CHECKSUM_FILE" ]]; then
  if [[ "$SKIP_CHECKSUM" -eq 1 ]]; then
    warn "--skip-checksum: not verifying the build"
  else
    verify_via_checksums_script
  fi
elif [[ -n "$CHECKSUM_FILE" && "$MODE" == "repo" ]]; then
  verify_against_file "$CANDIDATE" "$CHECKSUM_FILE"
fi

install_candidate
