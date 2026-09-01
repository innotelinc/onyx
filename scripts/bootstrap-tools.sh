#!/usr/bin/env bash
# Downloads a repo-local Go + protoc toolchain into .tools/.
# Never installs anything system-wide; safe to re-run (idempotent).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TOOLS="$ROOT/.tools"

GO_VERSION="${GO_VERSION:-1.27.0}"
PROTOC_VERSION="${PROTOC_VERSION:-36.1}"

ARCH="$(uname -m)"
case "$ARCH" in
  x86_64)  GO_ARCH="amd64"; PROTOC_ARCH="x86_64" ;;
  aarch64|arm64) GO_ARCH="arm64"; PROTOC_ARCH="aarch_64" ;;
  *) echo "unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

mkdir -p "$TOOLS/bin" "$TOOLS/gomod" "$TOOLS/gocache" "$TOOLS/gopath"

# --- Go ---
if [ ! -x "$TOOLS/go/bin/go" ]; then
  echo ">> downloading Go ${GO_VERSION} (linux/${GO_ARCH}) into .tools/ ..."
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${GO_ARCH}.tar.gz" -o "$TOOLS/go.tgz"
  tar -C "$TOOLS" -xzf "$TOOLS/go.tgz"
  rm -f "$TOOLS/go.tgz"
fi

# --- protoc + Go codegen plugins ---
if [ ! -x "$TOOLS/protoc/bin/protoc" ]; then
  echo ">> downloading protoc ${PROTOC_VERSION} into .tools/ ..."
  curl -fsSL "https://github.com/protocolbuffers/protobuf/releases/download/v${PROTOC_VERSION}/protoc-${PROTOC_VERSION}-linux-${PROTOC_ARCH}.zip" -o "$TOOLS/protoc.zip"
  mkdir -p "$TOOLS/protoc"
  unzip -q -o "$TOOLS/protoc.zip" -d "$TOOLS/protoc"
  rm -f "$TOOLS/protoc.zip"
fi

if [ ! -x "$TOOLS/bin/protoc-gen-go" ]; then
  echo ">> installing protoc-gen-go ..."
  GOBIN="$TOOLS/bin" GOMODCACHE="$TOOLS/gomod" GOCACHE="$TOOLS/gocache" GOPATH="$TOOLS/gopath" \
    "$TOOLS/go/bin/go" install google.golang.org/protobuf/cmd/protoc-gen-go@latest
fi
if [ ! -x "$TOOLS/bin/protoc-gen-go-grpc" ]; then
  echo ">> installing protoc-gen-go-grpc ..."
  GOBIN="$TOOLS/bin" GOMODCACHE="$TOOLS/gomod" GOCACHE="$TOOLS/gocache" GOPATH="$TOOLS/gopath" \
    "$TOOLS/go/bin/go" install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
fi

echo "toolchain ready:"
"$TOOLS/go/bin/go" version
"$TOOLS/protoc/bin/protoc" --version