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
grep -q 'packages: write' .github/workflows/release.yml
grep -q 'org.opencontainers.image.source=https://github.com/' .github/workflows/release.yml
grep -q 'org.opencontainers.image.source: https://github.com/Josh-Archer/interactive-node-controller' chart/Chart.yaml
grep -q 'gh release view .*--repo "\$GITHUB_REPOSITORY"' .github/workflows/release.yml
grep -q 'sigstore/cosign-installer' .github/workflows/release.yml

# Phase 5 Step 4 contract checks: Host reporter versioned release artifacts
grep -q 'node-activity-reporter-linux-amd64' .github/workflows/release.yml
grep -q 'node-activity-reporter-linux-arm64' .github/workflows/release.yml
grep -q 'node-activity-reporter-linux-amd64.tar.gz' .github/workflows/release.yml
grep -q 'node-activity-reporter-linux-arm64.tar.gz' .github/workflows/release.yml
grep -q 'checksums.txt' .github/workflows/release.yml
grep -q 'main.version=' .github/workflows/release.yml
grep -q 'actions/attest-build-provenance' .github/workflows/release.yml
grep -q 'cosign sign-blob' .github/workflows/release.yml
grep -q 'gh release upload' .github/workflows/release.yml

# Verify immutable release idempotency contract
grep -q 'cmp -s' .github/workflows/release.yml

# Verify verification runbooks for reporter artifacts
grep -q 'Verify Host Reporter Binaries & Checksums' docs/artifact-verification.md
grep -q 'cosign verify-blob' docs/artifact-verification.md
grep -q 'checksums.txt.bundle' docs/artifact-verification.md
grep -q 'gh attestation verify node-activity-reporter-linux-amd64' docs/artifact-verification.md
grep -q 'sha256sum -c' docs/artifact-verification.md

# Verify Ansible download and Renovate integration
grep -q "node_activity_reporter_install_method == 'download'" ansible/roles/node_activity_reporter/tasks/main.yml
grep -q 'datasource=github-releases depName=Josh-Archer/interactive-node-controller' ansible/roles/node_activity_reporter/defaults/main.yml
grep -q 'datasource=github-releases depName=Josh-Archer/interactive-node-controller' ansible/roles/node_activity_reporter/README.md

# Verify Makefile targets and version injection
grep -q 'build-linux-arm64:' Makefile
grep -q 'main.version=' Makefile

# Contract-test local build, version injection, and smoke checks
TMP_TEST_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_TEST_DIR" bin' EXIT
make VERSION=v9.9.9 build-linux-amd64 build-linux-arm64 >/dev/null
test "$(./bin/node-activity-reporter-linux-amd64 --version)" = "v9.9.9"
./bin/node-activity-reporter-linux-amd64 --config examples/config.yaml --check-config >/dev/null
readelf -h ./bin/node-activity-reporter-linux-arm64 | grep -q 'AArch64'
go version -m ./bin/node-activity-reporter-linux-arm64 | grep -q 'GOARCH=arm64'
(cd bin && sha256sum node-activity-reporter-linux-amd64 node-activity-reporter-linux-arm64 > "$TMP_TEST_DIR/checksums.txt")
(cd bin && sha256sum -c "$TMP_TEST_DIR/checksums.txt" >/dev/null)
rm -rf "$TMP_TEST_DIR" bin

echo 'phase 5 public release contract checks passed'
