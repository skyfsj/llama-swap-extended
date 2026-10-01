#!/bin/bash
#
# Build script for unified container with version pinning
#
# Usage:
#   ./build-image.sh --cuda                              # Build CUDA image
#   ./build-image.sh --vulkan                            # Build Vulkan image
#   ./build-image.sh --rocm                              # Build ROCm/HIP image
#   ./build-image.sh --cuda --no-cache                   # Build without cache
#   LLAMA_REF=b1234 ./build-image.sh --vulkan            # Pin llama.cpp to a commit hash
#   LLAMA_REF=v1.2.3 ./build-image.sh --cuda             # Pin llama.cpp to a tag
#   WHISPER_REF=v1.0.0 ./build-image.sh --vulkan         # Pin whisper.cpp to a tag
#   SD_REF=master ./build-image.sh --cuda                # Pin stable-diffusion.cpp to a branch
#   AUDIO_REF=main ./build-image.sh --cuda               # Pin audio.cpp to a branch
#   LS_VERSION=170 ./build-image.sh --cuda               # Override llama-swap version
#   IK_LLAMA_REF=main ./build-image.sh --cuda            # Pin ik_llama.cpp to main branch (CUDA only)
#

set -euo pipefail

BACKEND=""
NO_CACHE=false
WHISPER_FFMPEG="${WHISPER_FFMPEG:-yes}"

VARIANT_VALUE_PENDING=false
for arg in "$@"; do
    if $VARIANT_VALUE_PENDING; then
        VARIANT="$arg"
        VARIANT_VALUE_PENDING=false
        continue
    fi
    case $arg in
        --cuda)
            BACKEND="cuda"
            ;;
        --vulkan)
            BACKEND="vulkan"
            ;;
        --rocm|--hip)
            BACKEND="rocm"
            ;;
        --no-cache)
            NO_CACHE=true
            ;;
        --variant)
            VARIANT_VALUE_PENDING=true
            ;;
        --variant=*)
            VARIANT="${arg#*=}"
            ;;
        --help|-h)
            echo "Usage: ./build-image.sh --cuda|--vulkan|--rocm [--variant full|llamacpp|vllm|1cat-vllm] [--no-cache]"
            echo ""
            echo "Options:"
            echo "  --cuda      Build CUDA image (NVIDIA GPUs)"
            echo "  --vulkan    Build Vulkan image (AMD GPUs and compatible hardware)"
            echo "  --rocm      Build ROCm/HIP image (AMD GPUs)"
            echo "  --variant   Image contents: full (default), llamacpp, vllm, 1cat-vllm (CUDA only)"
            echo "  --no-cache  Force rebuild without using Docker cache"
            echo "  --help, -h  Show this help message"
            echo ""
            echo "Environment variables:"
            echo "  DOCKER_IMAGE_TAG     Set custom image tag (default: llama-swap:unified-<backend>,"
            echo "                       or llama-swap:<variant>-<backend> for non-full variants)"
            echo "  VARIANT              Image contents variant (same as --variant)"
            echo "  ROCM_IMAGE           ROCm builder/runtime image (default: rocm/dev-ubuntu-24.04:7.2.4)"
            echo "  GPU_TARGETS          Optional semicolon-separated AMD GPU targets (e.g. gfx1100;gfx1103)"
            echo "  LLAMA_REF            Pin llama.cpp to a commit, tag, or branch"
            echo "  WHISPER_REF          Pin whisper.cpp to a commit, tag, or branch"
            echo "  SD_REF               Pin stable-diffusion.cpp to a commit, tag, or branch"
            echo "  AUDIO_REF            Pin audio.cpp to a commit, tag, or branch"
            echo "  IK_LLAMA_REF         Pin ik_llama.cpp to a commit, tag, or branch (CUDA only)"
            echo "  LS_VERSION           Override llama-swap version (e.g., '170' or 'latest')"
            echo "  WHISPER_FFMPEG       Enable whisper.cpp FFmpeg support (default: yes)"
            exit 0
            ;;
    esac
done

VARIANT="${VARIANT:-full}"

case "${VARIANT}" in
    full|llamacpp|vllm|1cat-vllm) ;;
    *)
        echo "Error: unknown variant '${VARIANT}' (full|llamacpp|vllm|1cat-vllm)" >&2
        exit 1
        ;;
esac

