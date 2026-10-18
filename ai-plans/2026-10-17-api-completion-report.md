# API completion report: Gemini Interactions dialect + audit-finding fixes (2026-10-17)

Companion to `2026-10-17-api-compatibility-audit.md` (findings) and
`2026-10-17-api-completion-plan.md` (plan). This file records what was built,
what the official SDKs proved, and what the review loop found and closed.

## 1. What changed

### 1.1 New dialect: Gemini Interactions (`pkg/dialect/`)
Google's reference lists `CreateInteraction` (`POST /v1beta/interactions`, stable
`/v1/interactions`) as *"the recommended standard primitive for building with
Gemini"*. It was unimplemented; it now is:

| File | Contents |
|---|---|
| `format.go` | `GeminiInteractions` format; routes `/v1beta/interactions`, `/v1/interactions`, `/v/interactions`; target path |
| `decode_interactions.go` | request decode: `input` as string / Content / Content[] / Step[] (21 documented step types), `system_instruction`, `tools` (flat `type:"function"`), `generation_config` (incl. `thinking_level`), `response_format` (TextResponseFormat), `tool_choice` (`allowed_tools{mode,tools}`), stateful fields into `Extra` |
| `encode_interactions.go` | request encode + response decode/encode: `steps[]` (model_output / thought+signature / function_call / function_result), `usage` totals incl. `total_thought_tokens` and `total_tool_use_tokens` |
| `stream_interactions.go` | streaming both ways: `interaction.created` / `step.start` / `step.delta` / `step.stop` / `interaction.completed` / `error`; deltas `text`, `arguments_delta`, `thought_summary`, `thought_signature` |

Server wiring: route descriptors (`internal/route/registry.go`), provider type
`gemini-interactions` (`internal/config/provider.go`, gateway auth mapping, config
validation), surface/target tables (`internal/server/protocol_middleware.go`),
backend capability, usage parsing (`internal/server/metrics.go` understands
`total_input_tokens`/`total_output_tokens`/`total_cached_tokens`), stateful
passthrough routes for `GET /{id}`, `POST /{id}/cancel`, `DELETE /{id}`
(`interactionsPassthrough`, 501/503 with an explanation when no such provider is
configured), config schema enum, UI type union + select + locale labels, and the
provider probe (URL + `x-goog-api-key`).

### 1.2 Audit findings O3-O14
| # | Fix |
|---|---|
| O3 | replay of a stored Response skips provider-hosted items with a warning instead of failing the follow-up with 400 |
| O4 | `PartAudio`/`PartVideo`; every target diagnoses what it cannot carry instead of emitting an invalid document block |
| O7 | `cachedContent`/`cached_content` diagnosed on Chat/Claude/Gemini/Responses |
| O8 | orphan Gemini `functionResponse` dropped with a diagnostic and without reserving a call id |
| O9 | hosted tools injected by an extension hook survive the adapter strip (`hostedToolsBefore` + `markInjectedHostedTools`) |
| O10 | `prompt_cache_retention` modelled and diagnosed |
| O11 | Anthropic `cache_control` breakpoints counted and diagnosed on non-Anthropic targets |
| O14 | the extension loop collects tool calls from every choice (non-stream and stream) |
| M5 | the Interactions endpoint is deliberately not offered to extensions until the loop supports Steps (no half-working config) |

## 2. Verification with the vendors' official SDKs

`test/sdk-conformance/` (checked in):
- `fake_upstreams.py` — four upstreams speaking Chat, Anthropic Messages,
  generateContent and Interactions, each with non-stream, stream, tool-call and
  stateful paths.
- `conformance.py` — drives a live llama-swap with `openai` 2.30.0 (Chat +
  Responses), `anthropic` 1.15.0, `google-genai` 2.31.0 (generateContent +
  Interactions, including `get`/`cancel`/`delete`).

