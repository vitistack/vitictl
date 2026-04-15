#!/usr/bin/env bash
# install.sh — download and install the `viti` CLI (and optionally viti-gui).
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/vitistack/vitictl/main/install.sh | bash
#   curl -fsSL https://raw.githubusercontent.com/vitistack/vitictl/main/install.sh | bash -s -- --with-gui
#   curl -fsSL https://raw.githubusercontent.com/vitistack/vitictl/main/install.sh | bash -s -- --version v0.2.0
#   ./install.sh --prefix "$HOME/.local/bin" --skip-cosign
#
# Flags:
#   --version <tag>      install a specific release (default: latest)
#   --prefix  <dir>      install directory (default: /usr/local/bin if writable,
#                        else $HOME/.local/bin)
#   --with-gui           also install the viti-gui plugin binary
#   --skip-cosign        skip Sigstore signature verification (SHA-256 still enforced)
#   --skip-checksum      skip SHA-256 verification (not recommended)
#   -h | --help          show this help

set -euo pipefail

REPO="vitistack/vitictl"
BINARY="viti"
BINARY_GUI="viti-gui"
VERSION=""
PREFIX=""
WITH_GUI=0
SKIP_COSIGN=0
SKIP_CHECKSUM=0

log()  { printf '==> %s\n' "$*" >&2; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

usage() {
  sed -n '2,17p' "$0" | sed 's/^# \{0,1\}//'
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --version)       VERSION="${2:-}"; shift 2 ;;
    --version=*)     VERSION="${1#*=}"; shift ;;
    --prefix)        PREFIX="${2:-}"; shift 2 ;;
    --prefix=*)      PREFIX="${1#*=}"; shift ;;
    --with-gui)      WITH_GUI=1; shift ;;
    --skip-cosign)   SKIP_COSIGN=1; shift ;;
    --skip-checksum) SKIP_CHECKSUM=1; shift ;;
    -h|--help)       usage; exit 0 ;;
    *)               die "unknown flag: $1 (see --help)" ;;
  esac
done

require() {
  command -v "$1" >/dev/null 2>&1 || die "required tool not found on PATH: $1"
}

require curl
require tar
require mktemp
require uname

# -- Detect platform ---------------------------------------------------------
os_raw=$(uname -s)
arch_raw=$(uname -m)
case "$os_raw" in
  Linux)   OS=linux ;;
  Darwin)  OS=darwin ;;
  *) die "unsupported OS: $os_raw (this installer targets Linux/macOS; Windows users: download from https://github.com/${REPO}/releases)" ;;
esac
case "$arch_raw" in
  x86_64|amd64) ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) die "unsupported architecture: $arch_raw" ;;
esac