if [[ -z "$BACKEND" ]]; then
    echo "Error: No backend specified. Please use --cuda, --vulkan, or --rocm."
    echo ""
    echo "Usage: ./build-image.sh --cuda|--vulkan|--rocm [--variant full|llamacpp|vllm|1cat-vllm] [--no-cache]"
    exit 1
fi

# 1Cat vLLM builds only on CUDA: the wheel compile needs nvcc and the
# torch wheel it depends on is cu128.
if [[ "${VARIANT}" == "1cat-vllm" && "${BACKEND}" != "cuda" ]]; then
    echo "Error: variant '1cat-vllm' requires --cuda (got --${BACKEND})" >&2
    exit 1
fi

DEFAULT_TAG="llama-swap:unified-${BACKEND}"
if [[ "${VARIANT}" != "full" ]]; then
    DEFAULT_TAG="llama-swap:${VARIANT}-${BACKEND}"
fi
DOCKER_IMAGE_TAG="${DOCKER_IMAGE_TAG:-${DEFAULT_TAG}}"

# Git repository URLs
LLAMA_REPO="https://github.com/ggml-org/llama.cpp.git"
WHISPER_REPO="https://github.com/ggml-org/whisper.cpp.git"
SD_REPO="https://github.com/leejet/stable-diffusion.cpp.git"
AUDIO_REPO="https://github.com/0xShug0/audio.cpp.git"
# llama-swap source comes from this fork; override with env for upstream builds.
LLAMA_SWAP_REPO="${LLAMA_SWAP_REPO:-https://github.com/skyfsj/llama-swap-extended.git}"
IK_LLAMA_REPO="https://github.com/ikawrakow/ik_llama.cpp.git"

