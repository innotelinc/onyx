# syntax=docker/dockerfile:1
# onyx-storaged (Rust data plane): builds with tonic-build from proto/ (needs
# protoc in the builder), ships the static binary in an alpine runtime. In the
# compose stack it runs privileged (uevent netlink + device access) as the
# root-privileged data-plane container, mirroring its host role next to privd.
FROM rust:1-slim-bookworm AS build
RUN apt-get update \
    && apt-get install -y --no-install-recommends protobuf-compiler musl-tools \
    && rm -rf /var/lib/apt/lists/* \
    && rustup target add x86_64-unknown-linux-musl
WORKDIR /src
COPY . .
# Static musl build: the alpine runtime is musl — the default glibc-targeting
# build produces a binary the runtime can't even load ("not found" from the
# dynamic loader).
RUN mkdir -p /out \
    && cargo build --release --target x86_64-unknown-linux-musl \
       --manifest-path services/storaged/Cargo.toml \
    && cp services/storaged/target/x86_64-unknown-linux-musl/release/onyx-storaged /out/onyx-storaged

FROM alpine:3.20
COPY --from=build /out/onyx-storaged /usr/local/bin/onyx-storaged
ENTRYPOINT ["/bin/sh", "-c", "umask 000; mkdir -p /run/onyx; chmod 0777 /run/onyx 2>/dev/null; exec /usr/local/bin/onyx-storaged \"$@\"", "--"]
