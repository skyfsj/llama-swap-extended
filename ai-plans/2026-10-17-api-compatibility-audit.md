# LLM API compatibility audit (2026-10-17)

Scope: does llama-swap correctly and completely serve each vendor's LLM API, and
what happens on every conversion path? Everything below was read in source and
checked against the current official references; live-process tests were run for
the claims marked "verified live".

Official references consulted (fetched, not recalled):
- https://developers.openai.com/api/reference/responses/overview (+ create response reference)
- https://ai.google.dev/api (endpoint index) and https://ai.google.dev/api/interactions-api
- Anthropic Messages is the dialect already implemented and exercised by the existing suite.

## 1. Verdict: are the five APIs implemented?

| API | Status | Evidence |
|---|---|---|
| OpenAI Chat Completions | **Complete for a proxy's job** | `pkg/dialect/decode_request.go` + `encode_request.go`; routes via `internal/route/registry.go:164`. Live-verified 4 client dialects × chat upstream. |
| OpenAI Responses | **Substantially complete, 4 gaps** | Decode/encode + streaming state machine in `pkg/dialect/{decode,encode}_request.go`, `stream.go`; affinity store in `internal/server/responses_middleware.go`. Gaps: no `background`, no `conversation`, no `context_management`/compaction, no `access_programs`; hosted-item replay limited. |
| Anthropic Messages | **Complete for a proxy's job** | Full request/response/stream conversion incl. thinking blocks, `output_config.format`, tool_choice, cache breakpoints (`internal/protocol/anthropiccache/cache.go`). |
| Gemini GenerateContent | **Implemented, one correctness hole fixed today** | `thoughtSignature` was being dropped (see §3.1); fixed. `cachedContent` still unsupported (see §3.2). |
| **Gemini Interactions API** | **NOT IMPLEMENTED AT ALL** | Google's reference lists `CreateInteraction` (`POST /v1beta/interactions`, stable `/v1/interactions`) as *"the recommended standard primitive for building with Gemini, optimized for agentic workflows, server-side state management, and complex multi-modal, multi-turn conversations."* Zero occurrences of `interactions`/`previous_interaction_id` in the repository. |

### 1.1 Gemini Interactions API — what the official spec requires
Request body: `model` (ModelOption) **or** `agent` (AgentOption: deep-research, antigravity, …),
`input` (string | Content | array of Content | array of **Step**), `system_instruction`,
`tools[]`, `response_format`, `stream`, `store`, `background`,
`generation_config` (max_output_tokens, seed, speech_config, …), `previous_interaction_id`,
`safety_settings`.
Response: `object: "interaction"`, `id`, `created`, `updated`, `status`, `steps[]`
(`model_output` carrying `content[]`, plus thought/tool/url-context steps), and a
modality-broken-down `usage` (`input_tokens_by_modality`, `total_thought_tokens`,
`total_tool_use_tokens`, `total_cached_tokens`, …).
Streaming: typed **deltas** including `thought_signature`, `thought_summary`,
`url_context_call`, `url_context_result`, tool call/result deltas.

This is a genuinely different shape from `generateContent` (steps + stateful
`previous_interaction_id` + modality usage). It cannot be served by the existing
Gemini codec; a client using it gets a 404 today.

## 2. Feature matrix (inference-relevant surface)

