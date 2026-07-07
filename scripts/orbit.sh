#!/usr/bin/env bash
#
# ORBIT service manager — install, upgrade, or uninstall the ORBIT API server
# as a systemd service, in one script.
#
# Usage (run as root):
#   sudo bash scripts/orbit.sh install
#   sudo bash scripts/orbit.sh upgrade
#   sudo bash scripts/orbit.sh uninstall [--purge]
#   sudo bash scripts/orbit.sh status
#
# Or straight from GitHub:
#   curl -fsSL https://raw.githubusercontent.com/grewelltech/orbit/main/scripts/orbit.sh | sudo bash -s -- install
#
# Environment:
#   REF=main           git ref to clone + build when not run from a checkout (default: main)
#   VERSION=v1.2.3     download a released binary instead of building (if releases exist)
#   ORBIT_ARGS="..."   args written to /etc/orbit/orbit.env on a fresh install
#                      (e.g. "--listen 0.0.0.0:8412 --core-profile sdcore")
#
set -euo pipefail

REPO="grewelltech/orbit"
SERVICE_NAME="orbit"
INSTALL_DIR="/usr/local/bin"
SYSTEMD_DIR="/etc/systemd/system"
CONFIG_DIR="/etc/orbit"
BIN_PATH="${INSTALL_DIR}/orbit"
UNIT_PATH="${SYSTEMD_DIR}/${SERVICE_NAME}.service"
ENV_PATH="${CONFIG_DIR}/orbit.env"

if [[ -t 1 ]]; then
	RED=$'\033[0;31m'; GREEN=$'\033[0;32m'; YELLOW=$'\033[1;33m'; NC=$'\033[0m'
else
	RED=""; GREEN=""; YELLOW=""; NC=""
fi
log_info() { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1" >&2; }

require_root() {
	if [[ "${EUID:-$(id -u)}" -ne 0 ]]; then
		log_error "must run as root — use sudo."
		exit 1
	fi
}

require_systemd() {
	if ! command -v systemctl >/dev/null 2>&1; then
		log_error "systemctl not found — this manager targets systemd Linux."
		exit 1
	fi
}

# acquire_binary sets ACQUIRED_BIN to a path to a ready orbit binary, by (in
# order): downloading a release when VERSION is set; building from a local
# checkout; or cloning REF and building. Building needs Go (and git for a clone).
ACQUIRED_BIN=""
acquire_binary() {
	local here repo_root
	here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
	repo_root="$(cd "$here/.." && pwd)"

	if [[ -n "${VERSION:-}" ]]; then
		acquire_release "$VERSION"
		return
	fi
	if [[ -f "$repo_root/go.mod" ]] && grep -q "module github.com/bgrewell/orbit" "$repo_root/go.mod" 2>/dev/null; then
		log_info "Building from local checkout ($repo_root)"
		build_at "$repo_root"
		ACQUIRED_BIN="$repo_root/bin/orbit"
		return
	fi
	local ref="${REF:-main}" tmp
	tmp="$(mktemp -d)"
	log_info "Cloning ${REPO}@${ref} and building"
	git clone --depth 1 --branch "$ref" "https://github.com/${REPO}.git" "$tmp/src" 2>/dev/null ||
		git clone "https://github.com/${REPO}.git" "$tmp/src"
	( cd "$tmp/src" && git checkout "$ref" 2>/dev/null || true )
	[[ -n "${SUDO_USER:-}" ]] && chown -R "$SUDO_USER" "$tmp"
	build_at "$tmp/src"
	ACQUIRED_BIN="$tmp/src/bin/orbit"
}

# build_at builds bin/orbit in dir. Under sudo it builds as the invoking user
# (SUDO_USER), whose login shell has the Go toolchain and module cache — root's
# PATH usually does not. Falls back to root with common Go locations on PATH.
build_at() {
	local dir="$1"
	if [[ -n "${SUDO_USER:-}" ]] && sudo -u "$SUDO_USER" bash -lc 'command -v go >/dev/null 2>&1'; then
		log_info "Building as $SUDO_USER"
		sudo -u "$SUDO_USER" bash -lc "cd \"$dir\" && make build" >/dev/null
		return
	fi
	local g
	for g in /usr/local/go/bin /snap/bin "${HOME:-/root}/go/bin"; do
		[[ -x "$g/go" ]] && export PATH="$g:$PATH"
	done
	if ! command -v go >/dev/null 2>&1; then
		log_error "Go toolchain not found — install Go, or set VERSION= to download a release."
		exit 1
	fi
	( cd "$dir" && make build >/dev/null )
}

acquire_release() {
	local version="$1" arch os url tmp
	case "$(uname -m)" in
		x86_64) arch=amd64 ;;
		aarch64 | arm64) arch=arm64 ;;
		*) log_error "unsupported arch $(uname -m)"; exit 1 ;;
	esac
	os="$(uname -s | tr '[:upper:]' '[:lower:]')"
	url="https://github.com/${REPO}/releases/download/${version}/orbit_${version}_${os}_${arch}.tar.gz"
	tmp="$(mktemp -d)"
	log_info "Downloading release ${version} ($os/$arch)"
	if ! curl -fsSL "$url" -o "$tmp/orbit.tgz"; then
		log_error "release download failed ($url). No published release? Build instead (unset VERSION)."
		exit 1
	fi
	tar -xzf "$tmp/orbit.tgz" -C "$tmp"
	ACQUIRED_BIN="$(find "$tmp" -type f -name orbit | head -1)"
	[[ -n "$ACQUIRED_BIN" ]] || { log_error "no orbit binary in the release archive"; exit 1; }
}

