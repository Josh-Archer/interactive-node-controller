# Controller operations (Phase 2)

## Ownership and safety

`NodeActivity.spec.nodeName` is enrollment configuration and must be applied by
the consumer's GitOps system. The reporter's credential is limited to `patch`
on the corresponding `nodeactivities/status` resource name. It must never have
Node access.

The controller has Node `get`, `list`, `watch`, `patch`, and `update`
permission. It reconciles one configured taint key and, when configured, the
burst taint key. It removes and replaces only taints matching those keys;
unrelated Node taints remain untouched. It does not create, delete, evict, or
modify Pods, Deployments, StatefulSets, DaemonSets, or any other workload spec.

`NodeActivityReconciler.SetupWithManager` watches Node objects. A change to a
managed taint (key, value, or effect) or to the
`availability.interactive-node.io/schedulable` label enqueues every
`NodeActivity` that enrolls that Node, and the controller restores the desired
state. Node status updates that leave those fields unchanged stay off the
queue, and kubelet heartbeats keep their existing reconcile schedule. Node
create and delete events also enqueue those enrollments, which applies
protection when a Node appears and records `NodeNotFound` when it disappears.

## Scheduled availability windows

An enrollment may set `spec.windows` to recurring five-field cron schedules,
with a Go duration, an IANA `timeZone` (UTC when omitted), and state `game`,
`interactive`, or `idle-allowed`. `spec.leadTime` applies each window's taint
before it opens. Stale-heartbeat fail-closed handling takes precedence; otherwise
a live game wins, then a scheduled window, then live interactive activity, then
idle. For example:

```yaml
spec:
  nodeName: workstation-1
  leadTime: 30m
  windows:
    - schedule: "0 20 * * 5,6"
      duration: 4h
      timeZone: America/New_York
      state: game
```

For an ad-hoc session, set the NodeActivity annotation
`availability.interactive-node.io/hold: "2h"`. The controller anchors the
duration when first observed, applies the interactive taint while it is valid,
and removes the annotation when it expires. Metrics `inc_window_active` and
`inc_next_window_start_timestamp_seconds` report window state and the next start.

Keep exactly one live `NodeActivity` enrollment per Node. Aggregation, sibling
refresh, and Node drift mapping read a `spec.nodeName` field index, so that
work follows the enrollments for the Node. If multiple objects across any
namespaces target the same Node, the controller conservatively aggregates
their contributions: fresh active-game `NoSchedule` wins, followed by
fail-closed `NoSchedule`, interactive `PreferNoSchedule`, and idle/no taint.
Unknown/stale reports contribute the configured fail-closed policy. Equal
contributions are ordered by namespace/name for deterministic per-node metrics.
The earliest contribution deadline schedules reevaluation. Every duplicate
reports `TaintApplied=False` with reason `DuplicateEnrollment`; remove duplicates
to restore a single enrollment. Eviction is suppressed while enrollment is
ambiguous, even if one reporter claims an active game. Changes or deletion of
an enrollment enqueue its siblings so their conditions refresh promptly.

The controller attaches a finalizer (`availability.interactive-node.io/cleanup`)
to enrolled `NodeActivity` resources. While the controller is running, when a
`NodeActivity` is deleted, the controller removes its owned taint from the
enrolled Node before clearing the finalizer if no live enrollment remains.
If another enrollment survives, it recomputes protection from the survivors
and preserves their per-node metrics instead. If the enrolled Node has already
been deleted or cannot be found, the controller clears the finalizer
immediately so deletion is not wedged by a missing Node. However, this automatic
cleanup only occurs while the controller is running; if the controller
Deployment is deleted or scaled to `replicaCount: 0` before the `NodeActivity`
resources are deleted, nothing processes the finalizer and deletion will hang.
In that scenario, manual recovery requires patching the `NodeActivity` to remove
the `availability.interactive-node.io/cleanup` finalizer (`kubectl patch ...`)
and removing the owned taint with `kubectl taint nodes <node> <key>-`.

## State and stale behavior