# Resolve a git ref (commit hash, tag, or branch) to a full commit hash.
# Requires only: git, network access to the remote.
resolve_ref() {
    local repo_url="$1"
    local ref="$2"

    # Full 40-char SHA — use as-is
    if [[ "${ref}" =~ ^[0-9a-f]{40}$ ]]; then
        echo "${ref}"
        return
    fi

    # Try tag then branch (exact match)
    local hash
    hash=$(git ls-remote "${repo_url}" "refs/tags/${ref}" "refs/heads/${ref}" 2>/dev/null | head -1 | cut -f1)
    if [[ -n "${hash}" ]]; then
        echo "${hash}"
        return
    fi

    # Short hash (7+ chars): scan all refs for a SHA with this prefix
    if [[ "${ref}" =~ ^[0-9a-f]{7,}$ ]]; then
        hash=$(git ls-remote "${repo_url}" 2>/dev/null | grep "^${ref}" | head -1 | cut -f1)
        if [[ -n "${hash}" ]]; then
            echo "${hash}"
            return
        fi
    fi

    echo "ERROR: Could not resolve ref '${ref}' for ${repo_url}" >&2
    if [[ "${ref}" =~ ^[0-9a-f]+$ && ${#ref} -lt 7 ]]; then
        echo "  Short hashes must be at least 7 characters (got ${#ref})." >&2
    else
        echo "  Tried: tag, branch, git ls-remote prefix match" >&2
    fi
    echo "  Use a full 40-char SHA, a tag name, a branch name, or a 7+ char short hash." >&2
    return 1
}

# Resolve HEAD of a repo without needing to know the default branch name.
get_latest_hash() {
    git ls-remote "${1}" HEAD 2>/dev/null | head -1 | cut -f1
}

echo "=========================================="
echo "llama-swap Unified Build (${BACKEND})"
echo "=========================================="
echo ""

# Resolve llama.cpp ref
if [[ -n "${LLAMA_REF:-}" ]]; then
    LLAMA_HASH=$(resolve_ref "${LLAMA_REPO}" "${LLAMA_REF}") || exit 1
    echo "llama.cpp: ${LLAMA_REF} -> ${LLAMA_HASH}"
else
    LLAMA_HASH=$(get_latest_hash "${LLAMA_REPO}")
    if [[ -z "${LLAMA_HASH}" ]]; then
        echo "ERROR: Could not determine latest commit for llama.cpp" >&2
        exit 1
    fi
    echo "llama.cpp: latest HEAD: ${LLAMA_HASH}"
fi

# Resolve whisper.cpp ref
if [[ -n "${WHISPER_REF:-}" ]]; then
    WHISPER_HASH=$(resolve_ref "${WHISPER_REPO}" "${WHISPER_REF}") || exit 1
    echo "whisper.cpp: ${WHISPER_REF} -> ${WHISPER_HASH}"
else
    WHISPER_HASH=$(get_latest_hash "${WHISPER_REPO}")
    if [[ -z "${WHISPER_HASH}" ]]; then
        echo "ERROR: Could not determine latest commit for whisper.cpp" >&2
        exit 1
    fi
    echo "whisper.cpp: latest HEAD: ${WHISPER_HASH}"
fi

# Resolve stable-diffusion.cpp ref
if [[ -n "${SD_REF:-}" ]]; then
    SD_HASH=$(resolve_ref "${SD_REPO}" "${SD_REF}") || exit 1
    echo "stable-diffusion.cpp: ${SD_REF} -> ${SD_HASH}"
else
    SD_HASH=$(get_latest_hash "${SD_REPO}")
    if [[ -z "${SD_HASH}" ]]; then
        echo "ERROR: Could not determine latest commit for stable-diffusion.cpp" >&2
        exit 1
    fi
    echo "stable-diffusion.cpp: latest HEAD: ${SD_HASH}"
fi

# Resolve audio.cpp ref
if [[ -n "${AUDIO_REF:-}" ]]; then
    AUDIO_HASH=$(resolve_ref "${AUDIO_REPO}" "${AUDIO_REF}") || exit 1
    echo "audio.cpp: ${AUDIO_REF} -> ${AUDIO_HASH}"
else
    AUDIO_HASH=$(get_latest_hash "${AUDIO_REPO}")
    if [[ -z "${AUDIO_HASH}" ]]; then
        echo "ERROR: Could not determine latest commit for audio.cpp" >&2
        exit 1
    fi
    echo "audio.cpp: latest HEAD: ${AUDIO_HASH}"
fi

# Resolve ik_llama.cpp ref (CUDA only)
if [[ "$BACKEND" == "cuda" ]]; then
    if [[ -n "${IK_LLAMA_REF:-}" ]]; then
        IK_LLAMA_HASH=$(resolve_ref "${IK_LLAMA_REPO}" "${IK_LLAMA_REF}") || exit 1
        echo "ik_llama.cpp: ${IK_LLAMA_REF} -> ${IK_LLAMA_HASH}"
    else
        IK_LLAMA_HASH=$(get_latest_hash "${IK_LLAMA_REPO}")
        if [[ -z "${IK_LLAMA_HASH}" ]]; then
            echo "ERROR: Could not determine latest commit for ik_llama.cpp" >&2
            exit 1
        fi
        echo "ik_llama.cpp: latest HEAD: ${IK_LLAMA_HASH}"
    fi
else
    IK_LLAMA_HASH="n/a"
    echo "ik_llama.cpp: skipped (${BACKEND} build)"
fi

# Resolve llama-swap ref
if [[ -n "${LS_VERSION:-}" ]]; then
    LS_HASH=$(resolve_ref "${LLAMA_SWAP_REPO}" "${LS_VERSION}") || exit 1
    echo "llama-swap: ${LS_VERSION} -> ${LS_HASH}"
else
    LS_HASH=$(get_latest_hash "${LLAMA_SWAP_REPO}")
    if [[ -z "${LS_HASH}" ]]; then
        echo "ERROR: Could not determine latest commit for llama-swap" >&2
        exit 1
    fi
    echo "llama-swap: latest HEAD: ${LS_HASH}"
fi

echo ""
echo "=========================================="
echo "Starting Docker build..."
echo "=========================================="
echo ""

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

BUILD_TARGET="${VARIANT}"
if [[ "${VARIANT}" == "1cat-vllm" ]]; then
    # Dockerfile stage names cannot start with a digit.
    BUILD_TARGET="onecat-vllm"
fi

BUILD_ARGS=(
    --target "${BUILD_TARGET}"
    --build-arg "BACKEND=${BACKEND}"
    --build-arg "LLAMA_COMMIT_HASH=${LLAMA_HASH}"
    --build-arg "WHISPER_COMMIT_HASH=${WHISPER_HASH}"
    --build-arg "SD_COMMIT_HASH=${SD_HASH}"
    --build-arg "AUDIO_COMMIT_HASH=${AUDIO_HASH}"
    --build-arg "IK_LLAMA_COMMIT_HASH=${IK_LLAMA_HASH}"
    --build-arg "LS_VERSION=${LS_HASH}"
    --build-arg "WHISPER_FFMPEG=${WHISPER_FFMPEG}"
    -t "${DOCKER_IMAGE_TAG}"
    -f "${SCRIPT_DIR}/Dockerfile"
)
if [[ -n "${ROCM_IMAGE:-}" ]]; then
    BUILD_ARGS+=(--build-arg "ROCM_IMAGE=${ROCM_IMAGE}")
fi
if [[ -n "${GPU_TARGETS:-}" ]]; then
    BUILD_ARGS+=(--build-arg "GPU_TARGETS=${GPU_TARGETS}")
fi

if [[ "$NO_CACHE" == true ]]; then
    BUILD_ARGS+=(--no-cache)
    echo "Note: Building without cache"
elif [[ "${GITHUB_ACTIONS:-}" == "true" && "${ACT:-}" != "true" ]]; then
    # The cache image follows the repository namespace. Only export: a
    # --cache-from on a not-yet-existing image makes buildx abort the whole
    # build ("failed to configure registry cache importer"), so CI builds
    # stay cold until the first successful export.
    CACHE_REF="ghcr.io/${GITHUB_REPOSITORY:-skyfsj/llama-swap-extended}:${VARIANT}-${BACKEND}-cache"
    BUILD_ARGS+=(
        --cache-to "type=registry,ref=${CACHE_REF},mode=max,ignore-error=true"
    )
    echo "Note: exporting registry cache (${CACHE_REF})"
fi

DOCKER_BUILDKIT=1 docker buildx build --load "${BUILD_ARGS[@]}" "${SCRIPT_DIR}"

echo ""
echo "=========================================="
echo "Verifying build artifacts (variant=${VARIANT}, backend=${BACKEND})..."
echo "=========================================="
echo ""

# Expected binaries follow the variant: full ships everything, llamacpp
# only the llama.cpp servers, vllm only the wrapper. llama-swap and
# vllm-wrapper are present in every variant.
case "${VARIANT}" in
    llamacpp)
        EXPECTED_BINARIES=(llama-server llama-cli llama-tts llama-bench llama-swap vllm-wrapper)
        ;;
    vllm)
        EXPECTED_BINARIES=(llama-swap vllm-wrapper)
        ;;
    *)
        EXPECTED_BINARIES=(llama-server llama-cli llama-bench whisper-server whisper-cli sd-server sd-cli audiocpp_server audiocpp_cli llama-swap vllm-wrapper)
        ;;
