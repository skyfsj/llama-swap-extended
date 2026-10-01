#!/bin/bash
# Install whisper.cpp - clone, build, and install binaries
# Usage: BACKEND=cuda|vulkan|rocm ./install-whisper.sh <commit_hash>
set -e

COMMIT_HASH="${1:-master}"

# Refuse a moving ref by default: building from master/main produces images
# that cannot be reproduced later. Pass an explicit commit hash/tag (or use
# build-image.sh, which resolves refs) or set ALLOW_MOVING_REF=1 to opt in.
if [[ "master" == "${COMMIT_HASH}" && "${ALLOW_MOVING_REF:-0}" != "1" ]]; then
    echo "ERROR: whisper ref defaults to a moving branch ('master'). Pin an explicit commit or tag:" >&2
    echo "       install-whisper.sh <commit_hash>   (or ALLOW_MOVING_REF=1 to accept a non-reproducible build)" >&2
    exit 1
fi
BACKEND="${BACKEND:-cuda}"
WHISPER_FFMPEG="${WHISPER_FFMPEG:-yes}"

mkdir -p /install/bin /install/lib

if [[ "${WHISPER_FFMPEG,,}" == "yes" || "${WHISPER_FFMPEG,,}" == "true" ]]; then
    WHISPER_FFMPEG_ENABLED=1
else
    WHISPER_FFMPEG_ENABLED=0
fi

# Clone and checkout (init-based so cache-mounted /src/whisper.cpp/build dir doesn't break clone)
echo "=== Cloning whisper.cpp at ${COMMIT_HASH} ==="
mkdir -p /src/whisper.cpp
cd /src/whisper.cpp
if [ ! -d .git ]; then
    git init
    git remote add origin https://github.com/ggml-org/whisper.cpp.git
fi
git fetch --depth=1 origin "${COMMIT_HASH}"
git checkout FETCH_HEAD

# Common cmake flags
CMAKE_FLAGS=(
    -DGGML_NATIVE=OFF
    -DCMAKE_BUILD_TYPE=Release
    -DCMAKE_C_COMPILER_LAUNCHER=ccache
    -DCMAKE_CXX_COMPILER_LAUNCHER=ccache
)

if [ "$BACKEND" = "cuda" ]; then
    CMAKE_FLAGS+=(
        -DGGML_CUDA=ON
        -DGGML_VULKAN=OFF
        "-DCMAKE_CUDA_ARCHITECTURES=${CMAKE_CUDA_ARCHITECTURES:?CMAKE_CUDA_ARCHITECTURES must be set}"
        "-DCMAKE_CUDA_FLAGS=-allow-unsupported-compiler"
        "-DCMAKE_EXE_LINKER_FLAGS=-Wl,-rpath-link,/usr/local/cuda/lib64/stubs -lcuda"
        "-DCMAKE_SHARED_LINKER_FLAGS=-Wl,-rpath-link,/usr/local/cuda/lib64/stubs -lcuda"
    )
elif [ "$BACKEND" = "vulkan" ]; then
    CMAKE_FLAGS+=(
        -DGGML_CUDA=OFF
        -DGGML_VULKAN=ON
    )
elif [ "$BACKEND" = "rocm" ]; then
    CMAKE_FLAGS+=(
        -DGGML_CUDA=OFF
        -DGGML_VULKAN=OFF
        -DGGML_HIP=ON
    )
    if [ -n "${GPU_TARGETS:-}" ]; then
        CMAKE_FLAGS+=("-DGPU_TARGETS=${GPU_TARGETS}" "-DAMDGPU_TARGETS=${GPU_TARGETS}")
    fi
else
    echo "FATAL: unsupported backend ${BACKEND}; use cuda, vulkan, or rocm" >&2
    exit 1
fi

if [ "$WHISPER_FFMPEG_ENABLED" -eq 1 ]; then
    CMAKE_FLAGS+=(-DWHISPER_FFMPEG=ON)
fi

TARGETS=(whisper-cli whisper-server)

rm -rf build/CMakeCache.txt build/CMakeFiles 2>/dev/null || true

echo "=== Building whisper.cpp for ${BACKEND} ==="
cmake -B build "${CMAKE_FLAGS[@]}"
cmake --build build --config Release -j"$(nproc)" --target "${TARGETS[@]}"

for bin in "${TARGETS[@]}"; do
    if [ ! -f "build/bin/$bin" ]; then
        echo "FATAL: $bin not found in build/bin/" >&2
        exit 1
    fi
    cp "build/bin/$bin" "/install/bin/"
done
find build -name "*.so*" -type f -exec cp {} /install/lib/ \;

echo "=== whisper.cpp build complete ==="
ls -la /install/bin/