The chart defaults to a one-minute heartbeat deadline and `failClosed: true`.
A missing, stale, `unknown`, or unsupported status yields the configured
`NoSchedule` fail-closed taint. `idle` removes the owned taint. Interactive
desktop state applies `PreferNoSchedule`; active game state applies
`NoSchedule`.

Chart value `taint.burstKey` maps to `--burst-taint-key`. An empty value omits
the flag and preserves single-taint behavior. A configured value must be a
Kubernetes qualified name and must differ from `taint.key`. The chart rejects
an invalid key or a collision at render time, and the controller rejects the
same cases at startup. While the key is set, the controller applies
`<burstKey>=<failClosedValue>:NoSchedule` whenever the node is not fresh-idle
and removes that taint when the node is fresh-idle. Every fresh-idle node
receives `availability.interactive-node.io/schedulable=true`.

The controller records a `TaintApplied` condition and `status.managedTaint`.
The condition makes malformed enrollment, missing Nodes, and applied policy
observable without inspecting the Node manually. Current activity state and
managed taints are exported as Prometheus metrics
`interactive_node_controller_activity{node,state,activity}`,
`interactive_node_controller_taint{node,key,value,effect}`, and
`interactive_node_controller_schedulable{node}` (1 if the controller is standing down with a fresh idle heartbeat and no managed taint, 0 otherwise).

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
A second opt-in, the label or annotation
`interactive-node-controller.io/allow-lossy-eviction: "true"`, marks a Pod as
lossy. Pods with only the evictable label are standard pods. Missing either
opt-in never becomes eligible. This is not a blanket eviction policy.

The controller uses the Kubernetes `policy/v1` Eviction API.
Eviction requests include the inspected Pod's UID and resource version as
preconditions. A replacement or changed Pod is blocked and must pass the
safety gates again; missing identity metadata never permits an eviction.
PDB throttling (`429 TooManyRequests`) and other API conflicts are recorded as
blocked and retried for both standard and lossy pods. The controller never
bypasses a PDB by deleting a Pod. Standard and lossy workloads have separate
time-based budgets. Each budget has a per-node bucket and a process-wide
global bucket. An Eviction API call spends one token from each scope. Tokens
refill over the budget interval and do not reset when Reconcile runs, so a
faster heartbeat or status update cannot raise throughput. The default
allowance is one call per minute at each scope. The default burst equals that
allowance, so a full bucket cannot spend more than one interval of budget.

Duplicate enrollments suppress every row below. While more than one live
`NodeActivity` targets the Node, Reconcile applies the strongest taint and
does not call the Eviction API.

### Eviction matrix

`Fresh` means `status.heartbeatAt` is set and younger than `staleAfter`.
The optional burst `NoSchedule` taint (`--burst-taint-key`) is a scheduling
fence for every Node that is not fresh idle. It does not authorize eviction
of a fresh `unknown` report, and it never authorizes eviction of a standard pod.

| Live report | Standard | Lossy |
| --- | --- | --- |
| Fresh idle and no desired taint, including a scheduled `idle-allowed` window on a fresh idle report. A leftover burst `NoSchedule` taint does not evict. | no | no |
| Fresh active `interactive`, an availability hold, or a scheduled `interactive` window (`PreferNoSchedule` with the interactive value is desired and applied) | no | yes |
| Fresh active `game` with the active `NoSchedule` taint desired and applied, whether or not the burst fence is also present | yes | yes |
| Fresh `state=unknown` (reporter startup or debounce), including when fail-closed `NoSchedule` and the burst `NoSchedule` fence are both applied | no | no |
| Heartbeat missing or older than `staleAfter`, or reporter `state=stale`, while fail-closed `NoSchedule` is desired and applied. An expired heartbeat stays on this row even if the last activity was `game`. | no | yes |
| Scheduled `game` window while the live report is not an active game | no | yes only when the burst `NoSchedule` fence is configured and present; otherwise no |
| Fresh `state=active` whose activity is not `game` (interactive, or an unsupported activity that fail-closes scheduling) | no | yes |
| Two or more live enrollments for the Node | no | no |

