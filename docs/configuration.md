# config.yaml

llama-swap is designed to be very simple: one binary, one configuration file.

> API keys are split into management keys (control panel) and access keys
> (model traffic), each with their own restrictions. See
> [split-api-keys.md](split-api-keys.md) for the key model, the access key
> restrictions and the usage records page.

## minimal viable config

```yaml
models:
  model1:
    cmd: llama-server --port ${PORT} --model /path/to/model.gguf
```

This is enough to launch `llama-server` to serve `model1`. Of course, llama-swap is about making it possible to serve many models:

```yaml
models:
  model1:
    cmd: llama-server --port ${PORT} -m /path/to/model.gguf
  model2:
    cmd: llama-server --port ${PORT} -m /path/to/another_model.gguf
  model3:
    cmd: llama-server --port ${PORT} -m /path/to/third_model.gguf
```

With this configuration models will be hot swapped and loaded on demand. The special `${PORT}` macro provides a unique port per model which is useful if you want to run multiple models at the same time with the `matrix` feature.

## Incident log storage

llama-swap automatically writes a diagnostic snapshot when an inference
process exits unexpectedly and when an HTTP request finishes with an error
status. Snapshots are written atomically and contain the process-output tail,
or the request metadata plus the model it resolved to and the error body the
client was shown; request bodies are not persisted. By default, each incident
kind keeps the newest five snapshots.

```yaml
logStorage:
  # Empty uses ./logs, or logs beside store.path when a file-backed store is set.
  path: /var/lib/llama-swap/logs
  maxFiles: 5
```

A request-error snapshot records the same access-log fields as before (client,
method and path, status, size, user agent, duration) and adds:

- `model:` the configured model ID the request resolved to, when one could be
  resolved. This is what makes a 503 attributable: `/v1/chat/completions`
  carries the model in the JSON body, so the path alone does not say which
  model failed.
- a `response:` section holding the error body the client received, bounded to
  8 KiB and marked `… truncated …` when a longer body was cut. It is copied
  from the response as it is written, so streaming responses contribute a
  prefix rather than being re-read.

Both are omitted when empty, so requests that never resolved a model (a 404
for an unknown name) simply have no `model:` line.

`maxFiles` accepts values from 1 to 100 and is applied independently to
inference crashes and request errors. The Logs page lists the retained files;
opening an item reads the raw snapshot for download or inspection.

## Model file catalog

The control UI groups model files by repository/project and can scan configured directories, individual files, Hugging Face Hub caches, and ModelScope caches. Configure absolute paths under `modelFiles.sources`; cache sources without a path use their standard environment or platform cache location.

```yaml
modelFiles:
  maxFiles: 2000
  maxDepth: 8
  sources:
    local:
      type: directory
      path: /srv/models
      recursive: true
    huggingface:
      type: hf_cache
    modelscope:
      type: modelscope_cache
```

The page reports which files are registered in `models` and which registered models are currently running. Deletion requires an explicit confirmation and is rejected while a file is registered or in use. For a Hugging Face snapshot symlink, deletion removes only the snapshot link; its cache blob is not followed or removed.

The same page can enqueue a Hugging Face or ModelScope repository download. Downloads are
stored in SQLite, run through a FIFO worker queue, retry transient HTTP/network
failures with exponential backoff, and keep `<file>.part` files for HTTP Range
resume after a restart. The UI lets you choose a configured directory or the
provider-compatible cache and shows the resolved storage path. HF destinations
use the normal `models--.../blobs`, `snapshots`, and `refs` layout; ModelScope
destinations use `models/<namespace>--<repository>/snapshots`.

Completed, failed, and canceled queue records can be deleted from the download
panel. Deleting a queue record does not remove model files, cache entries, or
resumable `.part` files; those remain available for a later download or for
separate model-file deletion.

```yaml
modelFiles:
  downloads:
    enabled: true
    workers: 1
    fileWorkers: 4
    chunkWorkers: 4
    chunkSizeMiB: 16
    chunkThresholdMiB: 64
    maxRetries: 5
    maxTaskRetries: 3
    retryBackoff: 2s
    hfBaseURL: https://huggingface.co
    hfTokenEnv: HF_TOKEN
    # Optional; the Model files page can save this value in the config source.
    # hfToken: hf_example_token
    modelScopeBaseURL: https://modelscope.cn
    modelScopeTokenEnv: MODELSCOPE_API_TOKEN
    # Optional; the Model files page can save this value in the config source.
    # modelScopeToken: ms_example_token
```

Use the key icon beside “Download model” to configure either provider without
editing YAML. The control API reports only whether a key is present; it never
returns the key itself. A saved key is stored in the protected configuration
source and takes precedence over `hfTokenEnv` or `modelScopeTokenEnv`; clearing
it re-enables the environment fallback. Download tasks contain no credential
material. Transient network and provider failures automatically retry after
their request-level retry budget is exhausted; the task remains cancelable and
shows its next retry time. Canceling a task keeps partial files; retrying it
resumes them when the server supports Range. `workers` limits concurrent
repository tasks, while `fileWorkers` limits the files fetched concurrently
inside each repository task. For a large known-size file, `chunkWorkers` issues
parallel Range requests, `chunkSizeMiB` sets each segment size, and
`chunkThresholdMiB` controls when chunking begins. A sidecar state file keeps
completed chunks resumable; servers that ignore Range transparently use the
ordinary sequential downloader.

## Managed vLLM and llama.cpp backends

Existing models can continue to use `cmd`, `cmdStop`, and `proxy`. A model may
instead declare a backend and structured argv; arguments are executed directly
and are never passed through a shell.

The Runtime Manager is always active. The unified and legacy Docker example
configurations set its persistent root and register the bundled `llamacpp`
runtime automatically. The image asset is
seeded into the persistent runtime root only when no `current` pointer exists;
an image upgrade never replaces an operator-selected version. Custom images can
point discovery at another owned directory with
`LLAMA_SWAP_BUNDLED_LLAMA_CPP_PATH`, or declare an explicit `runtimes` entry to
take precedence over discovery.

