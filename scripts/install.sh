#!/usr/bin/env bash
#
# mato install script
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/ryansimmen/mato/main/scripts/install.sh | bash
#   curl -fsSL https://raw.githubusercontent.com/ryansimmen/mato/main/scripts/install.sh | sudo bash
#   wget -qO- https://raw.githubusercontent.com/ryansimmen/mato/main/scripts/install.sh | bash
#
# Environment variables:
#   VERSION   Release tag to install (e.g. "v0.1.4"). Defaults to latest.
#   PREFIX    Install prefix. Binary is placed in $PREFIX/bin/mato.
#             Defaults to /usr/local for root, $HOME/.local for non-root.
#   DESTDIR   Optional staging root for packaging. When set, writes to
#             $DESTDIR$PREFIX/bin/mato and does not prompt to modify PATH.
#
# Verifies sha256 checksums and, if cosign is installed, the cosign signature
# of both the checksums file and the downloaded archive.

set -euo pipefail

REPO="ryansimmen/mato"
RELEASES_URL="https://github.com/${REPO}/releases"

info() { printf '%s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
err()  { printf 'error: %s\n' "$*" >&2; }

# --- detect platform ----------------------------------------------------------

case "$(uname -s 2>/dev/null || echo unknown)" in
  Linux) OS="linux" ;;
  Darwin)
    err "macOS is not supported. mato requires Linux (uses /proc and a local Docker daemon)."
    exit 1
    ;;
  *)
    err "unsupported OS: $(uname -s 2>/dev/null || echo unknown). mato requires Linux. See ${RELEASES_URL}"
    exit 1
    ;;
esac

case "$(uname -m)" in
  x86_64|amd64)  ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *)
    err "unsupported architecture: $(uname -m). mato ships linux/amd64 and linux/arm64."
    exit 1
    ;;
esac

# --- resolve URLs -------------------------------------------------------------

if [ -z "${VERSION:-}" ]; then
  # Use GitHub's stable /releases/latest/download/<asset> redirect.
  BASE_URL="${RELEASES_URL}/latest/download"
  VERSION_DISPLAY="latest"
else
  case "$VERSION" in
    v*) ;;
    *)  VERSION="v${VERSION}" ;;
  esac
  BASE_URL="${RELEASES_URL}/download/${VERSION}"
  VERSION_DISPLAY="$VERSION"
fi

# --- pick a downloader --------------------------------------------------------

if command -v curl >/dev/null 2>&1; then
  DOWNLOADER="curl"
elif command -v wget >/dev/null 2>&1; then
  DOWNLOADER="wget"
else
  err "neither curl nor wget found; please install one"
  exit 1
fi

download() {
  # download URL DEST
  local url="$1" dest="$2"
  case "$DOWNLOADER" in
    curl) curl -fsSL "$url" -o "$dest" ;;
    wget) wget -q -O "$dest" "$url" ;;
  esac
}

# --- tempdir ------------------------------------------------------------------

TMP_DIR="$(mktemp -d)"
trap 'rm -rf -- "$TMP_DIR"' EXIT

# --- download checksums first to discover version when latest -----------------

info "Resolving mato ${VERSION_DISPLAY} for ${OS}/${ARCH}..."

CHECKSUMS_FILE="${TMP_DIR}/checksums.txt"
download "${BASE_URL}/checksums.txt" "${CHECKSUMS_FILE}"

# Determine the actual version string from a checksum line.
# checksums.txt entries look like: <sha256>  mato_0.1.4_linux_amd64.tar.gz
RESOLVED_VERSION="$(awk '
  /mato_[^ ]+_(linux|darwin)_(amd64|arm64)\.tar\.gz/ {
    n = split($2, parts, "_")
    print parts[2]
    exit
  }
' "$CHECKSUMS_FILE")"

if [ -z "$RESOLVED_VERSION" ]; then
  err "could not determine release version from checksums.txt"
  exit 1
fi

ARCHIVE_NAME="mato_${RESOLVED_VERSION}_${OS}_${ARCH}.tar.gz"
ARCHIVE_PATH="${TMP_DIR}/${ARCHIVE_NAME}"

info "Installing mato v${RESOLVED_VERSION} (${OS}/${ARCH})"

# --- download artefacts -------------------------------------------------------

download "${BASE_URL}/${ARCHIVE_NAME}" "${ARCHIVE_PATH}"

# Cosign bundles (best-effort; only needed when cosign is present).
COSIGN_AVAILABLE="false"
if command -v cosign >/dev/null 2>&1; then
  COSIGN_AVAILABLE="true"
  download "${BASE_URL}/${ARCHIVE_NAME}.sigstore.json" "${ARCHIVE_PATH}.sigstore.json"
  download "${BASE_URL}/checksums.txt.sigstore.json"   "${CHECKSUMS_FILE}.sigstore.json"
fi

# --- verify sha256 ------------------------------------------------------------

if command -v sha256sum >/dev/null 2>&1; then
  ( cd "$TMP_DIR" && sha256sum --ignore-missing -c checksums.txt >/dev/null )
  info "sha256 verified"
elif command -v shasum >/dev/null 2>&1; then
  ( cd "$TMP_DIR" && shasum -a 256 --ignore-missing -c checksums.txt >/dev/null )
  info "sha256 verified (shasum)"
else
  err "neither sha256sum nor shasum found on PATH; cannot verify checksums"
  err "install coreutils (sha256sum) or perl-tools (shasum) and re-run"
  exit 1
