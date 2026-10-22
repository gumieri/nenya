# Verifying Nenya Release Artifacts

Every release is signed at build time with [Sigstore](https://www.sigstore.dev/)
keyless signing (cosign). The signature binds each artifact to this repository's
GitHub Actions release workflow, so you can confirm that an image or archive was
built by the official pipeline and was not substituted afterwards.

This does not require trusting the distribution channel (a registry, a mirror, a
download link). If verification succeeds, the bytes you have are the bytes the
workflow produced.

Prerequisites: [cosign](https://github.com/sigstore/cosign) v2 or newer.

```
cosign version
```

## Container images

Images are published at `ghcr.io/gumieri/nenya` with the tags `vX.Y.Z` (multi-arch
index), `latest`, and per-arch tags `vX.Y.Z-amd64` / `X.Y.Z-arm64`.

Verify an image against the release workflow identity:

```
cosign verify \
  --certificate-identity-regexp '^https://github.com/gumieri/nenya/\.github/workflows/release\.yml@refs/tags/vX\.Y\.Z$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/gumieri/nenya:vX.Y.Z-amd64
```

Replace `X.Y.Z` with the release version. For a SHA-256 digest reference
(recommended, since a tag can be repointed after the fact):

```
cosign verify \
  --certificate-identity-regexp '^https://github.com/gumieri/nenya/\.github/workflows/release\.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/gumieri/nenya@sha256:<digest>
```

A successful verification prints the certificate and the signature; a failed one
exits non-zero with `VERIFY IMAGES FAILED` (no signature, wrong identity, or
tampered digest).

## SBOM attestations

Each per-arch image carries a Sigstore attestation of type `spdxjson` containing
the image software bill of materials. Inspect it with:

```
cosign verify-attestation \
  --type spdxjson \
  --certificate-identity-regexp '^https://github.com/gumieri/nenya/\.github/workflows/release\.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/gumieri/nenya:vX.Y.Z-amd64
```

The same SBOM documents are also attached to the GitHub release page as
`nenya-vX.Y.Z-linux-<arch>.spdx.json` and `nenya-vX.Y.Z-spdx.json`.

## Release archives and checksums

Binary release assets are checksummed, and `checksums.txt` itself is signed with
a Sigstore bundle (`checksums.txt.sigstore.json`) by the same workflow:

```
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/gumieri/nenya/\.github/workflows/release\.yml@refs/tags/vX\.Y\.Z$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
```

Then check your downloaded archive against the verified checksum:

```
sha256sum --ignore-missing --check checksums.txt
```

## What verification proves, and what it does not

A valid signature proves the artifact was produced by a run of this
repository's `release.yml` workflow at the given ref, and that the bytes were
not modified after signing. It does not certify the code itself; it anchors
trust in the repository and in GitHub's OIDC issuer. That is exactly the
property that matters against distribution-channel tampering and against a
re-tagged or mirrored artifact.
