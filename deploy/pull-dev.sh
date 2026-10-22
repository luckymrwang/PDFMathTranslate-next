#!/bin/bash
# Fast-forward origin/dev, build gateway, then restart both dev processes.
# Refuses to interrupt running translations unless -f is explicitly passed.
set -eu
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export SKIP_PULL=0
exec bash "$DIR/restart-dev.sh" "$@"
