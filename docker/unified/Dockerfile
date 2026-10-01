# Unified multi-stage Dockerfile for AI inference tools
# Supports CUDA, Vulkan, and ROCm/HIP backends via BACKEND build arg, and
# image content variants via VARIANT:
#
#   full       everything (default): llama.cpp + whisper + sd + audio + vllm-wrapper
#   llamacpp   llama.cpp servers only (no whisper/sd/audio)
#   vllm       vllm-wrapper only (no llama.cpp/whisper/sd/audio builds)
#   1cat-vllm  full CUDA image plus the 1Cat vLLM build toolchain (CUDA only;
#              the 1Cat wheel itself is installed at deploy time by the
#              runtime manager from a bind-mounted /runtime-sources)
#
# Usage:
#   docker buildx build --build-arg BACKEND=cuda --target full -t llama-swap:unified-cuda .
#   docker buildx build --build-arg BACKEND=cuda --target llamacpp -t llama-swap:llamacpp-cuda .
#   docker buildx build --build-arg BACKEND=vulkan --target vllm -t llama-swap:vllm-vulkan .
#   docker buildx build --build-arg BACKEND=cuda --target 1cat-vllm -t llama-swap:1cat-vllm-cuda .
#
# Each project has its own install script that handles cloning, building,
# and installing binaries. Build stages are independent for cache efficiency.

ARG BACKEND=cuda
ARG ROCM_IMAGE=rocm/dev-ubuntu-24.04:7.2.4
ARG GPU_TARGETS=""
ARG CUDA_RUNTIME_IMAGE=nvidia/cuda:12.9.1-runtime-ubuntu24.04
ARG CUDA_DEVEL_IMAGE=nvidia/cuda:12.9.1-devel-ubuntu24.04
ARG GO_BUILDER_IMAGE=golang:1.26-bookworm
ARG UBUNTU_IMAGE=ubuntu:24.04

# ── Builder bases ──────────────────────────────────────────────────────

FROM ${CUDA_DEVEL_IMAGE} AS builder-base-cuda

# Compute capabilities compiled as SASS, shared by every CUDA build stage.
# 60/61 are Pascal (P100, GTX 10xx, P40) -- the oldest architecture ggml still
# supports -- through 89 (Ada). CMake emits PTX alongside SASS for each entry,
# so architectures between and above the list (70 Volta, 80 Ampere, 90 Hopper,
# 100 and 120 Blackwell) still run by JIT-compiling the nearest lower PTX; they
# just pay a first-run JIT cost and miss arch-specific kernels. Add those
# numbers here to compile them natively, at the cost of build time in every
# stage.
ARG CMAKE_CUDA_ARCHITECTURES="60;61;75;86;89"
ENV DEBIAN_FRONTEND=noninteractive
ENV CMAKE_CUDA_ARCHITECTURES=${CMAKE_CUDA_ARCHITECTURES}
ENV CCACHE_DIR=/ccache
ENV CCACHE_MAXSIZE=2G
ENV PATH="/usr/lib/ccache:${PATH}"

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential cmake git python3 python3-pip libssl-dev \
    curl ca-certificates ccache make wget \
    pkg-config libavcodec-dev libavformat-dev libavutil-dev libswresample-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /build

# ──

FROM ubuntu:24.04 AS builder-base-vulkan

ENV DEBIAN_FRONTEND=noninteractive
ENV CCACHE_DIR=/ccache
ENV CCACHE_MAXSIZE=2G
ENV PATH="/usr/lib/ccache:${PATH}"

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential cmake git python3 python3-pip libssl-dev \
    curl ca-certificates ccache make wget software-properties-common \
    libvulkan-dev glslang-tools spirv-tools vulkan-validationlayers glslc \
    pkg-config libavcodec-dev libavformat-dev libavutil-dev libswresample-dev \
    spirv-headers \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /build

