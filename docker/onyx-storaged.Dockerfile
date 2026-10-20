# syntax=docker/dockerfile:1
# onyx-storaged (Rust data plane): builds with tonic-build from proto/ (needs
# protoc in the builder), ships the static binary in an alpine runtime. In the
# compose stack it runs privileged (uevent netlink + device access) as the
# root-privileged data-plane container, mirroring its host role next to privd.
FROM rust:1-slim-bookworm AS build
ARG TARGETARCH
# The Rust services report their version from a compile-time constant, so the
# release tag is injected as an environment variable the build reads.
ARG VERSION=0.4.0-dev
ENV ONYX_VERSION=${VERSION}
# musl-tools is the native toolchain for the builder's own architecture; the
# x86_64 musl cross-toolchain only exists on the amd64 package. CI builds both
# platforms with buildx (the arm64 pass under QEMU), so the target is selected
# per architecture — a hardcoded x86_64 target fails on arm64 with
# "cc: error: unrecognized command-line option '-m64'".
RUN apt-get update \
    && apt-get install -y --no-install-recommends protobuf-compiler musl-tools \
    && rm -rf /var/lib/apt/lists/* \
    && if [ "${TARGETARCH}" = "amd64" ]; then rustup target add x86_64-unknown-linux-musl; else rustup target add aarch64-unknown-linux-musl; fi
WORKDIR /src
COPY . .
# Static musl build: the alpine runtime is musl — the default glibc-targeting
# build produces a binary the runtime can't even load ("not found" from the
# dynamic loader).
#
# The crate registry and the target directory are cache mounts: VERSION is
# injected at compile time, so a release stamp re-runs this build, and a full
# dependency compile from scratch on a two-core host is the difference between
# minutes and an hour. Compiled output only — nothing here lands in the image.
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    --mount=type=cache,target=/src/services/storaged/target \
    mkdir -p /out \
    && if [ "${TARGETARCH:-amd64}" = "amd64" ]; then MUSL_TARGET=x86_64-unknown-linux-musl; else MUSL_TARGET=aarch64-unknown-linux-musl; fi \
    && cargo build --release --target "${MUSL_TARGET}" \
       --manifest-path services/storaged/Cargo.toml \
    && cp services/storaged/target/"${MUSL_TARGET}"/release/onyx-storaged /out/onyx-storaged

FROM alpine:3.20
# Keep the data-plane runtime able to inspect the kernel block-device view
# when its uevent/sysfs fallback runs inside the container.
RUN apk add --no-cache util-linux
COPY --from=build /out/onyx-storaged /usr/local/bin/onyx-storaged
ENTRYPOINT ["/bin/sh", "-c", "umask 000; mkdir -p /run/onyx; chmod 0777 /run/onyx 2>/dev/null; exec /usr/local/bin/onyx-storaged \"$@\"", "--"]