**Result: 29/29 pass** — 15 single-dialect cases (each SDK's non-stream, stream,
tool-call, plus Interactions `get`/`cancel`/`delete`/`requires_action`) and 9
cross-dialect cases (each client SDK against a different upstream surface).

The SDKs found real protocol bugs that byte-level tests missed:
- Interactions streamed tool arguments use the discriminator `arguments_delta`
  (the SDK's union), not `arguments` — both directions were wrong.
- The Interactions `Tool` is flat (`type:"function"` with `name`/`parameters` at
  the top level); a nested `{"function": {...}}` is rejected.
- `tool_choice` is `{"allowed_tools": {"mode": ..., "tools": [...]}}`.
- `response_format` is `{"type":"text","mime_type":...,"schema":...}`.
- `thoughtSignature` must be valid base64.
- `status:"requires_action"` is how a tool-call interaction reports itself.

## 3. Independent review loop (3 rounds, read-only reviewer)

Round 1 found 3 Critical, 5 High, 11 Medium: the `arguments_delta` discriminator,
tool results dropped when Claude/Gemini convert to Interactions, `Content[]`
input misread as Steps, `requires_action` rendered as `stop`, stateful fields
dropped silently, missing get/cancel/delete routes, the orphan id reservation,
and the O9 marker not covering hook-injected tools.

Round 2 confirmed the fixes, re-ran the harness (29/29), and found 5 remaining
must-fix items plus 7 non-blocking ones; all were fixed:
streaming `failed`/`cancelled` surfaced as success, tool-choice mode losing to the
single-name shortcut, the H5 baseline captured after the hooks, the bare-list
`function_result` decode, and the orphan id reservation.

Round 3 found one must-fix (the stream-bridge forward path did not mark hook-injected
hosted tools) plus five non-blocking items; all were fixed, including aligning every
documented interaction status between the streaming and non-streaming paths
(`budget_exceeded`, `requires_action` without a call, `queued`/`in_progress`/`running`).

Round 4 verified those fixes and found one remaining status gap (`budget_exceeded`
streaming), which was fixed with a streaming counterpart test.

**Round 5 (final acceptance): SAFE TO MERGE — yes.** No Medium-or-higher findings; the
only notes were an unknown/future-status default divergence (not reachable with any
documented status) and the by-design omission of an empty error `code` field.
The full status matrix (completed / incomplete / failed / cancelled / requires_action /
queued / in_progress / running / budget_exceeded) behaves identically on both paths.

## 4. Test evidence

- `go build ./...`, `go vet`, `staticcheck` on every changed package: clean.
- `go test ./pkg/dialect/`: 40+ tests including 30 Interactions cases; `-race` clean.
- `go test -short ./...`: every package passes except
  `TestPreset_DateTimeTableMatchesSystemTzdata`, which is a concurrently edited
  datetime-tools preset whose generated tzdata table is stale (unrelated to this
  work; the failure message names the regeneration command).
- Mutation checks: reverting each of the H5 marker, the `arguments_delta` alias,
  the Content[] classification, and the three round-1 stream fixes makes the
  corresponding test fail.
- SDK harness: 29/29 against a live proxy with four upstream surfaces.

## 5. Still open (with reasons)
| # | Sev | Item | Reason |
|---|---|---|---|
| O2 | High | Files/Uploads/Vector Stores APIs | a proxy that owns no storage cannot serve them; needs a product decision |
| O5 | Medium | `background:true` | needs a job store; Interactions covers the stateful-conversation case natively |
| O6 | Medium | `conversation`, `context_management` (compaction) | needs a server-side conversation store |
| O13 | Low | extension tree symlink entries | requires local filesystem access to the extension root |
| — | Low | extension server tools on the Interactions surface | the loop has no Steps branch; the endpoint is withheld rather than half-working |
| — | Low | multi-provider ambiguity in `interactionsPassthrough` | first matching route wins; a stateless passthrough cannot know which provider owns an id |
