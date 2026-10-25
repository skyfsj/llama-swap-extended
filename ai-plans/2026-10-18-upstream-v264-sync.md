# Upstream v264 manual sync (2026-10-18)

## Context

`main` diverged from `upstream/main` at `8a32e05` (v251). Upstream is now at
v264 (`a2796fb`), 83 commits ahead. An earlier `upstream-sync` branch ported
13 commits (v252–v257 era) but was never merged. This sync copies the logic
by hand — no `git merge` — because the fork has its own implementations of
many subsystems.

Already present in this fork (verified, not ported again):

- TTL idle window starts when a model becomes ready (`21bc145`)
- Trusted-proxy client IP resolution (`2edad4a`): the fork's
  `internal/server/clientip.go` is stricter (right-most appended hop, trusted
  proxy list) than upstream's version
- Separate log endpoints / multi-stream logging (`bbdb55d`): the fork has its
  own `logmon` layout and `/logs/stream/{id}`
- `cmd/wol-proxy` previously had neither liveness nor `-api-key`; both are
  ported here

Deliberately out of scope (fork has its own implementation or the subsystem
does not exist here):

| Upstream | Why skipped |
| --- | --- |
| `internal/tailcat` (v0.6/v0.7, peer connectivity) | not present in this fork |
| `cmd/kubeswap` | not present in this fork |
| `internal/reference`, `/api/tools`, Docs Agent, Help page (`6ffd67f`, `a77f107`, UI help commits) | no equivalent subsystem; extensions own this area |
| Hardware Detection rewrite (`1abc44f`, `ccccb20`, Apple/Intel detection) | fork has its own `internal/hw` implementation and schema |
| `internal/capcompat` auto-discovery (`eeac3e6`, `9db37d0`, `b51dde4`) | fork's capability handling is config-driven and feeds its own backend capability middleware / provider gateway; auto-discovery needs a design decision of its own |
| Store repository interface split (`aecf92e`) | fork's store layout already diverges |
| zstd capture pool (`0e1f797`) | fork has no `internal/server/captures.go`; its zstd use is the audit segment store |
| UI redesigns (sidebar, playground, chat stats, Help, profile links) | fork UI is a different application |
| Docker image / changelog / release / Makefile commits | fork's Docker layout differs; no CHANGELOG |

## Port list

### A. HTTP surface and request handling

1. **Content-Type auto-fill middleware** (`486376e`): missing/wrong
   Content-Type on model-dispatched POSTs is repaired when the body is JSON;
   unusable bodies get a precise 400. Reuse the fork's
   `swaputil.MaxRequestBodySize`.
2. **`/v1/systemone`** in the model POST JSON routes (`45b78e2`).
3. **`context_window`** mirrored alongside `context_length` in
   `/v1/models` and `/models` (`1fc0490`).
4. **`setParams` `key?` set-if-undefined** (`82ccfbf`) in config sanitizers
   and JSON filter application.
5. **CORS controls** (`6c36b88` + `769dbac`): `security.cors` block,
   permissive legacy default, Origin-conditional headers, configurable
   origins/methods/headers/max-age, private-network gate. Keep the fork's
   cross-origin management-write rejection.
6. **Strip browser headers to peers** (`d34ddb2`) and strip upstream CORS
   response headers so the proxy remains the only source.
7. **Global concurrency limit** (`41ec321`): `globalConcurrencyLimit`,
   429 + `Retry-After` before per-model admission.
8. **Per-model `disableKeepAlives`** (`d60d084`).
9. **`/audiocpp` passthrough** (`fd49827`) with reserved `audiocpp_auto`.
10. **Capabilities `voices` override** (`163fd1d`) for
    `GET /v1/audio/voices`.
11. **Reverse-proxy base path** (`a2796fb`): relative redirects on the server
    and `apiUrl()`-based same-origin URL resolution in the UI.

### B. Lifecycle / runtime

12. **Loading progress from health checks** (`d3ab665`): parse
    `{progress,message}`, extend the health-check deadline only on progress
    change, expose it in model status and SSE, render it in the UI.
13. **cmdStop stdout/stderr logging** (`111d2d3`).
14. **Perf GPU collector restart loop** (`5dd7f88`) merged with the fork's
    energy integration.
15. **Startup profile hook** (`32c3d66`): `hooks.on_startup.profile`.
16. **ComfyUI concurrency 50 → 999** (`7e18554`).
17. **gopsutil Apple M6 panic workaround** (`a2bf16e`).
18. **vllm-wrapper default transport** (`e1526c8`): remove the 300 s
    `ResponseHeaderTimeout` that truncates long generations.

