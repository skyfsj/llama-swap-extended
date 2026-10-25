package core

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Diagnostic reports something a conversion could not carry. Diagnostics are
// informational: the conversion still succeeds. They are surfaced in logs and
// API responses so a dropped field is never silent.
type Diagnostic struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Path names the offending field, when one exists.
	Path string `json:"path,omitempty"`
}

// String renders one diagnostic as "code: message (path)".
func (d Diagnostic) String() string {
	if d.Path != "" {
		return d.Code + ": " + d.Message + " (" + d.Path + ")"
	}
	return d.Code + ": " + d.Message
}

// Role is a canonical conversation role.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// PartKind discriminates one canonical content part.
type PartKind string

const (
	PartText       PartKind = "text"
	PartImage      PartKind = "image"
	PartAudio      PartKind = "audio"
	PartVideo      PartKind = "video"
	PartDocument   PartKind = "document"
	PartToolCall   PartKind = "tool_call"
	PartToolResult PartKind = "tool_result"
	PartReasoning  PartKind = "reasoning"
	PartRefusal    PartKind = "refusal"
)

// Part is one canonical content part.
//
// Media is carried either as inline base64 (Base64, with MediaType) or as a
// URL. Conversions never fetch remote media: a target that cannot address the
// URL natively receives a diagnostic instead.
type Part struct {
	Kind PartKind

	// Text is the payload for text, reasoning and refusal parts.
	Text string

	// MediaType and Base64 carry inline media (image/document).
	MediaType string
	Base64    string
	Filename  string
	// URL addresses media hosted elsewhere.
	URL string

	// ToolCallID/ToolName/Arguments describe a tool call. Arguments is a JSON
	// object; ArgumentsText preserves a partially streamed JSON string.
	ToolCallID    string
	ToolName      string
	Arguments     json.RawMessage
	ArgumentsText string
	// ToolNamespace is the Responses namespace the call was declared in. The
	// canonical ToolName is the flattened name a function-calling dialect
	// sees; the namespace is restored when rendering Responses output.
	ToolNamespace string
	// ToolKind distinguishes an ordinary JSON function call from a Responses
	// custom (freeform) tool call.
	ToolKind ToolKind

	// IsError marks a tool result as a failure report.
	IsError bool
	// Content carries a tool result's payload blocks.
	Content []Part

	// Signature carries an Anthropic thinking signature.
	Signature string
	// ThoughtSignature carries Google's opaque signature attached to Gemini 3
	// thinking and function-call parts. Gemini requires it to be echoed back
	// verbatim on the next turn, so every copy path must preserve it.
	ThoughtSignature string
	// Redacted marks opaque encrypted reasoning that must be echoed verbatim
	// to dialects that understand it (Anthropic redacted_thinking).
	Redacted bool
	// Data carries an opaque redacted reasoning payload.
	Data string
}

// Message is one canonical conversation turn. Parts from different source
// blocks may be mixed (for example a Claude user message containing tool
// results followed by text); encoders regroup them for their dialect.
type Message struct {
	Role  Role
	Parts []Part
	Name  string
}

// ToolKind discriminates the way a tool is invoked.
type ToolKind string

const (
	// ToolKindFunction is a JSON-schema function tool.
	ToolKindFunction ToolKind = "function"
	// ToolKindCustom is a Responses custom (freeform) tool. A function-calling
	// target receives it as a function with one string "input" argument.
	ToolKindCustom ToolKind = "custom"
)

// Tool describes a callable function.
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	Strict      *bool
	// Namespace is the Responses tool namespace the tool was declared in.
	// Tool.Name is the flattened upstream name.
	Namespace string
	// Kind defaults to ToolKindFunction when empty.
	Kind ToolKind
	// Format preserves a custom tool's grammar so Responses output can be
	// rendered faithfully.
	Format json.RawMessage
	// DeferLoading preserves the Responses defer_loading hint.
	DeferLoading *bool
}

