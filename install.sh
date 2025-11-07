#!/bin/bash
set -euo pipefail

VERSION="${1:-latest}"

# Detect OS
case "${RUNNER_OS:-}" in
  Linux)
    OS="linux"
    ;;
  macOS)
    OS="darwin"
    ;;
  Windows)
    OS="windows"
    ;;
  *)
    echo "Error: Unsupported OS: ${RUNNER_OS:-unknown}"
    exit 1
    ;;
esac

# Detect architecture
case "${RUNNER_ARCH:-}" in
  X64)
    ARCH="amd64"
    ;;
  ARM64)
    ARCH="arm64"
    ;;
  *)
    # Fallback to detecting from uname if RUNNER_ARCH is not set
    ARCH=$(uname -m)
    case "$ARCH" in
      x86_64)
        ARCH="amd64"
        ;;
      aarch64|arm64)
        ARCH="arm64"
        ;;
      *)
        echo "Error: Unsupported architecture: $ARCH"
        exit 1
        ;;
    esac
    ;;
esac

# Get version if latest
if [ "$VERSION" = "latest" ]; then
  echo "Fetching latest version..."
  # Use a more portable method to extract tag_name from JSON
  VERSION=$(curl -sSL -H "Accept: application/vnd.github.v3+json" \
    https://api.github.com/repos/runs-on/cli/releases/latest | \
    grep '"tag_name"' | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/' | head -1)
  
  if [ -z "$VERSION" ]; then
    echo "Error: Failed to fetch latest version"
    exit 1
  fi
  
  echo "Latest version: $VERSION"
fi

# Remove 'v' prefix if present for binary name
VERSION_NO_V="${VERSION#v}"

# Construct binary name
if [ "$OS" = "windows" ]; then
  BINARY_NAME="roc_${VERSION_NO_V}_${OS}_${ARCH}.exe"
  INSTALL_NAME="roc.exe"
else
  BINARY_NAME="roc_${VERSION_NO_V}_${OS}_${ARCH}"
  INSTALL_NAME="roc"
fi

# Construct download URL
DOWNLOAD_URL="https://github.com/runs-on/cli/releases/download/${VERSION}/${BINARY_NAME}"

echo "Downloading RunsOn CLI ${VERSION} for ${OS}/${ARCH}..."
echo "URL: ${DOWNLOAD_URL}"

# Create temp directory
TEMP_DIR=$(mktemp -d)
trap "rm -rf $TEMP_DIR" EXIT

# Download binary
if ! curl -fsSL -o "${TEMP_DIR}/${INSTALL_NAME}" "${DOWNLOAD_URL}"; then
  echo "Error: Failed to download binary from ${DOWNLOAD_URL}"
  exit 1
fi

# Make executable (for Unix systems)
if [ "$OS" != "windows" ]; then
  chmod +x "${TEMP_DIR}/${INSTALL_NAME}"
fi

# Verify binary exists
if [ ! -f "${TEMP_DIR}/${INSTALL_NAME}" ]; then
  echo "Error: Binary was not downloaded successfully"
  exit 1
fi

# Determine install location
if [ "$OS" = "windows" ]; then
  INSTALL_DIR="$HOME/.local/bin"
  mkdir -p "$INSTALL_DIR"
  INSTALL_PATH="${INSTALL_DIR}/${INSTALL_NAME}"
else
  INSTALL_DIR="$HOME/.local/bin"
  mkdir -p "$INSTALL_DIR"
  INSTALL_PATH="${INSTALL_DIR}/${INSTALL_NAME}"
fi

# Install binary
mv "${TEMP_DIR}/${INSTALL_NAME}" "$INSTALL_PATH"

# Add to PATH
echo "$INSTALL_DIR" >> "$GITHUB_PATH"

echo "Successfully installed RunsOn CLI to ${INSTALL_PATH}"
echo "Version: $VERSION"
echo "Binary: $INSTALL_PATH"

# Verify installation
if [ "$OS" = "windows" ]; then
  "$INSTALL_PATH" version || echo "Warning: Could not verify installation"
else
  "$INSTALL_PATH" version || echo "Warning: Could not verify installation"
fi

