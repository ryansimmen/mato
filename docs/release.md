# Release Process

Releases are built by `.github/workflows/release.yml` when a `v*` tag is pushed. The workflow uses GoReleaser, cosign keyless signing, Syft SBOM generation, and GitHub build provenance attestations.

## Repeatable Build Inputs

The release process pins the GoReleaser version in the workflow and sets deterministic timestamps in `.goreleaser.yaml`. These settings make the published binary archives and checksum file repeatable for the same tagged source tree and toolchain:

- Go build output timestamps use the tagged commit timestamp.
- Archive member timestamps use the tagged commit timestamp.
- Release builds use `-trimpath` and `CGO_ENABLED=0`.
- The archive contents are limited to the `mato` binary, `LICENSE`, `README.md`, and `CHANGELOG.md`.

## Local Archive Repeatability Check

To compare two local binary-archive builds without publishing, signing, or generating SBOMs:

```bash
tmp1="$(mktemp -d)"
tmp2="$(mktemp -d)"

goreleaser release --snapshot --clean --skip=sign,publish,validate,sbom --parallelism=1
cp -R dist "$tmp1/dist"

goreleaser release --snapshot --clean --skip=sign,publish,validate,sbom --parallelism=1
cp -R dist "$tmp2/dist"

diff -q "$tmp1/dist/checksums.txt" "$tmp2/dist/checksums.txt"
for file in "$tmp1"/dist/*.tar.gz; do
  base="$(basename "$file")"
  diff -q "$file" "$tmp2/dist/$base"
done
```

This compares the published release archives and checksum file. GoReleaser also writes internal run metadata such as `metadata.json` and `artifacts.json` under `dist/`; those files are not published release assets and may include run-local ordering or environment details.

The full release workflow additionally signs artifacts and generates SBOM and SLSA provenance assets. Those assets should be checked in the release environment because they depend on tools installed by the workflow. This document does not claim SBOMs, signatures, or attestations are bit-for-bit reproducible across independent runs; it documents repeatability for the binary archives and checksum file.
