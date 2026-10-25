#!/bin/bash
# Shared helpers for the llama-swap installer scripts.
#
# Sourced, not executed: `source /build/lib-release.sh`.

# resolve_latest_version <owner/repo>
#
# Prints the newest release version with the leading "v" stripped (e.g. "257").
#
# The releases API is the source of truth, but api.github.com rate limits
# unauthenticated callers by IP and Docker builds on shared CI runners hit that
# limit regularly (HTTP 403). A build stage has no credentials to authenticate
# with, so when the API refuses, fall back to the highest version tag on the git
# remote: that is a different endpoint, it is not rate limited the same way, and
# releases are tagged v<version>, so the highest tag is the latest release.
#
# The tag pattern accepts both numbering schemes in use here: the upstream
# sequential tags (v257) and this fork's semantic ones (v1.0.1). A dot-free
# pattern would silently never match the fork's own repository — the default
# REPO in the installer scripts — leaving the fallback with nothing to print
# whenever the API is rate limited.
resolve_latest_version() {
    local repo="$1"
    local version=""

    version=$(curl -fsSL "https://api.github.com/repos/${repo}/releases/latest" 2>/dev/null \
        | grep '"tag_name"' | head -1 | cut -d'"' -f4 | sed 's/^v//') || true

    if [ -z "${version}" ]; then
        echo "GitHub releases API unavailable, falling back to git tags" >&2
        version=$(git ls-remote --tags --refs "https://github.com/${repo}.git" 2>/dev/null \
            | sed 's|.*refs/tags/||' | grep -E '^v[0-9]+(\.[0-9]+)*$' | sort -V | tail -1 \
            | sed 's/^v//') || true
    fi

    echo "${version}"
}
