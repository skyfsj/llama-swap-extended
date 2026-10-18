# Full-project audit — complete findings ledger (2026-10-17)

Source of truth: five source-complete audit passes (auth/config, extensions, gateway,
protocol conversion, process/runtime), each finding verified by reading the code at the
cited line. Nothing from those reports is omitted below; items are only *tagged* with a
disposition: `FIXED`, `PLANNED`, `DEFERRED`, `NOT-A-DEFECT` (with reason).

---

## A. Authentication / authorization / secrets / config
(report: auth/config/store pass)

| ID | file:line | Sev | Finding | Disposition |
|----|-----------|-----|---------|-------------|
| AR1 | server/authz.go:65-69, 88-90 | H | Keyless deployments get a full-privilege `anonymous` identity (`Identity{Legacy:true}` → `Has()` true for every scope) on every route incl. config-admin/keys-admin/runtime-admin. | NOT-A-DEFECT: documented compatibility mode; any key configured closes it. Documented in report. |
| AR2 | server/control_api.go:95-102 + swaputil/http.go:796-804 | H | Browser session cookie stores the **raw API key**; `ExtractAPIKey` accepts it on every route → cookie theft/XSS reveals the actual long-lived secret. | DEFERRED (design): needs server-side session-token subsystem. Flagged as risk. |
| AR3 | server/log.go:353-367 | H | `clientIP` trusts `X-Forwarded-For`/`X-Real-IP` unconditionally; used for access-key IP allowlists (keylimits.go:85) and last-used audit (authz.go:113) → spoofable allowlist bypass / audit poisoning. | DEFERRED: needs trusted-proxy config surface (product decision). |
| AR4 | server/control_api.go:1238,1427-1434 | H | CC Switch deeplink falls back to constant key `"llama-swap"` on keyless deployments → client imports a publicly known credential. | PLANNED: remove fallback / refuse link when no secret. Likely upstream behavior depends on clients. Verify tests. |
| AR5 | server/control_api.go:1320-1362,1447-1463 | H | Raw retained secret of another key returned in a `ccswitch://` response body to keys-admin/self; secret transits response + URL (clipboard/history). | DEFERRED: feature's purpose is credential delivery; no-store/no-referrer set. Flagged. |
| AR6 | server/providers_api.go:195-296 | — | Provider probe fetches an operator-supplied URL. **Reclassified NOT-A-DEFECT**: provider endpoints legitimately point at loopback/LAN model servers (vLLM on 127.0.0.1), and the route is config-admin gated. Applying the extension public-host guard would break the feature. Residual risk documented (a compromised admin key can probe internal hosts), mitigated by scope design. |
| AR7 | server/auth.go:150-191 | H | CORS: preflight reflects `Authorization`, wildcard origin; cross-origin write block covers only `/api/*` with Origin header → drive-by writes against keyless deployments and non-/api state-changing GETs (e.g. `GET /unload`). | NOT-A-DEFECT for CORS credentials (no ACAO-credentials); keyless part is AR1. Flagged. |
| AR8 | server/authz.go:100-104 | M | No rate limit/lockout on invalid key attempts (only DB probe throttle keycache.go:225) → unthrottled online guessing of weak YAML keys. | DEFERRED: needs limiter design. |
| AR9 | server/authz.go:262-265, auth.go:67-70 | M | Legacy YAML keys match plaintext config values, get `Legacy:true` (all scopes/models, no expiry/IP/concurrency; keylimits.go:82 skips them). | NOT-A-DEFECT: documented legacy semantics. |
| AR10 | config/manager.go:883-893 | M | `redactNode` is name-heuristic only; credentials embedded in values (endpoint `?key=`, metadata, macro names like `openai_auth`) echo through `/api/config`. | PLANNED (hardening): redaction is layered; cannot catch all value-embedded secrets without false positives. Documented. |
| AR11 | server/api.go:618-653 | M | `handleUpstream` raw passthrough to backend, inference scope only, body unfiltered; with permissive CORS any origin can drive inference on keyless deployments; path traversal delegated to reverse proxy. | NOT-A-DEFECT (scope design) + AR1. Path handling verified with EscapedPathSuffix. |
| AR12 | server/control_api.go:174-179 + store/control.go:36-39 | M | Plaintext `KeySecret` persisted in SQLite for config-seeded keys (for CC Switch) → DB file disclosure leaks raw credentials. | DEFERRED: needed for deeplink feature; encryption-at-rest out of scope. |
| AR13 | store/control.go:227-230 | M | `KeyFingerprint` = unsalted SHA-256 of raw key used as durable ID → offline cracking oracle for weak legacy keys. | DEFERRED: ID migration would break API keys referencing IDs. |
| AR14 | server/metrics.go:551-560 + settings/spec/catalog.go | M | Full request/response bodies persisted by default (audit.enabled/storeMedia default true); body-embedded secrets not redacted (headers always are). | NOT-A-DEFECT: documented raw-audit contract with redaction switch. |
| AR15 | server/auth.go:195-201 | L | CSRF defense compares client-supplied Origin to Host; requests without Origin bypass; only `/api/` covered. | NOT-A-DEFECT: browser CSRF still requires Origin; non-browser clients are not CSRF. |
| AR16 | test surface | L | `keyProbeMinInterval` throttles store probes, not guess attempts (same as AR8). | duplicate of AR8. |
| AR17 | auth/keys.go:107-110 | L | Unsalted legacy digest accepted alongside salted hashes. | NOT-A-DEFECT: backward compatibility. |
| AR18 | store/control.go:243-307 | — | Verified good: ID length, hash format, scope/model normalization; revoked rows still count as configured; last-admin-key guard in 3 places; symlink rejection; SQL sort whitelisting. | NOT-A-DEFECT (positive findings). |

