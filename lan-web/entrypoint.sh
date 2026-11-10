#!/bin/sh
# Runs the Python translation API (loopback only) and the Go gateway (:8080)
# in one container. If either dies the container exits so the restart policy
# brings both back; SIGTERM stops both cleanly.
pdf2zh --api &
api=$!

# Wait for the API so the first request after start doesn't hit a dead upstream.
i=0
until python -c "import urllib.request;urllib.request.urlopen('http://127.0.0.1:${PDF2ZH_API_PORT:-11008}/health',timeout=2)" 2>/dev/null; do
    i=$((i + 1))
    if [ "$i" -gt 90 ] || ! kill -0 "$api" 2>/dev/null; then
        echo "pdf2zh API failed to start" >&2
        kill "$api" 2>/dev/null
        exit 1
    fi
    sleep 1
done

go-gateway &
gw=$!

trap 'kill "$api" "$gw" 2>/dev/null; wait; exit 0' TERM INT

while kill -0 "$api" 2>/dev/null && kill -0 "$gw" 2>/dev/null; do
    sleep 2 &
    wait $!
done
echo "a service exited; stopping container" >&2
kill "$api" "$gw" 2>/dev/null
wait
exit 1
