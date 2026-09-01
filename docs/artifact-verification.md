# Artifact Verification Guide

Interactive Node Controller publishes keylessly signed container images, build provenance, Software Bill of Materials (SBOM) attestations, and OCI-packaged Helm charts.

This runbook guides external consumers through verifying release integrity before deploying artifacts to production clusters.

---

## 1. Verify Container Image Signature (Cosign)

Container images published to GitHub Container Registry (`ghcr.io/josh-archer/interactive-node-controller`) are signed keylessly with [Sigstore Cosign](https://github.com/sigstore/cosign) using GitHub Actions OIDC tokens.

To verify a published release image (such as `v0.1.1` or by immutable digest):

```bash
# Verify by release tag:
cosign verify ghcr.io/josh-archer/interactive-node-controller:v0.1.1 \
  --certificate-identity-regexp "^https://github.com/Josh-Archer/interactive-node-controller/.github/workflows/release.yml@" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com"

# Or verify by pinned immutable digest:
cosign verify ghcr.io/josh-archer/interactive-node-controller@sha256:40b00ee4215d468c019ebc41d944c630baaf39ffe7364353274d5a44dafa22b5 \
  --certificate-identity-regexp "^https://github.com/Josh-Archer/interactive-node-controller/.github/workflows/release.yml@" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com"
```

A successful verification prints the signing certificate claims and claims payload from the Fulcio Certificate Authority and Rekor transparency log.

---

## 2. Verify Build Provenance & SBOM Attestation

Release builds attach SLSA provenance mode `max` and SBOM attestations generated during multi-arch compilation (`linux/amd64,linux/arm64`).

### Using GitHub CLI

```bash
gh attestation verify oci://ghcr.io/josh-archer/interactive-node-controller@sha256:40b00ee4215d468c019ebc41d944c630baaf39ffe7364353274d5a44dafa22b5 \
  --owner Josh-Archer
```

### Using Cosign

```bash
cosign verify-attestation \
  --type slsaprovenance \
  --certificate-identity-regexp "^https://github.com/Josh-Archer/interactive-node-controller/.github/workflows/release.yml@" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  ghcr.io/josh-archer/interactive-node-controller@sha256:40b00ee4215d468c019ebc41d944c630baaf39ffe7364353274d5a44dafa22b5
```

---

## 3. Pull & Inspect OCI Helm Charts

The Helm chart is published as an immutable OCI artifact at `oci://ghcr.io/josh-archer/charts/interactive-node-controller`.

### Pull Chart Archive

```bash
# Pull a specific version:
helm pull oci://ghcr.io/josh-archer/charts/interactive-node-controller \
  --version 0.1.0 \
  --untar
```

### Inspect Chart Metadata and Default Values

```bash
# Show chart metadata:
helm show chart oci://ghcr.io/josh-archer/charts/interactive-node-controller --version 0.1.0

# Show chart default values:
helm show values oci://ghcr.io/josh-archer/charts/interactive-node-controller --version 0.1.0

# Show chart CRDs:
helm show crds oci://ghcr.io/josh-archer/charts/interactive-node-controller --version 0.1.0
```

---

## 4. Release Pipeline Security Invariants

The [Release Workflow](../.github/workflows/release.yml) enforces the following release guarantees:
- **No Manual Direct Release**: Releases are triggered by versioned tags (`v*.*.*`) or gated `workflow_dispatch`.
- **Immutable Tags**: OCI chart push skips overwriting existing immutable versions.
- **Keyless Signing**: Signatures are rooted in Sigstore public transparency logs without long-lived private signing keys.
- **Multi-Arch Parity**: `linux/amd64` and `linux/arm64` container images share identical tags and manifest lists.
