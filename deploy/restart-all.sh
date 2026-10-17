#!/bin/bash
# Restart the PDF2ZH API and the Go gateway: stop whatever already listens on
# their ports, then start both detached. Usage: deploy/restart-all.sh [-f]
#   -f  restart even if translations are still running (they will be lost)
set -u

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
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

active=$(curl -s -m 3 "http://127.0.0.1:$API_PORT/health" 2>/dev/null \
    | sed -n 's/.*"active_tasks": *\([0-9][0-9]*\).*/\1/p')
if [ "${active:-0}" -gt 0 ] && [ "$FORCE" != 1 ]; then
    echo "$active translation(s) are running; restarting would lose them. Re-run with -f to force."
    exit 1
fi

# Gateway first (it depends on the API), then the API.
stop_port "gateway" "$GATEWAY_PORT" || { echo "gateway: could not stop"; exit 1; }
stop_port "api" "$API_PORT" || { echo "api: could not stop"; exit 1; }

mkdir -p "$LOG_DIR"
status=0

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
exit "$status"