FROM ${ROCM_IMAGE} AS builder-base-rocm

ARG GPU_TARGETS=""

ENV DEBIAN_FRONTEND=noninteractive
ENV ROCM_PATH=/opt/rocm
ENV HIP_PATH=/opt/rocm
ENV GPU_TARGETS=${GPU_TARGETS}
ENV CCACHE_DIR=/ccache
ENV CCACHE_MAXSIZE=2G
ENV PATH="/opt/rocm/bin:/opt/rocm/llvm/bin:/usr/lib/ccache:${PATH}"

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential cmake git python3 python3-pip libssl-dev \
    curl ca-certificates ccache make wget clang lld ninja-build \
    pkg-config libavcodec-dev libavformat-dev libavutil-dev libswresample-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /build

# ── Select builder base by BACKEND ────────────────────────────────────

FROM builder-base-${BACKEND} AS builder-base

# ── Build whisper.cpp (fastest build, run first) ──────────────────────

FROM builder-base AS whisper-build
ARG BACKEND=cuda
ARG WHISPER_FFMPEG=yes
ARG WHISPER_COMMIT_HASH=master
COPY install-whisper.sh /build/
RUN --mount=type=cache,id=ccache-${BACKEND},target=/ccache \
    --mount=type=cache,id=whisper-${BACKEND},target=/src/whisper.cpp/build \
    BACKEND=${BACKEND} WHISPER_FFMPEG=${WHISPER_FFMPEG} \
    bash /build/install-whisper.sh "${WHISPER_COMMIT_HASH}"

# ── Build stable-diffusion.cpp ────────────────────────────────────────

FROM builder-base AS sd-build
ARG BACKEND=cuda
ARG SD_COMMIT_HASH=master
COPY install-sd.sh /build/
RUN --mount=type=cache,id=ccache-${BACKEND},target=/ccache \
    --mount=type=cache,id=sd-${BACKEND},target=/src/stable-diffusion.cpp/build \
    BACKEND=${BACKEND} bash /build/install-sd.sh "${SD_COMMIT_HASH}"

# ── Build audio.cpp ───────────────────────────────────────────────────

FROM builder-base AS audio-build
ARG BACKEND=cuda
ARG AUDIO_COMMIT_HASH=main
COPY install-audio.sh /build/
RUN --mount=type=cache,id=ccache-${BACKEND},target=/ccache \
    --mount=type=cache,id=audio-${BACKEND},target=/src/audio.cpp/build \
    BACKEND=${BACKEND} bash /build/install-audio.sh "${AUDIO_COMMIT_HASH}"

# ── Build llama.cpp (slowest build, run last) ─────────────────────────

FROM builder-base AS llama-build
ARG BACKEND=cuda
ARG LLAMA_COMMIT_HASH=master
COPY install-llama.sh /build/
RUN --mount=type=cache,id=ccache-${BACKEND},target=/ccache \
    --mount=type=cache,id=llama-${BACKEND},target=/src/llama.cpp/build \
    BACKEND=${BACKEND} bash /build/install-llama.sh "${LLAMA_COMMIT_HASH}"

# ── Build ik_llama.cpp (CUDA only) ────────────────────────────────────
#
# Two named stages allow ARG BACKEND to select at build time:
#   - ik-llama-cuda  : real build (from builder-base-cuda)
#   - ik-llama-vulkan: no-op (empty /install/bin, skips CUDA pull entirely)
# BuildKit only evaluates the selected branch, so vulkan builds never
# pull nvidia/cuda:*-devel or compile ik_llama.cpp.

FROM builder-base-vulkan AS ik-llama-vulkan
RUN mkdir -p /install/bin

FROM builder-base-rocm AS ik-llama-rocm
RUN mkdir -p /install/bin

