#!/usr/bin/env bash
set -euo pipefail

test -s internal/controller/eviction.go
test -s internal/controller/eviction_test.go
test -s chart/templates/rbac.yaml
test -s chart/templates/deployment.yaml

# Keep the policy surface intentionally narrow: eviction is the only Pod
# subresource permission and there is no Pod delete or workload mutation grant.
grep -q 'resources: \[pods\]' chart/templates/rbac.yaml
grep -q 'resources: \[pods/eviction\]' chart/templates/rbac.yaml
grep -q 'verbs: \[create\]' chart/templates/rbac.yaml
! grep -q 'verbs:.*delete' chart/templates/rbac.yaml
! grep -q 'resources: \[deployments\|statefulsets\|daemonsets\]' chart/templates/rbac.yaml

grep -q 'eviction-enabled' chart/templates/deployment.yaml
grep -q 'eviction-audit' chart/templates/deployment.yaml
grep -q 'enabled: false' chart/values.yaml
grep -q 'audit: true' chart/values.yaml
grep -q 'interactive-node-controller.io/evictable' internal/controller/eviction.go
grep -q 'policy/v1' internal/controller/eviction.go
echo 'phase 3 safe eviction contract checks passed'