---

## B. Extensions / server-side tools
(report: extensions pass)

| ID | file:line | Sev | Finding | Disposition |
|----|-----------|-----|---------|-------------|
| EX1 | server/extensions_hostcalls.go:184-190 | H | `models.forward` merged script `request` fields **after** allowlist check → override `model` bypasses `modelAllowedForIdentity`, override `stream:true` disables buffering assumption. | **FIXED** (merge then force model/stream). |
| EX2 | extensions/workerpump.go:66 | M | `go onLog(record)` spawns unbounded goroutine per log line → script logging loop floods goroutines doing file I/O. | PLANNED: bounded dispatcher. |
| EX3 | extensions/images.go:39,149-151 + server/extensions_hostcalls.go:62 | M | `images.apply` decodes up to 32MiB in / 25MP (~100MB RGBA) **inside the daemon**, budget chosen by script; concurrent requests multiply allocation. | PLANNED: lower daemon-side default cap or move to worker; verify image tests. |
| EX4 | server/extensions_hostcalls.go:364-374 | M | Persistent KV limit is check-then-write (`CountExtensionKVs` then `SaveExtensionKV`) without lock → concurrent requests exceed `storageMaxKeys`. | PLANNED: serialize. |
| EX5 | extensions/worker.go:391 | M | `exported.ToObject(vm).Get("default")` before nil guard: bundle setting `__llamaSwapExtension = null` panics at JS boundary (recovered as opaque kill). | PLANNED: nil guard first. |
| EX6 | server/extensions_context.go:54-59, hostcalls.go:399-416 | M | kv `session`/`key` scopes and `session.usage` key off **client-supplied** metadata (`session_id`, `key_id`) → cross-caller namespace sharing. | DEFERRED: scope semantics product decision; `key_id` is identity-derived in normal path (needs trace). |
| EX7 | server/extensions_middleware.go:294-299 + responses_middleware.go:590-600 | M | Every non-streaming extension round buffers full upstream response up to 64MiB per request, unbounded by concurrency. | DEFERRED: memory amplification needs global budget. |
| EX8 | extensions/worker.go:33 | M | `publicHostCheck` is a mutable **package-level var** guarding SSRF; any assignment disables process-wide. | PLANNED: make it a const-injected dependency or verify only tests assign. |
| EX9 | extensions/worker_node.go:76 | M | Node debug executor inherits **full** `os.Environ()` (goja uses minimal env) → leaks daemon env to editor-draft code. | PLANNED: minimal env. |
| EX10 | server/extensions_stream.go:688-699, middleware.go:789-822 | M | Tool-round budget exhaustion silently drops withheld server calls and rewrites finish_reason to `stop`; client sees degraded answer with no error signal. | DEFERRED: product semantics (fail-open). Documented behavior. |
| EX11 | middleware.go:333-343,545-591 + worker.go:190 | M | `onResponse`/`onToolCall`/`onStreamEvent` run serially per extension, each 10s default; N extensions × rounds multiply latency, no aggregate deadline; `onError` runs synchronously too. | DEFERRED: needs aggregate deadline design (behavior change). |
| EX12 | manager.go:506-509 + worker.go:247 vs manager.go:832-841 | M | `Save`/`Reload` swaps `*Compiled` while in-flight requests hold old pointer; `c.LogSink` written under statusMu but read unguarded in `run()` → data race (acknowledged "benign" in middleware.go:163-165). | PLANNED: guard LogSink access. |
| EX13 | server/extensions_stream.go:68-71 | M | One upstream SSE event >4MiB aborts the whole bridged stream mid-response. | DEFERRED: conversion needs whole event; document limit. |
| EX14 | server/hostcalls.go:496-506 | L | `ephemeralKV.retain` scans whole map under one global mutex on every `kv.set`; all extensions contend → O(n) write, contention DoS. | DEFERRED: per-extension sharding. |
| EX15 | extensions/executor/executor.js:174-189 + worker.go:606-648 | L | Node loop accepts a new request line while previous hook pending (no serialization guard); goja promise pump relies on strict host-result ordering — unexpected id kills hook. | PLANNED: Node serialization guard; goja ordering is by design. |
| EX16 | middleware.go:795 | M | `dropWithheldCalls` inspects only `choices[0]`; multi-choice responses keep withheld calls in choices 1..n. | PLANNED: iterate all choices. |
| EX17 | extensions/cgroup_linux.go:29-31 | L | `attachExtensionCgroup` silently no-ops without cgroup fs; only polling enforcement remains. | NOT-A-DEFECT: graceful degradation, logged? verify. |
| EX18 | middleware.go:419,577-578 | L | `onError` invoked fire-and-forget, results discarded. | NOT-A-DEFECT: by design (error path). |
| EX19 | extensions/worker.go:89,124-150 | — | Verified: 512 host calls/hook cap, per-extension 2-slot semaphore, RSS/CPU polling kill, 8MiB hook input cap, 4MiB bundle cap, npm `--ignore-scripts`, minimal env. | NOT-A-DEFECT (positive). |
| EX20 | extensions/worker.go:1419 | — | Verified: files read/write via `AllowedPath`: absolute, EvalSymlinks root containment, write target regular non-symlink, 10MiB cap. | NOT-A-DEFECT (positive). |
| EX21 | extensions/worker.go:1333-1397 | — | Verified: fetch scheme/userinfo check, allowlist, SSRF guard on every redirect, method allowlist, 10MiB cap, header caps, cross-origin auth strip, timeout clamp. | NOT-A-DEFECT (positive). |
| EX22 | extensions/worker.go:893 | L | `abortSignal` handed to scripts is a hardcoded stub `{aborted:false}` — a long hook can never observe cancellation, so timeout kills the process instead of unwinding. | DEFERRED: needs worker-side cancel plumbing; documented capability gap. |
| EX23 | extensions/manager.go:703-753 | — | Verified: npm build staged temp dir, `--ignore-scripts`, minimal env, bundle 4MiB cap, hook input 8MiB cap. | NOT-A-DEFECT (positive). |

