#!/usr/bin/env bash
set -euo pipefail

# Phase 5 Step 1 contract checks: License and contribution terms
test -s LICENSE
grep -q 'Apache License' LICENSE
grep -q 'Version 2.0, January 2004' LICENSE
grep -q 'Interactive Node Controller Contributors' LICENSE
grep -q 'Apache License, Version 2.0' README.md
grep -q 'Apache License, Version 2.0' CONTRIBUTING.md

# Phase 5 Step 2 contract checks: Generic consumer install and GitOps examples
test -s examples/helm-values.yaml
test -s examples/gitops-kustomization.yaml
test -s examples/nodeactivity.yaml
test -s examples/nodeactivity-rbac.yaml
test -s examples/config.yaml

grep -q 'helm upgrade --install interactive-node-controller' README.md
grep -q 'oci://ghcr.io/josh-archer/charts/interactive-node-controller' README.md
grep -q 'sha256:' README.md
grep -q 'never directly deploys to a consumer cluster' README.md
grep -q 'examples/helm-values.yaml' README.md
grep -q 'examples/gitops-kustomization.yaml' README.md

grep -q 'oci://ghcr.io/josh-archer/charts/interactive-node-controller' VERSIONING.md
grep -q 'examples/helm-values.yaml' VERSIONING.md
grep -q 'publicly accessible' VERSIONING.md
grep -q 'registry authentication' VERSIONING.md

grep -q 'eviction:' examples/helm-values.yaml
grep -q 'interactive-node-controller.io/evictable' examples/helm-values.yaml
grep -q 'availability.interactive-node.io/state' examples/helm-values.yaml

# Phase 5 Step 3 contract checks: Cosign, SBOM, and chart verification runbook
test -s docs/artifact-verification.md
grep -q 'cosign verify' docs/artifact-verification.md
grep -q 'gh attestation verify' docs/artifact-verification.md
grep -q 'helm pull oci://ghcr.io/josh-archer/charts/interactive-node-controller' docs/artifact-verification.md
grep -q 'token.actions.githubusercontent.com' docs/artifact-verification.md
grep -q 'docs/artifact-verification.md' README.md
grep -q 'docs/artifact-verification.md' VERSIONING.md

# Verify release pipeline security invariants
grep -q 'cosign sign --yes' .github/workflows/release.yml
grep -q 'provenance: mode=max' .github/workflows/release.yml
grep -q 'sbom: true' .github/workflows/release.yml
grep -q 'helm push' .github/workflows/release.yml
grep -q 'sigstore/cosign-installer' .github/workflows/release.yml

echo 'phase 5 public release contract checks passed'
