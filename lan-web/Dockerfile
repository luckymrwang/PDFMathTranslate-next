# syntax=docker/dockerfile:1
# All-in-one intranet image: web UI + Go gateway + Python translation API in a
# single container, listening on :8080. No WeChat login, no payment/points.
# Build context: repository root.
#   docker compose -f lan-web/compose.yml build
#
# It layers the gateway (with the web UI go:embed-ed) on top of the API image
# built from deploy/Dockerfile.api, so the Python stack (babeldoc, fonts,
# models) is identical to production and nothing is re-downloaded. The public
# mini-program backend keeps using the two separate images unchanged.
# Build the API image first (docker compose -f deploy/docker-compose.yml build
# pdf2zh) or let BASE_IMAGE pull it from Docker Hub. amd64 only, like the base.

ARG BASE_IMAGE=luckymrwang/pdf2zh:latest

########## Stage 1: Go gateway ##########
FROM golang:1.24-alpine AS gateway
ARG GOPROXY
RUN if [ -n "$GOPROXY" ]; then go env -w GOPROXY="$GOPROXY"; fi
WORKDIR /src
# Copy selectively: examples/go-gateway/.env and data/ hold production secrets
# and orders and must never enter an image layer.
COPY examples/go-gateway/go.mod examples/go-gateway/go.sum ./
RUN go mod download
COPY examples/go-gateway/*.go ./
# Overlay the LAN-specific entry points only in the image build directory.
COPY lan-web/gateway/*.go ./
COPY lan-web/web ./web
RUN rm -f *_test.go \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/go-gateway .

########## Stage 2: API image + gateway ##########
FROM ${BASE_IMAGE}

COPY --from=gateway /out/go-gateway /usr/local/bin/go-gateway
COPY lan-web/entrypoint.sh /usr/local/bin/lan-entrypoint.sh
RUN chmod +x /usr/local/bin/lan-entrypoint.sh

ENV LAN_MODE=true \
    GATEWAY_ADDR=:8080 \
    PDF2ZH_API_URL=http://127.0.0.1:11008 \
    PDF2ZH_API_HOST=127.0.0.1 \
    PDF2ZH_API_PORT=11008 \
    PDF2ZH_MAX_CONCURRENT=2 \
    PDF2ZH_QPS=4 \
    PDF2ZH_POOL_MAX_WORKERS=4 \
    PDF2ZH_NO_MONO=true \
    PDF2ZH_MAX_PAGES_PER_PART=50

EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=10s --start-period=60s --retries=3 \
    CMD python -c "import urllib.request as u;u.urlopen('http://127.0.0.1:8080/health',timeout=3);u.urlopen('http://127.0.0.1:11008/health',timeout=3)"
ENTRYPOINT ["/usr/local/bin/lan-entrypoint.sh"]
