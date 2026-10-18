# Plan: close the API-compatibility gaps + SDK conformance verification

Source: ai-plans/2026-10-17-api-compatibility-audit.md (open findings O1-O14).

## Verification tooling (installed, verified)
- `openai` 2.30.0 — Chat Completions + **Responses** client (typed, strict parsing)
- `anthropic` 1.15.0 — Messages client
- `google-genai` 2.31.0 — **`client.interactions.create/get/cancel/delete`** exists, so the
  Interactions dialect can be verified with the official SDK, not hand-rolled JSON.

Authoritative Interactions schema taken from the SDK's generated types
(`google/genai/_gaos/types/interactions/*`), not from prose:
- request: `model`, `input` (string | Content | Content[] | Step[]), `system_instruction`,
  `tools`, `response_format`, `response_mime_type`, `generation_config`, `stream`, `store`,
  `background`, `previous_interaction_id`, `cached_content`, `safety_settings`, `labels`,
  `response_modalities`, `service_tier`, `webhook_config`, `environment`
- response: `id`, `created`, `updated`, `status`, `model`, `steps[]`, `usage`, `errors`
- steps: `user_input{content[]}`, `model_output{content[]}`, `thought{summary[], signature}`,
  `function_call{id,name,arguments}`, `function_result{call_id,result,is_error,name}`, plus
  provider-hosted step types (google_search/file_search/code_execution/url_context/mcp/…)
- usage: `total_input_tokens`, `total_output_tokens`, `total_thought_tokens`,
  `total_tool_use_tokens`, `total_cached_tokens`, `total_tokens`, per-modality breakdowns
- streaming: `interaction.created`, `step.start`, `step.delta`, `step.stop`,
  `interaction.completed`, `error`; deltas: `text`, `arguments`, `thought_summary`,
  `thought_signature`, `function_result`, `image`, `audio`, `video`, hosted-tool deltas

## Phase A — protocol bug fixes (all confirmed by reading code)
| # | Fix |
|---|---|
| O3 | `previous_response_id` replay: unknown/hosted output items are skipped with a diagnostic instead of failing the whole request with 400 |
| O4 | Add `PartAudio`/`PartVideo`; stop coercing non-image media into documents; diagnose per target |
| O7 | Gemini `cachedContent` → diagnostic when the target cannot honour it (and never silently drop) |
| O8 | Orphan Gemini `functionResponse` gets a synthesized id + diagnostic instead of an unpaired `tool_call_id` |
| O9 | Hosted search tools injected by an extension must survive the protocol adapter's strip (mark the request) |
| O10 | `prompt_cache_retention` modelled + diagnosed when dropped |
| O11 | Anthropic `cache_control` diagnosed when converting to a target that cannot carry it |
| O14 | Extension tool loop: inspect every choice (n>1), not just `choices[0]` |

## Phase B — Gemini Interactions dialect (O1)
- `pkg/dialect/format.go`: recognize `POST /v1/interactions`, `/v1beta/interactions`
  (plus `/{id}`, `/{id}/cancel` handled by the server, not the codec)
- `decode_interactions.go`: request → canonical (`input` string/Content[]/Step[],
  `system_instruction`, tools, tool_choice, generation_config, response_format,
  previous_interaction_id, store/stream)
- `encode_interactions.go`: canonical → Interactions request (for a provider that speaks it)
- `response.go`: decode `interaction` (steps → canonical parts, usage → canonical usage)
  and encode canonical → `interaction`
- `stream.go`: canonical events ↔ Interactions SSE (`step.start`/`step.delta`/`step.stop`,
  `interaction.created`/`completed`, delta payloads incl. `thought_signature`)
- server: route registration, protocol target table, streaming path, extension endpoint
  (`gemini.interactions`), provider surface `interactions`

## Phase C — SDK conformance tests (real clients, real HTTP)
Script harness in `test/sdk-conformance/`:
1. Start fake upstreams (Chat / Anthropic / Gemini generateContent / Gemini Interactions)
2. Start llama-swap with those providers
3. For each SDK: drive Chat, Responses, Anthropic, generateContent, Interactions against
   every upstream surface; assert typed responses parse, text/tool calls/usage/finish
   reasons are correct, streaming terminates in the dialect's terminal event
4. Cross-dialect matrix: each SDK against each upstream surface (SDK = client dialect)
Failures are reported per case; the harness is checked in so it can be re-run.

## Phase D — independent review loop
Read-only reviewer over the six dimensions (requirements, logic, boundaries, quality,
test coverage, actual run results); fix findings; re-review until pass or a documented block.

## Out of scope (documented, with reason)
- Files/Uploads/Vector Stores APIs: a proxy that owns no storage cannot serve them
  correctly; needs a product decision (per-upstream passthrough vs local cache).
- `background:true` / `conversation`: needs a job + conversation store; the Interactions
  dialect covers the stateful-conversation use case natively (`previous_interaction_id`).