```yaml
runtimeManager:
  root: /var/lib/llama-swap/runtimes
  sourceAllowlist:
    - https://pypi.org
    - https://files.pythonhosted.org
    - https://github.com
    - https://api.github.com
    - https://codeload.github.com
    - https://release-assets.githubusercontent.com
  # Container images are pulled only from these registries.
  containerRegistries: [docker.io, ghcr.io]
runtimes:
  vllm-cuda:
    kind: vllm
    source: {type: pypi}
    update: {version: "0.10.0", policy: automatic, channel: stable, minIdle: 30m}
  llama-from-git:
    kind: llamacpp
    source:
      type: git
      repository: https://github.com/ggml-org/llama.cpp.git
      ref: b5999
    update: {version: b5999, policy: automatic}
    build:
      driver: make
      args: [-j4]
      compiler: clang
      env: {GGML_CUDA: "1"}
      artifacts:
        - {from: build/bin/llama-server, to: bin/llama-server}
  # A source fork can be selected without changing llama-swap itself. For
  # example, 1Cat-vLLM can track a branch and build from its managed checkout.
  vllm-1cat:
    kind: vllm
    source:
      type: git
      repository: https://github.com/1CatAI/1Cat-vLLM.git
      trackRef: refs/heads/main
    update:
      policy: automatic
      checkEvery: 24h
      minIdle: 30m
      keepVersions: 2
      rollbackOnFailure: true
    build:
      driver: custom
      python: "3.12"
      env:
        CUDA_HOME: /usr/local/cuda-12.8
        TORCH_CUDA_ARCH_LIST: "7.0;8.0"
        FLASH_ATTN_V100_CUDA_ARCH_LIST: "7.0"
        MAX_JOBS: "12"
        NVCC_THREADS: "1"
      steps:
        - {workDir: "${SOURCE_DIR}", command: "${UV}", args: [pip, install, --python, "${PYTHON}", -r, requirements/build/cuda.txt]}
        - {workDir: "${SOURCE_DIR}", command: "${UV}", args: [pip, install, --python, "${PYTHON}", -r, requirements/cuda.txt]}
        - {workDir: "${SOURCE_DIR}", command: "${UV}", args: [pip, install, --python, "${PYTHON}", -r, requirements/common.txt]}
        - {workDir: "${SOURCE_DIR}", command: "${UV}", args: [pip, install, --python, "${PYTHON}", cmake, build]}
        - {workDir: "${SOURCE_DIR}", command: "${UV}", args: [pip, install, --python, "${PYTHON}", --no-build-isolation, --editable, .]}
  vllm-container:
    kind: vllm
    mode: container
    source:
      type: image
      image: ghcr.io/example/vllm:stable
      pullPolicy: always
    container:
      engine: docker
      name: llama-swap-vllm
      gpus: all
      shmSize: 16g
      mounts:
        - {source: /srv/models, target: /models, readOnly: true}
      command: [serve, /models/Qwen3, --port, "8000"]
models:
  qwen:
    proxy: http://127.0.0.1:${PORT}
    backend:
      type: vllm
      runtime: vllm-cuda
      protocol: responsesToChat
      args: [/models/qwen, --enable-sleep-mode]
```

Models refer to a managed installation by `backend.runtime` and list the
engine's own arguments in `backend.args`. The entrypoint is owned by
llama-swap, so a logical name such as `vllm` or `llama-server` is not
required; if one is written it is ignored at launch. The one mandatory part
is a model target: `--model`, `--model-path`, or the positional model path.
Host and port come from the model's `proxy` (so `${PORT}` keeps working
without being written in `args`), and operator-written `--host`/`--port`
duplicates are dropped. Arguments may be one token per list item, or one flag
per line, for example `- --gpu-memory-utilization 0.90`; both forms normalize
to the same launch. At process start, llama-swap resolves the runtime's
`current` pointer (or an explicit activation candidate), replaces the
entrypoint with the immutable version path, and injects its executable/library
directories into the child environment. Do not duplicate this binding with a
hard-coded runtime path or a second `${RUNTIME_*}` convention in legacy `cmd`.

Inference backends receive OpenAI Chat Completions requests. Llama-swap accepts
OpenAI Chat, OpenAI Responses, Anthropic Messages, and Gemini `generateContent`
clients, converts the latter three formats to Chat Completions, and converts
responses back. Gemini clients use
`/v1beta/models/{model}:generateContent` or
`/v1beta/models/{model}:streamGenerateContent` and may authenticate with
`x-goog-api-key`.

`backend.protocol` continues to accept the legacy values `native` and
`responsesToChat` for configuration compatibility. Neither selects a native
Responses, Anthropic, or Gemini backend: those client formats are always
normalized to Chat Completions. Token-count, embedding, audio, image, and
Realtime endpoints are not part of text-format conversion. Search and other
hosted tools remain the responsibility of Extensions.

For an explicit `backend.type: vllm` model, llama-swap sends a best-effort
non-streaming `POST /v1/chat/completions` warmup message after the readiness
check and before publishing the process as ready. The warmup tries each
configured name in order — `useModelName`, then the configured `aliases`, then
`backend.launch.servedModelName`, then the llama-swap model ID — and stops at
the first one the engine accepts. A name mismatch is reported as a logged
warmup error (the start itself does not fail) so a misconfigured served name
is visible at start time instead of only at the first aliased request. The
warmup runs once per process start, not on every `EnsureReady` call. While this start is in
flight, requests for the same model remain attached to the scheduler's active
swap waiters and are granted only after `EnsureReady` returns. Cancelling one
client request removes only that waiter; it does not cancel the shared model
start. An explicit unload or router shutdown aborts the start, releases its
waiters with an error, and leaves the process stopped so a later request can
start a fresh generation.

Managed runtimes use immutable version directories and `current`/`previous`
atomic pointers. Updates can be staged and checked while a backend is busy;
activation waits for idle and rolls back after a failed health check.

`backend.args` is the single source of truth for a managed model's command
line: llama-swap stores exactly the engine flags you write and injects only
the runtime-owned entrypoint plus the bind host/port from the model's proxy.

```yaml
models:
  qwen:
    proxy: http://127.0.0.1:${PORT}
    backend:
      type: vllm
      runtime: vllm-cuda
      args:
        - --model /models/Qwen3.8-27B-FP8
        - --served-model-name Qwen/Qwen3.8-27B-FP8
        - --max-model-len 262144
        - --max-num-seqs 2
        - --tensor-parallel-size 4
        - --gpu-memory-utilization 0.90
        - --trust-remote-code
        - --enable-prefix-caching
```

The model configuration UI presents the common fields (model target, served
name, context per request, max concurrency, tensor parallelism, GPU memory
utilization) as a *derived view*: they are read from the command text and
written back into it when edited, so a value is never stored twice. Selecting
more than one GPU writes `CUDA_VISIBLE_DEVICES` into the model environment and
materializes `--tensor-parallel-size` (one shard per GPU) unless you pinned
one yourself.

A `backend.launch` block from an older configuration is folded into
`backend.args` when the configuration loads — the flags are rendered exactly
as the launcher would have injected them, and the block is dropped. Ambiguous
legacy text is preserved verbatim. The whole command remains purely lexical
data — llama-swap never runs pasted commands through a shell.

### Runtime launch metadata

A runtime definition carries the launch metadata its models inherit: the launch
head and the quick-config defaults. The head is what a runtime that fronts a
custom server declares once — a model that must run its own server under the
runtime's environment inherits it. It runs verbatim (with the runtime's `PATH`
and libraries) followed by the model's args, and takes over the entire command
line: no engine flags, host or port are injected, so write `${PORT}` yourself.

```yaml
runtimes:
  qwen-asr:
    kind: vllm
    update: {policy: disabled}
    metadata:
      entrypoint: python3 /var/lib/llama-swap/scripts/qwen-asr-server.py

models:
  asr:
    proxy: http://127.0.0.1:${PORT}
    backend:
      type: vllm
      runtime: qwen-asr
      args:
        - --port ${PORT}
        - --model /models/Qwen/Qwen3-ASR-1.7B
```

The same block carries the quick-config defaults every model on the runtime
inherits unless it pins the flag itself:

```yaml
runtimes:
  vllm-cuda:
    kind: vllm
    metadata:
      contextPerRequest: 262144
      maxConcurrency: 2
      tensorParallelSize: 4
      gpuMemoryUtilization: 0.9
```

