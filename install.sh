#!/bin/sh
# ORBIT installer / updater.
#
#   curl -fsSL https://orbit.grewelltech.com/install.sh | sh
#
# Downloads the release archive for this OS/arch from GitHub Releases, verifies
# its SHA-256 against the release's signed checksums.txt, and installs the
# `orbit` binary. Re-running it upgrades in place.
#
# Supply-chain verification, strongest first; the installer uses what is
# available and always enforces at least the checksum:
#   1. SHA-256   — every artifact's hash is listed in checksums.txt; the
#                  archive is checked against it. Always enforced.
#   2. cosign    — checksums.txt is signed (keyless, via the release workflow's
#                  GitHub OIDC identity, logged in Rekor). Verified when cosign
#                  is installed; required with --require-signature.
#   3. SLSA      — build provenance is attested; verify out of band with
#                  `gh attestation verify <archive> --repo grewelltech/orbit`.
#
# Environment / flags:
#   --version <vX.Y.Z> | ORBIT_VERSION   install a specific release (default: latest)
#   --bin-dir <dir>    | ORBIT_BIN_DIR   install location (default: /usr/local/bin, else ~/.local/bin)
#   --require-signature                  fail unless the cosign signature verifies
#   --no-verify-signature                skip cosign even if present (SHA-256 still enforced)
#   --help

set -eu

REPO="grewelltech/orbit"
BIN="orbit"
GITHUB="https://github.com"
# cosign identity the release workflow signs as; verification pins this so a
# signature from any other workflow or repo is rejected.
CERT_IDENTITY_RE="^https://github.com/${REPO}/.github/workflows/release.yml@refs/tags/"
OIDC_ISSUER="https://token.actions.githubusercontent.com"

VERSION="${ORBIT_VERSION:-latest}"
BIN_DIR="${ORBIT_BIN_DIR:-}"
REQUIRE_SIG=0
VERIFY_SIG=1

log() { printf '%s\n' "orbit-install: $*" >&2; }
die() { printf '%s\n' "orbit-install: error: $*" >&2; exit 1; }

usage() {
  cat >&2 <<'EOF'
ORBIT installer / updater

  curl -fsSL https://orbit.grewelltech.com/install.sh | sh

Options:
  --version <vX.Y.Z>     install a specific release (default: latest)
  --bin-dir <dir>        install location (default: /usr/local/bin, else ~/.local/bin)
  --require-signature    fail unless the cosign signature over checksums.txt verifies
  --no-verify-signature  skip cosign even if installed (SHA-256 is still enforced)
  -h, --help             show this help

Environment: ORBIT_VERSION, ORBIT_BIN_DIR mirror --version and --bin-dir.
EOF
  exit 0
}

while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="${2:?--version needs a value}"; shift 2 ;;
    --version=*) VERSION="${1#*=}"; shift ;;
    --bin-dir) BIN_DIR="${2:?--bin-dir needs a value}"; shift 2 ;;
    --bin-dir=*) BIN_DIR="${1#*=}"; shift ;;
    --require-signature) REQUIRE_SIG=1; shift ;;
    --no-verify-signature) VERIFY_SIG=0; shift ;;
    -h|--help) usage ;;
    *) die "unknown argument: $1 (try --help)" ;;
  esac
done

# --require-signature demands verification; --no-verify-signature disables it.
# Together they are contradictory, so reject rather than silently skip.
if [ "$REQUIRE_SIG" -eq 1 ] && [ "$VERIFY_SIG" -eq 0 ]; then
  die "--require-signature and --no-verify-signature cannot be used together"
fi

# --- tools -----------------------------------------------------------------
have() { command -v "$1" >/dev/null 2>&1; }

if have curl; then
  dl() { curl -fsSL "$1" -o "$2"; }
  final_url() { curl -fsSLI -o /dev/null -w '%{url_effective}' "$1"; }