### C. Persistence and metrics

19. **Activity total after pruning** (`96e6f94`): unfiltered totals use
    monotonic `MAX(id)`; filtered totals keep row counts.
20. **TabbyAPI/oMLX usage rates** (`8fa8589`, `783c323`) as fallbacks under
    timings/metrics, including `usage.total_time` as a duration floor.

### D. Matrix and tools

21. **Matrix `+auto:orphans`** (`bea6428`).
22. **PWA manifest/icons bypass auth** (`5cc05f5`).
23. **`cmd/wol-proxy`**: liveness check (`a6f5ae3`), `-api-key` (`51d1969`),
    confirmation page + wake progress (`8db9566`).

## Verification

- `gofmt -w` on every changed Go file.
- `go test -v -run <new tests>` for each ported unit.
- `make test-dev` (go test -short + staticcheck) and `make test-ui`
  (svelte-check + vitest).
- `make test-all` (race) before handing to review.
- Independent review thread checks requirement completeness, logic,
  edge cases, code quality, test coverage, and runtime results; fix list
  loops until pass.

## Review round 1 (2026-10-19)

Independent review found one High and two lower items; all fixed and each
regression test verified by mutation (the test fails with the fix reverted):

- **H1** — the health-loop watcher goroutine returned after a single `select`,
  so once the start budget expired with progress already reported, an operator
  `Stop` or shutdown could no longer cancel the extended health loop, and the
  run loop (which waits for `doStart` to return before answering Stop) hung
  until the progress deadline. Fixed with a separate `abortCh` edge closed at
  the run loop's in-flight stop/shutdown points, plus a watcher that keeps
  running after the budget is spent. Test:
  `TestProcessCommand_LoadingProgressAbortsOnStop` (blocks the probe so only
  context cancellation can end it; fails at 60s without the fix, passes in
  ~5s with it).
- **M1** — the reload path that re-activates `hooks.on_startup.profile` was
  untested. Test: `TestServer_ReconcileConfigReactivatesStartupProfile`
  (fails when the reload hunk is removed).
- **L1** — no test asserted `Vary: Origin` /
  `Vary: Access-Control-Request-Headers` on CORS responses. Assertions added;
  removing `addVary(h, "Origin")` now fails them.

Also fixed a flaky perf test of the ported GPU-restart suite: listener
delivery is best-effort, so it now polls the collector call count instead of
chasing every sample (verified with `-count=10` and `-race -count=3`).

## Review round 2 (2026-10-19): docker skips re-examined

The first pass skipped docker commits as a class ("fork layout differs"). Re-checking
them individually showed that judgement was wrong for five of them, so they are now
ported:

- `0ac43bb` **V100 sm_70**: the fork's `docker/unified/Dockerfile` had
  `CMAKE_CUDA_ARCHITECTURES="60;61;75;86;89"` — missing exactly the 70 upstream
  added, for a V100 trip that no PTX JIT can recover from (ik_llama's WMMA
  flash-attention has no sm_61 kernel). Applied, with the comment and README
  updated to explain why the JIT fallback is not safe for every bundled project.
- `a6f3b09` **static eSpeak-ng**: ported `AUDIOCPP_STATIC_ESPEAK=ON`, the
  `libespeak` linkage check, the phoneme-data install (unpacked directory, not
  the packed `.bin`, which needs a writable `$HOME`), the GPL license staging,
  and the `build-image.sh` smoke assertion. Verified against audio.cpp's own
  build rules that option `AUDIOCPP_STATIC_ESPEAK` exists, that the data lives
  at `build/_deps/espeak-static/build/espeak-ng-data`, that licenses stage to
  `build/bin/licenses/espeak-ng/COPYING`, and that `CMAKE_RUNTIME_OUTPUT_DIRECTORY`
  is `build/bin`.
- `aa8031f` **mp3 + native model manager**: the fork's `install-audio.sh` never
  initialized the `external/audio.cpp-server-frontends` submodule and never set
  `AUDIOCPP_BUILD_NATIVE_MODEL_MANAGER` / `AUDIOCPP_BUILD_SERVER_FRONTENDS`
  (both default OFF). Without them the `/audiocpp` UI ported in this sync cannot
  download or manage models and has no MP3 support.
- `e054c90` **untagged-commit builds**: the fork's `install-vllm-wrapper.sh`
  silently fell back to `latest` when a commit had no release tag, and resolved
  `latest` through the rate-limited releases API. Both scripts now source
  `lib-release.sh`; a commit with no tag is built directly.