write_unit() {
	log_info "Writing unit  → $UNIT_PATH"
	cat >"$UNIT_PATH" <<'EOF'
[Unit]
Description=ORBIT — Open Radio Benchmark and Integration Testbed (API server)
Documentation=https://github.com/grewelltech/orbit
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=-/etc/orbit/orbit.env
ExecStart=/usr/local/bin/orbit serve $ORBIT_ARGS
Restart=on-failure
RestartSec=2
DynamicUser=yes
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
EOF
}

write_env_if_absent() {
	if [[ -f "$ENV_PATH" ]]; then
		log_info "Keeping config → $ENV_PATH"
		return
	fi
	log_info "Writing config → $ENV_PATH"
	mkdir -p "$CONFIG_DIR"
	cat >"$ENV_PATH" <<EOF
# Args passed to \`orbit serve\` (see \`orbit serve --help\`). Empty = listen on
# 127.0.0.1:8412 with the strict-3gpp core profile. The API carries subscriber
# credentials — expose beyond loopback only behind TLS / a trusted network.
# Examples: --listen 0.0.0.0:8412 · --core-profile sdcore · --log-level debug
ORBIT_ARGS=${ORBIT_ARGS:-}
EOF
}

install_binary() {
	log_info "Installing    → $BIN_PATH ($("$ACQUIRED_BIN" version 2>/dev/null || echo orbit))"
	install -Dm755 "$ACQUIRED_BIN" "$BIN_PATH"
}

do_install() {
	require_root
	require_systemd
	acquire_binary
	install_binary
	write_unit
	write_env_if_absent
	systemctl daemon-reload
	systemctl enable --now "${SERVICE_NAME}.service"
	echo
	log_info "orbit is installed and running as a service."
	echo "  status:  systemctl status orbit"
	echo "  logs:    journalctl -u orbit -f"
	echo "  config:  $ENV_PATH  (then: systemctl restart orbit)"
}

do_upgrade() {
	require_root
	require_systemd
	if [[ ! -f "$UNIT_PATH" ]]; then
		log_error "orbit is not installed — run: $0 install"
		exit 1
	fi
	acquire_binary
	install_binary
	write_unit # refresh the unit in case it changed
	systemctl daemon-reload
	systemctl restart "${SERVICE_NAME}.service"
	log_info "Upgraded and restarted ($("$BIN_PATH" version 2>/dev/null || echo orbit))."
}

do_uninstall() {
	require_root
	require_systemd
	local purge="${1:-}"
	if systemctl is-active --quiet "$SERVICE_NAME" 2>/dev/null; then
		log_info "Stopping service"
		systemctl stop "$SERVICE_NAME"
	fi
	if systemctl is-enabled --quiet "$SERVICE_NAME" 2>/dev/null; then
		log_info "Disabling service"
		systemctl disable "$SERVICE_NAME"
	fi
	[[ -f "$UNIT_PATH" ]] && { log_info "Removing unit    $UNIT_PATH"; rm -f "$UNIT_PATH"; }
	[[ -f "$BIN_PATH" ]] && { log_info "Removing binary  $BIN_PATH"; rm -f "$BIN_PATH"; }
	if [[ "$purge" == "--purge" ]]; then
		[[ -d "$CONFIG_DIR" ]] && { log_info "Removing config  $CONFIG_DIR"; rm -rf "$CONFIG_DIR"; }
	elif [[ -d "$CONFIG_DIR" ]]; then
		log_info "Keeping config   $CONFIG_DIR (use --purge to remove)"
	fi
	systemctl daemon-reload 2>/dev/null || true
	log_info "Uninstalled."
}

do_status() {
	systemctl status "$SERVICE_NAME" --no-pager 2>/dev/null || true
	[[ -x "$BIN_PATH" ]] && echo "binary: $("$BIN_PATH" version 2>/dev/null)"
}

usage() {
	sed -n '2,20p' "${BASH_SOURCE[0]}" | sed 's/^#//'
}

main() {
	case "${1:-}" in
		install) do_install ;;
		upgrade) do_upgrade ;;
		uninstall) do_uninstall "${2:-}" ;;
		status) do_status ;;
		"" | -h | --help | help) usage ;;
		*) log_error "unknown command: $1"; usage; exit 1 ;;
	esac
}

main "$@"