| Feature | Chat | Responses | Anthropic | Gemini | Where |
|---|---|---|---|---|---|
| Text in/out | ✅ | ✅ | ✅ | ✅ | pkg/dialect |
| System / instructions | ✅ | ✅ (`instructions`, developer items) | ✅ | ✅ | same |
| Images (base64 + URL) | ✅ | ✅ | ✅ | ✅ | `decode_request.go` media paths |
| PDF/documents | ❌ (diagnosed drop) | ✅ | ✅ `document` | ✅ inline/document | same |
| Audio / video parts | ❌ | audio: diagnosed drop | ❌ | ⚠ coerced to document, no diagnostic | `mediaKind` |
| Tool definitions | ✅ | ✅ | ✅ | ✅ | encoders |
| Tool choice (auto/none/required/named) | ✅ | ✅ | ✅ | ✅ | `encode*ToolChoice` |
| Parallel tool calls | ✅ | ✅ | ✅ | n/a (diagnosed) | same |
| Tool result correlation | ✅ | ✅ | ✅ | ⚠ synthetic ids (`call_gemini_N`) | `geminiCallTracker` |
| Reasoning / thinking | ✅ `reasoning_content` | ✅ `reasoning` items | ✅ thinking + signature | ✅ thought parts + **thoughtSignature (fixed today)** | same |
| Encrypted reasoning | pass-through only | diagnosed drop without `include` | `redacted_thinking` ✅ | n/a | same |
| Structured output | ✅ | ✅ | ✅ `output_config.format` | ✅ responseMimeType/Schema | same |
| Stop sequences | ✅ (4) | diagnosed drop | ✅ (4) | ✅ (5) | same |
| `n>1` | ✅ | rejected with 400 | rejected with 400 | ✅ candidateCount | same |
| Usage accounting | ✅ | ✅ | ✅ incl. cache | ✅ incl. cached/thought | `usage*` helpers |
| Streaming SSE | ✅ | ✅ typed events | ✅ typed events | ✅ | `stream.go` |
| Streaming usage request | ✅ `stream_options.include_usage` | ✅ | n/a | n/a | `encodeChatRequest` |
| Token counting | passthrough | passthrough | passthrough (`/v1/messages/count_tokens`) | passthrough (`:countTokens`) | route registry |
| Embeddings / rerank / audio / images | passthrough by model | — | — | — | route registry |

## 3. Findings fixed in this pass

### 3.1 Gemini `thoughtSignature` was dropped on every conversion (High, FIXED)
Gemini 3 attaches an opaque `thoughtSignature` to `functionCall` and `thought`
parts and **rejects a replay** whose signature was removed. The converter had no
field for it (`grep thoughtSignature` → 0 hits), so any Gemini→X→Gemini round trip
lost it silently. Fixed in `pkg/dialect/{types,decode_request,encode_request,stream}.go`:
the canonical `Part`/`Event` carry it, the Gemini encoder re-emits it, and every
dialect that cannot represent it now reports `thought_signature_dropped`.
Tests: `TestGeminiThoughtSignaturePreservedInRequests`,
`TestGeminiThoughtSignatureSurvivesRoundTripThroughConcepts`,
`TestGeminiStreamKeepsThoughtSignature`, `TestE2E_ThoughtSignatureThroughAdapter`;
**verified live** against a running proxy (upstream printed the signature).

### 3.2 Extension server tools were advertised but never executed for Gemini (High, FIXED)
`injectGeminiTools` put the extensions' tools into the request, but the response
side had no Gemini branch: `extensionCalls` fell through to the Responses parser
(`response["output"]`), found nothing, and the model's `functionCall` was handed
to the client — which cannot run an operator-installed tool. Fixed by adding the
Gemini branch to `extensionCalls`, a `gemini.generateContent` continuation turn in
`extensionContinue`, and a full streaming path (`consumeGemini`,
`ContinueResponse`, `FinishClientCalls`).
Tests: `TestServer_ExtensionsGeminiServerToolExecutes`,
`TestServer_ExtensionsGeminiStreamServerToolExecutes` (both mutation-verified).

### 3.3 Stored Responses were readable by any key that could use the model (High, FIXED)
`response_affinity` had no owner column, and both GET and cancel checked only
`modelAllowedForIdentity`. Added `identity_id` (migration `00025`), written on
every persist and enforced on read/cancel. Pre-migration rows keep the old
model-scoped behaviour so an upgrade strands nothing.
Tests: `TestServer_StoredResponseIsScopedToItsKey`,
`TestServer_LegacyStoredResponseStaysModelScoped` (mutation-verified).

### 3.4 `POST /v1/responses/{id}/cancel` claimed a cancellation it never performed (Medium, FIXED)
The handler rewrote a finished response's status to `cancelled` while never
touching the in-flight request (which has no stored row at all). It now returns
the response unchanged, and the 404 for an unknown id says that a live request is
cancelled by closing its connection. Tests: `TestServer_ResponseCancelDoesNotRewriteStatus`,
`TestServer_ResponseCancelUnknownIdSaysWhy`, plus the updated end-to-end test.

