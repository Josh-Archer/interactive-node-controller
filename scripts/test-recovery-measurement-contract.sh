#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"

test -s docs/desktop-recovery-canary.md
test -s examples/recovery-canary-values.yaml
test -s examples/recovery-workloads.yaml
test -s internal/recovery/budget.go
test -s internal/recovery/evaluate.go
test -s internal/controller/desktop_recovery_kind_test.go
test -s scripts/measure-desktop-recovery.sh
test -s cmd/recovery-report/main.go
bash -n scripts/measure-desktop-recovery.sh

# The harness creates its own Kind cluster and refuses a live scope.
grep -q 'RECOVERY_SCOPE' scripts/measure-desktop-recovery.sh
grep -q 'unset KUBECONFIG' scripts/measure-desktop-recovery.sh
grep -q 'kindest/node:v1.32.0' scripts/measure-desktop-recovery.sh
grep -q 'inc-recovery' scripts/measure-desktop-recovery.sh
grep -q 'kind delete cluster' scripts/measure-desktop-recovery.sh
grep -q 'RECOVERY_KIND=true' scripts/measure-desktop-recovery.sh
grep -q 'TestDesktopRecoveryKind' scripts/measure-desktop-recovery.sh

for scenario in idle-to-interactive idle-to-game return-to-idle scheduled-lead-time reporter-failure controller-restart-leader-change pdb-blocked-eviction; do
  grep -q "$scenario" internal/recovery/budget.go
  grep -q "$scenario" docs/desktop-recovery-canary.md
done

for phase in user-activity-detection admission-closure eviction-acceptance resource-release; do
  grep -q "$phase" internal/recovery/budget.go
  grep -q "$phase" docs/desktop-recovery-canary.md
done

grep -q 'eviction acceptance is not' docs/desktop-recovery-canary.md
grep -q 'pod-absent' docs/desktop-recovery-canary.md
grep -q 'requests-dropped' docs/desktop-recovery-canary.md
grep -q 'homelabdesktop' docs/desktop-recovery-canary.md
grep -q 'allowScheduling: false' docs/desktop-recovery-canary.md
grep -q 'ArgoCD' docs/desktop-recovery-canary.md
grep -q 'nvidia-smi' docs/desktop-recovery-canary.md
grep -q 'desktop-visible' docs/desktop-recovery-canary.md
grep -q 'Rollback' docs/desktop-recovery-canary.md
grep -q '90s' docs/desktop-recovery-canary.md
grep -q 'protected-neighbor' docs/desktop-recovery-canary.md
grep -q 'availability.interactive-node.io/burst' examples/recovery-canary-values.yaml
grep -q 'enabled: true' examples/recovery-canary-values.yaml
grep -q 'audit: false' examples/recovery-canary-values.yaml
grep -q 'staleAfter: 1m' examples/recovery-canary-values.yaml
grep -q 'enabled: false' chart/values.yaml
grep -q 'test-recovery:' Makefile
grep -q 'measure-desktop-recovery.sh' Makefile
grep -q 'recovery-kind:' .github/workflows/ci.yml
grep -q 'docs/desktop-recovery-canary.md' README.md

rendered=$(helm template interactive-node-controller chart -f examples/recovery-canary-values.yaml)
echo "$rendered" | grep -q -- '--eviction-enabled=true'
echo "$rendered" | grep -q -- '--eviction-audit=false'
echo "$rendered" | grep -q -- '--burst-taint-key=availability.interactive-node.io/burst'
echo "$rendered" | grep -q -- '--stale-after=1m'
default_render=$(helm template interactive-node-controller chart)
echo "$default_render" | grep -q -- '--eviction-enabled=false'
echo "$default_render" | grep -q -- '--eviction-audit=true'
if echo "$default_render" | grep -q -- '--burst-taint-key='; then
  echo 'chart defaults must not pass --burst-taint-key' >&2
  exit 1
fi

echo 'desktop recovery measurement contract checks passed'
