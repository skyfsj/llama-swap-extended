# dialect

Convert LLM API requests, responses and streams between five vendor dialects —
with a canonical model in the middle, structured diagnostics instead of silent
data loss, and no dependencies outside the Go standard library.

```go
import "github.com/mostlygeek/llama-swap/pkg/dialect"
```

MIT licensed (see `LICENSE.md` at the repository root).

## Dialects

| Dialect | Constant | Wire surface |
|---|---|---|
| OpenAI Chat Completions | `dialect.OpenAIChat` | `POST /v1/chat/completions` |
| OpenAI Responses | `dialect.OpenAIResponses` | `POST /v1/responses` |
| Anthropic Messages | `dialect.ClaudeMessages` | `POST /v1/messages` |
| Google Gemini generateContent | `dialect.Gemini` | `POST /v1beta/models/{model}:generateContent` |
| Google Gemini Interactions | `dialect.GeminiInteractions` | `POST /v1beta/interactions` |

Every pair converts: requests, whole responses and streaming SSE.

## Quick start

Recognize a request from its path:

```go
route, ok := dialect.RouteForRequest(r.Method, r.URL.Path)
if !ok {
    // not a generation request
}
// route.Format, route.Stream, route.Model (Gemini only), route.Action (Gemini only)
streamed := dialect.RequestStreamed(route, body)
path, _ := dialect.TargetPath(dialect.ClaudeMessages, "claude-sonnet-4-5", streamed)
```

Convert a request:

```go
converted, diagnostics, err := dialect.ConvertRequest(
    dialect.OpenAIChat, dialect.ClaudeMessages, body,
    dialect.ConvertOptions{Model: "claude-sonnet-4-5"},
)
if err != nil {
    // hard failure: the request cannot be represented in the target dialect
    // (for example n > 1 for a single-response dialect)
}
for _, d := range diagnostics {
    // soft loss: a field the target cannot carry
    log.Printf("conversion note: %s [%s] at %s", d.Message, d.Code, d.Path)
}
```

Convert a whole response:

```go
converted, diagnostics, err := dialect.ConvertResponse(
    dialect.ClaudeMessages, dialect.OpenAIChat, upstreamBody)
```

Convert a stream frame by frame:

```go
converter, err := dialect.NewStreamConverter(
    dialect.OpenAIChat, dialect.ClaudeMessages,
    dialect.StreamOptions{Model: "m", IncludeUsage: true},
)
for _, payload := range upstreamSSEDataPayloads {
    frames, err := converter.Feed(payload) // []dialect.Frame{Event, Data}
    for _, f := range frames {
        writeSSE(f) // f.Data == "[DONE]" marks the Chat sentinel
    }
}
frames, err := converter.Finish() // terminal frames for the client dialect
for _, d := range converter.Diagnostics() {
    log.Printf("stream conversion note: %s [%s]", d.Message, d.Code)
}
```

Normalize an upstream error for the client:

```go
body := dialect.ErrorEnvelope(upstreamBody, dialect.Gemini, upstreamStatus)
// → {"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"..."}}
```

## Design

```
wire A ──decode──▶ canonical ──encode──▶ wire B
```

* **One canonical model.** `Request`, `Response` and `Event` describe every
  dialect. Adding a dialect means one decoder and one encoder, not N×N
  converters.
* **Diagnostics, not silence.** Anything the target cannot carry is reported as
  a `Diagnostic` with a stable `Code` (`tool_result_dropped`,
  `thought_signature_dropped`, `cached_content_dropped`, …). Nothing is
  simulated: a provider-hosted tool that has no equivalent is dropped and named,
  never faked as executed.
* **Streaming is event based.** A source decoder turns each upstream frame into
  canonical `Event` values; a stateful encoder renders them in the client
  dialect, preserving the order of text, reasoning, tool-call argument deltas,
  usage and the terminal frame.
* **No panics on malformed input.** Invalid JSON, wrong types, unknown enum
  values and missing ids produce an error or a diagnostic.

### Layout

| Path | Contents |
|---|---|
| `dialect.go`, `format.go` | public API (the only package callers import) |
| `internal/core` | canonical model, format routing, diagnostics, estimates |
| `internal/codec` | decoders, encoders, response conversion, stream state machines |
| `internal/jsonx` | JSON helpers shared by core and codec |

### What is preserved, and what is reported as lost

| Feature | Behaviour |
|---|---|
| Text, system prompts, images (base64 + URL), PDFs | preserved where the target has a representation |
| Tool definitions, `tool_choice`, parallel tool calls | mapped per dialect; a policy a target lacks is diagnosed |
| Tool-call ↔ tool-result pairing | preserved; explicit ids are retained, and synthetic ids are minted only when absent |
| Multiple candidates | non-streaming Chat ↔ Gemini preserves all candidates; cross-dialect streams require one candidate |
| Empty / multimodal tool results | empty results stay valid; Gemini inline result media is preserved, and targets that cannot carry it report diagnostics |
| Reasoning / thinking | mapped to `reasoning_content`, Responses reasoning items, Anthropic thinking, Gemini thought parts |
| Google thought signatures | preserved for Gemini and Interactions targets, diagnosed as lost elsewhere |
| Structured output | `response_format` ↔ Anthropic `output_config.format` ↔ Gemini `responseMimeType`/`responseSchema` ↔ Interactions `TextResponseFormat` |
| Usage (incl. cached / reasoning / tool-use tokens) | mapped both ways |
| Provider-hosted tools (web search, code execution, computer use, MCP, file search) | dropped with a diagnostic |
| Audio / video parts | carried where the target has a part for them, diagnosed otherwise |
| Stateful fields (`previous_interaction_id`, `cached_content`, `conversation`, `background`) | forwarded on the dialect that owns them, diagnosed elsewhere |
| Error taxonomy | HTTP status mapped onto each dialect's error type/code/status |

## Testing

```bash
go test ./pkg/dialect/...            # unit + matrix + regression tests
go test -race ./pkg/dialect/...
go test ./pkg/dialect/ -bench . -benchmem
```

The suite includes a full 5×5 request/response/stream matrix, per-defect
regression tests (each written to fail when its fix is reverted), and an
end-to-end round-trip that replays a multi-turn tool conversation through every
dialect. Protocol correctness against the vendors themselves is verified
separately by `test/sdk-conformance/` in this repository, which drives a running
server with the official `openai`, `anthropic` and `google-genai` SDKs.

Benchmarks (Apple M5): Chat→Claude request ≈ 12 µs / 18.7 KB / 297 allocs,
Chat→Gemini ≈ 15 µs / 23.6 KB / 359 allocs, streaming chunk ≈ 5 µs.
