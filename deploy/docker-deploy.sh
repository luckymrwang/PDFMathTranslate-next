#!/bin/bash
# Docker-based deploy: guard against running translations -> git pull ->
# docker compose build -> docker compose up -d -> wait for health.
# Replaces restart-all.sh for the compose stack (that script remains the
# non-Docker fallback after `docker compose down`).
# Usage: deploy/docker-deploy.sh [-f]
#   -f  restart even if translations are running (they will be lost)
# Env: SKIP_PULL=1 skips git pull; REPO_DIR overrides the repo location.
# Requires: docker with the compose plugin on the server.
set -u

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="${REPO_DIR:-$(cd "$DIR/.." && pwd)}"
cd "$DIR"   # docker-compose.yml lives here

FORCE=0
[ "${1:-}" = "-f" ] && FORCE=1

# Query the compose'd API's /health from inside the container: the API port
# is not published, and exec needs no extra image pull (unlike docker run
# --network). A stopped or missing stack reports 0.
active_tasks() {
    docker compose exec -T pdf2zh python -c \
      "import json,urllib.request;print(json.load(urllib.request.urlopen('http://127.0.0.1:11008/health',timeout=3))['active_tasks'])" \
      2>/dev/null || echo 0
}

container_id() { docker compose ps -q "$1"; }

wait_healthy() {
    local service=$1 timeout=$2 i cid state
    for i in $(seq 1 "$timeout"); do
        cid=$(container_id "$service")
        state=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}nohealth{{end}}' "$cid" 2>/dev/null)
        [ "$state" = healthy ] && { echo "$service: healthy after ${i}s"; return 0; }
        sleep 1
    done
    echo "$service: NOT healthy after ${timeout}s; last log lines:"
    docker compose logs --tail 20 "$service"
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
        echo "git: pull failed; nothing was touched. Fix the repo (git status) or re-run with SKIP_PULL=1."
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
    local active
    active=$(active_tasks)
    if [ "${active:-0}" -gt 0 ] && [ "$FORCE" != 1 ]; then
        echo "$active translation(s) are running; recreating would lose them. Re-run with -f to force."
        return 1
    fi

    # Pull before touching the running stack, so a failed pull leaves it up.
    pull_latest || return 1

    # Build while the old containers keep serving; recreate only at `up -d`.
    # CACHEBUST busts the babeldoc refresh layer (replaces the random.org ADD
    # hack in the root Dockerfile). Date granularity = at most one refresh/day.
    if ! CACHEBUST="${CACHEBUST:-$(date +%Y%m%d%H)}" docker compose build; then
        echo "build failed; old containers keep running"
        return 1
    fi

    docker compose up -d || return 1

    wait_healthy pdf2zh 120 || return 1
    wait_healthy gateway 60 || return 1
    echo "gateway health: $(curl -s -m 5 http://127.0.0.1:8080/health || echo FAILED)"

    echo "logs: docker compose logs -f   (from $DIR)"
    echo "stop: docker compose down       (restart-all.sh remains the fallback)"
}

# One line, so bash never reads past it if git pull rewrites this file.
main "$@"; exit $?
