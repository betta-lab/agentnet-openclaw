#!/usr/bin/env bash
set -euo pipefail

REPO="betta-lab/agentnet-openclaw"
BINARY="agentnet"
INSTALL_DIR="${AGENTNET_INSTALL_DIR:-$HOME/.local/bin}"

# Detect OS and arch
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64)  ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *)
    echo "Unsupported architecture: $ARCH" >&2
    exit 1
    ;;
esac

ASSET="${BINARY}-${OS}-${ARCH}"

# Fetch latest release tag
echo "Fetching latest release..."
TAG=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
  | grep '"tag_name"' | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')

if [ -z "$TAG" ]; then
  echo "Failed to fetch latest release tag" >&2
  exit 1
fi

echo "Latest: $TAG"

# Download binary
URL="https://github.com/${REPO}/releases/download/${TAG}/${ASSET}"
TMP="$(mktemp)"

echo "Downloading ${ASSET}..."
if ! curl -fsSL "$URL" -o "$TMP"; then
  echo "Download failed: $URL" >&2
  rm -f "$TMP"
  exit 1
fi

# Install
mkdir -p "$INSTALL_DIR"
mv "$TMP" "${INSTALL_DIR}/${BINARY}"
chmod +x "${INSTALL_DIR}/${BINARY}"

echo ""
echo "✅ agentnet ${TAG} installed to ${INSTALL_DIR}/${BINARY}"

# Check PATH
if ! echo "$PATH" | grep -q "$INSTALL_DIR"; then
  echo ""
  echo "⚠️  Add this to your shell profile:"
  echo "   export PATH=\"\$HOME/.local/bin:\$PATH\""
fi