### 3.5 Fallback Responses ids collided within one second (Medium, FIXED)
`resp_<unix>` was used when the upstream supplied no id; two responses in the same
second produced the same id and the affinity upsert overwrote the first row. Now
`resp_<unix>_<8 hex>` (`newResponseID`).

## 4. Open findings (not fixed, with reasons)

| # | Sev | Finding | Evidence | Why open |
|---|---|---|---|---|
| O1 | High | **Gemini Interactions API unsupported** (`/v1/interactions`, `/v1beta/interactions`) | §1.1; 0 hits repo-wide | New dialect: needs decoder, encoder, step model, stateful `previous_interaction_id`, streaming deltas (~2k lines + tests). Feature work, not a bug fix. |
| O2 | High | **No Files APIs**: `/v1/files`, `/v1/uploads`, `/v1beta/files`, vector stores | No route descriptors; `internal/route/registry.go:160-207` | A proxy that does not own storage cannot serve them correctly; needs a design decision (proxy them per-upstream, or bridge to local cache). |
| O3 | High | **Hosted items poison `previous_response_id`** — replay of a stored response containing `web_search_call`/`computer_call`/`mcp_call`/… returns 400 for every follow-up | `responses_middleware.go:150-151` errors on unknown types | Fix is to skip (with a diagnostic) rather than error; needs a compatibility decision on silently dropping provider-side artifacts. |
| O4 | Medium | **Audio/video coerced to documents** without diagnostic; a Gemini audio part becomes an Anthropic `document` with `media_type: audio/*` | `decode_request.go:1021-1028`, `encode_request.go:645-671` | Needs `PartAudio`/`PartVideo` plus per-target capability checks. |
| O5 | Medium | **`background:true` unsupported** — no job API, so Responses background mode cannot be served | 0 hits | Ties to a background-job subsystem. |
| O6 | Medium | **`conversation` / `store`+`conversation` items unsupported**; `context_management` (compaction) ignored | 0 hits | Requires a server-side conversation store. |
| O7 | Medium | **Gemini `cachedContent` ignored** on conversion (silently, no diagnostic) and no `cachedContents` API | `decodeGeminiRequest` does not read it | Cheap part (diagnostic) is worth doing; full support needs upstream cache management. |
| O8 | Medium | **Orphan Gemini tool result** can produce a `tool_call_id` that no assistant call declares | `decode_request.go:809-811` | Fix = synthesize + diagnose; small. |
| O9 | Medium | **`openai-web-search` preset is inert** for chat/converted requests because the protocol adapter strips `web_search` after injection | `protocol_middleware.go:88-96` strips; injection happens later in the chain | Needs a marker so client-sent hosted tools are stripped but extension-injected ones are kept. |
| O10 | Low | `prompt_cache_retention` never modelled (dropped on conversion, no diagnostic) | 0 hits | Trivial field pass-through. |
| O11 | Low | Anthropic `cache_control` breakpoints dropped without diagnostic when converting Claude→Chat | `anthropic_middleware.go` rewrites, then `pkg/dialect` has no field | Diagnostic only. |
| O12 | Low | `extended-cache-ttl` beta header is never set although the cache fixer writes `ttl: 1h` | grep: no `anthropic-beta` hits | Needs a config decision (beta entitlement varies per account). |
| O13 | Low | `readTree`/`writeTree` do not reject a pre-existing symlink inside an extension directory | `internal/extensions/files.go:139-189` | Requires local filesystem access to the extension root. |
| O14 | Low | Chat multi-choice (`n>1`) server-tool calls in choices≥1 are not executed or withheld | `extensions_middleware.go:1216-1220` | Same class as the round-3 dialect fix; needs the extension loop to iterate choices. |

## 5. Conversion matrix (loss profile)

Legend: ✅ lossless · ⚠ lossless for representable content, diagnosed otherwise · ❌ dropped with diagnostic.