A model overrides a default by writing the corresponding flag in its own
`backend.args` (`--max-model-len`, `--max-num-seqs`, `--tensor-parallel-size`,
`--gpu-memory-utilization`). Defaults are never rendered when a custom launch
head is in effect — a custom server owns its whole command line. A model-level
`backend.entrypoint` still wins over the runtime's for one-off cases.

The model still gets the managed lifecycle: health checks, TTL, swapping and
runtime version pinning all apply.

For a vLLM model with `ttl > 0`, llama-swap automatically appends
`--enable-sleep-mode` to the private launch arguments when it is not already
present. This flag is not added when `ttl: 0`, and an explicitly configured
flag is preserved without duplication. When the TTL expires, the default vLLM
lifecycle releases the model weights through `/sleep` while keeping the
listener alive; the next request wakes it before proxying traffic. Set
`backend.lifecycle.mode: process` to retain the traditional behavior of
stopping the process at TTL expiry. Sleep and wake are also available from
the Backends page and `POST /api/backends/{model}/sleep` or `/wake`.

Runtime definitions, source allowlists, container registries, build arguments,
and update policies are replaced in place after a successful configuration
reload. Removing a runtime from YAML stops its background update checks but
does not delete its installed versions or atomic pointers. If another module
rejects the configuration candidate, llama-swap restores the previous runtime
definitions together with the router configuration.

`runtimeManager.root` is the installation root. When omitted, llama-swap uses
the operating system's per-user cache directory under `llama-swap/runtimes`;
container deployments should set it to a mounted persistent volume. Each runtime lives below
`<root>/<name>/versions/<version>`; `current` and `previous` are atomic
pointers. `source.repository` plus `source.ref` selects an exact Git checkout
and never advances by itself. Replacing `ref` with `trackRef` explicitly opts
into branch/tag tracking: the automatic checker resolves it with metadata-only
`git ls-remote`, records the immutable commit as `git-<commit>`, and performs
the actual clone/build only after the idle gate. For llama.cpp,
`build.driver: make` runs `make` in the checked-out tree and
`build.driver: cmake` runs a native `cmake -S/-B` configure plus build with
`GGML_NATIVE=OFF` and `$ORIGIN` runtime paths (so the staged binary keeps
running after the staging directory is renamed into place);
`build.driver: none` expects a prebuilt source layout — the provider copies
the entrypoint and its shared libraries straight from the checked/downloaded
archive into `bin/` without compiling, which is what a GitHub Releases
`source.type: release` with a versioned top-level directory (for example
`llama-b10785/`) needs; `build.steps` provides
an ordered shell-free custom argv sequence, `build.env` supplies build-only
environment variables, and `build.artifacts` copies verified outputs into the
immutable version directory. For vLLM, `source.type: git` accepts any
allowlisted fork (including 1Cat-vLLM), while `source.type: wheel` plus a
HTTPS URL or local path can be used for a pre-built wheel. `source.type: local`
also accepts a local source directory (it is copied into the staged version
before installation). With `build.driver: uv`, `build.installArgs` is inserted
into the `uv pip install` argv before the package specification and any
`build.steps` run afterwards. With `build.driver: custom`, a Git source is
checked out first at `${SOURCE_DIR}` and the ordered steps are the complete
install procedure; `${RUNTIME_DIR}`, `${SOURCE_DIR}`, `${BUILD_DIR}`,
`${PYTHON}`, and `${UV}` expand as individual argv values without a shell.

