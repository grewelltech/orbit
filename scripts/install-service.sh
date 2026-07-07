#!/usr/bin/env bash
# Install ORBIT as a systemd service so the API server runs in the background
# and survives reboots — no more manual `orbit serve &`.
#
#   ./scripts/install-service.sh
#
# Re-runnable. Builds the binary if bin/orbit is missing. Uses sudo when not
# root. Does not overwrite an existing /etc/orbit/orbit.env.
set -euo pipefail

PREFIX="${PREFIX:-/usr/local}"
BIN="$PREFIX/bin/orbit"
UNIT=/etc/systemd/system/orbit.service
ENVFILE=/etc/orbit/orbit.env
here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if ! command -v systemctl >/dev/null 2>&1; then
	echo "systemctl not found — this installer targets systemd Linux." >&2
	exit 1
fi

SUDO=""
[ "$(id -u)" -ne 0 ] && SUDO="sudo"

if [ ! -x "$here/bin/orbit" ]; then
	echo "Building orbit…"
	( cd "$here" && make build )
fi

echo "Installing binary  → $BIN"
$SUDO install -Dm755 "$here/bin/orbit" "$BIN"

echo "Installing unit    → $UNIT"
$SUDO install -Dm644 "$here/packaging/systemd/orbit.service" "$UNIT"

if [ ! -f "$ENVFILE" ]; then
	echo "Installing config  → $ENVFILE"
	$SUDO install -Dm644 "$here/packaging/systemd/orbit.env.example" "$ENVFILE"
else
	echo "Keeping existing   → $ENVFILE"
fi

$SUDO systemctl daemon-reload
$SUDO systemctl enable --now orbit.service

echo
echo "orbit is running as a service."
echo "  status:  systemctl status orbit"
echo "  logs:    journalctl -u orbit -f"
echo "  config:  $ENVFILE  (then: systemctl restart orbit)"
