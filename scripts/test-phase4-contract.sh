#!/usr/bin/env bash
set -euo pipefail

test -s docs/roadmap.md
test -s docs/controller.md
test -s chart/Chart.yaml
test -s chart/values.yaml
test -s examples/nodeactivity.yaml
test -s examples/nodeactivity-rbac.yaml

# Phase 4 canary GitOps contract checks
grep -q 'Phase 4' docs/roadmap.md
grep -q 'homelabdesktop' docs/roadmap.md
grep -q 'Phase 4 home canary GitOps integration' docs/controller.md
grep -q 'Node and storage review' docs/controller.md
grep -q 'Rollout verification' docs/controller.md
grep -q 'Rollback and recovery' docs/controller.md
grep -q 'allowScheduling: false' docs/controller.md

# Examples and chart defaults verify least privilege and safe defaults
grep -q 'homelabdesktop' examples/nodeactivity-rbac.yaml
grep -q 'resources: \["nodeactivities/status"\]' examples/nodeactivity-rbac.yaml
grep -q 'verbs: \["patch"\]' examples/nodeactivity-rbac.yaml
grep -q 'enabled: false' chart/values.yaml
grep -q 'audit: true' chart/values.yaml
grep -q 'failClosed: true' chart/values.yaml

echo 'phase 4 canary contract checks passed'
