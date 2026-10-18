// Package relaykit adapts the MIT-licensed github.com/mostlygeek/llama-swap/pkg/dialect
// module to the interfaces the server layer uses. It exists purely as a seam:
// protocol knowledge lives in pkg/dialect, which has no server dependencies.
package relaykit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

// Format names a wire dialect.
type Format = dialect.Format

const (
	OpenAIChat      = dialect.OpenAIChat
	OpenAIResponses = dialect.OpenAIResponses
	ClaudeMessages  = dialect.ClaudeMessages
	Gemini          = dialect.Gemini
	// GeminiInteractions is Google's stateful Interactions dialect.
	GeminiInteractions = dialect.GeminiInteractions
)

// Diagnostic is one conversion note.
type Diagnostic = dialect.Diagnostic

// Meta carries per-request conversion state.
type Meta struct {
	IsStream bool
	// EstimatePromptTokens seeds dialects that report input usage up front.
	EstimatePromptTokens int
}

// NewMeta builds the conversion metadata for one request.
func NewMeta(model string, stream bool) *Meta {
	return &Meta{IsStream: stream}
}

// Route describes a generation request recognised from its path.
type Route = dialect.Route

func RouteForRequest(method, path string) (Route, bool) {
	return dialect.RouteForRequest(method, path)
}

func RequestStreamed(route Route, body []byte) bool {
	return dialect.RequestStreamed(route, body)
}

func TargetPath(format Format, model string, stream bool) (string, error) {
	return dialect.TargetPath(format, model, stream)
}

func TargetPathForRequest(format Format, model string, stream bool, publicPath string) (string, error) {
	return dialect.TargetPathForRequest(format, model, stream, publicPath)
}

// RequestStep describes one hop of a multi-step conversion.
type RequestStep struct {
	From Format
	To   Format
}

// RequestResult is a converted request.
type RequestResult struct {
	Value       any
	From        Format
	To          Format
	Converter   string
	Quality     string
	Steps       []RequestStep
	Diagnostics []Diagnostic
}

// ResponseResult is a converted response or one converted stream frame.
type ResponseResult struct {
	Value       any
	From        Format
	To          Format
	Converter   string
	Quality     string
	Diagnostics []Diagnostic
}

// ConvertRequest converts a request body from source to target.
func ConvertRequest(ctx context.Context, meta *Meta, source, target Format, body []byte, model string) (*RequestResult, error) {
	if source == target {
		return &RequestResult{Value: append([]byte(nil), body...), From: source, To: target, Converter: "identity", Quality: "exact"}, nil
	}
	options := dialect.ConvertOptions{Model: model}
	if meta != nil && meta.IsStream {
		stream := true
		options.Stream = &stream
	}
	// Decode once, then encode: the canonical request is also what seeds the
	// Anthropic message_start estimate, so no second parse is needed.
	request, diagnostics, err := dialect.DecodeRequest(source, body)
	if err != nil {
		return nil, err
	}
	if request.Model == "" {
		request.Model = model
	}
	if meta != nil && meta.EstimatePromptTokens == 0 && meta.IsStream {
		// Seed Anthropic message_start usage; the real counts arrive in the
		// final message_delta. Only a stream needs the estimate because a
		// whole response carries the upstream's usage object.
		meta.EstimatePromptTokens = dialect.EstimateRequestTokens(request)
	}
	converted, encodeDiagnostics, err := dialect.EncodeRequest(request, target, options)
	if err != nil {
		return nil, err
	}
	diagnostics = append(diagnostics, encodeDiagnostics...)
	return &RequestResult{
		Value: converted, From: source, To: target, Converter: string(source) + "->" + string(target),
		Quality: qualityOf(diagnostics), Diagnostics: diagnostics,
	}, nil
}

// ConvertResponse converts a whole response body from source to target.
func ConvertResponse(ctx context.Context, meta *Meta, source, target Format, body []byte) (*ResponseResult, error) {
	if source == target {
		return &ResponseResult{Value: append([]byte(nil), body...), From: source, To: target, Converter: "identity", Quality: "exact"}, nil
	}
	converted, diagnostics, err := dialect.ConvertResponse(source, target, body)
	if err != nil {
		return nil, err
	}
	return &ResponseResult{
		Value: converted, From: source, To: target, Converter: string(source) + "->" + string(target),
		Quality: qualityOf(diagnostics), Diagnostics: diagnostics,
	}, nil
}

func qualityOf(diagnostics []Diagnostic) string {
	if len(diagnostics) > 0 {
		return "lossy"
	}
	return "exact"
}

// StripHostedSearchTools removes provider-native search tools.
func StripHostedSearchTools(body []byte, format Format) ([]byte, bool, error) {
	return dialect.StripHostedSearchTools(body, format)
}

// SSEFrame is one encoded stream frame carried through ResponseResult.Value.
type SSEFrame struct {
	Event string
	Data  []byte
}

// DecodeStreamResponse parses one upstream SSE data payload.
func DecodeStreamResponse(format Format, body []byte) (any, error) {
	if !json.Valid(body) {
		return nil, fmt.Errorf("%s stream event is not valid JSON", format)
	}
	return &SSEFrame{Data: append([]byte(nil), body...)}, nil
}