elif have wget; then
  dl() { wget -qO "$2" "$1"; }
  final_url() { wget -q --max-redirect=10 -S -O /dev/null "$1" 2>&1 | awk '/^  Location: /{u=$2} END{print u}'; }
else
  die "need curl or wget"
fi

if have sha256sum; then
  sha256() { sha256sum "$1" | awk '{print $1}'; }
elif have shasum; then
  sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
else
  die "need sha256sum or shasum"
fi

# verify_checksums_signature checks the cosign keyless signature over
# checksums.txt, pinning the release workflow's identity. It no-ops (with a
# note) when cosign or the bundle is absent, unless --require-signature; the
# SHA-256 check is enforced regardless by the caller.
verify_checksums_signature() {
  d="$1"
  if [ "$VERIFY_SIG" -eq 0 ]; then
    log "skipping signature verification (--no-verify-signature); sha256 still enforced"
    return 0
  fi
  if ! have cosign; then
    [ "$REQUIRE_SIG" -eq 1 ] && die "--require-signature set but cosign is not installed"
    log "cosign not installed; skipping signature verification (sha256 still enforced). See https://orbit.grewelltech.com for full verification."
    return 0
  fi
  if ! dl "${base}/checksums.txt.cosign.bundle" "${d}/checksums.txt.cosign.bundle" 2>/dev/null; then
    [ "$REQUIRE_SIG" -eq 1 ] && die "--require-signature set but this release has no cosign bundle"
    log "no cosign bundle in this release; skipping signature verification"
    return 0
  fi
  log "verifying checksums.txt signature (cosign, keyless)"
  if cosign verify-blob \
      --bundle "${d}/checksums.txt.cosign.bundle" \
      --certificate-identity-regexp "${CERT_IDENTITY_RE}" \
      --certificate-oidc-issuer "${OIDC_ISSUER}" \
      "${d}/checksums.txt" >/dev/null 2>&1; then
    log "cosign signature ok"
  else
    die "cosign signature verification FAILED for checksums.txt — refusing to install"
  fi
}

# --- platform --------------------------------------------------------------
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$os" in
  linux) ;;
  darwin) die "macOS builds are not published yet (linux only); build from source: git clone https://github.com/${REPO} && (cd orbit && go build ./cmd/orbit)" ;;
  *) die "unsupported OS: $os (linux only)" ;;
esac
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) die "unsupported architecture: $arch (amd64, arm64)" ;;
esac

# --- resolve version -------------------------------------------------------
if [ "$VERSION" = "latest" ]; then
  log "resolving the latest release"
  resolved=$(final_url "${GITHUB}/${REPO}/releases/latest") || die "could not reach GitHub"
  VERSION=${resolved##*/tag/}   # .../releases/tag/vX.Y.Z
  case "$VERSION" in
    v*) ;;
    *) die "could not resolve the latest release tag (got '$resolved')" ;;
  esac
fi
case "$VERSION" in
  v*) ;;
  *) VERSION="v${VERSION}" ;;   # accept "1.2.3" as "v1.2.3"
esac

pkg="${BIN}-${VERSION}-${os}-${arch}"
archive="${pkg}.tar.gz"
base="${GITHUB}/${REPO}/releases/download/${VERSION}"

# --- download into a scratch dir -------------------------------------------
tmp=$(mktemp -d "${TMPDIR:-/tmp}/orbit-install.XXXXXX") || die "mktemp failed"
cleanup() { rm -rf "$tmp"; }
trap cleanup EXIT INT TERM

log "downloading ${archive} (${VERSION}, ${os}/${arch})"
dl "${base}/${archive}" "${tmp}/${archive}" || die "download failed: ${base}/${archive}"

# --- verify ----------------------------------------------------------------
got=$(sha256 "${tmp}/${archive}")
if dl "${base}/checksums.txt" "${tmp}/checksums.txt" 2>/dev/null; then
  want=$(awk -v f="${archive}" '$2 == f || $2 == "*"f {print $1}' "${tmp}/checksums.txt" | head -n1)
  [ -n "$want" ] || die "checksums.txt has no entry for ${archive}"
  verify_checksums_signature "${tmp}"
