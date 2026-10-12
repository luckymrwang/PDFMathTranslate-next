#!/bin/sh
# Go gateway launcher (production deploy).
# Builds and runs in examples/go-gateway/, loading that directory's .env.
set -eu

# Resolve the gateway source dir relative to this script (override via GW_DIR).
GW_DIR="${GW_DIR:-$(cd "$(dirname "$0")/../examples/go-gateway" && pwd)}"
cd "$GW_DIR"

# Ensure the Go toolchain is reachable even without a login shell.
[ -d /usr/local/go/bin ] && PATH="$PATH:/usr/local/go/bin"
export PATH

# Load gateway configuration and secrets.
if [ -f .env ]; then
    set -a
    . ./.env
    set +a
fi

# Build once, then run the compiled binary (lighter than `go run` at steady state).
go build -o go-gateway .
exec ./go-gateway
