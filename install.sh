#!/bin/bash
set -euo pipefail

VERSION="${1:-latest}"
CURL_RETRY_ARGS=(--retry 3 --retry-delay 1 --retry-all-errors)

extract_json_string() {
  local key="$1"
  local body="$2"

  sed -n "s/.*\"${key}\"[[:space:]]*:[[:space:]]*\"\\([^\"]*\\)\".*/\\1/p" <<<"$body" | head -1
}

summarize_response() {
  local body="$1"

  tr '\r\n' ' ' <<<"$body" | sed 's/[[:space:]]\+/ /g' | cut -c1-200
}

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
  API_HEADERS=(
    -H "Accept: application/vnd.github+json"
    -H "User-Agent: runs-on-cli-installer"
    -H "X-GitHub-Api-Version: 2022-11-28"
  )
  if [ -n "${GITHUB_TOKEN:-}" ]; then
    API_HEADERS+=(-H "Authorization: Bearer ${GITHUB_TOKEN}")
  fi

  if ! RELEASE_RESPONSE=$(curl -sSL "${CURL_RETRY_ARGS[@]}" "${API_HEADERS[@]}" \
    -w $'\n%{http_code}' \
    https://api.github.com/repos/runs-on/cli/releases/latest); then
    echo "Error: Failed to fetch latest version metadata from GitHub"
    exit 1
  fi

  RELEASE_STATUS="${RELEASE_RESPONSE##*$'\n'}"
  RELEASE_BODY="${RELEASE_RESPONSE%$'\n'*}"

  if [[ ! "$RELEASE_STATUS" =~ ^[0-9]{3}$ ]]; then
    echo "Error: Failed to fetch latest version metadata from GitHub"
    echo "Unexpected HTTP status payload: $(summarize_response "$RELEASE_RESPONSE")"
    exit 1
  fi

  if [ "$RELEASE_STATUS" -ge 400 ]; then
    API_MESSAGE=$(extract_json_string "message" "$RELEASE_BODY")
    echo "Error: Failed to fetch latest version metadata from GitHub (HTTP ${RELEASE_STATUS})"
    if [ -n "$API_MESSAGE" ]; then
      echo "GitHub API message: ${API_MESSAGE}"
    else
      echo "GitHub API response: $(summarize_response "$RELEASE_BODY")"
    fi
    exit 1
  fi

  VERSION=$(extract_json_string "tag_name" "$RELEASE_BODY")

  if [ -z "$VERSION" ]; then
    echo "Error: Failed to fetch latest version"
    echo "GitHub API response: $(summarize_response "$RELEASE_BODY")"
    exit 1
  fi

  echo "Latest version: $VERSION"
fi

# Construct binary name
if [ "$OS" = "windows" ]; then
  BINARY_NAME="roc_${VERSION}_${OS}_${ARCH}.exe"
  INSTALL_NAME="roc.exe"
else
  BINARY_NAME="roc_${VERSION}_${OS}_${ARCH}"
  INSTALL_NAME="roc"
fi

# Construct download URL
DOWNLOAD_URL="https://github.com/runs-on/cli/releases/download/${VERSION}/${BINARY_NAME}"

echo "Downloading RunsOn CLI ${VERSION} for ${OS}/${ARCH}..."
echo "URL: ${DOWNLOAD_URL}"

# Create temp directory
TEMP_DIR=$(mktemp -d)
trap 'rm -rf "$TEMP_DIR"' EXIT

# Download binary
if ! curl -fsSL "${CURL_RETRY_ARGS[@]}" -o "${TEMP_DIR}/${INSTALL_NAME}" "${DOWNLOAD_URL}"; then
  echo "Error: Failed to download binary from ${DOWNLOAD_URL}"
  exit 1
fi

# Check the binary against the release's checksums.txt (published since v0.1.8)
CHECKSUMS_URL="https://github.com/runs-on/cli/releases/download/${VERSION}/checksums.txt"
if ! curl -fsSL "${CURL_RETRY_ARGS[@]}" -o "${TEMP_DIR}/checksums.txt" "${CHECKSUMS_URL}"; then
  echo "Error: Failed to download checksums from ${CHECKSUMS_URL}"
  exit 1
fi

EXPECTED_SHA256=$(awk -v name="$BINARY_NAME" '$2 == name || $2 == "*" name { print $1; exit }' "${TEMP_DIR}/checksums.txt")
if [ -z "$EXPECTED_SHA256" ]; then
  echo "Error: ${BINARY_NAME} is not listed in ${CHECKSUMS_URL}"
  exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
  ACTUAL_SHA256=$(sha256sum "${TEMP_DIR}/${INSTALL_NAME}" | awk '{ print $1 }')
elif command -v shasum >/dev/null 2>&1; then
  ACTUAL_SHA256=$(shasum -a 256 "${TEMP_DIR}/${INSTALL_NAME}" | awk '{ print $1 }')
else
  echo "Error: Neither sha256sum nor shasum is available to verify the download"
  exit 1
fi

if [ "$(tr '[:upper:]' '[:lower:]' <<<"$ACTUAL_SHA256")" != "$(tr '[:upper:]' '[:lower:]' <<<"$EXPECTED_SHA256")" ]; then
  echo "Error: Checksum mismatch for ${BINARY_NAME}"
  echo "Expected: ${EXPECTED_SHA256}"
  echo "Actual:   ${ACTUAL_SHA256}"
  exit 1
fi
echo "Checksum verified: ${ACTUAL_SHA256}"

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
