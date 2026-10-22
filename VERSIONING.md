# Versioning and releases

Releases use semantic versioning and immutable tags in the form `vMAJOR.MINOR.PATCH`.
The Helm chart version omits the leading `v`; its `appVersion` retains it.

Creating and pushing a release tag runs the Release workflow. It validates the
tag, builds and publishes a linux/amd64 and linux/arm64 image to GitHub Container
Registry, attaches SBOM and provenance attestations, signs the image keylessly,
publishes the Helm chart as an OCI artifact, and compiles version-injected
host reporter binaries and archives for linux/amd64 and linux/arm64 with signed
SHA-256 checksums and build provenance. The workflow also creates the matching
GitHub Release and attaches the reporter artifacts. A manual rerun must name an
existing release tag.

Consumers pin both the chart version and the image digest in reviewed GitOps:

```yaml
image:
  repository: ghcr.io/josh-archer/interactive-node-controller
  digest: sha256:...
```

Install the chart from the public OCI registry:

```bash
helm upgrade --install interactive-node-controller \
  oci://ghcr.io/josh-archer/charts/interactive-node-controller \
  --version <VERSION> \
  --namespace interactive-node-controller \
  --create-namespace \
  --values examples/helm-values.yaml
```

Container images and OCI Helm charts are intended to be publicly accessible
without registry authentication. If a package is private, an owner must change
its visibility in GitHub Packages settings before anonymous pulls will work.
Private forks or custom mirrors can configure standard Kubernetes
`imagePullSecrets`.

Host reporter binaries are published as immutable release assets on GitHub
Releases:
- `node-activity-reporter-linux-amd64`
- `node-activity-reporter-linux-arm64`
- Release archives (`.tar.gz`) containing the binary and documentation
- `checksums.txt` with SHA-256 digests
- `checksums.txt.bundle` containing the Sigstore Cosign verification bundle

## Artifact Verification

Published release images, OCI Helm charts, and host reporter binaries can be verified before deployment:
- **Cosign Signature Verification**: Signatures are keyless and rooted in Sigstore Fulcio and Rekor.
- **Provenance & SBOM Verification**: SLSA provenance and SBOM attestations are attached to each image and binary.
- **Helm OCI Inspection**: Pull and inspect chart templates and default values without installing.
- **Checksum Verification**: Validate host reporter binary SHA-256 checksums before installation.

See the [Artifact Verification Guide](docs/artifact-verification.md) for full commands and verification examples.