// ToolBinding records how one flattened function name was derived from a
// Responses tool declaration.
type ToolBinding struct {
	Namespace string
	Kind      ToolKind
}

// ToolState maps flattened upstream function names back to the Responses
// namespace and kind they came from, so a response can restore the call.
type ToolState struct {
	Bindings map[string]ToolBinding
}

// NewToolState returns an empty tool state.
func NewToolState() *ToolState {
	return &ToolState{Bindings: map[string]ToolBinding{}}
}

// Binding reports how an upstream function name was declared.
func (s *ToolState) Binding(name string) (ToolBinding, bool) {
	if s == nil {
		return ToolBinding{}, false
	}
	binding, ok := s.Bindings[name]
	return binding, ok
}

// Set records how an upstream function name was declared.
func (s *ToolState) Set(name string, binding ToolBinding) {
	if s == nil || name == "" {
		return
	}
	if s.Bindings == nil {
		s.Bindings = map[string]ToolBinding{}
	}
	s.Bindings[name] = binding
}

// Empty reports whether the state carries nothing to restore.
func (s *ToolState) Empty() bool { return s == nil || len(s.Bindings) == 0 }

// DefaultToolNamespace is the Responses namespace Codex declares its ordinary
// tools in. Tools there stay at their bare names upstream.
const DefaultToolNamespace = "functions"

// FlattenToolName returns the upstream function name of a tool declared in a
// Responses namespace: "<namespace>__<name>", or the bare name for the default
// namespace.
func FlattenToolName(namespace, name string) string {
	if namespace == "" || namespace == DefaultToolNamespace {
		return name
	}
	return namespace + "__" + name
}

// SplitToolName restores the namespace of a flattened upstream function name.
// A name that was not flattened returns an empty namespace.
func SplitToolName(state *ToolState, upstreamName string) (namespace, name string, kind ToolKind) {
	binding, ok := state.Binding(upstreamName)
	if !ok {
		return "", upstreamName, ""
	}
	name = upstreamName
	if binding.Namespace != "" && binding.Namespace != DefaultToolNamespace {
		name = strings.TrimPrefix(upstreamName, binding.Namespace+"__")
	}
	return binding.Namespace, name, binding.Kind
}

// ToolChoiceMode is the canonical tool-selection policy.
type ToolChoiceMode string

const (
	ToolChoiceAuto     ToolChoiceMode = "auto"
	ToolChoiceNone     ToolChoiceMode = "none"
	ToolChoiceRequired ToolChoiceMode = "required"
	ToolChoiceTool     ToolChoiceMode = "tool"
)

// ToolChoice is the canonical tool-selection policy.
type ToolChoice struct {
	Mode ToolChoiceMode
	// Name is set when Mode is ToolChoiceTool.
	Name string
	// Namespace is the Responses tool namespace Name was declared in. Name is
	// always the flattened upstream name.
	Namespace string
	// AllowedNames is the closed set of tools a Responses allowed_tools policy
	// permits. It is independent of Mode: mode auto with one allowed tool is
	// not a forced call, and mode required with several tools stays required.
	// An encoder that cannot express the policy natively must restrict the
	// tool list to this set, otherwise the model may call a tool the client
	// never allowed.
	AllowedNames []string
	// DisableParallel is honored by dialects that support it.
	DisableParallel *bool
}

// FormatKind is the canonical structured-output mode.
type FormatKind string

const (
	FormatText       FormatKind = "text"
	FormatJSONObject FormatKind = "json_object"
	FormatJSONSchema FormatKind = "json_schema"
)

// ResponseFormat requests a structured output contract.
type ResponseFormat struct {
	Kind        FormatKind
	Name        string
	Description string
	Schema      json.RawMessage
	Strict      *bool
}

// ReasoningConfig carries the client's reasoning controls.
type ReasoningConfig struct {
	// Effort is one of minimal, low, medium, high, xhigh, max (dialect
	// dependent).
	Effort string
	// Level is Gemini's model-native thinkingLevel. It is kept apart from
	// Effort because a Gemini target must receive it verbatim instead of a
	// synthesized token budget.
	Level string
	// BudgetTokens is the Anthropic-style thinking budget.
	BudgetTokens *int
	// Summary asks for a reasoning summary (Responses).
	Summary string
	// Enabled is false when the client explicitly disabled reasoning.
	Enabled *bool
}