fi

# --- verify cosign signature --------------------------------------------------

if [ "$COSIGN_AVAILABLE" = "true" ]; then
  CERT_IDENTITY="https://github.com/${REPO}/.github/workflows/release.yml@refs/tags/v${RESOLVED_VERSION}"
  CERT_ISSUER="https://token.actions.githubusercontent.com"

  if ! cosign verify-blob \
    --bundle "${CHECKSUMS_FILE}.sigstore.json" \
    --certificate-identity "${CERT_IDENTITY}" \
    --certificate-oidc-issuer "${CERT_ISSUER}" \
    "${CHECKSUMS_FILE}" >/dev/null 2>&1; then
    err "cosign signature verification failed for checksums.txt"
    exit 1
  fi
  info "cosign verified checksums.txt"

  if ! cosign verify-blob \
    --bundle "${ARCHIVE_PATH}.sigstore.json" \
    --certificate-identity "${CERT_IDENTITY}" \
    --certificate-oidc-issuer "${CERT_ISSUER}" \
    "${ARCHIVE_PATH}" >/dev/null 2>&1; then
    err "cosign signature verification failed for ${ARCHIVE_NAME}"
    exit 1
  fi
  info "cosign verified ${ARCHIVE_NAME}"
else
  warn "cosign not found; skipping signature verification"
  warn "install cosign to verify signatures: https://docs.sigstore.dev/cosign/installation/"
fi

# --- validate tarball ---------------------------------------------------------

if ! tar -tzf "${ARCHIVE_PATH}" >/dev/null 2>&1; then
  err "downloaded archive is not a valid tarball"
  exit 1
fi

# --- determine install prefix -------------------------------------------------

if [ -z "${PREFIX:-}" ]; then
  if [ "$(id -u 2>/dev/null || echo 1)" -eq 0 ]; then
    PREFIX="/usr/local"
  else
    PREFIX="${HOME}/.local"
  fi
fi
FINAL_INSTALL_DIR="${PREFIX}/bin"
INSTALL_DIR="${DESTDIR:-}${FINAL_INSTALL_DIR}"

if ! mkdir -p "${INSTALL_DIR}" 2>/dev/null; then
  err "cannot create ${INSTALL_DIR}; re-run with sudo or set PREFIX=\$HOME/.local"
  exit 1
fi

if [ ! -w "${INSTALL_DIR}" ]; then
  err "${INSTALL_DIR} is not writable; re-run with sudo or set PREFIX=\$HOME/.local"
  exit 1
fi

# --- install ------------------------------------------------------------------

if [ -e "${INSTALL_DIR}/mato" ]; then
  info "Replacing existing binary at ${INSTALL_DIR}/mato"
fi

tar -xzf "${ARCHIVE_PATH}" -C "${TMP_DIR}" mato
mv "${TMP_DIR}/mato" "${INSTALL_DIR}/mato"
chmod +x "${INSTALL_DIR}/mato"

if [ -n "${DESTDIR:-}" ]; then
  info "Staged mato v${RESOLVED_VERSION} to ${INSTALL_DIR}/mato"
  info "Final install path: ${FINAL_INSTALL_DIR}/mato"
  info "Run 'mato --help' after installing the staged binary into its final location."
  exit 0
fi

info "Installed mato v${RESOLVED_VERSION} to ${INSTALL_DIR}/mato"

# --- PATH check + interactive rc-file prompt ----------------------------------

case ":${PATH}:" in
  *":${INSTALL_DIR}:"*) ON_PATH="true" ;;
  *)                    ON_PATH="false" ;;
esac

if [ "$ON_PATH" = "false" ]; then
  CURRENT_SHELL="$(basename "${SHELL:-/bin/sh}")"
  case "$CURRENT_SHELL" in
    zsh)  RC_FILE="${ZDOTDIR:-$HOME}/.zshrc" ;;
    bash) RC_FILE="$HOME/.bashrc" ;;
    fish) RC_FILE="${XDG_CONFIG_HOME:-$HOME/.config}/fish/conf.d/mato.fish" ;;
    *)    RC_FILE="$HOME/.profile" ;;
  esac

  if [ "$CURRENT_SHELL" = "fish" ]; then
    PATH_LINE="fish_add_path \"${INSTALL_DIR}\""
  else
    PATH_LINE="export PATH=\"${INSTALL_DIR}:\$PATH\""
  fi

  info ""
  info "${INSTALL_DIR} is not in your PATH."

  if [ -t 0 ] || [ -e /dev/tty ]; then
    printf 'Add it to %s? [y/N] ' "$RC_FILE"
    REPLY=""
    if read -r REPLY </dev/tty 2>/dev/null; then
      case "$REPLY" in
        y|Y)
          mkdir -p "$(dirname "$RC_FILE")"
          printf '\n# Added by mato install.sh\n%s\n' "$PATH_LINE" >> "$RC_FILE"
          info "Appended PATH entry to ${RC_FILE}"
          info "Restart your shell or run: source ${RC_FILE}"
          ;;
        *)
          info "Add this to ${RC_FILE} manually:"
          info "  ${PATH_LINE}"
          ;;
      esac
    else
      info "Add this to ${RC_FILE} manually:"
      info "  ${PATH_LINE}"
    fi
  else
    info "Add this to ${RC_FILE} manually:"
    info "  ${PATH_LINE}"
  fi
fi

info ""
info "Run 'mato --help' to get started."
