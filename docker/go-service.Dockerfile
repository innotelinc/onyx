# syntax=docker/dockerfile:1
# Shared builder for the Go daemons (docs/design/11 §4). Build with:
#   docker build --build-arg SERVICE=onyx-core -f docker/go-service.Dockerfile .
# The compose stack does this for every Go service via the SERVICE build arg.
ARG SERVICE

FROM golang:1.27-bookworm AS build
ARG SERVICE
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
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
      *) echo "unknown SERVICE: ${SERVICE}" >&2; exit 1 ;; \
    esac \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/onyx ./services/${DIR}

# Runtime: alpine (has /bin/sh for the socket-perm wrapper; the binary is
# static). The wrapper runs with umask 000 so unix sockets in the shared
# /run/onyx volume are reachable by every daemon's container (privd runs as
# root; the rest run as the unprivileged "onyx" user).
FROM alpine:3.20
ARG SERVICE
RUN addgroup -S onyx && adduser -S -G onyx onyx \
    && mkdir -p /run/onyx \
    && for d in api core shared snapd backupd vmm appd ai objectstore; do mkdir -p "/var/lib/onyx/$d"; done \
    && chown -R onyx:onyx /run/onyx /var/lib/onyx \
    && chmod 0777 /run/onyx
COPY --from=build /out/onyx /usr/local/bin/${SERVICE}
USER onyx
ENV ONYX_SERVICE=${SERVICE}
ENTRYPOINT ["/bin/sh", "-c", "umask 000; mkdir -p /run/onyx; chmod 0777 /run/onyx 2>/dev/null; exec /usr/local/bin/${ONYX_SERVICE} \"$@\"", "--"]
