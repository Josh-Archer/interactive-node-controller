# Roadmap

Interactive Node Controller is a generic availability signal and safe transition system for interactive Kubernetes workers.

## Boundary and consumption

The project supplies a Go controller, versioned `NodeActivity` CRD, OCI Helm chart, host reporter framework/Ansible role, tests, and signed immutable artifacts. Consumers supply environment-specific node selection, signal configuration, policy, GitOps deployment, and smoke tests. The host reporter updates only its namespaced resource; the controller alone mutates its managed taint and eventually performs explicitly opt-in evictions.

## Phases

1. Release foundation and contract — [#1](https://github.com/Josh-Archer/interactive-node-controller/issues/1)
2. Phase 1 host-state reporter — [#2](https://github.com/Josh-Archer/interactive-node-controller/issues/2)
3. Phase 2 controller state machine, CRD, chart — [#3](https://github.com/Josh-Archer/interactive-node-controller/issues/3)
4. Phase 3 safe opt-in eviction — [#4](https://github.com/Josh-Archer/interactive-node-controller/issues/4)
5. Phase 4 home canary GitOps integration — [#5](https://github.com/Josh-Archer/interactive-node-controller/issues/5)

## Phase 1 acceptance criteria

- Versioned Go host reporter with provider interface and documented systemd/Ansible path.
- Configurable provider reports active, idle, or unknown without assuming Steam, logind, NVIDIA, or a desktop environment.
- Only the enrolled namespaced `NodeActivity` is updated; no Node patch permission.
- Heartbeats include observed-at time, generation, explicit unknown/stale state, and fail-closed defaults.
- Tests cover active, idle, unknown, stale, malformed config, and provider errors.
- CI runs formatting, vet, unit tests, and workflow/manifest validation.
- No live Kubernetes mutation is part of Phase 1.

Phase 1 implementation and operational details are documented in
[host-reporter.md](host-reporter.md). The reporter defaults to stdout until the
Phase 2 CRD and controller establish the live API contract. Phase 2 deliberately
does not evict Pods; that safety boundary remains Phase 3.

## Phase 3 acceptance criteria

- Eviction is audit-only and disabled by default; actual calls require both
  consumer enablement and explicit `interactive-node-controller.io/evictable:
  "true"` Pod opt-in.
- Only active game state with the controller's active `NoSchedule` taint can
  evict. Stale, unknown, idle, and interactive states never evict.
- Eviction uses `policy/v1` and preserves PDB and termination-grace behavior;
  throttles/conflicts are blocked and retried rather than bypassed.
- DaemonSet, mirror/static, protected namespace, terminating, unmanaged,
  non-empty node-selector, pinned/required-affinity, local-storage, and
  PVC-backed RWO-sensitive Pods are skipped with reasoned metrics and status
  evidence.
- Evictions have a configurable per-reconcile cap and retry backoff. The
  controller has no workload-spec mutation or Pod-delete permission.
- Unit/fake-client tests cover each gate, audit mode, PDB blocks, retries,
  rate limiting, and unchanged unrelated workload objects. No live consumer
  cluster is mutated in Phase 3.

## Phase 4 acceptance criteria

- Home pins an immutable release chart and container image digest with environment-specific values.
- Canary targets only `homelabdesktop` after live node and storage review (e.g. Longhorn replica scheduling disabled).
- Rollout verifies GitHub workflow, ArgoCD revision, Synced/Healthy state, workload readiness, and external smoke checks.
- Rollback and stale-agent recovery procedures are documented and tested.
- No direct kubectl-only deployment path is introduced.

## Release model

PR CI uses least privilege and concurrency cancellation. It validates the Go
controller, chart rendering, and a local container build. Tag/manual release
builds linux/amd64 and linux/arm64 images, publishes an OCI Helm chart,
generates SBOM/provenance, and keylessly signs the image. Consumers pin chart
version and image digest through reviewed GitOps changes; Phase 2 has not
created or deployed a release to any consumer cluster.