esac
if [[ "$BACKEND" == "cuda" && "${VARIANT}" != "vllm" ]]; then
    EXPECTED_BINARIES+=(ik-llama-server)
fi

MISSING_BINARIES=()
for binary in "${EXPECTED_BINARIES[@]}"; do
    if ! docker run --rm --entrypoint which "${DOCKER_IMAGE_TAG}" "${binary}" >/dev/null 2>&1; then
        MISSING_BINARIES+=("${binary}")
    fi
done

if [[ ${#MISSING_BINARIES[@]} -gt 0 ]]; then
    echo "ERROR: Build succeeded but the following binaries are missing:"
    for binary in "${MISSING_BINARIES[@]}"; do
        echo "  - ${binary}"
    done
    echo ""
    echo "Try running with --no-cache flag:"
    echo "  ./build-image.sh --${BACKEND} --no-cache"
    exit 1
fi

if [[ "${VARIANT}" != "vllm" ]] && ! docker run --rm --entrypoint test "${DOCKER_IMAGE_TAG}" \
        -x /opt/llama-swap/runtimes/llamacpp/bin/llama-server; then
    echo "ERROR: bundled llama.cpp managed-runtime asset is missing or not executable."
    exit 1
fi

VERIFIED_LIST="llama-server, llama-cli, llama-bench, whisper-server, whisper-cli, sd-server, sd-cli, audiocpp_server, audiocpp_cli, llama-swap, vllm-wrapper"
if [[ "$BACKEND" == "cuda" ]]; then
    VERIFIED_LIST="${VERIFIED_LIST}, ik-llama-server"
fi
echo "All expected binaries verified: ${VERIFIED_LIST}"

# audio.cpp must be a deployment build: the model_specs catalog is compiled into
# the binaries, since the image ships no model_specs/ directory for the runtime
# to discover. Without it every load of a package without an embedded spec fails
# with "model spec not found for family ...". The compiled catalog is raw JSON in
# .rodata, so grepping the binary for a known spec confirms it is there.
# Variants without audio.cpp skip this (and the llama.cpp runtime test below).
if [[ "${VARIANT}" != "vllm" ]] && ! docker run --rm --entrypoint grep "${DOCKER_IMAGE_TAG}" \
        -aq '"family": "pocket_tts"' /usr/local/bin/audiocpp_server; then
    echo "ERROR: audiocpp_server was not built with AUDIOCPP_DEPLOYMENT_BUILD=ON;"
    echo "       its compiled model spec catalog is missing."
    exit 1
fi

# Run the binary so a missing runtime library is caught here rather than on a
# user's first request. CUDA builds link libcuda.so.1, which the NVIDIA
# container runtime only injects with --gpus; point at the stub copied into the
# image so this works on a build machine with no GPU.
SMOKE_ARGS=(--rm)
if [[ "$BACKEND" == "cuda" ]]; then
    SMOKE_ARGS+=(-e "LD_LIBRARY_PATH=/usr/local/cuda/lib64/stubs:/usr/local/cuda/lib64")
fi

if [[ "${VARIANT}" == "vllm" ]]; then
    echo "audio.cpp checks skipped (not part of the vllm variant)"
else
    if ! docker run "${SMOKE_ARGS[@]}" --entrypoint audiocpp_server "${DOCKER_IMAGE_TAG}" --help >/dev/null; then
        echo "ERROR: audiocpp_server --help failed; the binary or its runtime"
        echo "       libraries are broken in the image."
        exit 1
    fi

    echo "audio.cpp verified: deployment build (compiled model spec catalog), binary runs"
fi

echo ""
echo "=========================================="
echo "Building rootless image..."
echo "=========================================="
echo ""

ROOTLESS_TAG="${DOCKER_IMAGE_TAG}-rootless"
# buildx resolves the base FROM the registry, not the local store, so the
# main image must be pushed before the rootless child can reference it.
docker push "${DOCKER_IMAGE_TAG}"
docker buildx build --load -t "${ROOTLESS_TAG}" - <<EOF
FROM ${DOCKER_IMAGE_TAG}
USER root
RUN groupadd --system --gid 10001 llama-swap && \\
    useradd --system --uid 10001 --gid 10001 \\
      --home /app --shell /sbin/nologin llama-swap && \\
    chown -R 10001:10001 /etc/llama-swap /var/lib/llama-swap /models
USER 10001
EOF

echo "Rootless image built: ${ROOTLESS_TAG}"

echo ""
echo "=========================================="
echo "Build complete!"
echo "=========================================="
echo ""
echo "Image tags:"
echo "  ${DOCKER_IMAGE_TAG}"
echo "  ${ROOTLESS_TAG}"
echo ""
echo "Built with:"
echo "  llama.cpp:            ${LLAMA_HASH}"
echo "  whisper.cpp:          ${WHISPER_HASH}"
echo "  stable-diffusion.cpp: ${SD_HASH}"
echo "  audio.cpp:            ${AUDIO_HASH}"
if [[ "$BACKEND" == "cuda" ]]; then
    echo "  ik_llama.cpp:         ${IK_LLAMA_HASH}"
fi
echo "  llama-swap:           $(docker run --rm --entrypoint cat "${DOCKER_IMAGE_TAG}" /versions.txt | grep llama-swap | cut -d' ' -f2-)"
echo ""
if [[ "$BACKEND" == "vulkan" ]]; then
    echo "Run with:"
    echo "  docker run -it --rm --device /dev/dri:/dev/dri ${DOCKER_IMAGE_TAG}"
    echo ""
    echo "Note: For AMD GPUs, you may also need:"
    echo "  docker run -it --rm --device /dev/dri:/dev/dri --group-add video ${DOCKER_IMAGE_TAG}"
elif [[ "$BACKEND" == "rocm" ]]; then
    echo "Run with:"
    echo "  docker run -it --rm --device /dev/kfd:/dev/kfd --device /dev/dri:/dev/dri --group-add video ${DOCKER_IMAGE_TAG}"
else
    echo "Run with:"
    echo "  docker run -it --rm --gpus all ${DOCKER_IMAGE_TAG}"
fi