---

## C. Gateway / scheduler / failover
(report: gateway pass)

| ID | file:line | Sev | Finding | Disposition |
|----|-----------|-----|---------|-------------|
| GW1 | gateway/gateway.go:1050 | H | `bufferRequestBody` trusts any existing `GetBody`; a middleware-replaced body with stale `GetBody` replays the **pre-rewrite** body upstream. | PLANNED: rewrite helpers re-point GetBody (see PR1). |
| GW2 | server/server.go:1552-1558 | H | `gatewayBodyFingerprint` restore prefers `GetBody()`; stale GetBody corrupts downstream body (historical 1MiB-truncation class). | PLANNED: same fix. |
| GW3 | gateway/gateway.go:796 | M | `g.logger.Warnf` with no nil check (inconsistent with :1321) → nil panic for embedders. | PLANNED: nil guard. |
| GW4 | gateway/gateway.go:1218-1229 + :201 | M | Deterministic client errors (400) trigger cross-credential retries with `skipCooldown`, up to 12 attempts/pool on unfixable requests. | NOT-A-DEFECT: intentional multi-surface probing; StopOnRequestScoped for multi-surface. Documented. |
| GW5 | gateway/gateway.go:554 | M | Per-request `newScheduler(members).pick` under `g.mu.RLock` → allocation churn, no rotation continuity. | PLANNED: hoist pool scheduler into route construction. |
| GW6 | gateway/gateway.go:927-943 | M | Sort comparator calls `BestCredential` → takes `sched.mu` per comparison; ignores weight; O(n log n) locking hot path. | PLANNED: precompute keys. |
| GW7 | gateway/gateway.go:1170-1183 | M | Bootstrap `io.ReadFull` 8KiB delays header commit; empty 2xx retried on every credential (empty-success retry loop). | NOT-A-DEFECT: documented anti-truncation strategy. |
| GW8 | gateway/gateway.go:1159-1163 | L | First-Read non-EOF error counts as lastStatus for final client error despite not reaching client. | NOT-A-DEFECT: intentional classification. |
| GW9 | server/metrics_middleware.go:68-71 | H | Audit buffering replaces `r.Body` **without installing GetBody** → feeds GW1/GW2. | PLANNED: install GetBody. |
| GW10 | gateway/gateway.go:1110-1116 | M | `ReplaceRequestModel` re-reads/rewrites entire body every attempt → O(body×attempts), 64MiB×12 worst case. | PLANNED: cache rewritten body per attempt source (or once). |
| GW11 | server/protocol_middleware.go:661-689 | H | `protocolStreamWriter.Write` returns error after head committed; `finish()` error dropped because `committed()` → truncated converted stream with no signal. | PLANNED: terminal error frame (see protocol rewrite). |
| GW12 | server/protocol_middleware.go:452-458 | M | `protocolBufferedWriter.Write` silently discards bytes past 64MiB and returns success; overflow reported only in some paths. | PLANNED: verify overflow reporting both paths. |
| GW13 | server/server.go:1279-1288 | M | `forwardAcrossSurfaces` buffers whole client body to 64MiB per request, no concurrency bound. | DEFERRED: needs global memory budget. |
| GW14 | server/metrics.go:224-232 | M | Deferred firstTokenObserver closes over reassigned `tm` (metrics.go:281) — fragile attribution; fires for rows whose record early-returns. | PLANNED: capture row/status explicitly. |
| GW15 | gateway/gateway.go:1382-1409 | M | `applyFailure` matches cooldown class by substring `"(auth)"` of cause string → reworded cause misclassifies. | PLANNED: typed cause class. |
| GW16 | gateway/scheduler.go:252-259 | L | `newScheduler` sorts caller's slice in place (`Route.Members`) — benign but surprising. | PLANNED: copy before sort. |
| GW17 | gateway/gateway.go:1657 | L | transportCache never evicts except Shutdown; config with many proxies grows unbounded across reloads (reload rebuilds gateway → OK). | NOT-A-DEFECT: gateway rebuild clears. |
| GW18 | gateway/scheduler.go:437 | H | `SessionHash` header branch ignored keyID → cross-tenant sticky pinning. | **FIXED**. |
| GW19 | swaputil/http.go:681-752 | M | `extractContext` over-limit path replaces the body with the truncated `Max+1`-byte buffer via deferred restore (a body larger than the limit is silently truncated rather than rejected at that seam). | PLANNED: verify every caller rejects post-hoc; body-limit tests. |
| GW20 | responses_middleware.go:19-37 | L | `readResponsesRequestBody` closes `r.Body` after reading; every caller must install a new body — a future caller forgetting it sees an empty stream. | NOT-A-DEFECT: current callers install; contract documented. |
| GW21 | apigroup.go:1179-1193 | L | `handleAPIEvents` spawns one goroutine per SSE client to fan power samples; teardown verified via request context. | NOT-A-DEFECT: per-client goroutine ends with the connection. |
| GW22 | gateway/gateway.go:716-730, 784-786 | — | Verified: sticky bind only on primary route, only on success, never overwrites an unexpired different binding; cooldowns reset on reload by design. | NOT-A-DEFECT (positive). |

