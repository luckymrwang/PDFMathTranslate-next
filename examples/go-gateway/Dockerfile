# syntax=docker/dockerfile:1
# Build context: examples/go-gateway/ (see .dockerignore — .env, the prebuilt
# binary and test files never enter the context).
# go.mod has zero dependencies, so the build needs no module download.

FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/go-gateway .

FROM alpine:3.20
# ca-certificates: outbound HTTPS to api.weixin.qq.com / OSS / R2.
# busybox wget (part of alpine) serves the HEALTHCHECK below.
RUN apk add --no-cache ca-certificates \
    && addgroup -S app && adduser -S -G app app \
    && mkdir -p /data && chown -R app:app /data
COPY --from=build /out/go-gateway /usr/local/bin/go-gateway
USER app
WORKDIR /data
EXPOSE 8080
# The port is pinned to 8080 (GATEWAY_ADDR is fixed in deploy/docker-compose.yml);
# changing one requires changing the other.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8080/health || exit 1
ENTRYPOINT ["/usr/local/bin/go-gateway"]
