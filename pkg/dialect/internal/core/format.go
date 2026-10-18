// Package dialect converts LLM API requests, responses and streams between
// the OpenAI Chat Completions, OpenAI Responses, Anthropic Messages and Google
// Gemini dialects.
//
// The package is deliberately self-contained: it imports only the standard
// library and does not depend on any server, configuration or storage code, so
// it can be lifted into another project unchanged. It is licensed under the
// same MIT license as the repository (see LICENSE.md at the repository root).
//
// Design:
//   - Every conversion pivots through a small canonical representation
//     (Request, Response, Event). Decoders read one wire dialect into the
//     canonical form; encoders render it into another. Adding a dialect means
//     one decoder and one encoder, not N×N converters.
//   - Unknown or unrepresentable fields are reported as Diagnostics instead of
//     being dropped silently, and a conversion never panics on malformed input.
//   - Streaming is event based: a source decoder turns one upstream frame into
//     canonical Events, and a stateful target encoder renders those events into
//     the client dialect. Tool-call argument deltas, reasoning deltas and usage
//     frames keep their ordering across the conversion.
package core

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Format names one wire dialect.
type Format string

const (
	// OpenAIChat is the OpenAI Chat Completions dialect (/v1/chat/completions).
	OpenAIChat Format = "openai.chat"
	// OpenAIResponses is the OpenAI Responses dialect (/v1/responses).
	OpenAIResponses Format = "openai.responses"
	// ClaudeMessages is the Anthropic Messages dialect (/v1/messages).
	ClaudeMessages Format = "anthropic.messages"
	// Gemini is the Google Gemini generateContent dialect.
	Gemini Format = "gemini"
	// GeminiInteractions is Google's stateful Interactions dialect
	// (POST /v1beta/interactions), the vendor's recommended primitive for
	// agentic, multi-turn work: requests carry Steps and responses carry a
	// steps[] transcript with a modality-broken-down usage object.
	GeminiInteractions Format = "gemini.interactions"
)

// Route describes a generation request recognised from its HTTP path.
type Route struct {
	Format Format
	// Model is the model named in the path (Gemini only).
	Model string
	// Stream is true for the Gemini streaming action.
	Stream bool
	// Action is the Gemini action suffix (generateContent or
	// streamGenerateContent).
	Action string
}

// RouteForRequest recognises a generation request. Only POST is considered
// because every supported dialect carries the prompt in the body.
func RouteForRequest(method, path string) (Route, bool) {
	if !strings.EqualFold(method, "POST") {
		return Route{}, false
	}
	switch path {
	case "/v1/chat/completions", "/v/chat/completions":
		return Route{Format: OpenAIChat}, true
	case "/v1/responses", "/v/responses":
		return Route{Format: OpenAIResponses}, true
	case "/v1/messages", "/v/messages":
		return Route{Format: ClaudeMessages}, true
	case "/v1/interactions", "/v1beta/interactions", "/v/interactions":
		return Route{Format: GeminiInteractions}, true
	}
	const prefix = "/v1beta/models/"
	if !strings.HasPrefix(path, prefix) {
		return Route{}, false
	}
	modelAction := strings.TrimPrefix(path, prefix)
	for _, action := range []struct {
		name   string
		stream bool
	}{{"streamGenerateContent", true}, {"generateContent", false}} {
		suffix := ":" + action.name
		if !strings.HasSuffix(modelAction, suffix) {
			continue
		}
		model := strings.Trim(strings.TrimSuffix(modelAction, suffix), "/")
		if model == "" {
			return Route{}, false
		}
		return Route{Format: Gemini, Model: model, Stream: action.stream, Action: action.name}, true
	}
	return Route{}, false
}

// RequestStreamed reports whether the request body asks for a streaming
// response. Gemini carries the choice in the path, all other dialects in the
// body.
func RequestStreamed(route Route, body []byte) bool {
	if route.Format == Gemini {
		return route.Stream
	}
	var request struct {
		Stream bool `json:"stream"`
	}
	return json.Unmarshal(body, &request) == nil && request.Stream
}

// TargetPath returns the upstream path for a dialect.
func TargetPath(format Format, model string, stream bool) (string, error) {
	switch format {
	case OpenAIChat:
		return "/v1/chat/completions", nil
	case OpenAIResponses:
		return "/v1/responses", nil
	case ClaudeMessages:
		return "/v1/messages", nil
	case Gemini:
		model = strings.Trim(model, "/")
		if model == "" {
			return "", fmt.Errorf("gemini target requires a model")
		}
		action := "generateContent"
		if stream {
			action = "streamGenerateContent"
		}
		return "/v1beta/models/" + model + ":" + action, nil
	case GeminiInteractions:
		// The Interactions dialect carries the model in the body, like Chat.
		return "/v1beta/interactions", nil
	default:
		return "", fmt.Errorf("unsupported dialect %q", format)
	}
}

// TargetPathForRequest is TargetPath with the client's version-prefix style
// preserved: a client that calls /v/chat/completions is forwarded to
// /v/chat/completions rather than /v1/chat/completions.
func TargetPathForRequest(format Format, model string, stream bool, publicPath string) (string, error) {
	path, err := TargetPath(format, model, stream)
	if err != nil || format == Gemini || !strings.HasPrefix(publicPath, "/v/") {
		return path, err
	}
	return "/v" + strings.TrimPrefix(path, "/v1"), nil
}