---

## D. Protocol conversion (relaykit audit) — all will be replaced by the MIT rewrite
(report: protocol pass; confirms defects in `relaykit@v0.3.0`)

| ID | relaykit site | Sev | Finding | Disposition |
|----|---------------|-----|---------|-------------|
| PR1 | claude→chat converter | H | `tool_choice` silently dropped (force/none/any lost). | FIXED in new module. |
| PR2 | gemini→chat converter | H | `responseMimeType`/`responseSchema`/`toolConfig` dropped. | FIXED in new module. |
| PR3 | chat→claude converter | H | `response_format` dropped (Anthropic supports `output_config.format`). | FIXED in new module. |
| PR4 | responses→chat converter:223-241 | H | Inline `input` items of type `reasoning`/unknown → empty user message. | FIXED in new module. |
| PR5 | claude→chat stream converter | H | Mid-stream Anthropic `error` event swallowed; client sees success. | FIXED in new module. |
| PR6 | claude/gemini→chat converters | M | Converted streaming requests never set `stream_options.include_usage` → zero usage. | FIXED in new module. |
| PR7 | chat→claude:117-121 | M | `item.(string)` panic on non-string stop entry. | FIXED in new module (validation). |
| PR8 | chat→claude/gemini | M | `n>1` silently dropped. | FIXED: explicit error. |
| PR9 | gemini→chat | L | stopSequences truncated to 4 (matches OpenAI limit; Gemini allows 5). | NOT-A-DEFECT: target-spec limit. |
| PR10 | convmeta estimate | L | `message_start.usage.input_tokens` always 0. | FIXED: estimate. |
| PR11 | server normalization:556-595 | M | Error taxonomy lost (Gemini UNKNOWN, Anthropic always api_error, OpenAI code=type). | FIXED: status mapping. |
| PR12 | server:684 + 836 | M | Mid-stream conversion failure after commit → raw EOF, no terminal frame. | FIXED: terminal error event. |
| PR13 | server:699-713 | M | SSE splitter searches `\n\n` before `\r\n\r\n` → mixed-delimiter events merge. | FIXED: proper line parser. |
| PR14 | server:715-727 | L | `sseData` trims each data line and joins with `\n` → multi-line payload whitespace corrupted. | FIXED: spec-correct join (strip one leading space). |
| PR15 | anthropiccache normalize | L | Keeps only last user-message cache_control breakpoint (Anthropic allows 4). | PLANNED: preserve up to 4. |
| PR16 | responses_middleware.go:547-554 | L | `responseFromSSE` buckets by output_index, missing→0 → merges distinct items. | PLANNED. |
| PR17 | realtime/frame.go:345-349 | L | `validCloseCode` rejects registered 1012-1014. | PLANNED: add codes. |
| PR18 | backend_adapter_middleware.go:148-172 | L | Gemini `alt=sse` without protocol mark can be misclassified for adapter seam. | PLANNED: verify/adjust. |
| PR19 | server protocol TargetPath etc. | — | Verified good: version-prefix handling, gemini path model extraction, model rewrite in body/path, previous_response_id chain, affinity capture. | NOT-A-DEFECT (positive, to be ported). |
| PR20 | claude_messages/to_oai_chat_req.go (TopK) | L | Claude→Chat forwards non-standard `top_k`; strict OpenAI upstreams reject unknown args. | PLANNED: keep (vLLM/llama.cpp accept) but document; new module makes it opt-in via meta option. |
| PR21 | protocol_middleware.go:731 | L | Upstream `[DONE]` is ignored and the client sentinel is regenerated at Finalize; if the upstream dies before the sentinel but after a finish frame, the client still gets a normal end. | By design for conversion; new module keeps regeneration but adds terminal error on decoder failure. |

