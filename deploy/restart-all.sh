#!/bin/bash
# Update the repo, then restart the PDF2ZH API and the Go gateway: stop whatever
# already listens on their ports and start both detached.
# Usage: deploy/restart-all.sh [-f]
#   -f  restart even if translations are still running (they will be lost)
# Env: SKIP_PULL=1 skips git pull; REPO_DIR overrides the repo location.
set -u

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="${REPO_DIR:-$(cd "$DIR/.." && pwd)}"
GW_DIR="$DIR/../examples/go-gateway"
API_PORT="${PDF2ZH_API_PORT:-11008}"
LOG_DIR="${LOG_DIR:-/root}"
API_LOG="$LOG_DIR/pdf2zh-api.log"
GATEWAY_LOG="$LOG_DIR/go-gateway.log"

FORCE=0
[ "${1:-}" = "-f" ] && FORCE=1

# The gateway port comes from GATEWAY_ADDR in its .env (default :8080).
GATEWAY_PORT="${GATEWAY_PORT:-}"
if [ -z "$GATEWAY_PORT" ]; then
    addr=$(sed -n 's/^GATEWAY_ADDR=//p' "$GW_DIR/.env" 2>/dev/null | tail -n 1 | tr -d "'\"")
    GATEWAY_PORT="${addr##*:}"
    GATEWAY_PORT="${GATEWAY_PORT:-8080}"
fi

# Match by listening port, never by process name: `pkill -f` can kill our own shell.
pids_on() {
    ss -tlnpH "sport = :$1" 2>/dev/null | grep -o 'pid=[0-9]*' | cut -d= -f2 | sort -u
}

stop_port() {
    local name=$1 port=$2 pids
    pids=$(pids_on "$port")
    if [ -z "$pids" ]; then
        echo "$name: not running"
        return 0
    fi
    echo "$name: stopping pid $(echo $pids)"
    kill $pids 2>/dev/null
    for _ in $(seq 1 15); do
        [ -z "$(pids_on "$port")" ] && return 0
        sleep 1
    done
    echo "$name: still listening after 15s, sending SIGKILL"
    kill -9 $pids 2>/dev/null
    sleep 1
    [ -z "$(pids_on "$port")" ]
}

# Explicit interpreter: the exec bit may be lost after a git checkout.
start_detached() {
    local interpreter=$1 script=$2 log=$3
    setsid nohup "$interpreter" "$script" > "$log" 2>&1 < /dev/null &
    disown
}

wait_listen() {
    local name=$1 port=$2 timeout=$3 log=$4 i
    for i in $(seq 1 "$timeout"); do
        if [ -n "$(pids_on "$port")" ]; then
            echo "$name: listening on :$port after ${i}s"
            return 0
        fi
        sleep 1
    done
    echo "$name: not listening after ${timeout}s, last log lines:"
    tail -n 15 "$log"
    return 1
}

# Fast-forward only: a server checkout must never create merge commits.
pull_latest() {
    if [ "${SKIP_PULL:-0}" = 1 ]; then
        echo "git: pull skipped (SKIP_PULL=1)"
        return 0
    fi
    local before after
    before=$(git -C "$REPO_DIR" rev-parse --short HEAD 2>/dev/null)
    echo "git: pulling in $REPO_DIR"
    if ! git -C "$REPO_DIR" pull --ff-only; then
        echo "git: pull failed; nothing was stopped. Fix the repo (git status) or re-run with SKIP_PULL=1."
        return 1
    fi
    after=$(git -C "$REPO_DIR" rev-parse --short HEAD)
    if [ "$before" = "$after" ]; then
        echo "git: already up to date at $after"
    else
        echo "git: updated $before -> $after"
    fi
}

main() {
    active=$(curl -s -m 3 "http://127.0.0.1:$API_PORT/health" 2>/dev/null \
        | sed -n 's/.*"active_tasks": *\([0-9][0-9]*\).*/\1/p')
    if [ "${active:-0}" -gt 0 ] && [ "$FORCE" != 1 ]; then
        echo "$active translation(s) are running; restarting would lose them. Re-run with -f to force."
        return 1
    fi

    # Pull before touching the running services, so a failed pull leaves them up.
    pull_latest || return 1

    # Gateway first (it depends on the API), then the API.
    stop_port "gateway" "$GATEWAY_PORT" || { echo "gateway: could not stop"; return 1; }
    stop_port "api" "$API_PORT" || { echo "api: could not stop"; return 1; }

    mkdir -p "$LOG_DIR"
    local status=0

    start_detached bash "$DIR/start-api.sh" "$API_LOG"
    wait_listen "api" "$API_PORT" 60 "$API_LOG" || status=1
    if [ "$status" = 0 ]; then
        echo "api health: $(curl -s -m 5 "http://127.0.0.1:$API_PORT/health" || echo FAILED)"
        # The gateway builds first (go build), so allow more time.
        start_detached sh "$DIR/start-gateway.sh" "$GATEWAY_LOG"
        wait_listen "gateway" "$GATEWAY_PORT" 180 "$GATEWAY_LOG" || status=1
    else
        echo "gateway: not started because the API failed"
    fi

    echo "logs: $API_LOG  $GATEWAY_LOG"
    return "$status"
}

# One line, so bash never reads past it if git pull rewrites this file.
main "$@"; exit $?
