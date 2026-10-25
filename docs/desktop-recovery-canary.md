# Desktop recovery canary

This runbook is the repeatable measurement for desktop recovery. It is the
groundwork for the home GitOps canary in
[#5](https://github.com/Josh-Archer/interactive-node-controller/issues/5) and
the API and Kind coverage in
[#37](https://github.com/Josh-Archer/interactive-node-controller/issues/37).
The measurement itself is
[#62](https://github.com/Josh-Archer/interactive-node-controller/issues/62).

This repository does not deploy to a consumer cluster. The Kind harness below
is the procedure that can run here. The live `homelabdesktop` canary stays in
the consumer GitOps repository (`Josh-Archer/home`) and is listed under
[What a live run still has to do](#what-a-live-run-still-has-to-do).

## What a sample proves

Recovery is four samples, recorded separately. A later sample does not replace
an earlier one.

| Phase | What is observed |
| --- | --- |
| `user-activity-detection` | The controller's `TaintApplied` condition matches the reported or scheduled state. |
| `admission-closure` | The owned taint and burst taint are on the node, the schedulable label is absent, and a new opted-out probe stays Pending with an empty `nodeName`. |
| `admission-reopen` | After a return to fresh idle, the owned taints are gone, the schedulable label is present, and a new probe reaches Running on the node. |
| `eviction-acceptance` | The API server accepted the controller's Eviction request for the running Pod's UID (evidence `eviction-accepted`). The harness records this time from the controller's Eviction client rather than by polling for a `deletionTimestamp`, so a Pod that terminates quickly is not missed. |
| `resource-release` | That UID is gone and no Pod of that workload still holds CPU or memory requests on the node. |

Recorded eviction acceptance is not proof of recovery. It only means the
Eviction API accepted the request. The Pod can keep its CPU and memory requests
until the kubelet finishes termination. Controller readiness, an ArgoCD
`Healthy` status, and a `TaintApplied` condition are not proof either.

The only sample that can satisfy recovery is `resource-release` whose evidence
contains both `pod-absent` and `requests-dropped`, whose acceptance-to-release
gap is at least one second, and whose duration is inside the end-to-end budget.
Evidence `still-bound` means the requests are still held.

## Target budget

The published target is `desktop-recovery-v1`. It is a budget to record against,
not a claim that a desktop already meets it.

| Budget | Duration | Why |
| --- | --- | --- |
| User-activity detection | 45s | Reporter sample interval 5s, interactive debounce 2, heartbeat 15s, plus one reconcile. |
| Admission closure or reopen | 45s | The taint and label change in the same reconcile as detection. |
| Eviction acceptance | 60s | Covers the chart `retryBackoff` of 30s and one retry. |
| Resource release and end-to-end | 90s | Termination grace plus kubelet cleanup after acceptance. |
| GPU release | 90s | Same end-to-end number. Live only. |
| Fail-closed slack | 45s | Added to `staleAfter` for reporter-failure detection. |
| Lead apply slack | 45s | How long after the computed lead boundary the taint must be visible. |
| Leader restore | 30s | A new leader repairs a drifted node. |
| Kind `staleAfter` | 20s | Harness only. Recorded as `harnessStaleAfter`. |
| Production `staleAfter` | 1m | Chart default. A live run must not shorten it. |
| Recommended lead time | 3m | Live windows should arm at least the 90s end-to-end budget before they start. |

Reporter-failure detection is allowed `staleAfter` plus the 45s slack. That is
65s in the Kind harness and 1m45s when `staleAfter` stays 1m.

Kind detection is `controller-only`: the trigger is an accepted `NodeActivity`
status patch, or a lead boundary, or a stopped synthetic reporter. A live
report uses `host-to-controller` and starts the clock at the desktop event.

The Kind scheduled window uses a lead of about 10 seconds so the boundary
arrives during the test. That lead is shorter than the 90s end-to-end budget,
so the harness leaves `preWindowReleaseProven` false. A live enrollment should
set `spec.leadTime` to at least 3m before claiming the node was clear before
the window.

## Workloads

Only disposable opted-in workloads may be evicted. Sizes are container
requests. None of them request a GPU. The consumer manifest is
`examples/recovery-workloads.yaml`. The Kind harness creates the same sizes
from `recovery.Catalog()` and adds control-plane and
`desktop-recovery.example/unrelated` tolerations so Pods can schedule on a Kind
node. Those tolerations are not opt-in and are not in the consumer manifest.

| Workload | Requests | Opt-in | Expected |
| --- | --- | --- | --- |
| `lossy-runner` | 100m CPU, 64Mi memory | evictable and lossy, schedulable selector | Evicted on interactive, game, lead, reporter failure, and leader change. |
| `standard-runner` | 50m CPU, 32Mi memory | evictable only, no selector | Evicted only for a fresh active game (including the game sample after a leader change). |
| `protected-neighbor` | 200m CPU, 128Mi memory | none | UID unchanged and still bound in every scenario. |
| `pinned-neighbor` | 20m CPU, 16Mi memory | evictable and pinned | UID unchanged and still bound in every scenario. |
| `pdb-runner` | 80m CPU, 48Mi memory | evictable and lossy, schedulable selector | Blocked while `minAvailable: 1` has zero disruptions allowed, then released after the PDB is removed. |
| `admission-probe` | 10m CPU, 8Mi memory | none, schedulable selector | Pending while closed. Running on the node after reopen. |

Protected namespaces `kube-system`, `kube-public`, and `kube-node-lease` are
snapshotted by Pod UID before the scenarios and after them. The sets must be
equal. The harness also adds `desktop-recovery.example/unrelated=keep:NoSchedule`
and requires that taint, plus the node's original taints, to still be present
at the end. The controller must not clear an unrelated taint.

Interactive admission closure depends on the burst key
`availability.interactive-node.io/burst`. `PreferNoSchedule` alone does not
reject a Pod. Chart defaults leave `taint.burstKey` empty, `eviction.enabled`
false, and `eviction.audit` true. `examples/recovery-canary-values.yaml` is the
consumer opt-in. It sets the burst key, enables eviction, turns audit off, and
keeps `staleAfter: 1m`.

## Scenarios

The harness runs these in order. Each one records timings, the workload sizes
above, and any blocked case.

1. `idle-to-interactive`. Fresh idle becomes interactive. Lossy runner is
   evicted. Standard runner stays. Admission probe stays Pending.
2. `return-to-idle`. The node becomes fresh idle again. Neither runner is
   evicted. The lossy Deployment is allowed to start a new Pod. Admission
   reopens. Eviction and release samples are recorded as absent, with
   `uid-unchanged`, and do not satisfy recovery.
3. `idle-to-game`. Fresh active game evicts both the lossy runner and the
   standard runner. Protected and pinned neighbors stay.
4. `scheduled-lead-time`. The live report stays idle. A window is patched with
   the computed lead. The node stays idle until the boundary, then the
   scheduled game taint is applied and only the lossy runner is evicted.
   `leadTime` and `windowStart` are recorded. `preWindowReleaseProven` stays
   false because the harness lead is shorter than 90s.
5. `reporter-failure`. The synthetic reporter stops heartbeats. After
   `staleAfter` the controller applies the fail-closed `NoSchedule` taint.
   Only the lossy runner is evicted. Detection uses the stale-after budget,
   not the 45s interactive budget.
6. `controller-restart-leader-change`. A second in-process manager is started,
   the first leader is cancelled, and the new leader is allowed to acquire the
   lease. The test then strips the schedulable label and adds a wrong primary
   taint. The new leader must restore idle protection inside 30s. A following
   game report then evicts both runners. Leader-election and leader-restore
   samples are extra and do not themselves satisfy recovery.
7. `pdb-blocked-eviction`. The node returns to idle, then `pdb-runner` is
   enrolled under a PDB with `minAvailable: 1`. The transition is interactive,
   so the standard runner is not eligible. The lossy runner is evicted. The
   PDB runner stays Running with the same UID and no `deletionTimestamp` for
   a checked interval. That blocked case is recorded before the PDB is
   deleted. After the PDB is removed, acceptance and release of `pdb-runner`
   are measured separately, including `after-pdb-removed`. A 429 from the
   Eviction API is never bypassed.

## Kind harness

`make test-recovery` is optional. It is not part of `make verify`. It needs
Docker and Kind (`sigs.k8s.io/kind@v0.27.0`, node image `kindest/node:v1.32.0`).

```bash
make test-recovery
```

The script ignores any ambient `KUBECONFIG`, creates cluster `inc-recovery`,
exports a private kubeconfig, runs `TestDesktopRecoveryKind`, writes
`desktop-recovery-report.json`, and deletes only that cluster. Set
`RECOVERY_REPORT` to choose another path. `RECOVERY_SCOPE` other than `kind`
exits immediately and points back at this runbook. The Go test also refuses a
context whose name does not start with `kind-`.

Check a report without re-running the cluster:

```bash
go run ./cmd/recovery-report -report desktop-recovery-report.json
```

The checker rejects a Kind report that sets `performanceClaimed`, a release
sample that lacks `pod-absent` and `requests-dropped`, a protected UID change,
and a PDB case marked accepted or released while it was blocked.

The harness controller runs in-process. It is not the Helm chart. It uses
`staleAfter` 20s, burst key `availability.interactive-node.io/burst`, and
eviction enabled with audit off. Disposable Pods use a 3 second termination
grace and a 2 second `preStop` sleep so acceptance and release differ by at
least one second. Pause exits promptly on `SIGTERM`, so the `preStop` sleep is
what creates that gap.

## Evidence collection

Reports use schema `desktop-recovery.v1`.

- `environment` is `kind` or `live-gitops`.
- `detectionScope` is `controller-only` or `host-to-controller`.
- `budget` is the target above. Kind may set `harnessStaleAfter` to 20s. A
  live budget must set that field equal to the production `staleAfter` of 1m.
- `workloadCatalog` records every workload size, including `protected-neighbor`.
- `scenarios` follow the order above. Each phase sample has `triggerAt`,
  `observedAt`, `durationMillis`, `budget`, `withinBudget`, `evidence`, and
  `satisfiesRecovery`.
- `blocked` records a refused eviction. `evictionAccepted` and
  `resourcesReleased` stay false. Evidence includes `pdb-blocked` and
  `still-bound`.
- `protectedNamespacesBefore` and `protectedNamespacesAfter` are sorted Pod
  UID lists.
- `unrelatedTaintPreserved` records the unrelated taint check.
- `gpuMeasured`, `desktopVisible`, and `performanceClaimed` stay false for Kind.
  Kind `gpuEvidence` says there is no GPU.
- `liveRunRemaining` lists the live steps that this run did not do.

Collect the same fields on a live run. Add the host-reporter log line that
shows the desktop transition, the node taint and label at each phase, the Pod
UID and requests before and after release, and a host GPU sample. Store the
JSON report next to the GitOps revision that was under test.

## Rollback

Stop eviction before deleting the enrollment, and delete the enrollment while
a controller is still running. Do not scale `replicaCount` to 0 first. A
scaled-down or pruned controller cannot run the finalizer, and both the
`NodeActivity` and the CRD then stick.

Live rollback, performed in the consumer GitOps repository:

1. Set `eviction.enabled` to false and `eviction.audit` to true, and sync that
   change. New Eviction API calls stop. Already-accepted Pods keep terminating
   under their own grace period.
2. Delete the disposable canary workloads if they are still present. Leave
   `protected-neighbor`, pinned workloads, and every protected namespace alone.
3. Delete the `NodeActivity` for `homelabdesktop` while the controller is
   running. The finalizer `availability.interactive-node.io/cleanup` removes
   the owned taint before the object disappears.
4. If a taint or label remains, remove the burst key as well as the primary
   key, and clear the schedulable label:

   ```bash
   kubectl taint nodes homelabdesktop availability.interactive-node.io/burst-
   kubectl taint nodes homelabdesktop availability.interactive-node.io/state-
   kubectl label nodes homelabdesktop availability.interactive-node.io/schedulable-
   ```

5. Revert the home pin (chart version and image digest) through GitOps. ArgoCD
   then prunes the Deployment, RBAC, and CRD only after the `NodeActivity`
   objects are gone.

Eviction acceptance is not proof the previous revision is restored. After
rollback, confirm the owned taints are absent, the unrelated taints are still
present, and protected workloads still have the same Pod UIDs.

The Kind harness is not this rollback. Its cleanup deletes the test namespace
and cluster `inc-recovery` only.

## What a live run still has to do

A Kind report keeps `performanceClaimed` false. These items are copied into
`liveRunRemaining` until a live report records them:

- Pin the reviewed chart version and image digest in the home repository
  GitOps source. This repository does not deploy it. Copy
  `examples/recovery-canary-values.yaml` and replace `image.digest` with the
  reviewed release digest. Render it with Helm in the home repository. Chart
  defaults in this repository stay eviction disabled and audit-only.
- Canary only `homelabdesktop`, and only after the node and Longhorn
  replica-scheduling review (`allowScheduling: false` in the home storage
  config). Interactive restarts must not rebuild storage replicas.
- Confirm the ArgoCD revision is `Synced` and `Healthy`, then confirm
  controller leadership and reporter heartbeats. Deployment readiness is not
  recovery.
- Drive `idle-to-interactive`, `idle-to-game`, `return-to-idle`,
  `scheduled-lead-time`, `reporter-failure`, `controller-restart-leader-change`,
  and `pdb-blocked-eviction` with the real host reporter. The Kind trigger is
  a synthetic status patch.
- Measure GPU release on the desktop with the device plugin and a host sample
  such as `nvidia-smi`. Kind has no GPU, so `gpuMeasured` stays false.
- Record desktop-visible recovery separately from eviction acceptance and from
  Pod readiness. A person at the desktop, or an equivalent session signal, has
  to observe the machine usable again. Kind sets `desktopVisible` false.
- Use the production `staleAfter` from the canary values. Do not shorten it to
  meet the budget.
- Roll back through GitOps: stop eviction, delete the `NodeActivity` while the
  controller is running, then revert the home pin.

`performanceClaimed` is valid only when `environment` is `live-gitops`,
`detectionScope` is `host-to-controller`, GPU evidence includes `nvidia-smi`,
desktop-visible evidence is non-empty, `liveRunRemaining` is empty, every
applicable sample is within budget, and the scheduled scenario proves
pre-window release under a `leadTime` of at least 90s.