Compatibility matrix from the audit (to preserve): chat→anything good; Claude→Chat and
Gemini→anything weakest; all conversions pivot through Chat. The rewrite keeps the pivot
architecture but adds the missing field mappings.

---

## E. Process / runtime / stability
(report: process pass)

| ID | file:line | Sev | Finding | Disposition |
|----|-----------|-----|---------|-------------|
| PC1 | server/api.go (`handleUpstream`, `handleComfyUI`) | H | Resource-budget admission, automatic eviction, queueLoads and 507 semantics bypassed on raw passthrough routes → GPU OOM. | **FIXED** (admission added). |
| PC2 | perf/monitor.go:156-182 | H | sys-stats goroutine reads `m.stopCtx` per loop; UpdateConfig Stop/Start race leaks goroutine + duplicate samples. | **FIXED** (capture ctx). |
| PC3 | runtime/autoupdate.go:660-662, 686-859 | H | Pin checked once before slow network check; operator pin set mid-check is overridden (stage/activate). | PLANNED: re-check pin before stage/activate. |
| PC4 | modeldownload/manager.go:1558-1559, 1770-1781, 2052-2053 | H | Pre-existing full-size **sparse** `.part` with no manifest/SHA can be finalized (size-only verify). | DEFERRED: needs downloader state-machine rework; narrow trigger. Documented. |
| PC5 | router/base.go:1625-1633 | M | Shutdown timeout decorative (`<-done` after timer) → wedged child blocks daemon exit forever. | **FIXED** (return timeout error + procCancel). |
| PC6 | process/process_command.go:1060-1062 | H | No post-SIGKILL deadline; D-state child wedges run loop and every waiter. | DEFERRED: supervisor redesign; minimal fix unsafe. |
| PC7 | server/resource_api.go:171-173 | M | Eviction holds `resourceMu` for full unload timeout; all admissions serialize; 100ms-tick CacheState call per loaded model. | DEFERRED: staged-lock redesign. |
| PC8 | event/event.go:244 | M | Broadcast waits when any subscriber queue full; a blocking subscriber would stall process run loop. | DEFERRED: document contract; all current subscribers non-blocking. |
| PC9 | process/fifo.go:1193-1196 | M | Reserve underflow panics daemon instead of degrading; no reachable double-release found. | NOT-A-DEFECT (fail-loud) pending reachable case. |
| PC10 | process/runtime_windows.go + process_command.go:1060 | M | Windows `taskkill` synchronous on run-loop without timeout. | DEFERRED (Windows path). |
| PC11 | modeldownload/manager.go:669-711 | M | Transient error racing shutdown marks task failed instead of retryable. | PLANNED. |
| PC12 | modeldownload/manager.go:726-737 | L | `m.targets` map never pruned (mutex per repo ever seen). | PLANNED. |
| PC13 | modeldownload/manager.go:548-567 | M | Non-atomic FindActive+Create → duplicate active tasks for same repo. | PLANNED. |
| PC14 | server/api.go + server.go:1130 | M | `/upstream` and `/comfyui` also skip `forwardMaintenanceModel` (thermal parking) — distinct from budget. | PLANNED: call thermal guard. |
| PC15 | process/process_command.go:78 | — | Verified good: process-group kill, WaitDelay, cmd.Wait at every spawn site. | NOT-A-DEFECT (positive). |
| PC16 | logmon drain | — | Verified good: two-stage drain, queue-full drops counted/reported. | NOT-A-DEFECT (positive). |
| PC17 | server/server.go:1279-1288 (dup GW13) | M | `forwardAcrossSurfaces` buffers to 64MiB per request with no global concurrency bound. | DEFERRED (global memory budget). |
| PC18 | perf/monitor.go:99-120 | L | `UpdateConfig` discards accumulated ring samples on every config change (new buffers), so the UI loses history on reload. | NOT-A-DEFECT: documented restart semantics; could copy forward later. |

