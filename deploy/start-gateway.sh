#!/bin/sh
# Go gateway launcher. GATEWAY_ENV_FILE selects an independent dev config.
set -eu

# Resolve the gateway source dir relative to this script (override via GW_DIR).
GW_DIR="${GW_DIR:-$(cd "$(dirname "$0")/../examples/go-gateway" && pwd)}"
cd "$GW_DIR"

# Ensure the Go toolchain is reachable even without a login shell.
[ -d /usr/local/go/bin ] && PATH="$PATH:/usr/local/go/bin"
export PATH

# Load gateway configuration and secrets.
ENV_FILE="${GATEWAY_ENV_FILE:-$GW_DIR/.env}"
if [ -f "$ENV_FILE" ]; then
    set -a
    . "$ENV_FILE"
    set +a
fi

# Build once, then run the compiled binary (lighter than `go run` at steady state).
if [ "${GATEWAY_SKIP_BUILD:-0}" != 1 ] || [ "${1:-}" = --build-only ]; then
    # Build to a temporary path so a failed build leaves the old binary intact.
    go build -o go-gateway.new .
    mv go-gateway.new go-gateway
fi
[ "${1:-}" = --build-only ] && exit 0
exec ./go-gateway
