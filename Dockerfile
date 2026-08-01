# syntax=docker/dockerfile:1

# ---- build ----------------------------------------------------------------
# Pinned to the build machine's own architecture: Go cross-compiles to the
# target itself, which is far faster than running the compiler under QEMU
# emulation during a multi-arch build.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies first, so a source-only change reuses the module layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Supplied by buildx; defaulted so a plain "docker build" still works.
ARG TARGETOS=linux
ARG TARGETARCH

# CGO stays off: modernc.org/sqlite is pure Go, which keeps the runtime image
# free of libc surprises and makes cross-building for a Synology NAS trivial.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# ---- runtime --------------------------------------------------------------
FROM alpine:3.21

# shadow provides usermod/groupmod for the PUID/PGID remap, su-exec drops
# privileges, tzdata makes TZ work, ca-certificates lets us reach an HTTPS
# tracker.
RUN apk add --no-cache ca-certificates tzdata su-exec shadow wget && \
    addgroup -g 1000 app && \
    adduser -D -u 1000 -G app app && \
    mkdir -p /data /torrents && \
    chown -R app:app /data /torrents

COPY --from=build /out/server /usr/local/bin/server
COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

ENV HTTP_PORT=8080 \
    DB_PATH=/data/app.db \
    TORRENT_FILES_DIR=/torrents \
    PUID=1000 \
    PGID=1000 \
    TZ=UTC

VOLUME ["/data", "/torrents"]
EXPOSE 8080

# Readiness covers the database and the output mount, which is exactly what
# breaks first when Synology volume permissions are wrong.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q -O- "http://127.0.0.1:${HTTP_PORT:-8080}/health/ready" >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["/usr/local/bin/server"]