else
  log "no checksums.txt; falling back to ${archive}.sha256"
  [ "$REQUIRE_SIG" -eq 1 ] && die "--require-signature set, but this release has no signed checksums.txt"
  dl "${base}/${archive}.sha256" "${tmp}/${archive}.sha256" || die "no checksum published for ${archive}"
  want=$(awk '{print $1}' "${tmp}/${archive}.sha256" | head -n1)
fi
[ "$got" = "$want" ] || die "checksum mismatch for ${archive}: got ${got}, want ${want}"
log "sha256 ok"

# --- install ---------------------------------------------------------------
tar -xzf "${tmp}/${archive}" -C "${tmp}" || die "could not extract ${archive}"
src="${tmp}/${pkg}/${BIN}"
if [ ! -f "$src" ]; then
  src=$(find "$tmp" -type f -name "$BIN" 2>/dev/null | head -n1)
fi
[ -n "$src" ] && [ -f "$src" ] || die "the archive did not contain a ${BIN} binary"
chmod +x "$src"

if [ -z "$BIN_DIR" ]; then
  if [ -w /usr/local/bin ] 2>/dev/null || [ "$(id -u)" = "0" ]; then
    BIN_DIR=/usr/local/bin
  else
    BIN_DIR="${HOME}/.local/bin"
  fi
fi
if ! mkdir -p "$BIN_DIR" 2>/dev/null; then
  # A protected --bin-dir (e.g. /opt/orbit/bin) that does not exist yet needs
  # sudo to create, the same as it needs sudo to install into.
  if have sudo; then
    log "creating ${BIN_DIR} with sudo"
    sudo mkdir -p "$BIN_DIR" || die "cannot create ${BIN_DIR} (even with sudo)"
  else
    die "cannot create ${BIN_DIR} and sudo not found; set --bin-dir to a writable directory"
  fi
fi

dest="${BIN_DIR}/${BIN}"
if [ -w "$BIN_DIR" ]; then
  install -m 0755 "$src" "$dest"
elif have sudo; then
  log "installing to ${BIN_DIR} with sudo"
  sudo install -m 0755 "$src" "$dest"
else
  die "no write permission to ${BIN_DIR} and sudo not found; set --bin-dir to a writable directory"
fi

log "installed ${BIN} ${VERSION} to ${dest}"

# A running orbit keeps executing the binary it started with, so an
# `orbit serve` started before this install still serves the old version
# until it is restarted. Name each one, and its systemd unit when it has one.
# /proc/<pid>/cmdline and cgroup are world-readable, so this sees other
# users' processes too.
running_notes() {
  [ -d /proc ] && have pgrep || return 0
  for pid in $(pgrep -x "$BIN" 2>/dev/null); do
    cmd=$(tr '\0' ' ' < "/proc/${pid}/cmdline" 2>/dev/null) || continue
    cg=$(head -n1 "/proc/${pid}/cgroup" 2>/dev/null)
    unit=$(printf '%s\n' "$cg" | sed -n 's#.*/\([^/]*\.service\)$#\1#p')
    case "$cg" in
      */user@*) restart="systemctl --user restart ${unit}" ;;
      *) restart="sudo systemctl restart ${unit}" ;;
    esac
    log "note: pid ${pid} (${cmd% }) is still running the binary it started with; restart it to run ${VERSION}${unit:+ (${restart})}"
  done
}
running_notes
case ":${PATH}:" in
  *":${BIN_DIR}:"*) ;;
  *) log "note: ${BIN_DIR} is not on your PATH; add it, e.g. export PATH=\"${BIN_DIR}:\$PATH\"" ;;
esac
"$dest" version 2>/dev/null || "$dest" --version 2>/dev/null || true
