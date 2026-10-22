#!/bin/bash
# Restart dev without pulling code. pull-dev.sh pulls origin/dev first.
set -eu
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export REPO_DIR="$(cd "$DIR/.." && pwd)"
if [ "$(git -C "$REPO_DIR" branch --show-current)" != dev ]; then
    echo "Expected dev branch; no services were changed."
    exit 1
fi
export GATEWAY_ENV_FILE="$DIR/dev/.env"
[ -f "$GATEWAY_ENV_FILE" ] || { echo "Missing $GATEWAY_ENV_FILE"; exit 1; }
export SKIP_PULL="${SKIP_PULL:-1}"
export PULL_BRANCH=dev
export WORKDIR="$REPO_DIR"
export LOG_DIR="${LOG_DIR:-$DIR/dev/logs}"
export PDF2ZH_API_HOST=127.0.0.1
export PDF2ZH_API_PORT=11009
export GATEWAY_PORT=8080
export PDF2ZH_WORK_DIR="${PDF2ZH_WORK_DIR:-/var/lib/pdfmath-dev/work}"
export PDF2ZH_MAX_CONCURRENT=1
export PDF2ZH_QPS=1
export PDF2ZH_POOL_MAX_WORKERS=1
export PDF2ZH_NO_MONO=true
export PDF2ZH_MAX_PAGES_PER_PART=50
exec bash "$DIR/restart-all.sh" "$@"
