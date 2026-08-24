#!/usr/bin/env bash
set -euo pipefail

test -s go.mod
test -s cmd/node-activity-reporter/main.go
test -s examples/config.yaml
test -s packaging/systemd/node-activity-reporter.service
test -s ansible/roles/node_activity_reporter/tasks/main.yml
test -s docs/host-reporter.md

grep -q '^User=nodeactivity$' packaging/systemd/node-activity-reporter.service
grep -q '^NoNewPrivileges=true$' packaging/systemd/node-activity-reporter.service
grep -q '^ProtectSystem=strict$' packaging/systemd/node-activity-reporter.service
grep -q 'resources: \["nodeactivities/status"\]' examples/nodeactivity-rbac.yaml
grep -q 'resourceNames: \["homelabdesktop"\]' examples/nodeactivity-rbac.yaml

if rg -n 'resources:.*\bnodes\b|/api/v1/nodes|corev1\.Node' \
  --glob '!docs/**' --glob '!**/*_test.go' --glob '!scripts/test-phase1-contract.sh' .; then
  echo 'Node mutation surface found in Phase 1 implementation' >&2
  exit 1
fi

python3 - <<'PY'
from pathlib import Path
import yaml

files = sorted(Path("examples").glob("*.yaml")) + sorted(Path("ansible").glob("**/*.yml"))
for path in files:
    with path.open(encoding="utf-8") as stream:
        list(yaml.safe_load_all(stream))
    print(path)
PY
echo 'phase 1 contract checks passed'
