# syntax=docker/dockerfile:1
# onyx-privd: the single privilege boundary. In the compose stack this is the
# only container that runs as root with host device/pool mounts; every other
# daemon reaches privileged operations only through its gRPC socket.
FROM rust:1-slim-bookworm AS build
ARG TARGETARCH
# musl-tools is the native toolchain for the builder's own architecture; the
# x86_64 musl cross-toolchain only exists on the amd64 package. CI builds both
# platforms with buildx (the arm64 pass under QEMU), so the cross target is
# selected per architecture — a hardcoded x86_64 target fails on arm64 with
# "cc: error: unrecognized command-line option '-m64'".
RUN apt-get update \
    && apt-get install -y --no-install-recommends protobuf-compiler musl-tools \
    && rm -rf /var/lib/apt/lists/* \
    && if [ "${TARGETARCH}" = "amd64" ]; then rustup target add x86_64-unknown-linux-musl; else rustup target add aarch64-unknown-linux-musl; fi
WORKDIR /src
COPY . .
# Static musl build: the alpine runtime is musl — the default glibc-targeting
# build produces a binary the runtime can't even load ("not found" from the
# dynamic loader). Fully static also keeps the privileged boundary
# self-contained.
RUN mkdir -p /out \
    && if [ "${TARGETARCH:-amd64}" = "amd64" ]; then MUSL_TARGET=x86_64-unknown-linux-musl; else MUSL_TARGET=aarch64-unknown-linux-musl; fi \
    && cargo build --release --target "${MUSL_TARGET}" \
       --manifest-path services/privd/Cargo.toml \
    && cp services/privd/target/"${MUSL_TARGET}"/release/onyx-privd /out/onyx-privd

FROM alpine:3.20
# Device discovery and pool inspection run through this root boundary. Keep
# the helpers in the runtime image; the host's binaries are not visible inside
# the container namespace.
# util-linux = wipefs/swapoff/losetup/lsblk/mount, e2fsprogs = mkfs.ext4,
# btrfs-progs = mkfs.btrfs, smartmontools = health, mdadm/device-mapper =
# stopping the md and dm stacks that hold a disk an operator is erasing
# (docs/design/05 §2.3). `device-mapper` is the Alpine package that actually
# ships dmsetup — lvm2 depends on its libs but not on the tool.
RUN apk add --no-cache util-linux btrfs-progs e2fsprogs smartmontools mdadm device-mapper
COPY --from=build /out/onyx-privd /usr/local/bin/onyx-privd
ENTRYPOINT ["/bin/sh", "-c", "umask 000; mkdir -p /run/onyx; chmod 0777 /run/onyx 2>/dev/null; exec /usr/local/bin/onyx-privd \"$@\"", "--"]
