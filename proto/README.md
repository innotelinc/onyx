# Onyx — proto contracts (source of truth)

`onyx/v1/*.proto` are the canonical gRPC contracts, per
[docs/design/04#contracts-and-codegen](../../docs/design/04-backend-service-architecture.md).
Every service-to-service call uses generated clients generated from these files.

Layout:

```
onyx/v1/health.proto     Health RPC implemented by every service (docs/design/04 §8)
onyx/v1/core.proto       onyx-core — orchestrator: status, policy, forwarding
onyx/v1/storaged.proto   onyx-storaged — pools, datasets, scrutiny + hotplug devices (ListDevices/MountDevice/UnmountDevice) (docs/design/05)
onyx/v1/privd.proto      onyx-privd — allowlisted privileged operations (docs/design/04 §7)
onyx/v1/shares.proto     shares — logical share model, CoreShares CRUD, onyx-shared config render (docs/design/05 §6)
```

Codegen:

- **Go:** `make gen` → `gen/go/onyx/v1/*.pb.go` (part of the root Go module).
- **Rust:** generated at build time by `services/storaged/build.rs` via `tonic-build`.

Rules (from design doc 04 §3):

1. The wire contract never forks from these files; a CI check will enforce that
   implementations match.
2. Messages are reused across services by importing other `.proto` files (e.g.
   `core.proto` imports `storaged.proto` for the `Pool` type).
3. New RPCs start here, then flow to codegen, then to implementation.