#!/usr/bin/env bash
# install.sh — thin compatibility shim delegating to go/cmd/pi-ext-install.
# Source of truth for install/list/audit logic now lives in Go under go/cmd/pi-ext-install.
set -euo pipefail

_src="${BASH_SOURCE[0]}"
_dir="${_src%/*}"; [ "$_dir" = "$_src" ] && _dir="."
REPO_ROOT="$(cd "$_dir" && pwd -P)"
export PI_EXT_REPO_ROOT="$REPO_ROOT"

if [ -n "${PI_EXT_INSTALL_BIN:-}" ] && [ -x "$PI_EXT_INSTALL_BIN" ]; then
  exec "$PI_EXT_INSTALL_BIN" "$@"
fi

if [ -x "$REPO_ROOT/go/bin/pi-ext-install" ]; then
  exec "$REPO_ROOT/go/bin/pi-ext-install" "$@"
fi

if [ -d "$REPO_ROOT/go/cmd/pi-ext-install" ] && command -v go >/dev/null 2>&1; then
  cd "$REPO_ROOT/go"
  exec go run ./cmd/pi-ext-install "$@"
fi

if command -v pi-ext-install >/dev/null 2>&1; then
  exec pi-ext-install "$@"
fi

echo "install.sh FAILED: pi-ext-install binary not found and go toolchain unavailable (build with: go -C go build -o bin/pi-ext-install ./cmd/pi-ext-install)" >&2
exit 1
