#!/bin/bash
# Install llama-swap - download latest release binary from GitHub
# Usage: ./install-llama-swap.sh [version]
#   version: release version number (e.g., "170") or "latest" (default)
set -e

# shellcheck source=lib-release.sh
source "$(dirname "$0")/lib-release.sh"

VERSION="${1:-latest}"
# Default to this fork's releases; LS_BINARY_REPO overrides (e.g. to
# mostlygeek/llama-swap when building with an upstream release binary).
REPO="${LS_BINARY_REPO:-skyfsj/llama-swap-extended}"

mkdir -p /install/bin

# If a commit hash (full or abbreviated) is given, find the release tag
# that points to it. git ls-remote hashes are full 40-char, so an
# abbreviated prefix matches by grep.
if echo "${VERSION}" | grep -qE '^[0-9a-f]{7,40}$'; then
    echo "=== Resolving commit ${VERSION:0:7} to release tag ==="
    TAG=$(git ls-remote --tags "https://github.com/${REPO}.git" 2>/dev/null \
        | grep "^${VERSION}" | sed 's|.*refs/tags/||' | grep -v '\^{}' | head -1)
    if [ -n "${TAG}" ]; then
        echo "Resolved to tag: ${TAG}"
        VERSION="${TAG#v}"
    else
        # A commit hash with no release tag cannot be honoured here: this
        # stage downloads a published release archive, and there is none for
        # that revision. Falling back to latest would silently build a
        # different revision than the operator pinned, so refuse instead.
        echo "FATAL: no release tag points at commit ${VERSION:0:7} in ${REPO}" >&2
        echo "       This stage installs a published release archive; pin a release tag" >&2
        echo "       (LS_VERSION=<tag>) or set LS_BINARY_REPO to a repository that has one." >&2
        exit 1
    fi
fi

# Strip leading 'v' prefix so both "198" and "v198" work
VERSION="${VERSION#v}"

# Resolve "latest" to actual version number
if [ "$VERSION" = "latest" ]; then
    echo "=== Resolving latest llama-swap release ==="
    VERSION=$(resolve_latest_version "${REPO}")
    if [ -z "$VERSION" ]; then
        echo "FATAL: Could not determine latest release version" >&2
        echo "       Pass an explicit release tag as LS_VERSION." >&2
        exit 1
    fi
    echo "Latest version: ${VERSION}"
fi


ARCH=$(uname -m)
case "$ARCH" in
    x86_64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) echo "FATAL: Unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

# Download and extract
URL="https://github.com/${REPO}/releases/download/v${VERSION}/llama-swap_${VERSION}_linux_${ARCH}.tar.gz"
echo "=== Downloading llama-swap v${VERSION} ==="
echo "URL: $URL"
curl -fSL -o /tmp/llama-swap.tar.gz "$URL"
tar -xzf /tmp/llama-swap.tar.gz -C /install/bin/
rm /tmp/llama-swap.tar.gz

# Validate
if [ ! -x "/install/bin/llama-swap" ]; then
    echo "FATAL: llama-swap binary not found or not executable" >&2
    ls -la /install/bin/ >&2
    exit 1
fi

echo "$VERSION" > /install/llama-swap-version

echo "=== llama-swap v${VERSION} installed ==="
ls -la /install/bin/llama-swap