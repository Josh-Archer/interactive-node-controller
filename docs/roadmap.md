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

## Release model

PR CI uses least privilege and concurrency cancellation. Tag/manual release will build linux/amd64 and linux/arm64 images, publish an OCI Helm chart, generate SBOM/provenance, and keylessly sign when artifacts exist. The scaffold gates artifact jobs because no implementation artifacts exist yet; a green scaffold run is not a usable release. Consumers pin chart version and image digest through reviewed GitOps changes.
