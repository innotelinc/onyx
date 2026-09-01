# onyx-sdk

Typed client libraries and tooling so apps and scripts talk to Onyx exactly
like the UI does (docs/design/04#3-contracts-and-codegen).

| Package | Language | Status |
|---------|----------|--------|
| [`go/`](go/) | Go (REST client + `onyx` CLI) | v0.1 skeleton |
| `ts/` | TypeScript | planned (v0.2, with the web UI) |

License: **Apache-2.0** (SDK stays open even though the core is AGPL-3.0).

## `onyx` CLI

`go/cmd/onyx` mirrors the REST API for scripting: `--json` everywhere, non-zero
exit codes with structured errors (docs/design/04#10-cli).

```bash
bin/onyx version            # onyx 0.1.0-dev (api v1)
bin/onyx status [--json]    # aggregate service health
bin/onyx pool list [--json] # storage pools
bin/onyx device list        # hotplug/USB/SATA drives (show/attach/detach)
bin/onyx events [--stream]  # device audit trail, live SSE tail
bin/onyx share create media /mnt/onyx/pool1/@data/media --smb --nfs --readonly
bin/onyx share list|show|delete
```

Endpoint defaults to `http://127.0.0.1:8080`; override with `--api` or
`ONYX_API` (e.g. `ONYX_API=https://nas.local bin/onyx status`).