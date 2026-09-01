# Onyx — v0.1 "Cinder" skeleton build.
#
# Toolchain is repo-local (never touches the system):
#   make bootstrap   # downloads Go + protoc into .tools/
# Then gen/build/check/dev all work offline-ish from there.

SHELL := /bin/bash
TOOLS := $(CURDIR)/.tools
GO    := $(TOOLS)/go/bin/go
PROTOC := $(TOOLS)/protoc/bin/protoc
BIN   := $(CURDIR)/bin

export GOMODCACHE := $(TOOLS)/gomod
export GOCACHE    := $(TOOLS)/gocache
export GOPATH     := $(TOOLS)/gopath
export CARGO_HOME := $(TOOLS)/cargo
export PATH       := $(TOOLS)/bin:$(TOOLS)/protoc/bin:$(PATH)
# tonic-build (services/storaged/build.rs) uses PROTOC to find protoc.
export PROTOC     := $(TOOLS)/protoc/bin/protoc

.PHONY: bootstrap gen build check vet test dev clean

## bootstrap — download repo-local Go + protoc toolchains and codegen plugins
bootstrap:
	@bash scripts/bootstrap-tools.sh

## gen — regenerate Go gRPC stubs from proto/ (the source of truth)
gen:
	@mkdir -p proto/gen/go
	@$(PROTOC) --proto_path=proto \
		--go_out=proto/gen/go --go_opt=module=onyx.dev/onyx/proto/gen/go \
		--go-grpc_out=proto/gen/go --go-grpc_opt=module=onyx.dev/onyx/proto/gen/go \
		proto/onyx/v1/*.proto
	@echo "generated Go stubs in proto/gen/go/"

## build — compile all binaries into bin/
build: gen
	@mkdir -p $(BIN)
	$(GO) build -o $(BIN)/onyx-core ./services/core
	$(GO) build -o $(BIN)/onyx-api ./services/api
	$(GO) build -o $(BIN)/onyx-shared ./services/shared
	$(GO) build -o $(BIN)/onyx ./sdk/go/cmd/onyx
	@cd services/storaged && cargo build --quiet
	@cp services/storaged/target/debug/onyx-storaged $(BIN)/onyx-storaged
	@cd services/privd && cargo build --quiet
	@cp services/privd/target/debug/onyx-privd $(BIN)/onyx-privd
	@echo "built: onyx-core onyx-api onyx-shared onyx-storaged onyx-privd onyx (in bin/)"

## check — vet + test all Go and Rust code
check: vet test
	@cd services/storaged && cargo test --quiet
	@cd services/privd && cargo check --quiet

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

## dev — build and start the control+data plane locally (sockets in .run/onyx)
dev: build
	@bash scripts/dev.sh start

## clean — remove build artifacts and dev state (keeps .tools/)
clean:
	rm -rf $(BIN) services/storaged/target services/privd/target .run