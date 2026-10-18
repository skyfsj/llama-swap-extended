// Package dialect converts LLM API requests, responses and streams between the
// OpenAI Chat Completions, OpenAI Responses, Anthropic Messages, Google Gemini
// generateContent and Google Gemini Interactions dialects.
//
// # What it is
//
// The package is deliberately self-contained: it imports only the standard
// library and does not depend on any server, configuration or storage code, so
// it can be lifted into another project unchanged. It is licensed under the
// same MIT license as the repository (see LICENSE.md at the repository root).
//
// # Design
//
// Every conversion pivots through a small canonical representation
// ([Request], [Response], [Event]). Decoders read one wire dialect into the
// canonical form; encoders render it into another. Adding a dialect means one
// decoder and one encoder, not N×N converters:
//
//	wire A ──decode──▶ canonical ──encode──▶ wire B
//
// Unknown or unrepresentable fields are reported as [Diagnostic] values instead
// of being dropped silently, and a conversion never panics on malformed input.
//
// Streaming is event based: a source decoder turns one upstream frame into
// canonical [Event] values and a stateful target encoder renders those into the
// client dialect, so tool-call argument deltas, reasoning deltas and usage
// frames keep their ordering across the conversion.
//
// # Layout
//
//	dialect.go, format.go   public API (this package)
//	internal/core           canonical model, format routing, diagnostics
//	internal/codec          the decoders, encoders and stream state machines
//	internal/jsonx          JSON helpers shared by the two above
//
// # Usage
//
// Convert a request and read the diagnostics:
//
//	converted, diagnostics, err := dialect.ConvertRequest(
//	    dialect.OpenAIChat, dialect.ClaudeMessages, body,
//	    dialect.ConvertOptions{Model: "claude-sonnet-4-5"},
//	)
//	if err != nil {
//	    return err // the request cannot be represented in the target dialect
//	}
//	for _, d := range diagnostics {
//	    log.Printf("conversion note: %s (%s)", d.Message, d.Code)
//	}
//
// Convert a whole response:
//
//	converted, diagnostics, err := dialect.ConvertResponse(
//	    dialect.ClaudeMessages, dialect.OpenAIChat, upstreamBody)
//
// Convert a stream frame by frame:
//
//	converter, err := dialect.NewStreamConverter(
//	    dialect.OpenAIChat, dialect.ClaudeMessages,
//	    dialect.StreamOptions{Model: "m", IncludeUsage: true},
//	)
//	// for every upstream SSE data payload:
//	frames, err := converter.Feed(payload)
//	// when the upstream stream ends:
//	frames, err := converter.Finish()
//
// Recognize a request from its HTTP path and route it:
//
//	if route, ok := dialect.RouteForRequest(r.Method, r.URL.Path); ok {
//	    // route.Format, route.Model (Gemini), route.Stream
//	}
//
// # Guarantees and limits
//
//   - A same-dialect conversion is a pass-through; the package is only used
//     when a dialect actually changes.
//   - Provider-hosted tools (web search, code execution, computer use, MCP,
//     file search) have no cross-dialect representation: they are dropped with
//     a diagnostic, never simulated.
//   - Google's thought signatures are preserved for Gemini and Interactions
//     targets and reported as lost for every other target.
//   - Errors keep their taxonomy: an upstream 4xx/5xx becomes the client
//     dialect's error envelope with a matching type/code/status
//     ([ErrorEnvelope]).
package dialect
