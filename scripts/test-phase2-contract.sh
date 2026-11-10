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
    assert data["metadata"]["annotations"]["argocd.argoproj.io/sync-wave"] == "-1"
    assert data["spec"]["names"]["kind"] == "NodeActivity"
    version = data["spec"]["versions"][0]
    assert version["subresources"] == {"status": {}}
    assert version["schema"]["openAPIV3Schema"]["properties"]["spec"]["required"] == ["nodeName"]
assert documents[0] == documents[1], "chart CRD must be byte-for-byte semantic equivalent of config CRD"
print("phase 2 CRD contract checks passed")
PY

grep -q 'resources: \[nodes\]' chart/templates/rbac.yaml
grep -q 'resources: \[nodeactivities/status\]' chart/templates/rbac.yaml
grep -q 'resources: \[customresourcedefinitions\]' chart/templates/rbac.yaml
grep -q 'resourceNames: \[nodeactivities.availability.interactive-node.io\]' chart/templates/rbac.yaml
grep -q 'argocd.argoproj.io/sync-wave: "0"' chart/templates/deployment.yaml
grep -q 'argocd.argoproj.io/sync-wave: "1"' examples/nodeactivity.yaml
grep -q 'NewCRDSchemaReadyzCheck' cmd/interactive-node-controller/main.go
grep -q 'TaintAppliedCondition' internal/controller/nodeactivity_controller.go
grep -q 'reconcileOwnedTaint' internal/controller/nodeactivity_controller.go
grep -q 'Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.requestsForEnrolledNode)' internal/controller/nodeactivity_controller.go

python3 - <<'PY'
from pathlib import Path

lines = Path("chart/templates/rbac.yaml").read_text().splitlines()
for index, line in enumerate(lines):
    if line.strip() == "resources: [nodes]":
        verbs = lines[index + 1].strip()
        expected = "verbs: [get, list, watch, patch, update]"
        assert verbs == expected, f"node RBAC verbs = {verbs!r}"
        break
else:
    raise SystemExit("nodes rule missing from chart RBAC")
print("node watch RBAC contract checks passed")
PY
python3 - <<'PY'
from pathlib import Path
import re
import yaml

crd = yaml.safe_load(Path("config/crd/bases/availability.interactive-node.io_nodeactivities.yaml").read_text())
assert crd["kind"] == "CustomResourceDefinition"
crd_name = crd["metadata"]["name"]
assert crd_name, "CRD metadata.name must be set"
assert "*" not in crd_name

chart_crd = yaml.safe_load(Path("chart/crds/availability.interactive-node.io_nodeactivities.yaml").read_text())
assert chart_crd["metadata"]["name"] == crd_name

sanitized = re.sub(r"\{\{.*?\}\}", "placeholder", Path("chart/templates/rbac.yaml").read_text())
documents = [doc for doc in yaml.safe_load_all(sanitized) if doc]
cluster_role = next(doc for doc in documents if doc.get("kind") == "ClusterRole")

crd_rules = []
for rule in cluster_role.get("rules", []):
    api_groups = rule.get("apiGroups") or []
    resources = rule.get("resources") or []
    matches_group = "apiextensions.k8s.io" in api_groups or "*" in api_groups
    matches_resource = any(
        resource in {"customresourcedefinitions", "customresourcedefinitions/*", "*"}
        for resource in resources
    )
    if matches_group and matches_resource:
        crd_rules.append(rule)

assert len(crd_rules) == 1, f"ClusterRole must grant CRD access with exactly one rule, got {crd_rules!r}"
rule = crd_rules[0]
assert rule.get("apiGroups") == ["apiextensions.k8s.io"], rule
assert rule.get("resources") == ["customresourcedefinitions"], rule
assert rule.get("verbs") == ["get"], rule
assert rule.get("resourceNames") == [crd_name], rule
for field in ("apiGroups", "resources", "verbs", "resourceNames"):
    assert "*" not in (rule.get(field) or []), f"{field} must not use a wildcard: {rule.get(field)!r}"
for verb in ("list", "watch", "create", "update", "patch", "delete"):
    assert verb not in (rule.get("verbs") or []), rule
print("scoped CRD get RBAC contract checks passed")
PY
echo 'phase 2 contract checks passed'
