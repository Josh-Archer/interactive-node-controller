# Interactive Node Controller

Generic availability signaling for interactive Kubernetes workers. A host reporter updates only its own namespaced `NodeActivity` resource; the cluster controller owns the managed node taint and later safe, opt-in evictions.

This repository owns the Go controller, versioned CRD, OCI Helm chart, host reporter framework/Ansible role, tests, and immutable signed releases. Consuming repositories such as `Josh-Archer/home` own node selection, policy, GitOps pins, and smoke checks. This project never directly deploys to a consumer cluster.

Published releases are available as a GitHub Container Registry image at
`ghcr.io/josh-archer/interactive-node-controller` and an OCI Helm chart at
`oci://ghcr.io/josh-archer/charts/interactive-node-controller`. See
[VERSIONING.md](VERSIONING.md) for the release and consumption contract.

## Security boundary

The host reporter never patches Kubernetes `Node` objects. It may update only its enrolled `NodeActivity`; only the controller may manage the taint or evict. Workload specifications remain Git/Argo-owned. Stale heartbeats fail closed by default.

## Non-goals

- Replacing a scheduler, autoscaler, or GitOps controller.
- Mutating workload replicas or implicitly draining every pod.
- Assuming a desktop, GPU vendor, runtime, or cloud provider.

## Phase 1 host reporter

Phase 1 includes a production-oriented Linux host reporter, systemd unit, and
Ansible role. The reporter samples optional host signals, applies deterministic
`game > interactive > idle` precedence, debounces transitions, and emits an
explicit heartbeat with state, activity class, reason, observation time, and
generation. Provider uncertainty reports `unknown`, then `stale`; it never
opens the node for scheduling.

The safe default writes JSON to stdout because the `NodeActivity` CRD does not
exist until Phase 2. Kubernetes mode has one hard-coded operation: patch the
`status` subresource of one validated, namespaced `NodeActivity` name. It has no
Node client or generic resource path.

See [the host reporter guide](docs/host-reporter.md) for configuration, signal
semantics, credentials, systemd/Ansible installation, and the Phase 1 runbook.

## Phase 2 control plane

The `NodeActivity` CRD is namespaced. GitOps owns its enrollment
(`spec.nodeName`); the host reporter has only `patch` permission to its one
enrolled object's `status` subresource. The controller is the only component
with Node permissions, and it owns only its configured taint key.

| Host report | Managed taint |
| --- | --- |
| `idle` | Remove this controller's taint key |
| interactive `active` | `PreferNoSchedule` |
| game `active` | `NoSchedule` |
| `unknown`, `stale`, or expired heartbeat | `NoSchedule` fail-closed (default) |

The controller never changes workload specifications. Phase 3 adds a disabled,
audit-only-by-default safe eviction path. Actual evictions require an explicit
`interactive-node-controller.io/evictable: "true"` Pod label, active game
state, the managed `NoSchedule` taint, and both consumer opt-in flags. The
policy/v1 Eviction API is used so PDBs and grace periods are respected; PDB
blocks are reported and retried. See [controller operations](docs/controller.md).
Install with the Helm chart only through a consumer-owned, version/digest-pinned
GitOps revision; see [controller operations](docs/controller.md).

## Development

Go 1.23 or newer is required.

```bash
make fmt
make verify
make build-linux-amd64
make build-controller
make chart-lint
```

`make verify` runs formatting checks, unit tests, vet, and repository contract
checks, including Helm rendering. See [docs/roadmap.md](docs/roadmap.md) for
later phases.
