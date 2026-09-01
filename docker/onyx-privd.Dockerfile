# syntax=docker/dockerfile:1
# onyx-privd: the single privilege boundary. In the compose stack this is the
# only container that runs as root with host device/pool mounts; every other
# daemon reaches privileged operations only through its gRPC socket.
FROM rust:1-slim-bookworm AS build
RUN apt-get update \
    && apt-get install -y --no-install-recommends protobuf-compiler \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY . .
RUN mkdir -p /out \
    && cargo build --release --manifest-path services/privd/Cargo.toml \
    && cp services/privd/target/release/onyx-privd /out/onyx-privd

FROM alpine:3.20
COPY --from=build /out/onyx-privd /usr/local/bin/onyx-privd
ENTRYPOINT ["/bin/sh", "-c", "umask 000; mkdir -p /run/onyx; chmod 0777 /run/onyx 2>/dev/null; exec /usr/local/bin/onyx-privd \"$@\"", "--"]
