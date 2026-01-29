#!/bin/bash
#
# Pseudocoder Install Script
#
# Downloads and installs the pseudocoder CLI from GitHub Releases.
#
# Usage:
#   curl -sSL https://raw.githubusercontent.com/diab-ma/pseudocoder-host/main/install.sh | bash
#

set -euo pipefail

# Configuration
REPO="diab-ma/pseudocoder-host"  # TODO: Update with actual repo path
INSTALL_DIR="/usr/local/bin"
BINARY_NAME="pseudocoder"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

info() {
    echo -e "${GREEN}==>${NC} $1"
}

warn() {
    echo -e "${YELLOW}Warning:${NC} $1"
}

error() {
    echo -e "${RED}Error:${NC} $1" >&2
    exit 1
}

# Detect OS
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$OS" in
    darwin) OS="darwin" ;;
    linux) OS="linux" ;;
    *) error "Unsupported operating system: $OS" ;;
esac

# Detect architecture
ARCH="$(uname -m)"
case "$ARCH" in
    x86_64) ARCH="amd64" ;;
    amd64) ARCH="amd64" ;;
    arm64) ARCH="arm64" ;;
    aarch64) ARCH="arm64" ;;
    *) error "Unsupported architecture: $ARCH" ;;
esac

# macOS on Intel vs Apple Silicon
if [[ "$OS" == "darwin" && "$ARCH" == "amd64" ]]; then
    # Check if running under Rosetta
    if [[ "$(sysctl -n sysctl.proc_translated 2>/dev/null || echo 0)" == "1" ]]; then
        info "Detected Rosetta - using native arm64 binary instead"
        ARCH="arm64"
    fi
fi

ASSET_NAME="${BINARY_NAME}-${OS}-${ARCH}"
info "Detected platform: ${OS}-${ARCH}"

# Get latest release info (includes prereleases)
info "Fetching latest release..."
RELEASE_JSON=$(curl -sSL \
    -H "Accept: application/vnd.github+json" \
    "https://api.github.com/repos/${REPO}/releases")

# Extract version and asset URL
VERSION=$(echo "$RELEASE_JSON" | grep -o '"tag_name": "[^"]*' | head -1 | cut -d'"' -f4)
if [[ -z "$VERSION" ]]; then
    error "Could not find latest release. Check https://github.com/${REPO}/releases"
fi

ASSET_URL=$(echo "$RELEASE_JSON" | grep -o "\"browser_download_url\": \"[^\"]*${ASSET_NAME}\"" | head -1 | cut -d'"' -f4)
if [[ -z "$ASSET_URL" ]]; then
    error "Could not find asset ${ASSET_NAME} in release ${VERSION}"
fi

info "Found ${VERSION}"

# Create temp directory
TMP_DIR=$(mktemp -d)
trap "rm -rf ${TMP_DIR}" EXIT

# Download binary
info "Downloading ${ASSET_NAME}..."
curl -sSL \
    -o "${TMP_DIR}/${BINARY_NAME}" \
    "${ASSET_URL}"

# Make executable
chmod +x "${TMP_DIR}/${BINARY_NAME}"

# Remove macOS quarantine attribute
if [[ "$OS" == "darwin" ]]; then
    info "Removing macOS quarantine attribute..."
    xattr -d com.apple.quarantine "${TMP_DIR}/${BINARY_NAME}" 2>/dev/null || true
fi

# Install to destination
info "Installing to ${INSTALL_DIR}/${BINARY_NAME}..."
if [[ -w "$INSTALL_DIR" ]]; then
    mv "${TMP_DIR}/${BINARY_NAME}" "${INSTALL_DIR}/${BINARY_NAME}"
else
    warn "Need sudo to install to ${INSTALL_DIR}"
    sudo mv "${TMP_DIR}/${BINARY_NAME}" "${INSTALL_DIR}/${BINARY_NAME}"
fi

# Verify installation
if command -v "$BINARY_NAME" &> /dev/null; then
    INSTALLED_VERSION=$("$BINARY_NAME" --version 2>/dev/null || echo "unknown")
    info "Successfully installed: ${INSTALLED_VERSION}"
    echo ""
    echo "Run 'pseudocoder --help' to get started."
else
    warn "Installed to ${INSTALL_DIR}/${BINARY_NAME} but not found in PATH"
    echo ""
    echo "Add ${INSTALL_DIR} to your PATH, or run:"
    echo "  ${INSTALL_DIR}/${BINARY_NAME} --help"
fi
