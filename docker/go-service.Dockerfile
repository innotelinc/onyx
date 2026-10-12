# syntax=docker/dockerfile:1
# Shared builder for the Go daemons (docs/design/11 §4). Build with:
#   docker build --build-arg SERVICE=onyx-core -f docker/go-service.Dockerfile .
# The compose stack does this for every Go service via the SERVICE build arg.
ARG SERVICE

# iCloud Drive requires rclone >= 1.69; Alpine 3.20's package predates it.
FROM rclone/rclone:1.70.3 AS rclone

FROM golang:1.27-bookworm AS build
ARG SERVICE
ARG VERSION=0.3.0-dev
ARG COMMIT=unknown
WORKDIR /src
COPY go.mod go.sum ./
# Optional trusted proxy CA for development environments. BuildKit mounts the
# certificate only for this step; it is never copied into the shipped image.
RUN --mount=type=secret,id=build_ca \
    if [ -f /run/secrets/build_ca ]; then SSL_CERT_FILE=/run/secrets/build_ca go mod download; else go mod download; fi
COPY proto ./proto
COPY services ./services
COPY sdk ./sdk
# SERVICE is the daemon name (onyx-api); the source lives in services/<dir>
# without the onyx- prefix. Map name -> source directory.
RUN case "${SERVICE}" in \
      onyx-api) DIR=api ;; \
      onyx-core) DIR=core ;; \
      onyx-shared) DIR=shared ;; \
      onyx-snapd) DIR=snapd ;; \
      onyx-backupd) DIR=backupd ;; \
      onyx-vmm) DIR=vmm ;; \
      onyx-appd) DIR=appd ;; \
      onyx-ai) DIR=ai ;; \
      onyx-objectstore) DIR=objectstore ;; \
      onyx-davd) DIR=davd ;; \
      *) echo "unknown SERVICE: ${SERVICE}" >&2; exit 1 ;; \
    esac \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/innotelinc/onyx/services/version.Version=${VERSION} -X github.com/innotelinc/onyx/services/version.Commit=${COMMIT}" -o /out/onyx ./services/${DIR}

# Runtime: alpine (has /bin/sh for the socket-perm wrapper; the binary is
# static). The wrapper runs with umask 000 so unix sockets in the shared
# /run/onyx volume are reachable by every daemon's container (privd runs as
# root; the rest run as the unprivileged "onyx" user).
FROM alpine:3.20
ARG SERVICE
COPY --from=rclone /usr/local/bin/rclone /usr/local/bin/rclone
COPY --from=rclone /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
RUN if [ "${SERVICE}" = "onyx-appd" ]; then apk add --no-cache docker-cli docker-cli-compose; fi \
    && addgroup -S onyx && adduser -S -G onyx onyx \
    && mkdir -p /run/onyx \
    && for d in api core shared snapd backupd vmm appd ai objectstore; do mkdir -p "/var/lib/onyx/$d"; done \
    && chown -R onyx:onyx /run/onyx /var/lib/onyx \
    && chmod 0777 /run/onyx
COPY --from=build /out/onyx /usr/local/bin/${SERVICE}
USER onyx
ENV ONYX_SERVICE=${SERVICE}
ENTRYPOINT ["/bin/sh", "-c", "umask 000; mkdir -p /run/onyx; chmod 0777 /run/onyx 2>/dev/null; exec /usr/local/bin/${ONYX_SERVICE} \"$@\"", "--"]
