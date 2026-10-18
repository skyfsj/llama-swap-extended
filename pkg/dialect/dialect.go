package dialect

import (
	"github.com/mostlygeek/llama-swap/pkg/dialect/internal/codec"
	"github.com/mostlygeek/llama-swap/pkg/dialect/internal/core"
)

// The public surface of the package. Everything below is either an alias into
// the canonical model (internal/core) or a thin wrapper over the codec
// (internal/codec), so callers only ever import this one package.

// Format names one wire dialect.
type Format = core.Format

const (
	// OpenAIChat is the OpenAI Chat Completions dialect (/v1/chat/completions).
	OpenAIChat = core.OpenAIChat
	// OpenAIResponses is the OpenAI Responses dialect (/v1/responses).
	OpenAIResponses = core.OpenAIResponses
	// ClaudeMessages is the Anthropic Messages dialect (/v1/messages).
	ClaudeMessages = core.ClaudeMessages
	// Gemini is the Google Gemini generateContent dialect.
	Gemini = core.Gemini
	// GeminiInteractions is Google's stateful Interactions dialect
	// (POST /v1beta/interactions).
	GeminiInteractions = core.GeminiInteractions
)

// Route describes a generation request recognised from its HTTP path.
type Route = core.Route

// RouteForRequest recognises a generation request. Only POST is considered
// because every supported dialect carries the prompt in the body.
func RouteForRequest(method, path string) (Route, bool) { return core.RouteForRequest(method, path) }

// RequestStreamed reports whether the request body asks for a streaming
// response. Gemini carries the choice in the path, all other dialects in the
// body.
func RequestStreamed(route Route, body []byte) bool { return core.RequestStreamed(route, body) }

// TargetPath returns the upstream path for a dialect.
func TargetPath(format Format, model string, stream bool) (string, error) {
	return core.TargetPath(format, model, stream)
}

// TargetPathForRequest is TargetPath with the client's version-prefix style
// preserved: a client that calls /v/chat/completions is forwarded to
// /v/chat/completions rather than /v1/chat/completions.
func TargetPathForRequest(format Format, model string, stream bool, publicPath string) (string, error) {
	return core.TargetPathForRequest(format, model, stream, publicPath)
}

// Canonical model types. They are aliases so that a value built by this package
// and a value built through the internal packages are the same type.
type (
	// Diagnostic reports something a conversion could not carry. Diagnostics are
	// informational: the conversion still succeeds.
	Diagnostic = core.Diagnostic
	// ConversionError is a hard conversion failure: the request cannot be
	// represented in the target dialect.
	ConversionError = core.ConversionError

	// Role is a canonical conversation role.
	Role = core.Role
	// PartKind discriminates one canonical content part.
	PartKind = core.PartKind
	// Part is one canonical content part.
	Part = core.Part
	// Message is one canonical conversation turn.
	Message = core.Message
	// Tool describes a callable function.
	Tool = core.Tool
	// ToolChoiceMode is the canonical tool-selection policy.
	ToolChoiceMode = core.ToolChoiceMode
	// ToolChoice is the canonical tool-selection policy.
	ToolChoice = core.ToolChoice
	// FormatKind is the canonical structured-output mode.
	FormatKind = core.FormatKind
	// ResponseFormat requests a structured output contract.
	ResponseFormat = core.ResponseFormat
	// ReasoningConfig carries the client's reasoning controls.
	ReasoningConfig = core.ReasoningConfig
	// Request is the canonical request representation.
	Request = core.Request
	// Usage is the canonical token accounting.
	Usage = core.Usage
	// Response is the canonical non-streaming response.
	Response = core.Response
	// StreamError is the canonical mid-stream/whole-response error payload.
	StreamError = core.StreamError
	// EventKind discriminates a canonical stream event.
	EventKind = core.EventKind
	// Event is one canonical stream event.
	Event = core.Event
	// Frame is one encoded SSE frame.
	Frame = core.Frame

	// ConvertOptions configures request/response conversion.
	ConvertOptions = core.ConvertOptions
	// StreamOptions configures a stream conversion.
	StreamOptions = core.StreamOptions
)

