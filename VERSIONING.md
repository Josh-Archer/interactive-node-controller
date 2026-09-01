# Versioning and releases

Releases use semantic versioning and immutable tags in the form `vMAJOR.MINOR.PATCH`.
The Helm chart version omits the leading `v`; its `appVersion` retains it.

Creating and pushing a release tag runs the Release workflow. It validates the
tag, builds and publishes a linux/amd64 and linux/arm64 image to GitHub Container
Registry, attaches SBOM and provenance attestations, signs the image keylessly,
and publishes the Helm chart as an OCI artifact. The workflow also creates the
matching GitHub Release. A manual rerun must name an existing release tag.

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

Published container images and OCI Helm charts are publicly accessible and do
not require registry authentication to pull. Private forks or custom mirrors
can configure standard Kubernetes `imagePullSecrets`.

## Artifact Verification

Published release images and OCI Helm charts can be verified before deployment:
- **Cosign Signature Verification**: Signatures are keyless and rooted in Sigstore Fulcio and Rekor.
- **Provenance & SBOM Verification**: SLSA provenance and SBOM attestations are attached to each image.
- **Helm OCI Inspection**: Pull and inspect chart templates and default values without installing.

See the [Artifact Verification Guide](docs/artifact-verification.md) for full commands and verification examples.