// EncodeSSE renders one converted frame in the client dialect.
func EncodeSSE(format Format, response any) ([]byte, error) {
	value, ok := response.(*SSEFrame)
	if !ok {
		// A converted whole object (non-stream) can still be encoded.
		encoded, err := Marshal(response)
		if err != nil {
			return nil, err
		}
		value = &SSEFrame{Data: encoded}
	}
	if value.Event != "" {
		if strings.ContainsAny(value.Event, "\r\n") {
			return nil, fmt.Errorf("invalid stream event type %q", value.Event)
		}
		return []byte("event: " + value.Event + "\ndata: " + string(value.Data) + "\n\n"), nil
	}
	if string(value.Data) == "[DONE]" {
		return []byte("data: [DONE]\n\n"), nil
	}
	return []byte("data: " + string(value.Data) + "\n\n"), nil
}

// DoneSSE returns the client-dialect end-of-stream sentinel.
func DoneSSE(format Format) []byte {
	if format == OpenAIChat {
		return []byte("data: [DONE]\n\n")
	}
	return nil
}

// StreamResponseID extracts the response id from one upstream frame so a
// lazily-created Responses stream can be seeded with it.
func StreamResponseID(format Format, response any) string {
	frame, ok := response.(*SSEFrame)
	if !ok {
		return ""
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(frame.Data, &object) != nil {
		return ""
	}
	if raw, exists := object["id"]; exists {
		var id string
		if json.Unmarshal(raw, &id) == nil {
			return cleanResponseID(id)
		}
	}
	if raw, exists := object["message"]; exists {
		var message struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &message) == nil && message.ID != "" {
			return cleanResponseID(message.ID)
		}
	}
	if raw, exists := object["response"]; exists {
		var nested struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &nested) == nil && nested.ID != "" {
			return cleanResponseID(nested.ID)
		}
	}
	return ""
}

func cleanResponseID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || strings.HasPrefix(id, "resp_") {
		return id
	}
	return "resp_" + id
}

// Marshal encodes a converted value. Converters already hand back JSON bytes;
// any other value is encoded as JSON.
func Marshal(value any) ([]byte, error) {
	switch typed := value.(type) {
	case []byte:
		return append([]byte(nil), typed...), nil
	case json.RawMessage:
		return append([]byte(nil), typed...), nil
	case nil:
		return nil, errors.New("cannot marshal a nil conversion result")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode converted value: %w", err)
	}
	return encoded, nil
}

// StreamOptions configures a stream conversion. It mirrors the previous
// conversion library's options so call sites keep their shape.
type StreamOptions struct {
	Model               string
	ID                  string
	Created             int64
	IncludeUsage        bool
	EstimateInputTokens int
}

// ResponseStreamState is one in-flight stream conversion.
type ResponseStreamState struct {
	converter *dialect.StreamConverter
}

// NewResponseStreamState builds a stream converter for source -> target.
func NewResponseStreamState(source, target Format, options StreamOptions) (*ResponseStreamState, error) {
	estimate := options.EstimateInputTokens
	converter, err := dialect.NewStreamConverter(source, target, dialect.StreamOptions{
		Model:               options.Model,
		ID:                  options.ID,
		Created:             options.Created,
		IncludeUsage:        options.IncludeUsage,
		EstimateInputTokens: estimate,
	})
	if err != nil {
		return nil, err
	}
	return &ResponseStreamState{converter: converter}, nil
}

// ConvertStreamResponseChunk converts one upstream frame.
func ConvertStreamResponseChunk(ctx context.Context, meta *Meta, state *ResponseStreamState, response any) ([]ResponseResult, error) {
	if state == nil || state.converter == nil {
		return nil, errors.New("stream state is required")
	}
	frame, ok := response.(*SSEFrame)
	if !ok {
		return nil, fmt.Errorf("unexpected stream response type %T", response)
	}
	frames, err := state.converter.Feed(frame.Data)
	if err != nil {
		return nil, err
	}
	return wrapFrames(frames), nil
}

// FinalizeStreamResponse flushes the terminal frames.
func FinalizeStreamResponse(ctx context.Context, meta *Meta, state *ResponseStreamState) ([]ResponseResult, error) {
	if state == nil || state.converter == nil {
		return nil, errors.New("stream state is required")
	}
	frames, err := state.converter.Finish()
	if err != nil {
		return nil, err
	}
	return wrapFrames(frames), nil
}

// Diagnostics reports the conversion notes gathered for a stream.
func (s *ResponseStreamState) Diagnostics() []Diagnostic {
	if s == nil || s.converter == nil {
		return nil
	}
	return s.converter.Diagnostics()
}

func wrapFrames(frames []dialect.Frame) []ResponseResult {
	results := make([]ResponseResult, 0, len(frames))
	for _, item := range frames {
		results = append(results, ResponseResult{Value: &SSEFrame{Event: item.Event, Data: item.Data}})
	}
	return results
}

// ResponseFailed reports whether a whole upstream response says it failed.
func ResponseFailed(source Format, body []byte) bool {
	return dialect.ResponseFailed(source, body)
}

// ErrorEnvelope renders an upstream error body in the client dialect.
func ErrorEnvelope(body []byte, target Format, status int) []byte {
	return dialect.ErrorEnvelope(body, target, status)
}