// Request is the canonical request representation.
type Request struct {
	Model            string
	Stream           bool
	System           []Part
	Messages         []Message
	Tools            []Tool
	ToolChoice       *ToolChoice
	ParallelTools    *bool
	MaxTokens        *int
	Temperature      *float64
	TopP             *float64
	TopK             *int
	Stop             []string
	N                *int
	Seed             *int64
	PresencePenalty  *float64
	FrequencyPenalty *float64
	User             string
	Metadata         map[string]string
	ResponseFormat   *ResponseFormat
	Reasoning        *ReasoningConfig

	// CacheBreakpoints counts Anthropic cache_control markers in the source
	// request. Only an Anthropic target can carry them; other targets report
	// the loss instead of dropping it silently.
	CacheBreakpoints int

	// Extra preserves dialect-specific fields that have a canonical-ish
	// meaning and can be forwarded when the target understands them.
	Extra map[string]json.RawMessage

	// ToolState records flattened Responses tool names. The request side
	// fills it; the response side uses it to restore namespaces and custom
	// tool calls.
	ToolState *ToolState
}

// Usage is the canonical token accounting.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
	// CachedTokens is the portion of InputTokens served from cache.
	CachedTokens int `json:"cached_tokens,omitempty"`
	// CacheReadTokens/CacheCreationTokens are Anthropic's cache accounting.
	CacheReadTokens     int `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int `json:"cache_creation_tokens,omitempty"`
	// ReasoningTokens is the portion of OutputTokens spent reasoning.
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
	// ToolUseTokens counts tokens spent on provider-hosted tool use
	// (Interactions reports it separately from output tokens).
	ToolUseTokens int `json:"tool_use_tokens,omitempty"`
}

// Normalize fills TotalTokens when a source omitted it.
func (u *Usage) Normalize() {
	if u.TotalTokens == 0 && (u.InputTokens != 0 || u.OutputTokens != 0) {
		u.TotalTokens = u.InputTokens + u.OutputTokens
	}
}

// Add merges a later (usually cumulative) usage frame into u. Non-zero fields
// in other win; a frame that omits TotalTokens still gets a consistent total,
// because a partial delta frame would otherwise leave total < input+output.
func (u *Usage) Add(other Usage) {
	if other.InputTokens != 0 {
		u.InputTokens = other.InputTokens
	}
	if other.OutputTokens != 0 {
		u.OutputTokens = other.OutputTokens
	}
	if other.TotalTokens != 0 {
		u.TotalTokens = other.TotalTokens
	}
	if other.CachedTokens != 0 {
		u.CachedTokens = other.CachedTokens
	}
	if other.CacheReadTokens != 0 {
		u.CacheReadTokens = other.CacheReadTokens
	}
	if other.CacheCreationTokens != 0 {
		u.CacheCreationTokens = other.CacheCreationTokens
	}
	if other.ReasoningTokens != 0 {
		u.ReasoningTokens = other.ReasoningTokens
	}
	if other.ToolUseTokens != 0 {
		u.ToolUseTokens = other.ToolUseTokens
	}
	if sum := u.InputTokens + u.OutputTokens; u.TotalTokens < sum {
		u.TotalTokens = sum
	}
}

// Canonical finish reasons.
const (
	FinishStop          = "stop"
	FinishLength        = "length"
	FinishToolUse       = "tool_use"
	FinishContentFilter = "content_filter"
	FinishStopSequence  = "stop_sequence"
	FinishPause         = "pause"
	FinishOther         = "other"
)

// Response is the canonical non-streaming response.
type Response struct {
	ID           string
	Model        string
	Created      int64
	Parts        []Part
	FinishReason string
	StopSequence string
	Usage        Usage
	// Status is completed, incomplete or failed.
	Status string
	// Error is set for failed responses.
	Error *StreamError
	// Metadata is copied from the request when the source exposes it.
	Metadata map[string]string
	// ToolState restores flattened Responses tool names and custom kinds.
	// A source that already speaks Responses fills it while decoding; a
	// converted response inherits it from the request.
	ToolState *ToolState
}

// StreamError is the canonical mid-stream/whole-response error payload.
type StreamError struct {
	Type    string
	Code    string
	Message string
	Status  string
	Details map[string]any
}

// EventKind discriminates a canonical stream event.
type EventKind int

const (
	// EventStart opens a stream and carries the response id/model.
	EventStart EventKind = iota
	// EventTextDelta appends assistant text.
	EventTextDelta
	// EventReasoningDelta appends reasoning text.
	EventReasoningDelta
	// EventRefusalDelta appends a refusal.
	EventRefusalDelta
	// EventToolCallStart opens a tool call at Index.
	EventToolCallStart
	// EventToolCallDelta appends raw JSON arguments at Index.
	EventToolCallDelta
	// EventToolCallEnd closes the tool call at Index.
	EventToolCallEnd
	EventToolExecution
	// EventUsage reports token accounting.
	EventUsage
	// EventFinish closes the response with a canonical finish reason.
	EventFinish
	// EventError reports a terminal error.
	EventError
	// EventPing is a keep-alive.
	EventPing
)

// Event is one canonical stream event.
type Event struct {
	Kind       EventKind
	Text       string
	Index      int
	ToolCallID string
	ToolName   string
	// ToolNamespace and ToolKind mirror Part's fields for a starting call.
	ToolNamespace  string
	ToolKind       ToolKind
	ArgumentsDelta string
	Parts          []Part
	// ThoughtSignature carries Google's opaque signature for the tool call or
	// reasoning delta it belongs to (Gemini 3 must receive it back verbatim).
	ThoughtSignature string
	FinishReason     string
	StopSequence     string
	Usage            *Usage
	Error            *StreamError
	ID               string
	Model            string
	Created          int64
}

// Frame is one encoded SSE frame.
type Frame struct {
	// Event is the SSE event name; empty means an unnamed data frame.
	Event string
	// Data is the JSON payload, or the literal [DONE] sentinel.
	Data []byte
}

// IsDone reports whether the frame is the [DONE] sentinel.
func (f Frame) IsDone() bool { return string(f.Data) == "[DONE]" }

// StreamOptions configures a stream conversion.
type StreamOptions struct {
	// Model is the client-facing model name.
	Model string
	// ID seeds the response id; a target may derive its own.
	ID string
	// Created is the response creation timestamp.
	Created int64
	// IncludeUsage asks the upstream for usage frames and emits them to the
	// client.
	IncludeUsage bool
	// EstimateInputTokens seeds source dialects that report input usage in
	// their first frame (Anthropic message_start).
	EstimateInputTokens int
	// Request is the canonical request this stream answers. Responses output
	// uses it to restore tool namespaces and echo the request's tool policy.
	Request *Request
}

// ConvertOptions configures request/response conversion.
type ConvertOptions struct {
	// Model is the target model name written into the converted request.
	Model string
	// Stream overrides the request's stream flag when non-nil.
	Stream *bool
	// UpstreamFormat is informational, for diagnostics.
	UpstreamFormat Format
	// Request is the canonical request this conversion answers. A response
	// encoder reads its tool state to restore Responses namespaces and its
	// tool policy to echo it back.
	Request *Request
}

// ConversionError is a hard conversion failure (the request cannot be
// represented in the target dialect).
type ConversionError struct {
	Code    string
	Message string
}

// Error renders the failure, prefixed with its machine-readable code.
func (e *ConversionError) Error() string {
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

// ConversionErrorf builds a hard conversion failure.
func ConversionErrorf(code, format string, args ...any) error {
	return &ConversionError{Code: code, Message: fmt.Sprintf(format, args...)}
}
