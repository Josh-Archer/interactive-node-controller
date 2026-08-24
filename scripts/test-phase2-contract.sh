#!/usr/bin/env bash
set -euo pipefail

test -s Dockerfile
test -s cmd/interactive-node-controller/main.go
test -s internal/controller/nodeactivity_controller.go
test -s config/crd/bases/availability.interactive-node.io_nodeactivities.yaml
test -s chart/Chart.yaml
test -s chart/crds/availability.interactive-node.io_nodeactivities.yaml
test -s examples/nodeactivity.yaml

python3 - <<'PY'
from pathlib import Path
import yaml

documents = []
for path in [Path("config/crd/bases/availability.interactive-node.io_nodeactivities.yaml"), Path("chart/crds/availability.interactive-node.io_nodeactivities.yaml")]:
    data = yaml.safe_load(path.read_text())
    documents.append(data)
    assert data["kind"] == "CustomResourceDefinition"
    assert data["spec"]["names"]["kind"] == "NodeActivity"
    version = data["spec"]["versions"][0]
    assert version["subresources"] == {"status": {}}
    assert version["schema"]["openAPIV3Schema"]["properties"]["spec"]["required"] == ["nodeName"]
assert documents[0] == documents[1], "chart CRD must be byte-for-byte semantic equivalent of config CRD"
print("phase 2 CRD contract checks passed")
PY

grep -q 'resources: \[nodes\]' chart/templates/rbac.yaml
grep -q 'resources: \[nodeactivities/status\]' chart/templates/rbac.yaml
grep -q 'TaintAppliedCondition' internal/controller/nodeactivity_controller.go
grep -q 'reconcileOwnedTaint' internal/controller/nodeactivity_controller.go
echo 'phase 2 contract checks passed'
