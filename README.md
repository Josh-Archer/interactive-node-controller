# Interactive Node Controller

Generic availability signaling for interactive Kubernetes workers. A host reporter updates only its own namespaced `NodeActivity` resource; the cluster controller owns the managed node taint and later safe, opt-in evictions.

This repository owns the Go controller, versioned CRD, OCI Helm chart, host reporter framework/Ansible role, tests, and immutable signed releases. Consuming repositories such as `Josh-Archer/home` own node selection, policy, GitOps pins, and smoke checks. This project never directly deploys to a consumer cluster.

## Security boundary

The host reporter never patches Kubernetes `Node` objects. It may update only its enrolled `NodeActivity`; only the controller may manage the taint or evict. Workload specifications remain Git/Argo-owned. Stale heartbeats fail closed by default.

## Non-goals

- Replacing a scheduler, autoscaler, or GitOps controller.
- Mutating workload replicas or implicitly draining every pod.
- Assuming a desktop, GPU vendor, runtime, or cloud provider.

## Development

Go is the planned implementation language. The initial scaffold contains no Phase 1 logic. Run `make test` and see [docs/roadmap.md](docs/roadmap.md).
