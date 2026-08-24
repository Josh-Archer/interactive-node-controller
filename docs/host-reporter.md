# Phase 1 host reporter

The host reporter turns local Linux observations into a narrow `NodeActivity`
status heartbeat. It does not taint nodes, evict pods, mutate workloads, or
contain a generic Kubernetes client.

## Build and run

```bash
make build-linux-amd64
sudo install -m 0755 bin/node-activity-reporter-linux-amd64 /usr/local/bin/node-activity-reporter
sudo install -D -m 0640 examples/config.yaml /etc/node-activity-reporter/config.yaml
/usr/local/bin/node-activity-reporter --config /etc/node-activity-reporter/config.yaml --check-config
```

The example uses `reporter.mode: stdout`, so it works without Kubernetes. Run
the service in the foreground first and verify its JSON status and journal
messages. Install `packaging/systemd/node-activity-reporter.service` only after
creating the unprivileged `nodeactivity` service account.

The Ansible role at `ansible/roles/node_activity_reporter` performs those host
steps. It accepts either a controller-supplied binary or a package name from an
already trusted package repository. It does not enroll a cluster identity.

## State and signal contract

Every status contains:

- `state`: `active`, `idle`, `unknown`, or `stale`;
- `activity`: `game`, `interactive`, `idle`, or `unknown`;
- `reason`: the winning observation or degradation reason;
- `observedAt` and `heartbeatAt` timestamps;
- `generation`, incremented when state or activity changes; and
- `failClosed: true`.

The versioned Phase 1 wire types and API group constants live in
`api/v1alpha1/types.go`; the CRD itself remains a Phase 2 deliverable.

Positive observations use fixed precedence: `game > interactive > idle`.
Game and interactive observations win even when another provider is degraded.
Idle is emitted only when every enabled provider reports idle. Any uncertainty
therefore becomes unknown, then stale after `agent.stale_after` without a good
observation. Phase 2's controller will use the heartbeat deadline to retain a
`NoSchedule` posture when the host reporter disappears.

Transitions require consecutive samples. Defaults are one game sample, two
interactive samples, and three idle samples, making activity protection fast
and return-to-idle conservative. During a transition the previous established
state is retained; the reason says that debouncing is in progress.

### logind

When enabled, the reporter executes the configured absolute `loginctl` path
with fixed argument arrays—never a shell. An active session whose type is in
`graphical_types` reports interactive; otherwise it reports idle. A missing
binary, timeout, D-Bus/logind failure, or session inspection error degrades to
unknown. Session identifiers are validated before reuse as command arguments.

### Game processes

When enabled, the reporter reads numeric directories below the configured
absolute proc root. `names` are exact `/proc/<pid>/comm` matches;
`command_line_contains` are literal substring matches against NUL-normalized
`cmdline` data. Neither accepts regular expressions or shell fragments. A
vanishing process is ignored, while a proc root with no readable processes is
unknown rather than idle.

### NVIDIA utilization

When enabled, the reporter executes the configured absolute `nvidia-smi` path
with fixed query arguments. Any GPU at or above the configured threshold is a
game observation. This is deliberately opt-in because utilization is not
universally equivalent to a game. Missing tools, timeouts, `N/A`, malformed
output, or no GPUs degrade to unknown.

## NodeActivity reporting API

Kubernetes mode issues only this merge patch:

```text
PATCH /apis/availability.interactive-node.io/v1alpha1/namespaces/{namespace}/nodeactivities/{name}/status
Content-Type: application/merge-patch+json

{"status": { ...heartbeat fields... }}
```

The API server must be HTTPS, namespace and name must be DNS labels, the CA is
explicit, and the bearer token is re-read before every request so an external
enrollment mechanism can rotate it. Use the eventual Role shape in
`examples/nodeactivity-rbac.yaml`: `patch` on `nodeactivities/status`,
restricted with `resourceNames` to exactly one object.
Do not provide a kubeconfig or credential with Node, workload, eviction, or
cluster-wide permissions. Prefer short-lived TokenRequest credentials delivered
and rotated by a consumer-owned enrollment mechanism.

## Runbook

1. Validate configuration with `--check-config`.
2. Start in stdout mode and check journal reasons across login, game launch,
   game exit, and idle transitions.
3. Confirm missing `loginctl` or `nvidia-smi` produces unknown/stale without a
   crash.
4. After Phase 2 installs the CRD, pre-create the one `NodeActivity`, bind only
   its status subresource, provision rotating token and CA files, then switch to
   Kubernetes mode.
5. Confirm the API object heartbeat advances and generation changes only on
   state/activity transitions. Revoke the reporter identity if the host is
   unenrolled.

## Phase 1 limitations

- The CRD, controller, effective production RBAC, stale-heartbeat enforcement,
  taint management, and eviction policy land in Phase 2 or later.
- Kubernetes mode cannot succeed until a consumer pre-creates a compatible
  `NodeActivity` and status subresource.
- Phase 1 does not create or rotate credentials. A static, long-lived broad
  credential is explicitly unsupported.
- The NVIDIA signal is utilization-based, not process attribution. Leave it
  disabled unless that meaning fits the host.
- The shipped binary target is Linux amd64. Other builds may compile but are
  not Phase 1's supported deployment target.
