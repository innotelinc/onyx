# syntax=docker/dockerfile:1
# onyx-storaged (Rust data plane): builds with tonic-build from proto/ (needs
# protoc in the builder), ships the static binary in an alpine runtime. In the
# compose stack it runs privileged (uevent netlink + device access) as the
# root-privileged data-plane container, mirroring its host role next to privd.
FROM rust:1-slim-bookworm AS build
RUN apt-get update \
    && apt-get install -y --no-install-recommends protobuf-compiler \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY . .
RUN mkdir -p /out \
    && cargo build --release --manifest-path services/storaged/Cargo.toml \
    && cp services/storaged/target/release/onyx-storaged /out/onyx-storaged

FROM alpine:3.20
COPY --from=build /out/onyx-storaged /usr/local/bin/onyx-storaged
ENTRYPOINT ["/bin/sh", "-c", "umask 000; mkdir -p /run/onyx; chmod 0777 /run/onyx 2>/dev/null; exec /usr/local/bin/onyx-storaged \"$@\"", "--"]
