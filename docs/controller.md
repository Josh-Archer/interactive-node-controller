# Controller operations (Phase 2)

## Ownership and safety

`NodeActivity.spec.nodeName` is enrollment configuration and must be applied by
the consumer's GitOps system. The reporter's credential is limited to `patch`
on the corresponding `nodeactivities/status` resource name. It must never have
Node access.

The controller has the required Node update permission, but reconciles only one
configured taint key. It removes and replaces only taints matching that key;
unrelated Node taints remain untouched. It does not create, delete, evict, or
modify Pods, Deployments, StatefulSets, DaemonSets, or any other workload spec.

## State and stale behavior

The chart defaults to a one-minute heartbeat deadline and `failClosed: true`.
A missing, stale, `unknown`, or unsupported status yields the configured
`NoSchedule` fail-closed taint. `idle` removes the owned taint. Interactive
desktop state applies `PreferNoSchedule`; active game state applies
`NoSchedule`.

The controller records a `TaintApplied` condition and `status.managedTaint`.
The condition makes malformed enrollment, missing Nodes, and applied policy
observable without inspecting the Node manually.

## Install boundary

The chart includes the CRD, controller Deployment, ClusterRole, leader-election
permission, health probes, and metrics Service. It is an application artifact,
not an operator that deploys itself. A consumer must pin a chart version and an
image digest (`image.digest`) in its GitOps source, create the desired `NodeActivity`, and use a
separate secure path for the reporter credential.

[`examples/nodeactivity.yaml`](../examples/nodeactivity.yaml) is the minimal
Git-owned enrollment. The reporter credential must use the same namespace/name
and is restricted to that object's `status` subresource.

## Phase 3 safe eviction

Eviction is disabled and audit-only by default. A consumer must explicitly set
`eviction.enabled=true` and `eviction.audit=false`, and each workload Pod must
carry the exact label `interactive-node-controller.io/evictable: "true"`.

Evictions run only while the enrolled activity is `active` with `game`
activity and the controller's managed taint is `NoSchedule` with the active
value. Stale, unknown, idle, and interactive states never trigger eviction.

The controller uses the Kubernetes `policy/v1` Eviction API. PDB throttling
(`429 TooManyRequests`) and other API conflicts are recorded as blocked and
retried; the controller never bypasses a PDB by deleting a Pod. The per-reconcile
rate cap and retry backoff are configurable.

Safety gates skip and explain DaemonSet-owned, mirror/static, protected
namespace, terminating, unmanaged, critical-priority, any non-empty
`spec.nodeSelector`, hostPath/emptyDir/local storage, PVC-backed RWO, required
node-affinity, and explicitly pinned Pods. A node selector can target a scarce
capability or a single node, so the controller does not attempt a scheduler
feasibility proof during eviction.
Skipped and blocked outcomes are exported in
`interactive_node_controller_evictions_total{outcome,reason}` and summarized
by the `Eviction` NodeActivity condition. Workload specs are never mutated.

## Phase 4 home canary GitOps integration

### Node and storage review

`homelabdesktop` is enrolled as the initial canary interactive worker. Before
enabling the controller in the consuming environment:
1. Confirm the node is Ready and labeled with `longhorn-system: "true"` for
   attach-side storage support.
2. Disable Longhorn replica scheduling on `homelabdesktop` in
   `gitops/storage/longhorn/nodes-config.yaml` (`allowScheduling: false`).
   Interactive desktop restarts and taint state transitions must never cause
   storage rebuild storms or degrade storage volumes.

### GitOps deployment contract

Deploy the controller via ArgoCD using the published OCI Helm chart:
- Pinned chart version: `oci://ghcr.io/josh-archer/charts/interactive-node-controller`
- Pinned immutable image: `ghcr.io/josh-archer/interactive-node-controller@sha256:...`
- Minimal RBAC: Controller owns only the `availability.interactive-node.io/state` taint on Nodes;
  reporter owns only `patch` on `nodeactivities/status` for `homelabdesktop`.
- Safe defaults: `eviction.enabled=false`, `eviction.audit=true`, `failClosed=true`.

### Rollout verification

1. ArgoCD syncs the `ops` application and reports `Synced` and `Healthy`.
2. The `interactive-node-controller` Deployment transitions to `Ready` with 1/1 replicas.
3. The `NodeActivity` resource for `homelabdesktop` is created in namespace `interactive-node-controller`.
4. The host reporter sends heartbeats; the controller updates `status.managedTaint` and applies/removes the node taint.
5. Prometheus metrics endpoint `:8080/metrics` exports activity and taint metrics.

### Rollback and recovery

1. **Stale Agent / Host Offline**: When host reporter heartbeats cease for longer than `staleAfter` (default 1m), the controller enters fail-closed mode and applies `NoSchedule` with value `unavailable`. Normal operations resume automatically upon fresh heartbeat reception.
2. **Canary Rollback**: To roll back without affecting cluster workloads:
   - Remove the `NodeActivity` CR for `homelabdesktop`. The controller removes any owned taint upon finalizer/cleanup.
   - Or revert the GitOps commit in `home`; ArgoCD will prune the Deployment and CRD.
   - Alternatively, disable the controller by setting replicaCount to 0 or disabling the chart in `gitops/ops/kustomization.yaml`.