| source ↓ / target → | Chat | Responses | Anthropic | Gemini |
|---|---|---|---|---|
| **Chat** | pass-through | ✅ (n>1 rejected) | ⚠ (thinking/`cache_control` dropped; `response_format`→`output_config.format`) | ⚠ (`response_format`→schema; `n`→candidateCount) |
| **Responses** | ✅ (reasoning items preserved; hosted items dropped) | pass-through | ⚠ (same as Chat→Anthropic) | ⚠ |
| **Anthropic** | ⚠ (thinking signature → `reasoning_content`; `cache_control` dropped) | ⚠ (tool results split into `function_call_output`) | pass-through | ⚠ (thinking→thought; signature dropped) |
| **Gemini** | ⚠ (**thoughtSignature dropped, now diagnosed**; tool ids synthesized) | ⚠ | ⚠ | pass-through (**signature preserved**) |

Notable in all directions: images survive; PDFs survive to Responses/Anthropic/Gemini but
drop to Chat; audio/video degrade (O4); hosted provider tools are dropped with diagnostics
(never simulated); `n>1` is rejected wherever the target has one output slot.

## 6. Server-side tools matrix

| Tool | Executing party | Support | Notes |
|---|---|---|---|
| Extension `defineTool` (server execution) | **gateway** (goja/node worker) | Chat ✅ / Responses ✅ / Anthropic ✅ / **Gemini ✅ after this pass** | isolated worker, per-extension storage sandbox |
| Extension `defineTool` (client execution) | **client** | Chat/Responses/Anthropic ✅ | re-id'd `call_ls_*` pending record with owner check |
| `ctx.forward` (`models.forward`) | gateway | ✅ (depth ≤ 2, body cap) | inner chain skips extensions |
| Web-search presets (brave/tavily/exa/…) | gateway worker | ✅ | manifest-declared `networkHosts`, SSRF guard |
| `openai-web-search` preset | upstream | ⚠ **inert** (O9) | strip happens after injection |
| Client-sent `web_search`/`web_search_options` | upstream | stripped with diagnostic | prevents upstream 400s |
| `file_search`, `code_interpreter`, `computer_use`, `mcp`, `local_shell`, `tool_search`, `image_generation`, `googleSearch`, `codeExecution` | none | dropped with diagnostic | never faked |

## 7. Storage & cache matrix

| Item | Storage | Lifetime | Restart-visible | Isolation |
|---|---|---|---|---|
| Responses canonical body | `response_affinity.response_json` | 30 days, 4 MiB/row | only with `store.path` | **owner key (fixed today)**, legacy rows model-scoped |
| `previous_response_id` chain | same row | 30 days | same | same |
| Cancel state | none (cancellation is client-disconnect) | — | — | — |
| Extension pending turn | `extension_pending` (+ identity) | 1 h | with `store.path` | identity + profile + endpoint |
| Extension KV | per-extension namespace | manifest `storage` | persistent only | manifest id stamped by the worker |
| Audit bodies/blobs | SQLite + sha256 CAS | retention/byte budget | yes | key-scoped filters |
| Anthropic prompt cache | stateless repair middleware | — | — | — |
| Gemini/OpenAI prompt caches | not implemented (pass-through parameters only) | — | — | — |
| LMCache | supervised external server | per model | — | refcounted |

## 8. Test report

- `go build ./...` — clean.
- `go test -short ./...` — **40 packages, 0 failures**.
- `go test -race ./pkg/dialect/ ./internal/store/ ./internal/gateway/ ./internal/perf/` — clean.
- `go vet` + `staticcheck` on every changed package — clean.
- New tests this pass: 3 thought-signature, 2 Gemini-extension (non-stream + stream),
  4 affinity/isolation/cancel, 1 live-signature E2E; 3 of them mutation-verified in
  addition to the ones verified inline while implementing.
- Live process test: built with `-tags embed_ui`, three fake upstreams (Chat, Anthropic,
  Gemini) configured as providers; 4 client dialects × 3 upstreams + 4 streaming shapes
  = 32/32 pass (previous session), plus today's signature round trip against a running
  proxy.
- Not verified here (needs real vendor credentials): behavior against api.openai.com,
  api.anthropic.com and generativelanguage.googleapis.com; Gemini Interactions (no
  implementation to test); Files APIs (not implemented). These are marked 尚未验证, not
  claimed as working.