---

## F. Cross-cutting themes

1. **Body rewrite × replay**: `GetBody` is the single most dangerous seam — any rewrite that
   forgets it poisons retries/fingerprint (GW1, GW2, GW9, GW10). The rewrite helpers will be
   made the *only* way to replace a body, and they always refresh GetBody.
2. **Check-then-act without serialization**: extension KV limits (EX4), downloader enqueue
   (PC13), auto-update pin (PC3). All need per-key serialization.
3. **Silent degradation**: budget bypass (PC1), dropped tool rounds (EX10), swallowed stream
   errors (PR5/PR12), dropped request fields (PR1-PR8). Fix by surfacing errors or by
   carrying semantics through.
4. **Unbounded work in daemon memory**: images.apply (EX3), non-streaming extension rounds
   (EX7), forwardAcrossSurfaces (GW13). Needs global budgets or worker-side execution.
5. **License**: relaykit is **AGPL-3.0**; this repo is MIT. The protocol engine must be
   replaced with an in-repo MIT module (user directive). That rewrite also fixes PR1-PR14
   by design.


---

## G. Session outcome (append-only log)

### G1. License remediation (user directive)
`github.com/QuantumNous/new-api/relaykit` is **AGPL-3.0**; this repository is MIT.
Dependency removed entirely (`go.mod`/`go.sum` contain zero references). Replaced by a
new self-contained MIT module:

- `pkg/dialect/` — ~3,900 lines: canonical model + 4 request decoders + 4 request
  encoders + 4 response decoders/encoders + a 4×4 streaming state machine + per-dialect
  error envelopes. Standard library only; no server, config or store imports.
- `internal/protocol/relaykit/convert.go` — thin adapter preserving the previous call
  surface so `internal/server` needed no structural change.

### G2. Findings fixed in the new module (found by the independent review loop)
18 protocol defects, each covered by a regression test in
`pkg/dialect/review_regression_test.go`:

1. Claude tool_choice dropped (Claude→Chat).
2. Gemini responseMimeType/responseSchema/toolConfig dropped.
3. Chat response_format dropped targeting Claude (now `output_config.format`).
4. Inline Responses `reasoning` items became empty user messages.
5. Mid-stream Anthropic `error` events swallowed (now surfaced in every target).
6. Converted streams never requested `stream_options.include_usage`.
7. Non-string `stop` entries panicked (now diagnosed + dropped).
8. `n>1` silently collapsed (now rejected for single-choice targets; extra stream
   choices diagnosed and dropped).
9. `message_start.usage.input_tokens` always 0 (estimate seeded from the request).
10. Error taxonomy flattened (status→per-dialect error type/code/status mapping).
11. Mid-stream conversion failure left a truncated stream (terminal error frame emitted).
12. SSE splitter merged mixed LF/CRLF events.
13. `sseData` trimmed significant whitespace.
14. Gemini stream requests converted to Claude lost `stream`.
15. Anthropic/Gemini tool results dropped when targeting Responses.
16. Parallel/interleaved Chat tool calls emitted deltas after `content_block_stop`.
17. Usage lost/corrupted (Chat ignored message_start; totals inconsistent).
18. Gemini double finish frame overwrote MAX_TOKENS/SAFETY; streamed function calls had
    empty ids; same-frame tool call mapped to `stop`; Responses output_index mismatch;
    Responses assistant item order; mid-conversation system turns hoisted; `file_url`
    unread; non-base64 data URLs mis-typed; Gemini PDFs typed as images; missing
    json_schema name; failed upstream Responses returned as 200 success; legacy
    function_call ids empty; hosted-search strip deleted sibling declarations;
    `[DONE]` emitted twice; disable-parallel semantics lost; O(n²) Gemini conversion.