# -- Resolve version ---------------------------------------------------------
if [[ -z "$VERSION" ]]; then
  log "resolving latest release"
  VERSION=$(curl -fsSL \
    -H "Accept: application/vnd.github+json" \
    "https://api.github.com/repos/${REPO}/releases/latest" \
    | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' \
    | head -n1)
  [[ -n "$VERSION" ]] || die "could not determine latest release tag for ${REPO}"
fi

BASE_URL="https://github.com/${REPO}/releases/download/${VERSION}"
CHECKSUMS="viti-${VERSION}-SHA256SUMS"

# -- Pick install prefix -----------------------------------------------------
if [[ -z "$PREFIX" ]]; then
  if [[ -w /usr/local/bin ]] || { [[ $(id -u) -eq 0 ]] && [[ -d /usr/local/bin ]]; }; then
    PREFIX=/usr/local/bin
  else
    PREFIX="$HOME/.local/bin"
  fi
fi
mkdir -p "$PREFIX" || die "cannot create install prefix: $PREFIX"
[[ -w "$PREFIX" ]] || die "install prefix is not writable: $PREFIX (try --prefix \$HOME/.local/bin)"

# -- Download the shared SHA256SUMS file once --------------------------------
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

if [[ $SKIP_CHECKSUM -eq 0 ]]; then
  curl -fsSL "${BASE_URL}/${CHECKSUMS}" -o "${TMP}/${CHECKSUMS}"
fi

# -- Pick a SHA-256 command up front -----------------------------------------
if command -v sha256sum >/dev/null 2>&1; then
  SHA_CMD=(sha256sum)
elif command -v shasum >/dev/null 2>&1; then
  SHA_CMD=(shasum -a 256)
else
  SHA_CMD=()
fi

# install_one <binary-name>
# Downloads, verifies, extracts, and installs one release artifact whose
# binary name is <binary-name> (e.g. "viti" or "viti-gui").
install_one() {
  local bin="$1"
  local asset="${bin}-${VERSION}-${OS}-${ARCH}.tar.gz"

  log "installing ${bin} ${VERSION} for ${OS}/${ARCH}"
  log "downloading ${asset}"
  curl -fSL --progress-bar "${BASE_URL}/${asset}" -o "${TMP}/${asset}"

  if [[ $SKIP_CHECKSUM -eq 0 ]]; then
    [[ ${#SHA_CMD[@]} -gt 0 ]] || die "no sha256sum or shasum found on PATH"
    local expected actual
    expected=$(awk -v f="${asset}" '$2 == f || $2 == "*"f {print $1; exit}' "${TMP}/${CHECKSUMS}")
    [[ -n "$expected" ]] || die "no SHA-256 entry for ${asset} in ${CHECKSUMS}"
    actual=$("${SHA_CMD[@]}" "${TMP}/${asset}" | awk '{print $1}')
    [[ "$expected" == "$actual" ]] || die "SHA-256 mismatch for ${asset}: expected ${expected}, got ${actual}"
    log "SHA-256 ok (${bin})"
  fi

  if [[ $SKIP_COSIGN -eq 0 ]]; then
    if ! command -v cosign >/dev/null 2>&1; then
      warn "cosign not found on PATH — skipping signature verification for ${bin}. Install cosign (https://docs.sigstore.dev/cosign/installation/) or re-run with --skip-cosign to silence this warning."
    else
      log "verifying Sigstore signature with cosign (${bin})"
      curl -fsSL "${BASE_URL}/${asset}.cosign.bundle" -o "${TMP}/${asset}.cosign.bundle"
      local identity_regex="^https://github.com/${REPO}/.github/workflows/release.yml@refs/tags/"
      cosign verify-blob \
        --bundle "${TMP}/${asset}.cosign.bundle" \
        --certificate-identity-regexp "$identity_regex" \
        --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
        "${TMP}/${asset}" >/dev/null
      log "cosign signature ok (${bin})"
    fi
  fi

  log "extracting ${bin}"
  tar -xzf "${TMP}/${asset}" -C "${TMP}"

  local staged="${TMP}/${bin}-${VERSION}-${OS}-${ARCH}/${bin}"
  [[ -f "$staged" ]] || die "archive layout unexpected: ${staged} not found"

  install -m 0755 "$staged" "${PREFIX}/${bin}"
  log "installed ${PREFIX}/${bin}"
}

[[ $SKIP_CHECKSUM -ne 0 ]] && warn "skipping SHA-256 verification"
[[ $SKIP_COSIGN   -ne 0 ]] && warn "skipping cosign signature verification"

install_one "$BINARY"
[[ $WITH_GUI -eq 1 ]] && install_one "$BINARY_GUI"

# -- PATH hint ---------------------------------------------------------------
case ":$PATH:" in
  *":${PREFIX}:"*) ;;
  *) warn "${PREFIX} is not on your PATH. Add it, e.g.:  export PATH=\"${PREFIX}:\$PATH\"" ;;
esac

"${PREFIX}/${BINARY}" --help >/dev/null 2>&1 \
  && log "run '${BINARY} --help' to get started" \
  || warn "${BINARY} installed but failed to run; check architecture mismatch or corrupted download"

if [[ $WITH_GUI -eq 1 ]]; then
  log "run 'viti gui' (viti will dispatch to ${BINARY_GUI})"
fi