The `vllm-1cat` example follows the repository's current editable source-build
path for operators changing CUDA/C++/Triton code. Upstream currently validates
Ubuntu 24.04, Python 3.12, CUDA 12.8, and Tesla V100/SM70, and recommends its
[release wheels](https://github.com/1CatAI/1Cat-vLLM/releases/latest) for normal
deployment. To use a wheel instead, set `source.type: wheel`, put the exact
release asset URL in `source.url`, keep an explicit `update.version`, and pass
the upstream PyTorch CUDA index through `build.installArgs` when required. If
`runtimeManager.sourceAllowlist` is set, both `https://github.com` and the
release redirect host `https://release-assets.githubusercontent.com` must be
listed; every artifact redirect is revalidated against the allowlist.

`update.policy: automatic` enables the background checker. `manual`,
`disabled`, and `pinned` definitions do not advance automatically. The Pin
action in the Runtime Manager UI/API is a stronger per-version operator lock:
while any version is pinned, the automatic loop does not check, download,
build, or activate a replacement. `keepVersions` is the retention target after
a successful activation; `current`, `previous`, and the pinned version are
always protected, and older unprotected versions are deleted first. Manual
activation and rollback remain available while a runtime is pinned.

Version switching is driven by `update.version` (plus `source.ref` for Git):
changing it to a version that is already installed stages without
re-downloading or rebuilding and then activates the existing directory, so a
switch back and forth is a pointer operation; changing it to a new version
first stages the full download/build/verify cycle and only activates once the
new directory passes its checks. Both old and new versions stay on disk until
`keepVersions` prunes them.

For a first native installation, an exact Git `ref`, PyPI source, or
release/channel source does not invent an initial version: set
`update.version` or submit an explicit Stage request. Once a PyPI or
release/channel runtime has an active version, remove `update.version` to track
the selected channel automatically. A Git `trackRef` and a mutable container
tag can derive their first immutable commit/digest without this bootstrap
field. This separation prevents an unversioned declaration from silently
choosing initial native code while still allowing explicit automatic tracking.

With `mode: container`, llama-swap checks a mutable tag with metadata-only
`docker/podman manifest inspect`, then pulls and records the immutable OCI
digest using Docker or Podman after the update gate. It verifies that digest
on every check/activation. Use `pullPolicy: always` for automatic tag updates;
`if-missing` deliberately reuses a local image and `never` forbids pulling.
`container` supplies typed `run` settings (engine, GPU, mounts, ports,
environment and command); no arbitrary shell string is accepted. The selected
Docker/Podman CLI and its daemon/socket access must be available to the
llama-swap process.
For the runtime API, the image may be supplied as `source.image` or
`container.image`; `repository`/`trackRef` and the equivalent `build.steps`,
`build.env`, and `build.installArgs` fields are accepted by the stage request as
well.

An optional `verify.smokeCommand` runs after the provider's filesystem checks
during activation/check. It uses a small quoted-argv parser and executes
directly without a shell; non-zero exit status or malformed quoting fails the
health check. `verify.healthPath` is retained as a validated absolute HTTP
path for backend-specific probes and is never treated as an arbitrary URL.

vLLM's batched Chat Completions endpoint (`POST /v1/chat/completions/batch`) is
also registered. Its outer `model` field is optional at the upstream protocol
level; llama-swap infers it only when exactly one local model is configured and
returns an explicit routing error when the choice would be ambiguous.

## Outbound network proxy

Everything the daemon sends to the internet — model downloads, pricing
refreshes, runtime metadata, extension `ctx.http.fetch`, provider-gateway
fallbacks, and the package managers runtime builds run (`pip`, `uv`, `npm`,
`git`) — follows one policy:

```yaml
network:
  proxy: http://proxy.example.com:3128   # or https://, socks5://, socks5h://
  noProxy:
    - localhost
    - .corp.example.com
```

`proxy` accepts `http`, `https`, `socks5` and `socks5h` URLs (with optional
`user:password`). `direct` (or `none`) forces direct connections even when the
process environment defines a proxy. Leaving it empty keeps the historical
behavior: whichever `HTTP_PROXY` / `HTTPS_PROXY` / `ALL_PROXY` / `NO_PROXY`
variables the daemon was started with.

`noProxy` entries are matched the way `curl --noproxy` does: an exact host, a
bare domain, or a leading-dot suffix all match that domain and its
subdomains. A single `"*"` disables proxying entirely.

Once a proxy is configured, traffic that a proxy cannot help with is always
sent directly: loopback addresses (`127.0.0.1`, `::1`, `localhost`, and the
model backends behind them), private, link-local and unique-local addresses
(the LAN and the model containers), and single-label host names such as a
container name. Only the public internet goes through the proxy, so local
routing, peers and model traffic keep working exactly as before.

The policy is applied when the configuration is loaded and again on every
reload — clients that build their own HTTP transport follow the process
default transport, which resolves the policy per request, so a change takes
effect without a restart. A per-credential `proxy` on a provider still
overrides it for that one upstream. Credentials in the URL are redacted from
logs.

Models launched as host commands inherit the same policy. A model that runs
inside a container is different: its environment comes from `container.env`,
so a proxy that must be visible inside needs an explicit entry there. Note that
a proxy listening on the host's loopback is `127.0.0.1` only to the host — from
inside a container that address is the container itself, so use
`host.docker.internal` or host networking.

## Caller address and reverse proxies

The address llama-swap treats as "the caller" decides two things: which
address the per-key `allowedIps` allowlist is checked against, and what the
access log and audit rows record.

By default that address is the **TCP peer** — the connection's own address.
`X-Forwarded-For` and `X-Real-IP` are ignored, because a direct client can put
any value in those headers and would otherwise be able to pick an address that
passes an allowlist.

When llama-swap sits behind a reverse proxy you control, opt in:

```yaml
network:
  trustForwardedFor: true
  trustedProxies:
    - 127.0.0.1/32        # nginx / caddy on the same host
    - 10.0.0.0/8          # an internal load balancer
```

With `trustForwardedFor: true` the caller is read from `X-Forwarded-For`
(falling back to `X-Real-IP`).

When `trustedProxies` is set, the chain is walked right to left and trusted
hops are skipped, so the first untrusted address is reported; a request whose
peer is not in the trusted list ignores its headers entirely.

When `trustedProxies` is empty, **only the right-most hop is used** — the
address the directly connected proxy wrote. That is the safe single-switch
form: reverse proxies append by default (nginx's `$proxy_add_x_forwarded_for`,
Caddy), so a client that pre-seeds `X-Forwarded-For` only affects entries to
the left of the appended one. If your proxy overwrites the header instead of
appending, the right-most and only hop is the client address, which is still
correct.

If the appended (right-most) segment is not a valid address — some proxies
write `unknown` — llama-swap ignores the whole `X-Forwarded-For` value and uses
the connection address. It never falls back to an entry further left, because
everything to the left may have been supplied by the client.

`X-Real-IP` is consulted only when `X-Forwarded-For` yields no usable address.
A proxy must therefore **overwrite** `X-Real-IP` with the address it observed,
not pass a client-supplied value through.

`trustedProxies` requires `trustForwardedFor: true`; configuring the list alone
is rejected at load time rather than silently doing nothing.

## HTTP server, response headers and failed-login throttling

```yaml
server:
  maxHeaderBytes: 65536   # request line + headers, 4 KiB - 1 MiB
  readTimeout: 0s         # 0 disables it (large payloads on slow links)
  securityHeaders: true   # X-Content-Type-Options, X-Frame-Options, CSP, ...
```

`maxHeaderBytes` defaults to 64 KiB, well below Go's 1 MiB default. A gateway
never needs a megabyte of headers, and the cap stops one request from pinning
that much memory before routing starts.

`securityHeaders` adds `X-Content-Type-Options: nosniff`,
`X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`,
`Permissions-Policy`, and — for the embedded UI — a
`Content-Security-Policy` that denies framing, plugins and cross-origin
requests. The `/upstream` and `/comfyui` passthroughs are excluded because
those responses belong to the proxied application.

`readTimeout` is off by default: it bounds reading one complete request
(headers and body), so a very large chat payload on a slow link would be cut
off. Enable it when your clients are known to be fast.

Failed credential checks are throttled per caller address:

```yaml
security:
  authRateLimit:
    disabled: false
    maxFailures: 10   # inside one window
    window: 1m
    blockFor: 5m
```

A failed check is counted when the authentication layer rejects the presented
credential. A valid credential that merely hits a scope or policy boundary
keeps its normal `403` and is not penalised, and a `401` produced further
downstream (a provider whose own key expired, a backend protected by
`--api-key`) is never charged to the caller. Any accepted credential clears
the counter, so mistyping a key a few times does not lock anyone out. A blocked
caller receives `429` with a `Retry-After` header. The defaults are chosen so
an operator who mistypes a key is never locked out while online guessing of
weak YAML keys stays expensive.

Both session endpoints participate: `POST /api/auth/session` submits a
credential, and `GET /api/auth/session` reports whether the presented one is
valid. The login screen's anonymous poll carries no credential and is never
counted.

### Cross-origin requests (CORS)

`security.cors` controls the `Access-Control-*` headers sent to browsers:

```yaml
security:
  cors:
    allowedOrigins:
      - https://dashboard.example.com
    allowCredentials: true
    maxAge: 86400
```

Leaving `allowedOrigins` out keeps the permissive default llama-swap has always
had: any origin may read responses. Setting it restricts that to the listed
origins, each written as a bare `scheme://host[:port]` (a path or trailing
slash never matches, so it is rejected at load time). `"*"` allows any origin
deliberately; writing an empty list is an error rather than a silent
allow-all. `allowCredentials` (cookies and `Authorization` across origins) and
`allowPrivateNetwork` (Chrome's Private Network Access preflight) cannot be
combined with `"*"`.

Headers are only sent when the request carries an `Origin`, so `curl` and most
SDKs see no CORS headers at all. Upstream CORS headers are stripped from
proxied responses, keeping llama-swap the only source and preventing the
duplicate-value fold that strict clients reject.

`allowedOrigins` decides who may **read** a response. State-changing
cross-origin requests to the management API (`/api/`) are rejected regardless:
a browser page on another origin has no legitimate reason to drive the control
plane with an ambient session cookie.

### Global concurrency limit

`globalConcurrencyLimit` caps how many inference requests are served at once
across every model combined, using one shared semaphore:

```yaml
globalConcurrencyLimit: 8
```

The default `0` means no limit, and the middleware is then left out of the
request chain entirely. Once the limit is reached, further requests are
rejected immediately with `429 Too Many Requests` and `Retry-After: 1` rather
than queued. Unlike `models.*.concurrencyLimit`, which only counts requests to
one model, this protects shared hardware from traffic spread across many
models.

## LMCache (optional KV-cache accelerator)

LMCache is an optional module that accelerates repeated vLLM prompts with a
shared KV cache. It is **not installed by default**: the Runtime Manager page
shows an empty module card until the operator enables it. Enabling stages the
dedicated, version-managed server runtime in its own virtualenv — it does not
require any vLLM runtime and never touches one. The per-model connector is
installed into the model's own vLLM runtime automatically at model start,
pinned to the server's current version. `POST /api/lmcache/enable` and
`POST /api/lmcache/disable` perform the install and uninstall, and
`GET /api/lmcache` reports the full module status.

### Isolation and lifecycle

The standalone LMCache server runs in its own **version-managed virtualenv**
(`<runtimeManager.root>/lmcache/versions/<version>/.venv`) managed by the
Runtime Manager — it never runs from a vLLM venv, and the two dependency trees
are independent. Each vLLM runtime keeps its own venv; the connector package
inside it is pinned to the server's current version at model start. Re-staging
a vLLM runtime rebuilds its venv and removes the connector, which is
reinstalled the next time a model on it starts.

The server is supervised with an explicit seven-state lifecycle:
`NOT_INSTALLED`, `STOPPED`, `STARTING`, `RUNNING`, `STOPPING`, `ERROR`,
`UPDATING`. `RUNNING` requires the process to be alive **and** the management
frontend's `/healthcheck` to answer healthy; a bare PID never counts as
serving. Crashes and health failures degrade to `ERROR` with the cause
recorded. llama-swap sends SIGTERM, then SIGKILL after a grace period, and
logs the server to `<runtimeManager.root>/lmcache/server.log` (distinct from
per-model logs).

Model startup is **fail-closed**: before an LMCache-enabled model spawns,
llama-swap waits for the server to be `RUNNING` and healthy (starting it if
needed; concurrent model starts share one in-flight start) and ensures the
model's vLLM venv contains the connector pinned to the server's current
version — installing it if missing or drifted (concurrent starts against the
same venv share one in-flight install). If the server cannot become healthy or
the connector cannot be installed, the model start fails with the LMCache
error, the log path, and the most recent log lines — there is no fallback to
an LMCache-less start. A first model start on a freshly staged vLLM runtime
therefore blocks on the connector install (roughly one to three minutes on a
cold package cache).

Because running models hold in-memory KV cache in the server, llama-swap
tracks references per running model and **refuses** every operation that would
drop that cache while any reference is held: stop, restart, disable, and all
version update steps return HTTP 409 with the list of models still using the
server (UI buttons are disabled for the same reason; the API is authoritative).
When the last referencing model stops, the server stays up by default
(`autoStop: false`) so the cache remains warm.

### Configuration

```yaml
lmcache:
  # pip requirement name; exact version pin and extras allowed.
  package: lmcache
  # Legacy compatibility alias. Prefer update.version below; if both are
  # present they must match exactly and conflicts reject the configuration.
  # version: 0.5.4
  # interpreter for the server virtualenv; defaults to 3.11.
  # pythonVersion: "3.11"
  # optional HTTPS package index mirror, still subject to sourceAllowlist.
  # indexURL: https://pypi.org
  # Enable the module: install and supervise the dedicated server runtime.
  enabled: true
  # start the standalone server at daemon boot; default true. false defers
  # the first start to a model's pre-start gate.
  # autoStart: true
  # stop the server when the last referencing model stops; default false
  # keeps the KV cache warm across model turnover.
  # autoStop: false
  server:
    enabled: true
    # ZMQ endpoint vLLM engines attach to.
    host: localhost
    port: 5555
    # Management frontend. Defaults away from llama-swap's own 8080.
    httpHost: 127.0.0.1
    httpPort: 8900
    # Pinned-DRAM pool ("L2", system memory) size. Overrides the legacy
    # l1SizeGB when enabled. KB/MB/GB are powers of 1000, KiB/MiB/GiB are
    # powers of 1024; a bare number is bytes. Restart-required. This is the
    # server's ONLY memory tier: LMCache renders it as --l1-size-gb.
    # l2:
    #   enabled: true
    #   maxBytes: 30GB
    # Legacy pinned-DRAM size in GB; superseded by l2.maxBytes. Still
    # accepted, but new configurations should set l2.maxBytes only.
    # l1SizeGB: 20
    # Disk tier ("L3"): absolute directory, auto-created. The path must be
    # writable with enough free space, or the server start fails closed.
    # maxBytes only sets the minimum free space (default 1 GiB); the LMCache
    # fs adapter itself has no size cap. Restart-required.
    # l3:
    #   enabled: true
    #   path: /var/lib/lmcache
    #   maxBytes: 100GB
    # LRU | IsolatedLRU | noop
    evictionPolicy: LRU
    # tokens per cache chunk. 0 (or unset) means auto: the --chunk-size flag
    # is omitted and the server applies its own library default.
    # chunkSize: 0
  # Versioned update policy for the dedicated server runtime. `update.version`
  # is the standard exact target and is authoritative over the legacy
  # top-level `version` alias and a version embedded in `package`. Absent or
  # empty `policy` disables background activity; only an explicit
  # `automatic` policy enables background checks, staging and activation.
  # `pinned` freezes background checks/downloads/activation and cleanup, but
  # explicit manual upgrade and rollback remain available. LMCache always
  # rolls back a candidate whose real server health check fails, so
  # `rollbackOnFailure: false` is invalid for this module.
  # update:
  #   policy: manual # disabled | manual | automatic | pinned
  #   channel: stable # stable | prerelease
  #   version: 0.5.5 # standard exact version target
  #   checkEvery: 12h
  #   minIdle: 10m
  #   activateOnlyWhenIdle: true
  #   keepVersions: 2
  #   rollbackOnFailure: true
```

L2/L3 usage reporting is best-effort: the pinned-DRAM pool's used bytes come
from the server's `/status` endpoint when it is up; L3 (the filesystem
adapter) has no byte counter in the current LMCache release, so its usage is
reported as unavailable rather than fabricated.

### Model attachment and version pinning

A vLLM model attaches to LMCache with `backend.lmcache` under its `backend`
block. It requires a native managed vLLM runtime (container and legacy `cmd`
models are not supported) and the module to be enabled; the connector is
installed into the model's runtime automatically at model start.

```yaml
backend:
  type: vllm
  runtime: vllm-cuda
  # optional: launch this model from a specific staged vLLM version instead
  # of the runtime's current one; the version must already be staged or the
  # model start fails closed.
  # runtimeVersion: 0.11.0
  args: [/models/qwen]
  lmcache:
    enabled: true
    # mp = standalone lmcache server (recommended);
    # inProcess = legacy single-process connector.
    mode: mp
    # kv_both | kv_producer | kv_consumer
    role: kv_both
    # mp: ZMQ host/port of the lmcache server.
    host: localhost
    port: 5555
    # inProcess: 0 keeps the library default (sent as LMCACHE_CHUNK_SIZE).
    chunkSize: 256
```

Injection happens at process launch, not in the stored configuration: when the
model process starts, llama-swap appends a `--kv-transfer-config` argument to
the bound launch snapshot. `mp` mode selects `LMCacheMPConnector` with the
server host/port in the connector extra config (and the documented
`kv_connector_module_path` when the active vLLM version is 0.20 or newer);
`inProcess` mode selects `LMCacheConnectorV1` and sets `LMCACHE_CHUNK_SIZE`
when `chunkSize` is non-zero. The change takes effect on the model's next
restart.

The two modes differ in what has to run alongside the model. `mp` is the
standalone deployment: the module must be enabled, the server must be up, and
the model's start is held until both are healthy — stopping the server is
refused while an `mp` model is running. `inProcess` is the non-standalone
deployment: the LMCache library runs inside the model's own vLLM process, so
no server is required or started and the model holds no server reference
(stopping the service never waits for it). Its connector still installs into
the model's own vLLM runtime at first start, pinned to the server runtime's
version when one is staged and otherwise to `lmcache.version`, or to the
newest published release when no version is pinned.

### API

| Endpoint | Description |
| --- | --- |
| `GET /api/lmcache` | Unified module status: server state plus real `healthy`/`healthCheckedAt`, current/previous/staged/available versions, policy/channel/target, pin, last check/update and error fields. |
| `GET /api/lmcache/dashboard` | Safe read-only projection of the managed server's `/healthcheck`, `/status`, `/config/adapters`, version, metrics and periodic-thread-health endpoints. It returns `available: false` without contacting the server while stopped and accepts only explicit healthy results. |
| `POST /api/lmcache/check` | Side-effect-free check of the current artifact, real server health and the configured package metadata source. It never installs or switches a version; failures return the status object with an error. |
| `POST /api/lmcache/enable` / `POST /api/lmcache/disable` | Enable: stage the dedicated server runtime (in its own virtualenv) and start the server if configured. Disable: stop the server and remove the connector from active vLLM runtimes. Disable requires zero references. |
| `POST /api/lmcache/server/start` / `stop` | Start the server (a cold start is always allowed) / stop it. Stop requires zero references (409 with the model list). |
| `POST /api/lmcache/server/restart` | Stop and start again to apply a pending configuration change; drops the in-memory KV cache, so it is refused (409) while any model references the server. |
| `POST /api/lmcache/update` | Body `{"action": "upgrade" \| "stage" \| "activate" \| "rollback", "version": "<optional>"}`. `upgrade` performs check/resolve, stage and activate; a supplied version is exact, while an empty version follows `update.version` or the configured channel. It returns 200 only after the new server passes `/healthcheck`; busy references return 409. The split stage/activate/rollback actions remain compatible. |

The lifecycle has two independent locks. A declarative `update.version` is an
exact configuration target and conflicting legacy pins are rejected. The
runtime Pin/Unpin operation freezes background checks, downloads, activation
and cleanup at the pinned version, but does not block explicit manual upgrade
or rollback. Automatic activation additionally requires no LMCache references,
no in-flight request or model/control-plane transition, and the configured
`minIdle` window; a reference race is reported as `WAITING_FOR_IDLE`, not
`DEGRADED`. The default channel is `stable`, check interval `24h`, minimum idle
window `30m`, retention `2`, and background updates are off unless
`policy: automatic` is written explicitly.

Server start/update failures return the log path and the most recent lines of
`<runtimeManager.root>/lmcache/server.log` in the error body, so a failure is
diagnosable without leaving the control panel.

Changes to restart-class parameters (ports, L2/L3, eviction policy, chunk
size, enable/disable) are applied immediately when no model references the
server; otherwise the change is saved and flagged `pendingRestart`, and the
live process is left alone until an explicit restart.

## Resource budget and safe eviction

Resource accounting is opt-in. `resourceBudget` sets aggregate local VRAM/RAM
limits, while each model's `backend.resources` declares its footprint and
eviction priority. With `autoEvict: true`, a request may unload only models
that have no in-flight requests; if the budget still cannot be satisfied the
request receives HTTP 507 instead of terminating a protected model. The
current usage and safe eviction candidates are available from
`GET /api/resources`.

```yaml
resourceBudget:
  vramMiB: 24576
  ramMiB: 32768
  autoEvict: true
  queueLoads: true
```

The `anthropic.cacheFix` section enables the Go-native Claude Code cache repair
pipeline (`off`, `auto`, or `force`). Auto mode requires both an explicit
Claude Code signal and Anthropic-shaped fields, so ordinary Anthropic SDK
requests are left byte-for-byte unchanged. `audit` stores redacted raw
conversations for 30 days/5 GiB by default and exposes usage, cache counters,
and estimated (not billed) pricing through the control API.

### Audit storage

Conversation audit lives in its own database with its own connection, beside
the operational one:

```
data/
  db.sqlite            activity rows, keys, pricing, power, …
  audit/
    audit.db           conversation rows, block index, session totals
    segments/*.seg     zstd-compressed body blocks, one day per file
```

Bodies are stored compressed and content-addressed, and conversation requests
are split at message boundaries before hashing. A session that re-sends its
whole history every turn therefore pays only for the new messages — the
accumulated context keeps a single stored copy:

```yaml
audit:
  enabled: true
  retention: 720h          # conversation retention
  maxBytes: 5368709120     # compressed on-disk budget for the audit store
  storeMedia: true         # inline media file content in the capture
  activityRetention: 0s    # 0 keeps request rows forever; e.g. 2160h for 90 days
```

`maxBytes` counts the compressed bytes the audit store actually occupies, so
the budget matches the directory's size. Retention drops whole expired day
segments and compacts the rest; a conversation's detail view reports its
session's accumulated requests, tokens, stored bytes and the context bytes the
deduplication avoided.

Existing installations are migrated once at startup: conversations written
before the split are copied into the audit store (compressed and deduplicated)
in the background while new requests already write to the new store. The copy
resumes after a restart and drops the legacy table when it completes; the freed
pages are reclaimed by an offline `VACUUM` at the operator's convenience.

## Advanced control with `cmd`

llama-swap is also about customizability. You can use any CLI flag available:

```yaml
models:
  model1:
    cmd: | # support for multi-line
      llama-server --PORT ${PORT} -m /path/to/model.gguf
      --ctx-size 8192
      --jinja
      --cache-type-k q8_0
      --cache-type-v q8_0
```

## Documentation for agents (MCP)

Gateway-owned tools, resource/prompt context, file uploads and conversation
checkpoints are documented in [Protocol runtime](protocol-runtime.md).

`POST /api/mcp` serves llama-swap's own documentation and a read-only view of
the running configuration to an MCP client (Claude Code, Codex, any agent host):

```json
{"mcpServers": {"llama-swap": {"url": "http://localhost:8080/api/mcp"}}}
```

Three tool families are exposed:

| Tool | Answers |
| --- | --- |
| `docs__list`, `docs__search`, `docs__get` | this documentation, indexed one section per entry, with the source file and line so a citation can be checked |
| `config__get` | a jq query against the configuration this server is running right now, with credentials redacted |
| `sys__overview` | build version, how many models/providers are configured, which are running, host hardware |

`config__get` is the one to reach for when an agent gives advice about a
deployment: it reads the live, source-aware configuration rather than a sample
file, so the answer describes the server in front of it. Queries are evaluated
against the document alone — the process environment is not in scope — and a
result larger than 32 KB is refused rather than truncated.

The endpoint speaks two MCP revisions, because both are in the wild: the
stateless `2026-07-28` revision (`server/discover`, `Mcp-Protocol-Version` and
`Mcp-Method` headers) and the classic `2025-06-18` handshake (`initialize`). It
is authenticated like the rest of the inference API, so a client needs one of
the configured API keys.

## Loading progress from the health check

While a model starts, its health check endpoint may answer with a non-200
status and a JSON body describing progress:

```json
{"progress": 0.42, "message": "loading tensors"}
```

`progress` is a fraction from 0 to 1 and `message` a short step description;
both are optional, and the message is trimmed to 1024 characters. The UI shows
the percentage next to the model's state, a progress bar along the row, and
the message.

Aside from being informational, `progress` changes the timeout: every new
value restarts the `healthCheckTimeout` countdown, so the timeout bounds how
long a load may go **without progress** rather than how long it may take in
total. A change in `message` alone does not count — a message can change while
a load is stuck, e.g. a growing ETA. Bodies that do not carry either field,
including llama-server's own loading response, behave exactly as before.

## Support for any OpenAI API compatible server

llama-swap supports any OpenAI API compatible server. If you can run it on the CLI llama-swap will be able to manage it. Even if it's run in Docker or Podman containers.

```yaml
models:
  "Q3-30B-CODER-VLLM":
    name: "Qwen3 30B Coder vllm AWQ (Q3-30B-CODER-VLLM)"
    # cmdStop provides a reliable way to stop containers
    cmdStop: docker stop vllm-coder
    cmd: |
      docker run --init --rm --name vllm-coder
        --runtime=nvidia --gpus '"device=2,3"'
        --shm-size=16g
        -v /mnt/nvme/vllm-cache:/root/.cache
        -v /mnt/ssd-extra/models:/models -p ${PORT}:8000
        vllm/vllm-openai:v0.10.0
        --model "/models/cpatonn/Qwen3-Coder-30B-A3B-Instruct-AWQ"
        --served-model-name "Q3-30B-CODER-VLLM"
        --enable-expert-parallel
        --swap-space 16
        --max-num-seqs 512
        --max-model-len 65536
        --max-seq-len-to-capture 65536
        --gpu-memory-utilization 0.9
        --tensor-parallel-size 2
        --trust-remote-code
```

## Aliases and the name the engine serves

An alias is a llama-swap-side name. Routing, `/v1/models`, API-key
authorization and selectors all resolve it to the configured model, but the
engine process knows nothing about it — it serves exactly one name:
`useModelName` when set, otherwise the model ID. So llama-swap rewrites the
request's `model` field to that name on the way out, for JSON bodies and form
fields, on both the model-dispatched routes and the `/upstream/` passthrough.
A request made with an alias therefore works without any extra engine
configuration.

Two details worth knowing:

- The rewrite only fires when the body's model field names the model this
  request resolved through. A body naming something else — a profile
  replacement, or an engine name you already use — is forwarded untouched, so
  the `/upstream/` passthrough never rewrites a name it was not asked about.
- `filters.setParamsByID` keys are auto-registered as aliases, so a variant
  key can be requested as a model name and gets the same rewrite.

Setting `useModelName` (or `backend.launch.servedModelName` for a managed
launch) overrides the name that is used, which is the escape hatch when the
engine must serve a name other than the model ID.

## Per-model request filters

`filters.setParams` and `filters.setParamsByID` force values into a request
body, `stripParams` removes them, and `useModelName` rewrites the model field.
A key ending in `?` is **set-if-undefined**: it is applied only when the request
does not already carry that parameter.

```yaml
models:
  qwen:
    cmd: llama-server --port ${PORT} -m qwen.gguf
    filters:
      stripParams: "top_k"
      setParams:
        temperature: 0.2       # always forced
        max_tokens?: 32768     # only when the client sent none
```

Filters apply like a pipe — `stripParams`, then `setParams`, then
`setParamsByID` — so a stripped parameter counts as undefined and an earlier
stage's value counts as defined. When a key is spelled both ways (`top_p` and
`top_p?`), the plain spelling wins and is always applied.