### G3. Verification evidence
- `go build ./...` clean.
- `go test -short ./...` clean (all packages).
- `go test -race` clean on perf, gateway, modeldownload, extensions, dialect.
- `go vet` and `staticcheck` clean on every changed package.
- Benchmarks (Apple M5, arm64): Chat→Claude request 10.9 µs / 18.7 KB / 297 allocs;
  Chat→Gemini 13.3 µs / 23.6 KB / 359 allocs; streaming chunk 4.6 µs / 8.8 KB.
- Mutation checks: reverting the `execForward` merge order and the perf context capture
  makes the new tests fail (the perf one via the race detector).

### G3b. Independent review loop (5 rounds)
Rounds 1-4 each produced findings that were fixed and re-verified; round 5 returned
`SAFE TO MERGE: YES` with zero open High/Medium items. Cumulative protocol defects found
and fixed by the loop: 34 (18 in the first pass, 10 in round 2, 5 in round 3, 3 in
round 4, 2 robustness notes in round 5). Test suite for the module: 102 passing tests
plus 3 benchmarks.

### G3c. User-selected deferred items — now FIXED
- **D1** process teardown deadline: `killProcess` now waits for `cmdDone` at most
  `processKillWaitTimeout` (30s) after SIGKILL and abandons the wait with an error log,
  so one unkillable child can no longer wedge the run loop. Callers can still Stop/Run.
- **D2** sparse `.part`: a full-size part with no advertised checksum is now rejected when
  the filesystem reports fewer allocated blocks than its size (`fileIsSparse`, unix) and
  the download restarts. Windows reports false (documented).
- **D3** eviction lock: admission was split into `evaluateResourceLoad` (under
  `resourceMu`, no I/O) plus out-of-lock `releaseResourceCandidate` calls; a slow unload
  no longer serializes unrelated cold starts. An already-attempted eviction is not
  repeated within one admission.

Each has a regression test, and each test was mutation-verified (reverting the fix makes
it fail): `TestModelDownload_SparsePartWithoutChecksumIsNotFinalized`,
`TestServer_EvictionDoesNotHoldAdmissionLock`.

### G3d. Real end-to-end evidence (not just unit tests)
- **Server-level 4×4 matrix** (`internal/server/e2e_protocol_matrix_test.go`): 16
  combinations through the real middleware chain; 16/16 pass.
- **Module round trip** (`pkg/dialect/e2e_roundtrip_test.go`): a 5-turn conversation with
  a tool call converted Chat→{Responses,Claude,Gemini}→Chat preserves system prompt,
  prompt, tool name/arguments, tool result and final turn; 3/3 pass.
- **Streaming matrix**: 12 ordered dialect pairs fed real upstream frames; every client
  gets the text and its own terminal frame.
- **Live process test**: built the binary with `-tags embed_ui`, ran it against three fake
  HTTP upstreams (OpenAI chat, Anthropic, Gemini) configured as providers, and curled all
  4 client dialects × 3 upstreams plus 4 streaming shapes: **32/32 pass**.
- This live run exposed one real defect not visible in unit tests: an upstream that
  returns **bare JSON when SSE was requested** produced opening frames and then nothing
  (empty answer). Fixed in `protocolStreamWriter.finish` via `forwardBareJSON`, with
  `TestProtocol_UpstreamSendsJSONWhenSSEWasRequested` (mutation-verified).

### G3e. relaykit removal — proof
- `grep -c 'QuantumNous\|new-api/relaykit' go.mod go.sum` → 0 / 0
- `grep -rn` over all Go sources → 0 hits
- `go list -m all | grep -i relaykit` → empty
- No `vendor/` directory exists.
- `pkg/dialect` imports only the standard library + local packages.

### G4. Still open (documented, not fixed here)
See section 3 (D1-D9) and the DEFERRED rows above. The most consequential are:
- D1 (process teardown has no post-SIGKILL deadline),
- D2 (sparse `.part` accepted without a checksum in a narrow window),
- D3 (eviction holds `resourceMu`),
- AR2 (session cookie carries the raw API key),
- AR3 (X-Forwarded-For trusted for IP allowlists).
