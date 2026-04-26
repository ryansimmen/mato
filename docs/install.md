# Install

`mato` only supports Linux at runtime — it reads `/proc` for process supervision and assumes a Docker daemon on the local host. macOS and Windows are not supported, including from source.

`mato` ships signed `linux/amd64` and `linux/arm64` binaries with each release.

The CLI runs the queue. For the normal first-run workflow, also install the bundled `mato` task-planning skill; it creates the markdown task files that populate `.mato/backlog` and `.mato/waiting`.

## Linux binary (recommended)

The install script downloads the archive, verifies its `sha256` checksum, and (when [`cosign`](https://docs.sigstore.dev/cosign/installation/) is on `PATH`) verifies the cosign signature before installing the binary.

### Inspect-then-run

```bash
curl -fsSLO https://raw.githubusercontent.com/ryansimmen/mato/main/scripts/install.sh
less install.sh   # review the script
bash install.sh
```

### One-liner

```bash
curl -fsSL https://raw.githubusercontent.com/ryansimmen/mato/main/scripts/install.sh | bash
```

### System-wide install (`/usr/local/bin`)

```bash
curl -fsSL https://raw.githubusercontent.com/ryansimmen/mato/main/scripts/install.sh | sudo bash
```

### Environment variables

The script honors:

- `VERSION` — release tag (e.g. `v0.1.6`). Defaults to the latest release.
- `PREFIX` — install prefix; the binary is placed in `$PREFIX/bin/mato`. Defaults to `/usr/local` for root, `$HOME/.local` for non-root.
- `DESTDIR` — optional staging root for packaging; when set, the binary is written to `$DESTDIR$PREFIX/bin/mato` and no shell `PATH` prompt is shown.

```bash
curl -fsSL https://raw.githubusercontent.com/ryansimmen/mato/main/scripts/install.sh \
  | VERSION=v0.1.6 PREFIX=$HOME/custom bash
```

Package-style staging example:

```bash
curl -fsSLO https://raw.githubusercontent.com/ryansimmen/mato/main/scripts/install.sh
DESTDIR=/tmp/mato-package PREFIX=/usr/local VERSION=v0.1.6 bash install.sh
# writes /tmp/mato-package/usr/local/bin/mato
```

### Uninstall

Remove the binary from the prefix where it was installed:

```bash
rm -f "$HOME/.local/bin/mato"
sudo rm -f /usr/local/bin/mato
```

`gh skill` currently provides `install`, `preview`, `publish`, `search`, and `update`, but no uninstall/remove command. If you installed the bundled skill, remove its installed directory for your agent host. Common locations include:

```bash
rm -rf "$HOME/.copilot/skills/mato"
rm -rf "$HOME/.claude/skills/mato"
rm -rf "$HOME/.config/opencode/skills/mato"
```

## Bundled `mato` Skill

Install the task-planning skill with the [GitHub CLI](https://cli.github.com/) (`gh` v2.90.0 or later):

```bash
gh skill install ryansimmen/mato mato --scope user
```

`gh skill` writes to the appropriate per-host directory, such as `~/.copilot/skills/mato/` for GitHub Copilot or `~/.claude/skills/mato/` for Claude Code. To target another supported host, pass `--agent claude-code|cursor|codex|gemini|antigravity`.

Update the installed skill after new releases with:

```bash
gh skill update mato
```

OpenCode is not yet a `gh skill`-supported host. Install there with an explicit directory as a workaround:

```bash
gh skill install ryansimmen/mato mato --dir ~/.config/opencode/skills
```

## Authentication

Before running `mato run`, make sure both GitHub CLI and Copilot CLI authentication are ready on the host:

```bash
gh auth login
copilot login
```

At runtime, `mato` forwards `COPILOT_GITHUB_TOKEN`, `GH_TOKEN`, or `GITHUB_TOKEN` into Docker containers when they are set. If none are set, it makes a best-effort `gh auth token` lookup so host-side GitHub authentication can still be used inside agent containers.

## Verify a manual download

Each release publishes a `*.intoto.jsonl` SLSA build provenance bundle, per-archive cosign `.sigstore.json` bundles, a signed `checksums.txt`, and per-archive [SPDX 2.3](https://spdx.dev/) SBOMs. The install script verifies automatically; the steps below let you verify a manually-downloaded archive.

### With `gh` (recommended)

```bash
gh release download v0.1.6 -R ryansimmen/mato -p 'mato_0.1.6_linux_amd64.tar.gz'
gh attestation verify -R ryansimmen/mato mato_0.1.6_linux_amd64.tar.gz
```

A successful verification exits 0; in non-interactive shells the command is silent on success. Use `--format json` for full attestation details.

### Without `gh`

Using `sha256sum` and [`cosign`](https://docs.sigstore.dev/cosign/installation/):

```bash
VERSION=v0.1.6
ASSETS="mato_${VERSION#v}_linux_amd64.tar.gz checksums.txt checksums.txt.sigstore.json mato_${VERSION#v}_linux_amd64.tar.gz.sigstore.json"
for f in $ASSETS; do
  curl -fsSLO "https://github.com/ryansimmen/mato/releases/download/${VERSION}/${f}"
done

sha256sum --ignore-missing -c checksums.txt

CERT_ID="https://github.com/ryansimmen/mato/.github/workflows/release.yml@refs/tags/${VERSION}"
ISSUER="https://token.actions.githubusercontent.com"

cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity "$CERT_ID" \
  --certificate-oidc-issuer "$ISSUER" \
  checksums.txt

cosign verify-blob \
  --bundle "mato_${VERSION#v}_linux_amd64.tar.gz.sigstore.json" \
  --certificate-identity "$CERT_ID" \
  --certificate-oidc-issuer "$ISSUER" \
  "mato_${VERSION#v}_linux_amd64.tar.gz"
```

## SBOM and SLSA provenance

Each release also attaches:

- `mato_<version>_linux_<arch>.tar.gz.sbom.json` — SPDX 2.3 SBOM per archive, generated by [Syft](https://github.com/anchore/syft).
- `mato_<version>.intoto.jsonl` — DSSE-encoded SLSA v1 build provenance for all archives and SBOMs, produced by [`actions/attest-build-provenance`](https://github.com/actions/attest-build-provenance).

The provenance bundle is what `gh attestation verify` consumes.

## Build from source

For Linux contributors who want to build from a local checkout. If you have [Go](https://go.dev/doc/install) 1.26+ installed:

```bash
go install github.com/ryansimmen/mato/cmd/mato@latest
```

Or clone and build with `make`:

```bash
git clone https://github.com/ryansimmen/mato.git
cd mato
make build
# binary written to bin/mato
```

Note: binaries built via `go install` do not embed a version string (`mato --version` reports `dev`). The `make build` target wires the tag via `-ldflags` when invoked from a tagged commit.