## Upstream keep-alive

`disableKeepAlives` makes llama-swap open a new connection for every request to
that model instead of reusing pooled ones:

```yaml
models:
  llama:
    cmd: llama-server --port ${PORT} -m model.gguf
    disableKeepAlives: true
```

It works around upstreams that advertise `Keep-Alive` and then close the
connection — llama-server does this after a streamed response, so a request
made right after a stream can fail with `502 proxy error: EOF` without ever
reaching the model. Keep-alive stays on by default, so `timeouts.idleConn` and
the pool settings keep working everywhere else; with this option on,
`timeouts.idleConn` has no effect.

## Speech model voice lists

`GET /v1/audio/voices` is a non-standard endpoint, and upstream answers vary in
shape. `capabilities.voices` pins the list llama-swap returns, without starting
the model or contacting the upstream:

```yaml
models:
  tts:
    cmd: audio-server --port ${PORT}
    capabilities:
      out: [audio]
      voices: [alloy, echo, fable]
```

The list is answered verbatim, is never discovered at runtime, and is not
rendered into `/v1/models`. Models without it keep forwarding the request
upstream.

## Capability discovery

A `capabilities` block says what a model accepts and produces. Anything left
out of it is filled in automatically: when a model becomes ready, llama-swap
asks its upstream what it supports and remembers the answer, so the model list
advertises a context length and modalities without you having to write them
down.