- `lib-release.sh` regex widened to `^v[0-9]+(\.[0-9]+)*$`: the fork tags
  semantic versions (`v1.0.0`, `v1.0.1`), which upstream's `^v[0-9]+$` never
  matches, so the API-rate-limited fallback was dead code for the installer
  scripts' own default REPO.

Verified live: `resolve_latest_version` returns `1.0.1` for the fork's repo and
`265` for upstream (both through the git-tag fallback, the API being 403 from
here), and a raw-commit checkout of the fork's repo yields a working
`cmd/vllm-wrapper`.

Still not assessed (deliberately, they are CI/layout redesigns rather than
features): `e31a1ad` per-project build stages, `6c7f958` cuda13/arm64 multi-platform
and rootless images, `7a14664` CUDA version configurability — the fork already has
`ARG CUDA_DEVEL_IMAGE` / `ARG CMAKE_CUDA_ARCHITECTURES` knobs of its own.

## Round 3 (2026-10-19): the "gaps" the user asked for

### Capability auto-discovery (capcompat) — ported

Upstream's `internal/capcompat` (2541 lines incl. fixtures) plus `server/capabilities.go`
and the store cache. Adapted to this fork:

- `internal/store/cache.go` + migration `00028_capability_cache.sql`: a general
  blob cache (key/data/expires_at/updated_at) rather than a capability-shaped
  table, with expiry-on-read and a prune.
- `internal/capcompat`: `capcompat.go` (Client/Detect/Probers), `llamaserver.go`
  (`/props`: modalities, tool support, slot context, decision models read from
  the listing), `halogen.go` (`/health`), `default.go` (listing-only fallback),
  `service.go` (store-backed cache + memo + singleflight + retry). `Service`
  depends on a narrow `Cache` interface so the probe logic is testable without a
  database.
- `config.ModelCapConfig`: `Merge` (configured fields win per field),
  `DisableAuto`, and `decisions` accepted as an output-only modality.
- `internal/server/capabilities.go`: refresh on the ready transition (off the
  event dispatcher's goroutine), `resolveCapabilities` merging config + cache,
  wired into `/v1/models` and the `/api/events` modelStatus payload.
- New `swaputil.ModelCapabilitiesChangedEvent` (0x0B) so discovery announces
  itself; the fork kept a precise event rather than overloading the status one.
- 48 ported package tests + 11 server tests + 7 store tests + config tests.

Verified end to end against a stub llama-server (`owned_by: llamacpp`, `/props`
reporting vision+tools+8192 while the listing says 4096/131072): the model list
advertises 8192 and `vision`/`function_calling` only after the model becomes
ready, and a restart with the model unloaded keeps advertising them from the
store (30-day TTL row in `cache_entries`).

### Hardware DMI identity — ported

`System{vendor, model, family}` on the snapshot, read from `/sys/class/dmi/id`
with placeholder and revision filtering, plus the CPU-topology socket-count
fallback for arm64 (no `physical id` in `/proc/cpuinfo`). UI type + a "Device"
row, locale keys in all three catalogs. The linux tests were run for real in a
container (`docker run golang:1.26`), not just cross-compiled.

Deliberately not ported from the same upstream commit: the DGX Spark/GB10
detection and `nominal_power_watts`, which depend on a rewritten NVIDIA probe
this fork does not have.

### Documentation MCP endpoint — ported (engine + this fork's docs)

- `docs/embed.go`: embeds the fork's own documentation (`*.md`, `extensions`,
  `examples`) — the fork's docs describe the fork, where upstream's `docs/kb`
  corpus describes upstream-only features. Screenshots and the Grafana dashboard
  are excluded on purpose.
- `internal/docagent`: index (one doc per H2 section, stable slugs, real source
  line for citations), search (stop words, heading/summary/body weighting),
  provider (docs__list/search/get, config__get, sys__overview) and the jq query
  layer (gojq, environment loader disabled, 5s/32KB bounds).
- `internal/server/apimcp.go`: stateless MCP over POST /api/mcp. Accepts BOTH
  revisions — the modern `2026-07-28` (server/discover + routing headers) and
  the classic `2025-06-18` (initialize) — where upstream supports only the
  modern one; supporting both is a superset and avoids locking out clients.
- `config__get` reads the same source-aware redacted snapshot the settings UI
  serves, so an agent cannot see a different configuration than the operator.
- 16 server tests + 9 index/search tests; verified live against a running
  binary (52 sections indexed from the real docs, `docs__get` citing
  configuration.md line 1102 which is the real heading line, jq queries, and
  401 without a key).