FROM builder-base-cuda AS ik-llama-cuda
ARG IK_LLAMA_COMMIT_HASH=main
COPY install-ik-llama.sh /build/
RUN --mount=type=cache,id=ccache-cuda,target=/ccache \
    --mount=type=cache,id=ik-llama-cuda,target=/src/ik_llama.cpp/build \
    bash /build/install-ik-llama.sh "${IK_LLAMA_COMMIT_HASH}"

ARG BACKEND=cuda
FROM ik-llama-${BACKEND} AS ik-llama-build

# ── Download llama-swap release binary ────────────────────────────────
#
# Plain ubuntu base: this stage only runs curl/tar, so variants that skip
# the C++ toolchain (vllm) do not need to resolve the CUDA devel image.

FROM ${UBUNTU_IMAGE} AS llama-swap-download
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates && rm -rf /var/lib/apt/lists/*
ARG LS_VERSION=latest
COPY install-llama-swap.sh /build/
RUN bash /build/install-llama-swap.sh "${LS_VERSION}"

# ── Build vllm-wrapper from llama-swap source ─────────────────────────
#
# vllm-wrapper is not shipped in the llama-swap release archives, so it is
# compiled from the same revision as the llama-swap binary above.

FROM ${GO_BUILDER_IMAGE} AS vllm-wrapper-build
ARG LS_VERSION=latest
COPY install-vllm-wrapper.sh /build/
RUN --mount=type=cache,id=go-build,target=/root/.cache/go-build \
    --mount=type=cache,id=go-mod,target=/go/pkg/mod \
    bash /build/install-vllm-wrapper.sh "${LS_VERSION}"

# ── Runtime bases ─────────────────────────────────────────────────────

FROM ${CUDA_RUNTIME_IMAGE} AS runtime-cuda

ENV DEBIAN_FRONTEND=noninteractive
ENV LD_LIBRARY_PATH="/usr/local/cuda/lib64:${LD_LIBRARY_PATH}"
ENV PATH="/usr/local/bin:${PATH}"

RUN apt-get update && apt-get install -y --no-install-recommends \
    libgomp1 python3 curl ca-certificates \
    libavcodec60 libavformat60 libavutil58 libswresample4 \
    ffmpeg \
    && rm -rf /var/lib/apt/lists/*

# CUDA stub drivers for container compatibility
COPY --from=builder-base-cuda /usr/local/cuda/lib64/stubs/libcuda.so /usr/local/cuda/lib64/stubs/libcuda.so
COPY --from=builder-base-cuda /usr/local/cuda/lib64/stubs/libcuda.so /usr/local/cuda/lib64/stubs/libcuda.so.1

# ──

FROM ubuntu:24.04 AS runtime-vulkan

ENV DEBIAN_FRONTEND=noninteractive
ENV PATH="/usr/local/bin:${PATH}"

RUN apt-get update && apt-get install -y --no-install-recommends \
    libgomp1 libvulkan1 mesa-vulkan-drivers \
    rocm-smi \
    python3 curl ca-certificates \
    libavcodec60 libavformat60 libavutil58 libswresample4 \
    ffmpeg \
    && rm -rf /var/lib/apt/lists/*

FROM ${ROCM_IMAGE} AS runtime-rocm

ENV DEBIAN_FRONTEND=noninteractive
ENV ROCM_PATH=/opt/rocm
ENV HIP_PATH=/opt/rocm
ENV LD_LIBRARY_PATH="/opt/rocm/lib:/opt/rocm/lib64"
ENV PATH="/opt/rocm/bin:/opt/rocm/llvm/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

RUN apt-get update && apt-get install -y --no-install-recommends \
    libgomp1 python3 curl ca-certificates \
    libavcodec60 libavformat60 libavutil58 libswresample4 \
    ffmpeg \
    && rm -rf /var/lib/apt/lists/*

# ── 1Cat vLLM build toolchain (CUDA only) ─────────────────────────────
#
# 1Cat vLLM ships as a self-built wheel; the runtime manager compiles it
# against this image at deploy time (driver: custom, builder container =
# this image). Toolchain only — no wheel is baked into the image.


# ── Select runtime base by BACKEND ────────────────────────────────────

# ── Shared assembly ───────────────────────────────────────────────────
#
# The four variant targets (below) build FROM this stage. Each one aliases
# the optional component stages before building, which is what selects the
# image's contents.

FROM runtime-${BACKEND} AS assemble

ARG BACKEND=cuda
ARG LLAMA_COMMIT_HASH=unknown
ARG WHISPER_COMMIT_HASH=unknown
ARG SD_COMMIT_HASH=unknown
ARG IK_LLAMA_COMMIT_HASH=unknown
ARG AUDIO_COMMIT_HASH=unknown
ARG RUN_UID=0

RUN apt-get update && apt-get install -y --no-install-recommends \
    python3-numpy python3-sentencepiece python3-pip nodejs npm \
    && rm -rf /var/lib/apt/lists/*

# Create non-root user when RUN_UID != 0
RUN if [ "$RUN_UID" != "0" ]; then \
      groupadd --system --gid $RUN_UID llama-swap && \
      useradd --system --uid $RUN_UID --gid $RUN_UID \
        --home /app --shell /sbin/nologin llama-swap; \
    fi && \
    mkdir -p /etc/llama-swap/config /var/lib/llama-swap/runtimes && \
    chown -R ${RUN_UID}:${RUN_UID} /etc/llama-swap /var/lib/llama-swap

# Runtime versions, journals, managed keys and audit state must survive an
# image replacement.  Keep them below one explicit volume so an upgraded image
# cannot overwrite an operator-managed `current` runtime pointer.
VOLUME ["/var/lib/llama-swap"]

WORKDIR /app

# Optional components are NOT copied here: each variant target below copies
# exactly the components it includes, and BuildKit skips the build stages
# that are not reachable from the requested target.

# Keep a managed-runtime shaped copy of the bundled llama.cpp server. The
# persistent Runtime Manager root is a volume, so this image-owned asset is
# safe to seed once without replacing an operator-selected version later.
# SKIP_LLAMA_SEED=1 in the vllm-only variant (no bundled llama.cpp binaries).
RUN if [ -f /usr/local/bin/llama-server ]; then \
      mkdir -p /opt/llama-swap/runtimes/llamacpp/bin /opt/llama-swap/runtimes/llamacpp/build && \
      cp /usr/local/bin/llama-server /opt/llama-swap/runtimes/llamacpp/bin/llama-server && \
      cp /usr/local/bin/llama-cli /opt/llama-swap/runtimes/llamacpp/bin/llama-cli && \
      cp /usr/local/bin/llama-tts /opt/llama-swap/runtimes/llamacpp/bin/llama-tts && \
      cp /usr/local/bin/llama-bench /opt/llama-swap/runtimes/llamacpp/bin/llama-bench && \
      printf '%s\n' '{"mode":"native","sourceType":"bundled","builtin":"true"}' > /opt/llama-swap/runtimes/llamacpp/metadata.json; \
    fi

# Copy ik-llama-server (CUDA only; empty copy for Vulkan/ROCm)
COPY --from=ik-llama-build /install/bin/ /usr/local/bin/

# Install uv
RUN pip install uv --break-system-packages

# Copy llama-swap binary
COPY --from=llama-swap-download /install/bin/llama-swap /usr/local/bin/
COPY --from=llama-swap-download /install/llama-swap-version /tmp/

# Copy vllm-wrapper binary
COPY --from=vllm-wrapper-build /install/bin/vllm-wrapper /usr/local/bin/

RUN ldconfig

COPY config.example.yaml /etc/llama-swap/config/config.yaml

# audiocpp_server takes its own JSON config. Ship a starter with this image's
# backend baked in -- the binary defaults to "cuda", so a non-CUDA image that
# copies an unedited example would try to load every model on a backend it was
# not built with.
COPY audiocpp-server.example.json /etc/llama-swap/audiocpp-server.example.json
RUN sed -i "s/__BACKEND__/${BACKEND}/" /etc/llama-swap/audiocpp-server.example.json && \
    chown -R ${RUN_UID}:${RUN_UID} /etc/llama-swap /var/lib/llama-swap

# Version tracking
RUN echo "llama.cpp: ${LLAMA_COMMIT_HASH}" > /versions.txt && \
    echo "whisper.cpp: ${WHISPER_COMMIT_HASH}" >> /versions.txt && \
    echo "stable-diffusion.cpp: ${SD_COMMIT_HASH}" >> /versions.txt && \
    echo "ik_llama.cpp: ${IK_LLAMA_COMMIT_HASH}" >> /versions.txt && \
    echo "audio.cpp: ${AUDIO_COMMIT_HASH}" >> /versions.txt && \
    echo "llama-swap: $(cat /tmp/llama-swap-version)" >> /versions.txt && \
    echo "backend: ${BACKEND}" >> /versions.txt && \
    echo "build_timestamp: $(date -u +%Y-%m-%dT%H:%M:%SZ)" >> /versions.txt

RUN mkdir -p /models && chown ${RUN_UID}:${RUN_UID} /models
WORKDIR /models
USER ${RUN_UID}
ENTRYPOINT ["llama-swap"]
CMD ["-config", "/etc/llama-swap/config/config.yaml", "-listen", "0.0.0.0:8080", "-watch-config"]

# ── Variant targets ───────────────────────────────────────────────────
#
# BuildKit executes only the stages reachable from the requested target, so
# each variant pays for exactly the components it includes.

# full (default): everything — `--target full`
FROM assemble AS full
COPY --from=whisper-build /install/ /usr/local/
COPY --from=sd-build /install/ /usr/local/
COPY --from=audio-build /install/ /usr/local/
COPY --from=llama-build /install/ /usr/local/

# llamacpp: llama.cpp servers only — `--target llamacpp`
FROM assemble AS llamacpp
COPY --from=llama-build /install/ /usr/local/

# vllm: vllm-wrapper and python only — `--target vllm`
FROM assemble AS vllm

# 1Cat vLLM build toolchain: nvcc + headers live only in the devel image;
# copy the full CUDA toolkit over the runtime image's runtime-only subset
# (standard devel-over-runtime overlay). Build toolchain apt packages are
# installed directly in the final stage.
FROM ${CUDA_DEVEL_IMAGE} AS onecat-toolchain

# 1cat-vllm: full image + 1Cat vLLM build toolchain layered on top
# (CUDA-only) — `--target 1cat-vllm`. The wheel itself is compiled by the
# runtime manager at deploy time; this image only carries the toolchain.
FROM full AS onecat-vllm
ARG DEBIAN_FRONTEND=noninteractive
# Copy the toolkit directory the devel image actually contains (version-suffixed,
# e.g. cuda-12.9) — a hardcoded cuda-12.8 path does not exist in the 12.9 image.
COPY --from=onecat-toolchain /usr/local/cuda-12.9/ /usr/local/cuda-12.9/
ENV CUDA_HOME=/usr/local/cuda \
    LD_LIBRARY_PATH=/usr/local/cuda/lib64:${LD_LIBRARY_PATH} \
    TORCH_CUDA_ARCH_LIST="7.0" \
    MAX_JOBS=12 \
    NVCC_THREADS=1 \
    UV_CACHE_DIR=/var/lib/llama-swap/uv-cache
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential cmake ninja-build pkg-config git \
    libnuma-dev python3-dev python3-venv \
    && rm -rf /var/lib/apt/lists/* \
    && ln -sfn /usr/local/cuda-12.9 /usr/local/cuda \
    && mkdir -p /runtime-sources /var/lib/llama-swap/uv-cache