```yaml
models:
  qwen:
    cmd: llama-server --port ${PORT} -m qwen.gguf
  # discovers: in=text,image, out=text, tools, context from /props
```

Discovery reads the upstream's own report, and a field you set always wins over
what it finds — so hand-setting one value does not cost you discovery for the
rest:

| Upstream | What it is asked | What is discovered |
| --- | --- | --- |
| llama-server, ik_llama.cpp (`owned_by: llamacpp`) | `/props` | input modalities (text, image, audio, video), tool support, context length of one generation slot |
| halogen-flash-server (`owned_by: halogen`) | `/health` | vision input, tool support, context length |
| any other OpenAI-compatible server | `/v1/models` | text in/out, and the context length the listing reports |

Fields a server cannot report stay unset rather than guessed: a reranking
server does not advertise `reranker`, and a
server that lists no context length leaves it at zero.

The probe runs once per model start and its result is cached (keyed by the
model's command, proxy and served name), so a model that is unloaded and
reloaded keeps advertising what it reported, and a restart of llama-swap does
not lose it. Upstreams with no capability surface — an image or transcription
server — simply produce nothing; that is not an error.

To opt a model out, because its upstream reports something you do not want
advertised:

```yaml
models:
  sd:
    cmd: sd-server --port ${PORT}
    capabilities:
      disableAuto: true
```

## Cost estimation (pricing)

The usage records page estimates a cost per request from the public
[models.dev](https://models.dev) catalog. The catalog is synchronized in the
background (default every 24h, `pricing.modelsDev.refreshEvery`) and stored in
SQLite, so estimates survive a restart. Estimates are never a bill.

A request is priced by looking its recorded model name up in the catalog.
Exact matches work on their own; everything else can be steered:

- **A model entry can pin its catalog row** — `models.<id>.backend.pricing:
  {provider, model}` for local models, `providers.<id>.models[].pricing` for
  provider models. The model editor's pricing panel writes this for you.
- **Provider models resolve by default** against the provider's own id
  lowercased and the upstream model name, so a provider named `DeepSeek` prices
  `deepseek-flash` against models.dev's `deepseek` entries with no
  configuration. `providers.<id>.pricingProvider` overrides the catalog
  provider.
- **Several providers pricing the same name** is the normal case
  (`glm-5.2` is listed by dozens of resellers). Resolution runs in order: a
  lone row you edited by hand wins (an explicit price is not a guess), then
  the **official vendor's** row — `pricing.modelsDev.vendors` maps name
  prefixes to the catalog provider that counts as official. The built-in
  table covers the major families (`gpt`/`o1`/`o3`/`o4` → openai,
  `gemini` → google, `qwen`/`qvq`/`qwq` → alibaba, `glm` → zhipuai,
  `claude` → anthropic, `kimi` → moonshotai, `grok` → xai, `doubao` →
  volcengine, the mistral trademark family, and others), each verified
  against the catalog; entries in `vendors` override or extend it. Then the
  row of the provider that actually served the request, then the only row
  with non-zero rates. `pricing.modelsDev.ambiguity` decides how it ends when
  those steps still leave several candidates: `strict` refuses to estimate,
  `provider-first` (the default) stops there, `lowest` falls back to the
  cheapest remaining row. The provider of a row is matched
  case-insensitively.

Rows you add or edit by hand in the pricing panel (for a locally hosted model
the catalog does not know, for example) are marked as yours and survive every
catalog refresh; a refresh only rebuilds the rows it owns. Deleting a
catalog-owned row is temporary by design — the next refresh restores what the
source lists.

## audio.cpp UI passthrough

A local model configured under the reserved ID `audiocpp_auto` is exposed at
`/audiocpp/`, the same way `/comfyui/` serves `comfyui_auto`. Requests do not
name a model; every path under `/audiocpp/` is forwarded to the model with the
prefix stripped. Only a request for `/audiocpp/` itself may start an unloaded
model, so a stale browser tab cannot reload it.

## Maintenance mode (disabling a model)
`disabled: true` on a model takes it out of service without deleting it. The
configuration, aliases and files stay exactly as they are, so bringing it back
is a one-field change instead of a re-create:

```yaml
models:
  Qwen/Qwen3.6-35B-A3B-FP8:
    disabled: true
    # everything else stays
```

A disabled model is not servable: it is hidden from `/v1/models`, a request
naming it (directly or through an alias) returns `503` with a reason, and it is
never preloaded or started by hand. It still appears in the management
surface — the model list shows a "已停用" badge and the load button is disabled
— so an operator can find it and switch it back on. The toggle lives in the
model settings under 兼容与过滤.

This is the alternative to deleting a model you intend to bring back: a delete
also prunes the routing blocks that referenced the model, whereas maintenance
mode leaves the topology intact.

## Maintenance mode (after a failed configuration start)

`rollbackOnModelStartFailure` decides what happens when a model fails to start
with a newly applied configuration.

The default, `true`, restores the last configuration that started so the model
keeps serving while the operator fixes the change. The failed attempt's
output is preserved in the model log after a `— process replaced —` marker, so
the reason is diagnosable after the fact.

Setting it to `false` chooses the other trade: the new configuration is kept
and the model is taken out of service rather than silently restored to a
definition the operator just replaced.

```yaml
rollbackOnModelStartFailure: false
```

A model in this state shows a "维护中" badge in the model list. Incoming
requests naming it return `503` with the reason and are never queued behind a
start that would fail again. It leaves the state on its own once an explicitly
started process both becomes ready and serves a request, so a fix that works
clears the badge without further operator action.

The toggle lives in 基础 → 运行时 alongside the other service defaults.

## Deleting a model

Removing a model — through the UI's delete dialog, `DELETE
/api/config/models/<id>`, or a hand-written patch that drops the `models` entry
— also removes the model from the routing blocks that name it. The validator
rejects a block whose members no longer exist, so without this cascade the
delete would fail with an error like `var key "big" references unknown model
"..."` and the model would stay.

The adjustment is limited to references, and is reported back to the caller in
the `pruned` field:

| Block | What is removed | What happens when it empties |
| --- | --- | --- |
| `matrix.vars` | the entry naming the model | the var is gone, so expressions using it lose that leaf |
| `matrix.evict_costs` | the entry keyed by the model, and by a var that no longer resolves | the cost map omits it |
| `matrix.sets` | the model's leaf from each expression (`a & b` becomes `b`), and any `+set` reference to a set that was dropped | the set is dropped; a matrix with no set left is removed and the router falls back to the default group engine |
| `groups[*].members` | the member | the group is dropped; the default group is synthesized when none remain |
| `selectors[*].targets` | the target | the selector is dropped |
| `gpus` card lists | the model on the card | an empty card is dropped, and the router selection with it |
| `routing.scheduler.settings.fifo.priority` | the entry | the map omits it |
| `profiles[*].pins` | the pin whose replacement names the model | the profile is dropped |

A model that is removed and re-added in the same patch is not pruned — it is
still there in the resulting document. A reference held by a *different*
configuration source than the one the patch writes is not rewritten; that
patch is rejected and rolled back rather than committing a configuration that
cannot load.

The Settings page's routing section has a purpose-built matrix editor for the
blocks the schema-driven form cannot express: a vars table (short name →
model), an eviction cost table, and one combination table per set where each row
is a group of models that may run together and the rows are alternatives. The
editor renders the table back into the DSL, so `(a & b) | c` round-trips as two
rows. An expression that references another set (`+name`) cannot be shown as a
table; those stay in their raw DSL form.

## Many more features..

llama-swap supports many more features to customize how you want to manage your environment.

| Feature   | Description                                    |
| --------- | ---------------------------------------------- |
| `ttl`     | automatic unloading of models after a timeout  |
| `macros`  | reusable snippets to use in configurations     |
| `matrix`  | run multiple models at a time                  |
| `gpus`    | swap models by GPU card occupancy              |
| `hooks`   | event driven functionality                     |
| `env`     | define environment variables per model         |
| `aliases` | serve a model with different names             |
| `filters` | modify requests before sending to the upstream |
| `profiles` | switch model ID replacements at runtime       |
| `...`     | And many more tweaks                           |

## Full Configuration Example

Check [config.example.yaml](https://github.com/mostlygeek/llama-swap/blob/main/config.example.yaml) for the most up to date reference for all example configurations. It has grown quite complex but your favorite local LLM can help with a local configuration.

#### Tool failure handling

A tool that cannot run — an upstream search API out of quota, a provider
timeout, a handler that throws — is reported to the model as that tool's
result (`is_error` in the Anthropic shape) instead of failing the request. The
model then decides what to do next: retry, fall back to another tool, or answer
without it.

Set `failOnToolError: true` in an extension's manifest to keep the strict
behavior, where a failing tool handler aborts the request:

```yaml
# extension manifest.yaml
id: web-search-doubao
enabled: true
failOnToolError: false   # default: report the failure to the model
```
