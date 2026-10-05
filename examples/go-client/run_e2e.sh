#!/usr/bin/env bash
#
# End-to-end test for the pdf2zh_next HTTP API + the Go client.
#
# It submits a PDF through a REAL translation engine, waits for completion,
# downloads the mono/dual result PDFs, and validates them. This exercises the
# whole stack: multipart upload -> SSE/polling -> file download.
#
# Requires a working translation engine (network access and/or credentials).
# Run inside the environment where pdf2zh_next is installed (e.g. the babledoc
# conda env) and with Go available.
#
# Usage:
#   ./run_e2e.sh [path/to/input.pdf]
#   ENGINE=openai OPENAI_API_KEY=sk-... ./run_e2e.sh paper.pdf
#   ENGINE=deepl  DEEPL_KEY=xxx         ./run_e2e.sh paper.pdf
#   ENGINE=google ./run_e2e.sh paper.pdf          # needs internet to Google
#   API_URL=http://127.0.0.1:11008 ./run_e2e.sh paper.pdf   # reuse running server
#
# Environment:
#   ENGINE            google|bing|openai|deepl           (default: google)
#   OPENAI_API_KEY, OPENAI_MODEL, OPENAI_BASE_URL
#   DEEPL_KEY
#   LANG_IN, LANG_OUT                                    (default: en, zh)
#   API_URL           use an already-running server instead of starting one
#   USE_POLL=1        follow progress by polling instead of SSE

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENGINE="${ENGINE:-google}"
LANG_IN="${LANG_IN:-en}"
LANG_OUT="${LANG_OUT:-zh}"

fail() { echo "FAIL: $*" >&2; exit 1; }

# --- 1. resolve input PDF (generate a tiny valid one if none given) ------------
INPUT_PDF="${1:-}"
WORKDIR="$(mktemp -d)"
cleanup() {
  [ -n "${SERVER_PID:-}" ] && kill "$SERVER_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

if [ -z "$INPUT_PDF" ]; then
  INPUT_PDF="$WORKDIR/sample.pdf"
  printf '%%PDF-1.4\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 612 792]/Resources<</Font<</F1 4 0 R>>>>/Contents 5 0 R>>endobj\n4 0 obj<</Type/Font/Subtype/Type1/BaseFont/Helvetica>>endobj\n5 0 obj<</Length 44>>stream\nBT /F1 24 Tf 72 700 Td (Hello World) Tj ET\nendstream endobj\nxref\n0 6\n0000000000 65535 f \ntrailer<</Root 1 0 R/Size 6>>\nstartxref\n0\n%%%%EOF\n' > "$INPUT_PDF"
  echo "No PDF given; generated sample at $INPUT_PDF"
fi
[ -f "$INPUT_PDF" ] || fail "input PDF not found: $INPUT_PDF"

# --- 2. build the Go client ----------------------------------------------------
echo "Building Go client..."
CLI_BIN="$WORKDIR/pdf2zhcli"
( cd "$SCRIPT_DIR" && go build -o "$CLI_BIN" . ) || fail "go build failed"

# --- 3. start the API server unless one is provided ----------------------------
if [ -z "${API_URL:-}" ]; then
  PORT="${PDF2ZH_API_PORT:-11099}"
  API_URL="http://127.0.0.1:$PORT"
  echo "Starting API server on $API_URL ..."
  PDF2ZH_API_PORT="$PORT" python -m pdf2zh_next.http_api > "$WORKDIR/server.log" 2>&1 &
  SERVER_PID=$!
  for _ in $(seq 1 60); do
    if curl -sf "$API_URL/health" >/dev/null 2>&1; then break; fi
    if ! kill -0 "$SERVER_PID" 2>/dev/null; then
      cat "$WORKDIR/server.log" >&2; fail "server exited during startup"
    fi
    sleep 1
  done
  curl -sf "$API_URL/health" >/dev/null 2>&1 || fail "server did not become healthy"
fi
echo "Using API: $API_URL"

# --- 4. assemble the full argument list ----------------------------------------
# Single array keeps expansion safe under `set -u` on bash 3.2 (macOS default).
OUTDIR="$WORKDIR/out"
mkdir -p "$OUTDIR"

ARGS=(-url "$API_URL" -file "$INPUT_PDF" -lang-in "$LANG_IN" -lang-out "$LANG_OUT" -out "$OUTDIR" -engine "$ENGINE")
case "$ENGINE" in
  google|bing) ;;
  openai)
    [ -n "${OPENAI_API_KEY:-}" ] || fail "ENGINE=openai requires OPENAI_API_KEY"
    ARGS+=(-openai-key "$OPENAI_API_KEY" -openai-model "${OPENAI_MODEL:-gpt-4o-mini}")
    [ -n "${OPENAI_BASE_URL:-}" ] && ARGS+=(-openai-base-url "$OPENAI_BASE_URL")
    ;;
  deepl)
    [ -n "${DEEPL_KEY:-}" ] || fail "ENGINE=deepl requires DEEPL_KEY"
    ARGS+=(-deepl-key "$DEEPL_KEY")
    ;;
  *) fail "unknown ENGINE: $ENGINE" ;;
esac
[ "${USE_POLL:-0}" = "1" ] && ARGS+=(-poll)

# --- 5. run the translation ----------------------------------------------------
echo "Translating ($ENGINE, $LANG_IN -> $LANG_OUT) ..."
"$CLI_BIN" "${ARGS[@]}" || fail "translation run failed"

# --- 6. validate downloaded PDFs -----------------------------------------------
shopt -s nullglob
OUT_PDFS=("$OUTDIR"/*.pdf)
[ "${#OUT_PDFS[@]}" -ge 1 ] || fail "no result PDFs were downloaded"

for f in "${OUT_PDFS[@]}"; do
  size=$(wc -c < "$f" | tr -d ' ')
  magic=$(head -c 4 "$f")
  [ "$magic" = "%PDF" ] || fail "$f is not a PDF (magic=$magic)"
  [ "$size" -gt 1000 ] || fail "$f is suspiciously small ($size bytes)"
  echo "OK: $(basename "$f") ($size bytes)"
done

echo "PASS: downloaded ${#OUT_PDFS[@]} valid result PDF(s)"
