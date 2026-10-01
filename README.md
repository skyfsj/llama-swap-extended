![License](https://img.shields.io/badge/license-MIT-blue)
![Upstream](https://img.shields.io/badge/fork_of-mostlygeek%2Fllama--swap-8a2be2)

# llama-swap-extended

Run multiple generative AI models on your machine and hot-swap between them on demand. llama-swap works with any OpenAI and Anthropic API compatible server and is used by thousands of people to power their local AI workflows.

> **About this fork:** `llama-swap-extended` is a feature-rich fork of
> [mostlygeek/llama-swap](https://github.com/mostlygeek/llama-swap). On top of
> upstream it adds an extension system (JavaScript hooks and tools with a
> bundled SDK), a runtime center for building and managing inference backends,
> multi-user access keys, usage analytics, per-request speed telemetry and an
> overhauled management UI. Upstream's MIT license remains in effect — see
> `LICENSE.md`.

Built in Go for performance and simplicity, llama-swap has zero dependencies and is incredibly easy to set up. Get started in minutes - just one binary and one configuration file.

## Features:

- ✅ Easy to deploy and configure: one binary, one configuration file. no external dependencies
- ✅ On-demand model switching for many local AI servers (llama.cpp + forks, vllm, stable-diffusion.cpp, audio.cpp, ComfyUI, etc.)
  - future proof, upgrade your inference servers at any time.
- ✅ [Managed llama.cpp and vLLM runtimes](docs/configuration.md#managed-vllm-and-llamacpp-backends) from configurable Git/release/PyPI sources or Docker/Podman images, with immutable versions, automatic updates, pinning, rollback, and retention.
- ✅ [Optional LMCache KV-cache accelerator for vLLM](docs/configuration.md#lmcache-optional-kv-cache-accelerator): a supervised standalone server in its own version-managed virtualenv (isolated from vLLM venvs), seven-state lifecycle with health checks, fail-closed model startup, L2/L3 (system-memory/disk) tiers, per-model dependency tracking that refuses cache-dropping operations while models run, and versioned stage/activate/rollback updates.
- ✅ [Programmable Extensions](docs/extensions/README.md) for OpenAI-compatible request and response hooks, function tools, and controlled JavaScript execution.
- ✅ OpenAI API supported endpoints:
  - `v1/completions`
  - `v1/chat/completions`
  - `v1/chat/completions/batch` (vLLM batched Chat Completions; `model` may be omitted when one local model is configured)
  - `v1/responses`
  - `v1/embeddings`
  - `v1/models` - list available models
  - `v1/audio/speech` ([#36](https://github.com/mostlygeek/llama-swap/issues/36))
  - `v1/audio/transcriptions` ([docs](https://github.com/mostlygeek/llama-swap/issues/41#issuecomment-2722637867))
  - `v1/audio/voices`
  - `v1/images/generations`
  - `v1/images/edits`
- ✅ Anthropic API supported endpoints:
  - `v1/messages`
  - `v1/messages/count_tokens`
- ✅ llama-server (llama.cpp) supported endpoints
  - `v1/rerank`, `v1/reranking`, `/rerank`
  - `/infill` - for code infilling
  - `/completion` - for completion endpoint
  - `/models` - list available models. same behavior as `v1/models`
  - `/props` - requires `?model={model_id}` query parameter to be provided. The autoload parameter is not supported and will be ignored.
- ✅ SDAPI via [stable-diffusion.cpp's server](https://github.com/leejet/stable-diffusion.cpp/tree/master/examples/server)
  - `/sdapi/v1/txt2img`
  - `/sdapi/v1/img2img`
  - `/sdapi/v1/loras` - requires `model` in request body to fetch the correct loras
- ✅ [audio.cpp](https://github.com/0xShug0/audio.cpp) supported [extra endpoints](https://github.com/0xShug0/audio.cpp/blob/main/app/server/README.md#post-v1tasksrun)
  - `/audioapi/v1/tasks/run`
- ✅ `/comfyui/` - ComfyUI custom endpoint ([#1001](https://github.com/mostlygeek/llama-swap/issues/1001)) for more reliable swapping
- ✅ llama-swap API
  - `/ui` - web UI
  - `/upstream/:model_id` - direct access to upstream server ([demo](https://github.com/mostlygeek/llama-swap/pull/31))  
  - `/running` - list currently running models ([#61](https://github.com/mostlygeek/llama-swap/issues/61))
  - `POST /api/models/unload` - manually unload all running models ([#58](https://github.com/mostlygeek/llama-swap/issues/58))
  - `POST /api/models/unload/:model_id` - unload a specific model
  - `GET /api/profiles` - list configured profiles and the active selection
  - `PUT /api/profiles/active` - activate a profile or select none
  - `/logs` - remote log monitoring
    - `GET /logs` returns buffered plain text logs.
      - If `Accept: text/html` is sent, `/logs` redirects to `/ui/`.
    - `GET /logs/stream` keeps the connection open for live log streaming.
      - Stream endpoints send buffered history first by default; add `?no-history` to stream only new lines.
    - `GET /logs/stream/proxy` streams proxy logs only.
    - `GET /logs/stream/upstream` streams upstream process logs only.
    - `GET /logs/stream/{model_id}` streams logs for one model (including IDs with slashes, like `author/model`).
  - `/health` - just returns "OK"
  - `/metrics` - system and GPU metrics for prometheus
- ✅ API Key support - define keys to restrict access to API endpoints
- ✅ Customization
  - Switch model ID routing at runtime with profiles
  - Run concurrent models with a custom DSL swap matrix ([#643](https://github.com/mostlygeek/llama-swap/issues/643))
  - Automatic unloading of models after timeout by setting a `ttl`
  - Docker and Podman support using `cmd` and `cmdStop` together
  - Preload models on startup with `hooks` ([#235](https://github.com/mostlygeek/llama-swap/pull/235))
  - Apply filters to requests to control inference with `stripParams`, `setParams` and `setParamsByID`

### Web UI

The management UI covers models and load state, a playground, activity with
per-request speed telemetry, usage analytics, audit transcripts, managed
runtimes, extension workbench (editor + debug chat), API keys, settings and
real-time log streaming. Start the server and open `/ui/` to explore it.

## Installation

This fork does not publish package-manager taps or nightly images. Build it
from source, or use the upstream binaries — the core swap engine is
upstream's.

### Building from source (recommended for this fork)

1. Building requires Go 1.26+ and Node.js (for the UI).
2. `git clone https://github.com/skyfsj/llama-swap-extended.git`
3. `make mac` / `make linux` / `make windows`
4. look in the `build/` subdirectory for the binary

### Running as a container

Containerfiles live under [`docker/`](docker/). Build locally:

```shell
# llama-swap only (based on llama.cpp's images)
docker build -f docker/llama-swap.Containerfile -t llama-swap-extended .

# unified image with llama-server, stable-diffusion.cpp and whisper.cpp built in
docker build -f docker/unified/Dockerfile -t llama-swap-extended:unified .

# run with a custom configuration and models directory
docker run -it --rm --runtime nvidia -p 9292:8080 \
 -v /path/to/models:/models \
 -v /path/to/custom/config.yaml:/app/config.yaml \
 llama-swap-extended:unified
```

### Upstream distribution channels

Homebrew, MacPorts, WinGet and the pre-built
[upstream releases](https://github.com/mostlygeek/llama-swap/releases) track
upstream only and do not include this fork's changes.

## Configuration

```yaml
# minimum viable config.yaml

models:
  model1:
    cmd: llama-server --port ${PORT} --model /path/to/model.gguf
```

That's all you need to get started:

1. `models` - holds all model configurations
2. `model1` - the ID used in API calls
3. `cmd` - the command to run to start the server.
4. `${PORT}` - an automatically assigned port number

Almost all configuration settings are optional and can be added one step at a time:

- Advanced features
  - `matrix` to run concurrent models with a custom swap logic DSL
  - `gpus` to run concurrent models based on which GPU card each model occupies
  - `hooks` to run things on startup
  - `macros` reusable snippets
- Model customization
  - `ttl` to automatically unload models
  - `unloadTimeout` to tune graceful unloads (manual, API and `ttl` expiry)
  - `aliases` to use familiar model names (e.g., "gpt-4o-mini")
  - `env` to pass custom environment variables to inference servers
  - `cmdStop` gracefully stop Docker/Podman containers
  - `useModelName` to override model names sent to upstream servers
  - `${PORT}` automatic port variables for dynamic port assignment
  - `filters` rewrite parts of requests before sending to the upstream server

See the [configuration documentation](docs/configuration.md) for all options.

## How does llama-swap work?

When a request is made to an OpenAI compatible endpoint, llama-swap will extract the `model` value and load the appropriate server configuration to serve it. If the wrong upstream server is running, it will be replaced with the correct one. This is where the "swap" part comes in. The upstream server is automatically swapped to handle the request correctly.

In the most basic configuration llama-swap handles one model at a time. For more advanced use cases, using a `matrix` allows multiple models to be loaded at the same time. You have complete control over how your system resources are used.

## Reverse Proxy Configuration (nginx)

If you deploy llama-swap behind nginx, disable response buffering for streaming endpoints. By default, nginx buffers responses which breaks Server‑Sent Events (SSE) and streaming chat completion. ([#236](https://github.com/mostlygeek/llama-swap/issues/236))

Recommended nginx configuration snippets:

```nginx
# SSE for UI events/logs
location /api/events {
    proxy_pass http://your-llama-swap-backend;
    proxy_buffering off;
    proxy_cache off;
}

# Streaming chat completions (stream=true)
location /v1/chat/completions {
    proxy_pass http://your-llama-swap-backend;
    proxy_buffering off;
    proxy_cache off;
}
```

As a safeguard, llama-swap also sets `X-Accel-Buffering: no` on SSE responses. However, explicitly disabling `proxy_buffering` at your reverse proxy is still recommended for reliable streaming behavior.

## Monitoring Logs on the CLI

```sh
# sends up to the last 10KB of logs
$ curl http://host/logs

# streams combined logs
curl -Ns http://host/logs/stream

# stream llama-swap's proxy status logs
curl -Ns http://host/logs/stream/proxy

# stream logs from upstream processes that llama-swap loads
curl -Ns http://host/logs/stream/upstream

# stream logs only from a specific model
curl -Ns http://host/logs/stream/{model_id}

# stream and filter logs with linux pipes
curl -Ns http://host/logs/stream | grep 'eval time'

# appending ?no-history will disable sending buffered history first
curl -Ns 'http://host/logs/stream?no-history'
```

## Do I need to use llama.cpp's server (llama-server)?

Any OpenAI compatible server would work. llama-swap was originally designed for llama-server and it is the best supported.

For Python based inference servers like vllm or tabbyAPI it is recommended to run them via podman or docker. This provides clean environment isolation as well as responding correctly to `SIGTERM` signals for proper shutdown.