A fresh `state=unknown` report is not a fresh `state=active` report whose
activity is `unknown`. The latter is an unsupported activity: scheduling
fail-closes, and lossy pods are eligible. A missing or expired heartbeat is
not a fresh unknown debounce; it follows the stale row even if `state` is
still `unknown`. Stale, unknown, idle, interactive, and scheduled states never
evict standard pods. Only a fresh active game with the active `NoSchedule`
taint does.

### Lossy Eviction Opt-In for Ephemeral Workloads

Ephemeral burst workloads (such as disposable CI runners) opt in by setting
the annotation or label `interactive-node-controller.io/allow-lossy-eviction: "true"`
in addition to `interactive-node-controller.io/evictable: "true"`.

Loss of scratch data and interrupted work are part of the lossy contract.
Eviction stops the container and abandons the in-flight step.
[`examples/disposable-runner.yaml`](../examples/disposable-runner.yaml) shows
schedulable-label admission, burst-taint behavior, and both opt-in labels.
Copy that manifest into the consuming GitOps repository.

Lossy opt-in relaxes only these standard gates:

- `emptyDir` volumes (scratch space is discarded on eviction).
- Explicit node selectors (`spec.nodeSelector`).
- Required node affinity (`spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution`).

The lossy budget is separate from the standard budget, but it is not a relaxed
gate. Unset lossy fields use the same per-node count, global count, interval,
and burst as standard pods. There is no hard-coded minimum of four.

### Volume and Pod safety contract

These gates apply to standard and lossy pods. Lossy opt-in does not bypass them:

- DaemonSet-owned pods
- Unmanaged pods (no owner reference)
- Mirror/static pods
- Protected namespaces
- Terminating pods
- Direct pinned pods (`interactive-node-controller.io/pinned: "true"`)
- Critical priority pods (`priority >= 2000000000`)
- Any `hostPath` volume
- Unbound PersistentVolumeClaims
- PersistentVolumes with a local or hostPath source
- Access modes `ReadWriteOnce` and `ReadWriteOncePod`

PVC eligibility then differs by class, on purpose:

- **Standard pods** may remain eligible with a bound PVC when the volume is not
  local or hostPath and no access mode is `ReadWriteOnce` or `ReadWriteOncePod`.
  Bound `ReadWriteMany` and `ReadOnlyMany` claims are eligible. That is the
  Phase 3 rule: only RWO-sensitive claims are unsafe to move.
- **Lossy pods** are ineligible for every PersistentVolumeClaim, including a
  bound `ReadWriteMany` claim. The lossy opt-in does not make multi-attach
  storage evictable. The skip reason is `pvc-not-allowed`.

Blocked attempts retain a per-node, per-Pod-UID retry deadline. The delay is
the configured retry backoff, raised when the API status carries a longer
`Retry-After` hint (`status.details.retryAfterSeconds`, including PDB `429`
responses), plus a stable jitter in `[0, retryJitter]`. Jitter is capped at
the retry backoff. It never makes a retry earlier than the backoff or the
hint. The same Pod UID keeps the same jitter; a replacement UID is a new key
and does not inherit the previous deadline. Heartbeat and status events
continue to reconcile taints, but cannot retry eviction before that deadline.
Safety gates run again when the deadline expires. Disappeared, replaced, or
ineligible Pods and inactive node states clear their retry entries; expired
entries are pruned on subsequent eviction passes. A Pod inside its deadline
does not spend budget. An attempt after the deadline still spends a token, so
an empty bucket keeps that Pod queued until the budget refills.

Eligible Pods that do not fit in the current per-node and global tokens are
not attempted. The pass records them as `rate-limited` and requeues for the
moment both buckets can spend another token. That requeue is sufficient:
queued candidates are considered again without a heartbeat, Pod watch, or
other unrelated event. Pods are still evaluated in list order and must pass
every safety gate on that later pass. Audit mode does not spend tokens.

