#!/bin/bash
# Build and push both images to Docker Hub (run on the dev machine; the
# server pulls them via deploy/docker-deploy.sh). Requires `docker login`
# as luckymrwang. The API image is ~1.9 GB — the push takes a while.
# Usage: deploy/docker-push.sh
# Env: SKIP_CACHEBUST=1 reuses the babeldoc refresh layer (no PyPI upgrade).
set -u

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$DIR"   # docker-compose.yml lives here

# CACHEBUST busts the babeldoc refresh layer (replaces the random.org ADD
# hack in the root Dockerfile). Date granularity = at most one refresh/day.
if [ "${SKIP_CACHEBUST:-0}" = 1 ]; then
    docker compose build
else
    CACHEBUST="${CACHEBUST:-$(date +%Y%m%d%H)}" docker compose build
fi

docker compose push