### Measured, not ported: tailcat and kubeswap

Both are whole subsystems with their own dependency weight, measured in a
scratch module rather than estimated:

| | modules added | hello-world binary |
| --- | --- | --- |
| tailcat (`github.com/tailscale/tailcat` + `tailscale.com/{tailcfg,types/key,wgengine/filter}`) | 666 | 24 MB |
| kubeswap (all its imports incl. `k8s.io/client-go`) | 78 | 31 MB |

The fork's own module graph is ~100 modules today. Tailcat would multiply it
~6-7x for WireGuard private networking that this fork already covers with its
own `peers:` implementation, and it rewires config/router/server peer handling
the user actively maintains. Recommendation: do not port tailcat.
Kubeswap is more contained (standalone `cmd/`, additive), but it is 8.5k lines
plus a Helm chart that cannot be validated here without a cluster.

### Not assessed

docker `e31a1ad` (per-project build stages) and `6c7f958` (cuda13 / arm64
multi-platform / rootless) restructure the image skeleton the fork maintains
its own variant system on; that is an engineering decision, not a port.

## Round 4 (2026-10-19): review of round 3

The independent review of the capability/DMI/MCP work found two High and four
Medium items; all six are fixed, each with a regression test verified by
mutation (the test fails with the fix reverted):

- **H1** — `parsePage` treated a `## ...` line inside a fenced code block as a
  heading, so a shell or YAML example split its section, invented a bogus
  section id (`#not-a-heading`) and truncated the body mid-fence. Fixed by
  tracking fence state while scanning. Test:
  `TestDocs_FencedCodeDoesNotSplitSections`.
- **H2** — two sections sharing a heading produced the same id, and `byID` kept
  only the last, so `docs__get` on the first returned the second one's body with
  the wrong citation line. Fixed with ordinal suffixes; the first occurrence
  keeps the plain id. Test: `TestDocs_DuplicateHeadingsStayAddressable`.
- **M1** — no test pinned that a JSON `null` id is a request, not a
  notification. Test: `TestMCP_NullIDIsARequestNotANotification`.
- **M2** — the preamble and windowed `SourceLine` arithmetic was untested. Test:
  `TestDocs_SourceLinesPointAtTheFile`.
- **M3** — `Service.Forget` had no production caller, so the memo grew by one
  entry per config edit for the life of the process. Now
  `forgetStaleCapabilities` runs on every applied config candidate, dropping the
  key of any model that was removed or whose command, proxy or served name
  changed. Test: `TestAPI_ReconfigureForgetsStaleCapabilityKeys` (it removes the
  stored row so the assertion observes the memo, not the TTL).
- **M4** — `install-llama-swap.sh` silently fell back to `latest` for a commit
  with no release tag, which would ship a different revision than the operator
  pinned. It now refuses with a message naming both ways out.

Low items also fixed: the `llama-swap.go` comment no longer claims a reload
builds a new Server, the dead `TrimSuffix` in `pageID` is gone, and the
`defaultProber` comment now matches `Matches()` (a listing whose entries report
a context length).

Not caused by this work, left alone: `gofmt` reports `internal/config/config.go`,
`internal/config/load.go` and `internal/config/protocol_runtime.go`, and
`go.mod` needs a tidy — all from the user's concurrent `protocolRuntime`/MCP
work in the same tree.

## Round 5 (2026-10-19): round-4 review verdict

The second pass verified every fix at the source plus an independent mutation of
each, and re-checked the two areas I asked about:

- `docs/configuration.md` indexes 24 sections for its 24 real H2 headings (the
  reviewer re-counted with a fence-aware scan). No fenced `##` is split, no real
  H2 is missed.
- Cross-file page ids collide by construction (`examples/x.md` and
  `examples/x/README.md` both derive `examples/x`), and the `uniqueID` added for
  H2 also covers it: the second becomes `examples/x-2`.

Final: no findings, `VERDICT: PASS`.

Still outstanding, and not mine: the user's concurrent work in the same tree
(`internal/gateway`, `pkg/dialect`, `internal/protocol/relaykit`,
`internal/server/conversations.go`, `mcp_resume.go`, `protocol_upload.go`,
`extensions_*.go`, plus `gofmt`/`go.mod` drift). Two extension tests fail from
that work: `TestServer_ExtensionsGzippedUpstreamRound` and
`TestProxy_ExtensionForwardSkipsExtensions`, both on a `conversations.go`
continuation check.