Per-node means one bucket per node per class. Evicting on node A does not
spend node B's per-node tokens. Global means one bucket per class for the
whole process. Node A and node B both spend it, so the process cannot exceed
the global rate by reconciling many nodes. With the default inheritance, the
global allowance equals the per-node allowance. Raise the global count when
more than one node should evict during the same interval. A burst larger than
the steady count allows that many calls immediately, after which refill
returns to the steady rate. A burst left at zero inherits the steady count.

Retry deadlines and budget buckets are process-local. After a restart or
leader switch the new process starts with full bursts and no retry deadlines,
so an eligible Pod may be tried immediately, still only through the Eviction
API and all safety gates. Spent budget and pending deadlines are not carried
across processes. Leadership loss does not write budget state to the API.

Safety gates skip and explain DaemonSet-owned, mirror/static, protected
namespace, terminating, unmanaged, critical-priority, pinned, `hostPath`, and
local storage. Standard pods also skip any non-empty `spec.nodeSelector`,
`emptyDir`, required node affinity, and `ReadWriteOnce` or `ReadWriteOncePod`
claims. Lossy pods may use those scheduling and scratch exceptions, and still
skip every PersistentVolumeClaim, including bound `ReadWriteMany`. A node
selector can target a scarce capability or a single node, so the controller
does not attempt a scheduler feasibility proof during eviction of standard
workloads.
Skipped and blocked outcomes are exported in
`interactive_node_controller_evictions_total{outcome,reason}` and summarized
by the `Eviction` NodeActivity condition. `BudgetWaiting` means eligible Pods
remain and the reconcile was requeued until budget is available. Workload
specs are never mutated.

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
5. Prometheus metrics endpoint `:8080/metrics` exports activity and taint metrics (`interactive_node_controller_activity` and `interactive_node_controller_taint`).

### Rollback and recovery

1. **Stale Agent / Host Offline**: When host reporter heartbeats cease for longer than `staleAfter` (default 1m), the controller enters fail-closed mode and applies `NoSchedule` with value `unavailable`. Normal operations resume automatically upon fresh heartbeat reception.
2. **Canary Rollback and Uninstall**: To roll back or uninstall without wedging deletions or affecting cluster workloads:
   - **Delete `NodeActivity` CRs first while the controller is running**: Remove the `NodeActivity` CR for `homelabdesktop` (via GitOps or `kubectl delete`). While the controller is running, it removes the owned taint from the Node upon finalizer cleanup (`availability.interactive-node.io/cleanup`) before releasing the resource. If the Node is already gone, the controller clears the finalizer immediately so deletion is not wedged by a missing Node.
   - **Remove the controller and CRD second**: Once all `NodeActivity` CRs are fully removed, revert the GitOps commit in `home`; ArgoCD will then prune the Deployment, RBAC, and CRD, or disable the chart in `gitops/ops/kustomization.yaml`.
   - **Avoid premature pruning or replicaCount 0**: If the controller Deployment is pruned or scaled to `replicaCount: 0` before the `NodeActivity` objects are deleted, nothing is running to remove the finalizer; the `NodeActivity` deletion and CRD deletion will hang. Setting `replicaCount: 0` has the exact same wedging effect on deletions as removing the Deployment.
   - **Manual Recovery**: If the controller is already gone or disabled (`replicaCount: 0`) and `NodeActivity` deletion is hanging on the finalizer:
     1. Remove the finalizer to unblock CR and CRD deletion:
        ```bash
        kubectl patch nodeactivity homelabdesktop -n interactive-node-controller --type=merge -p '{"metadata":{"finalizers":null}}'
        ```
     2. Manually remove any leftover owned taint from the node:
        ```bash
        kubectl taint nodes homelabdesktop availability.interactive-node.io/state-
        ```
        (general form: `kubectl taint nodes <node> <key>-`).

## Desktop recovery measurement

Deployment readiness and eviction acceptance are not resource recovery. A repeatable Kind measurement records user-activity detection, admission closure, eviction acceptance, and CPU/memory release separately, and a canary runbook lists the live `homelabdesktop` evidence that is still required. See [desktop recovery canary](desktop-recovery-canary.md). When a burst taint key is configured, rollback also clears that key; the runbook has the command.
