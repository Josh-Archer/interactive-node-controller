# Phase 1 host reporter

The host reporter turns local Linux observations into a narrow `NodeActivity`
status heartbeat. It does not taint nodes, evict pods, mutate workloads, or
contain a generic Kubernetes client.

## Download and install from GitHub Releases

Published release binaries for `linux/amd64` and `linux/arm64` and their SHA-256 checksums are attached to each GitHub Release.

```bash
# Pin version and architecture
VERSION="v0.1.5"
ARCH="amd64" # or arm64

# Download release binary and checksums
curl -fsSLO "https://github.com/Josh-Archer/interactive-node-controller/releases/download/${VERSION}/node-activity-reporter-linux-${ARCH}"
curl -fsSLO "https://github.com/Josh-Archer/interactive-node-controller/releases/download/${VERSION}/checksums.txt"

# Verify SHA-256 checksum
sha256sum -c --ignore-missing checksums.txt

# Install binary and default configuration
sudo install -m 0755 "node-activity-reporter-linux-${ARCH}" /usr/local/bin/node-activity-reporter
sudo install -D -m 0640 examples/config.yaml /etc/node-activity-reporter/config.yaml
/usr/local/bin/node-activity-reporter --version
/usr/local/bin/node-activity-reporter --config /etc/node-activity-reporter/config.yaml --check-config
```

See [docs/artifact-verification.md](artifact-verification.md) to verify the keyless Cosign signature on `checksums.txt` or SLSA build provenance attestations via `gh attestation verify`.

## Build from source

```bash
make build-linux-amd64 # or make build-linux-arm64
sudo install -m 0755 bin/node-activity-reporter-linux-amd64 /usr/local/bin/node-activity-reporter
sudo install -D -m 0640 examples/config.yaml /etc/node-activity-reporter/config.yaml
/usr/local/bin/node-activity-reporter --config /etc/node-activity-reporter/config.yaml --check-config
```

The example uses `reporter.mode: stdout`, so it works without Kubernetes. Run
the service in the foreground first and verify its JSON status and journal
messages. Install `packaging/systemd/node-activity-reporter.service` only after
creating the unprivileged `nodeactivity` service account.

The Ansible role at `ansible/roles/node_activity_reporter` automates installation
via version-pinned GitHub Release download (`install_method: download`), local binary,
or trusted package manager. It does not enroll a cluster identity.

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
`graphical_types` reports interactive unless locked (`LockedHint=yes`) or idle
(`IdleHint=yes`); locked or idle sessions report idle. If no active, interactive
graphical session exists, it reports idle. A missing binary, timeout,
D-Bus/logind failure, or session inspection error degrades to unknown. Session
identifiers are validated before reuse as command arguments.

### Game processes

When enabled, the reporter reads numeric directories below the configured
absolute proc root. `names` are exact `/proc/<pid>/comm` matches;
`command_line_contains` are literal substring matches against NUL-normalized
`cmdline` data. Neither accepts regular expressions or shell fragments. A
vanishing process is ignored, while a proc root with no readable processes is
unknown rather than idle.

### NVIDIA utilization and compute apps

When enabled, the reporter executes the configured absolute `nvidia-smi` path
twice, with fixed argument arrays and never a shell. One query reads GPU
utilization. The other lists compute applications (`--query-compute-apps`).
Any parsed compute application is a game observation, whatever the utilization.
That includes 0%, readings below `utilization_floor_percent`, and a host where
every GPU utilization reading is non-numeric. A resident compute application
keeps the node busy. The reason names the application and the utilization so
the observation is distinct from an empty compute-app list.

Utilization at or above the floor with no compute application stays idle.
Desktop compositing and other graphics clients can exceed the floor without
holding a compute context. The floor only distinguishes that idle reason
(`meets N% floor without compute apps` versus `below N% floor`). It does not
gate a resident compute application.

This signal is deliberately opt-in. A compute application is not universally
equivalent to a game, and graphics-only clients (including games that never
create a compute context) are not attributed here. Use the game-process signal
for those binaries.

When `nvidia-smi` executes successfully but every GPU reports a non-numeric
unavailable utilization reading (such as `N/A`, `[N/A]`, or `[Not Supported]`),
the provider still queries compute applications. A parsed compute application
is a game. With no compute application, the provider reports an idle
observation with reason `gpu utilization unavailable`. Unavailable readings are
also tolerated when valid numeric utilization readings are present on other
GPUs. Missing tools, non-zero command exits, timeouts, empty utilization
output, garbage or out-of-range utilization, and unparsable compute-app rows
degrade to unknown. An empty compute-app listing is a successful observation
that no compute application is running.

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
   unenrolled. If the controller reports `InvalidHeartbeat` /
   `FutureHeartbeat`, compare NTP on this host with the controller clock.
   `heartbeatAt` further ahead than `heartbeat-skew` (default 15s) fails
   closed and does not authorize eviction. See the controller clock-skew
   section in [controller operations](controller.md).

## Phase 1 limitations

- The CRD, controller, effective production RBAC, stale-heartbeat enforcement,
  taint management, and eviction policy land in Phase 2 or later.
- Kubernetes mode cannot succeed until a consumer pre-creates a compatible
  `NodeActivity` and status subresource.
- Phase 1 does not create or rotate credentials. A static, long-lived broad
  credential is explicitly unsupported.
- The NVIDIA signal attributes a game whenever a compute application is
  running, including when utilization is 0%, below the floor, or unavailable.
  Utilization at or above the floor with no compute application, including
  desktop compositing, stays idle. Leave the signal disabled unless that
  meaning fits the host.
- The officially published binary targets are Linux amd64 and arm64. Other
  builds may compile from source but are not covered by automated release testing.
