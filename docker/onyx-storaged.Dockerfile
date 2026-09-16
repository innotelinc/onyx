# syntax=docker/dockerfile:1
# onyx-storaged (Rust data plane): builds with tonic-build from proto/ (needs
# protoc in the builder), ships the static binary in an alpine runtime. In the
# compose stack it runs privileged (uevent netlink + device access) as the
# root-privileged data-plane container, mirroring its host role next to privd.
FROM rust:1-slim-bookworm AS build
ARG TARGETARCH
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
RUN mkdir -p /out \
    && if [ "${TARGETARCH:-amd64}" = "amd64" ]; then MUSL_TARGET=x86_64-unknown-linux-musl; else MUSL_TARGET=aarch64-unknown-linux-musl; fi \
    && cargo build --release --target "${MUSL_TARGET}" \
       --manifest-path services/storaged/Cargo.toml \
    && cp services/storaged/target/"${MUSL_TARGET}"/release/onyx-storaged /out/onyx-storaged

FROM alpine:3.20
COPY --from=build /out/onyx-storaged /usr/local/bin/onyx-storaged
ENTRYPOINT ["/bin/sh", "-c", "umask 000; mkdir -p /run/onyx; chmod 0777 /run/onyx 2>/dev/null; exec /usr/local/bin/onyx-storaged \"$@\"", "--"]
