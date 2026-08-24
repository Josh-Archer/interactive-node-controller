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

Phase 3 adds opt-in, PDB-aware eviction. Do not infer drain behavior from this
phase: `NoSchedule` blocks new placements only.
