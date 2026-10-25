#!/usr/bin/env bash
# Measure desktop recovery on a disposable Kind cluster.
#
# This script never reads an ambient kubeconfig and never targets a consumer
# cluster. It creates kind cluster inc-recovery, runs the in-process controller
# harness, writes a JSON report, and deletes that cluster on exit.
#
# A live GitOps canary is a different procedure. See
# docs/desktop-recovery-canary.md. Set RECOVERY_SCOPE=live to print that
# boundary and exit.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"

if [[ "${RECOVERY_SCOPE:-kind}" != "kind" ]]; then
  echo "This harness only creates a disposable Kind cluster." >&2
  echo "Live GitOps steps, rollback, and the evidence that is still required are in docs/desktop-recovery-canary.md." >&2
  exit 2
fi

if ! command -v kind >/dev/null 2>&1; then
  echo "kind is required (CI installs sigs.k8s.io/kind@v0.27.0). Refusing to use any existing cluster." >&2
  exit 2
fi
if ! command -v docker >/dev/null 2>&1; then
  echo "docker is required to create the disposable Kind cluster." >&2
  exit 2
fi

# Ignore any kubeconfig from the environment, including a home cluster.
if [[ -n "${KUBECONFIG:-}" || -n "${RECOVERY_KUBECONFIG:-}" ]]; then
  echo "ignoring ambient kubeconfig; creating disposable Kind cluster inc-recovery" >&2
fi
unset KUBECONFIG
unset RECOVERY_KUBECONFIG

name=${RECOVERY_CLUSTER_NAME:-inc-recovery}
report=${RECOVERY_REPORT:-"$root/desktop-recovery-report.json"}
tmpdir=$(mktemp -d)
export KUBECONFIG="$tmpdir/kubeconfig"
export RECOVERY_KIND=true
export RECOVERY_REPORT="$report"

cleanup() {
  kind delete cluster --name "$name" >/dev/null 2>&1 || true
  rm -rf "$tmpdir"
}
trap cleanup EXIT

kind create cluster --name "$name" --image kindest/node:v1.32.0 --wait 90s --kubeconfig "$KUBECONFIG"

set +e
go test ./internal/controller -run '^TestDesktopRecoveryKind$' -count=1 -timeout 15m -v
status=$?
set -e

if [[ -f "$RECOVERY_REPORT" ]]; then
  echo "recovery report: $RECOVERY_REPORT"
fi
if [[ "$status" -ne 0 ]]; then
  exit "$status"
fi
go run ./cmd/recovery-report -report "$RECOVERY_REPORT"
