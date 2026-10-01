# Unified Docker Container

These scripts create a custom llama-swap container that contains:

- llama-server for LLMs, rerank and embedding model support
- sd-server (stable-diffusion.cpp) for image generation
- whisper.cpp for ASR
- audiocpp_server (audio.cpp) for TTS and audio tasks (`/audioapi/v1/tasks/run`)
- vllm-wrapper for vLLM sleep mode support (see [cmd/vllm-wrapper](../../cmd/vllm-wrapper/README.md))

`vllm-wrapper` is built from the same llama-swap revision as the `llama-swap`
binary in the image. It expects a vLLM server started with `--enable-sleep-mode`
that is reachable from the container; vLLM itself is not included in the image.

Runtime Manager is always active. The shipped config registers `llamacpp` from
the image-owned `/opt/llama-swap/runtimes/llamacpp` asset. The first start seeds it
into the `/var/lib/llama-swap` volume; later image upgrades leave the active
runtime untouched.

### Persistent runtime and control-plane state

The image declares `/var/lib/llama-swap` as a volume. Its shipped config stores
managed runtime versions under `/var/lib/llama-swap/runtimes`; runtime
journals, API-key hashes, audit records and response affinity are kept there and
are not replaced by an image upgrade. Mount a named or host volume in production:

```bash
docker volume create llama-swap-state
docker run --rm --runtime nvidia -p 9292:8080 \
  -v llama-swap-state:/var/lib/llama-swap \
  -v /path/to/models:/models \
  -v /path/to/config.yaml:/etc/llama-swap/config/config.yaml \
  llama-swap:unified-cuda
```

For a rootless image, build with `RUN_UID=10001` (or use the build script's
rootless tag) and ensure the mounted volume is writable by UID/GID 10001. The
container does not require `docker.sock`, privileged mode or host PID access.

## audio.cpp

`audiocpp_server` needs its own JSON config listing the models it serves. The
image ships a starter with the backend it was built for already set:

```bash
docker run --rm --entrypoint cat llama-swap:unified-cuda \
  /etc/llama-swap/audiocpp-server.example.json > /path/to/models/audiocpp-server.json
```

Replace the example entries with your models, then point the `audio` entry in
`config.yaml` at it (see `config.example.yaml`). Every model needs a `family`
matching an audio.cpp model spec, and a `path` to the package inside the
container.

The `backend` field must match the image: `cuda`, `vulkan`, or `rocm` (HIP).
`audiocpp_server` defaults to `cuda` regardless of how it was compiled, so a
non-CUDA image with an unset backend fails to load models.

audio.cpp is compiled as a **deployment build**
(`AUDIOCPP_DEPLOYMENT_BUILD=ON`), which compiles the `model_specs/*.json`
catalog into the binaries. Without it the runtime can only use a spec embedded
in a GGUF or found in a `model_specs/` directory near the working directory,
neither of which a container of bare binaries has. The catalog is also
installed at `/usr/local/share/audiocpp/model_specs` for `--model-spec-override`
when a spec needs to be edited or pinned:

```yaml
cmd: |
  audiocpp_server
  --config /models/audiocpp-server.json
  --model-spec-override /usr/local/share/audiocpp/model_specs
  --port ${PORT}
```

### GPU support

The CUDA image compiles SASS for compute capabilities `60;61;75;86;89`, so
Pascal (P100, GTX 10xx, P40) and newer NVIDIA GPUs are supported. Architectures
between and above those entries — Volta (70), Ampere (80), Hopper (90) and
Blackwell (100 on datacenter parts, 120 on GeForce and RTX PRO) — run by
JIT-compiling the nearest lower PTX, which costs time on first load. Add the
number to `CMAKE_CUDA_ARCHITECTURES` in the Dockerfile to compile one of them
natively; the list is shared with llama.cpp, whisper.cpp, stable-diffusion.cpp
and ik_llama.cpp, so each addition lengthens every build.

The Vulkan image builds audio.cpp with `ENGINE_ENABLE_VULKAN=ON`. audio.cpp is
tuned for CUDA, and the server prints a notice on startup that a non-CUDA
backend may have lower performance and model coverage.

The ROCm image uses the AMD-published `rocm/dev-ubuntu-24.04` development
image and builds llama.cpp/whisper.cpp with `GGML_HIP=ON`, stable-diffusion.cpp
with `SD_HIPBLAS=ON`, and audio.cpp with `ENGINE_ENABLE_HIP=ON`. Set
`GPU_TARGETS` when the image must contain code for a specific AMD architecture;
otherwise the ROCm toolchain's default target is used:

```bash
GPU_TARGETS='gfx1100;gfx1103' ./build-image.sh --rocm
docker run -it --rm \
  --device /dev/kfd:/dev/kfd --device /dev/dri:/dev/dri --group-add video \
  llama-swap:unified-rocm
```
