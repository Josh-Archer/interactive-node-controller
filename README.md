# Interactive Node Controller

Generic availability signaling for interactive Kubernetes workers. A host reporter updates only its own namespaced `NodeActivity` resource; the cluster controller owns the managed node taint and later safe, opt-in evictions.

This repository owns the Go controller, versioned CRD, OCI Helm chart, host reporter framework/Ansible role, tests, and immutable signed releases. Consuming environments own node selection, policy configuration, GitOps pins, and smoke checks. This project produces reusable artifacts and never directly deploys to a consumer cluster.

Published releases are available as signed container images at
`ghcr.io/josh-archer/interactive-node-controller` and OCI Helm charts at
`oci://ghcr.io/josh-archer/charts/interactive-node-controller`. See
[VERSIONING.md](VERSIONING.md) for the release contract and
[docs/artifact-verification.md](docs/artifact-verification.md) for Cosign signature and SBOM verification.

## Security boundary

The host reporter never patches Kubernetes `Node` objects. It may update only its enrolled `NodeActivity`; only the controller may manage the taint or evict. Workload specifications remain Git/Argo-owned. Stale heartbeats fail closed by default.

## Non-goals

- Replacing a scheduler, autoscaler, or GitOps controller.
- Mutating workload replicas or implicitly draining every pod.
- Assuming a desktop, GPU vendor, runtime, or cloud provider.

## Quickstart & Installation

### 1. Install Controller via Helm

Install the controller into your cluster from the public OCI registry, pinning the chart version and image digest:

```bash
helm upgrade --install interactive-node-controller \
  oci://ghcr.io/josh-archer/charts/interactive-node-controller \
  --version 0.1.0 \
  --namespace interactive-node-controller \
  --create-namespace \
  --set image.digest="sha256:40b00ee4215d468c019ebc41d944c630baaf39ffe7364353274d5a44dafa22b5"
```

Alternatively, customize values using [`examples/helm-values.yaml`](examples/helm-values.yaml) or a GitOps kustomization ([`examples/gitops-kustomization.yaml`](examples/gitops-kustomization.yaml)).

### 2. Enroll an Interactive Worker Node

Apply a `NodeActivity` resource for each interactive workstation or GPU node ([`examples/nodeactivity.yaml`](examples/nodeactivity.yaml)):

```yaml
apiVersion: availability.interactive-node.io/v1alpha1
kind: NodeActivity
metadata:
  name: workstation-1
  namespace: interactive-node-controller
spec:
  nodeName: workstation-1
```

### 3. Grant Reporter Status-Only RBAC

Create a ServiceAccount and Role with `patch` permission on `nodeactivities/status` ([`examples/nodeactivity-rbac.yaml`](examples/nodeactivity-rbac.yaml)):

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: workstation-1-reporter
  namespace: interactive-node-controller
rules:
  - apiGroups: ["availability.interactive-node.io"]
    resources: ["nodeactivities/status"]
    resourceNames: ["workstation-1"]
    verbs: ["patch"]
```

### 4. Configure & Start Host Reporter

Install the host reporter on the worker node using the systemd unit or Ansible role, pointing its config ([`examples/config.yaml`](examples/config.yaml)) at the cluster API server and projected token.


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

## License

Interactive Node Controller is licensed under the [Apache License, Version 2.0](LICENSE).