// Canonical role, part, policy and finish-reason constants.
const (
	RoleSystem    = core.RoleSystem
	RoleUser      = core.RoleUser
	RoleAssistant = core.RoleAssistant
	RoleTool      = core.RoleTool

	PartText       = core.PartText
	PartImage      = core.PartImage
	PartAudio      = core.PartAudio
	PartVideo      = core.PartVideo
	PartDocument   = core.PartDocument
	PartToolCall   = core.PartToolCall
	PartToolResult = core.PartToolResult
	PartReasoning  = core.PartReasoning
	PartRefusal    = core.PartRefusal

	ToolChoiceAuto     = core.ToolChoiceAuto
	ToolChoiceNone     = core.ToolChoiceNone
	ToolChoiceRequired = core.ToolChoiceRequired
	ToolChoiceTool     = core.ToolChoiceTool

	FormatText       = core.FormatText
	FormatJSONObject = core.FormatJSONObject
	FormatJSONSchema = core.FormatJSONSchema

	EventStart          = core.EventStart
	EventTextDelta      = core.EventTextDelta
	EventReasoningDelta = core.EventReasoningDelta
	EventRefusalDelta   = core.EventRefusalDelta
	EventToolCallStart  = core.EventToolCallStart
	EventToolCallDelta  = core.EventToolCallDelta
	EventToolCallEnd    = core.EventToolCallEnd
	EventUsage          = core.EventUsage
	EventFinish         = core.EventFinish
	EventError          = core.EventError
	EventPing           = core.EventPing

	FinishStop          = core.FinishStop
	FinishLength        = core.FinishLength
	FinishToolUse       = core.FinishToolUse
	FinishContentFilter = core.FinishContentFilter
	FinishStopSequence  = core.FinishStopSequence
	FinishPause         = core.FinishPause
	FinishOther         = core.FinishOther
)

// ConvertRequest decodes a request in source and renders it in target. A
// non-nil error means the request cannot be represented in the target dialect
// (for example n>1 for a single-response dialect); diagnostics describe fields
// that were carried lossily.
func ConvertRequest(source, target Format, body []byte, options ConvertOptions) ([]byte, []Diagnostic, error) {
	return codec.ConvertRequest(source, target, body, options)
}

// ConvertResponse decodes a whole response in source and renders it in target.
func ConvertResponse(source, target Format, body []byte) ([]byte, []Diagnostic, error) {
	return codec.ConvertResponse(source, target, body)
}

// DecodeRequest reads a wire request into the canonical representation.
func DecodeRequest(format Format, body []byte) (*Request, []Diagnostic, error) {
	return codec.DecodeRequest(format, body)
}

// EncodeRequest renders the canonical request in the target dialect.
func EncodeRequest(request *Request, target Format, options ConvertOptions) ([]byte, []Diagnostic, error) {
	return codec.EncodeRequest(request, target, options)
}

// DecodeResponse reads a wire response into the canonical representation.
func DecodeResponse(format Format, body []byte) (*Response, []Diagnostic, error) {
	return codec.DecodeResponse(format, body)
}

// EncodeResponse renders the canonical response in the target dialect.
func EncodeResponse(response *Response, target Format) ([]byte, []Diagnostic, error) {
	return codec.EncodeResponse(response, target)
}

// ResponseFailed reports whether a decoded response body represents a failure:
// an explicit failed status, or an error object that carries information.
func ResponseFailed(source Format, body []byte) bool { return codec.ResponseFailed(source, body) }

// StripHostedSearchTools removes provider-native hosted search tools, which no
// function-calling upstream can execute. changed reports whether the body was
// rewritten.
func StripHostedSearchTools(body []byte, format Format) (out []byte, changed bool, err error) {
	return codec.StripHostedSearchTools(body, format)
}

// ErrorEnvelope renders an upstream error body in the client dialect, mapping
// the HTTP status onto that dialect's error taxonomy.
func ErrorEnvelope(body []byte, target Format, status int) []byte {
	return codec.ErrorEnvelope(body, target, status)
}

// EstimateTokens approximates the token count of text. It is used only for the
// Anthropic message_start input count, which the real usage frame corrects; it
// never affects billing.
func EstimateTokens(text string) int { return core.EstimateTokens(text) }

// EstimateRequestTokens approximates the prompt size of a canonical request.
func EstimateRequestTokens(request *Request) int { return core.EstimateRequestTokens(request) }

// StreamConverter converts one upstream SSE stream into the client dialect.
// Feed receives one decoded SSE data payload at a time; Finish flushes the
// terminal frames. It is not safe for concurrent use.
type StreamConverter = codec.StreamConverter

// NewStreamConverter builds a converter for one stream.
func NewStreamConverter(source, target Format, options StreamOptions) (*StreamConverter, error) {
	return codec.NewStreamConverter(source, target, options)
}
