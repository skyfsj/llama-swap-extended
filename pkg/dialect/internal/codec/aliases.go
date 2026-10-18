package codec

import (
	"github.com/mostlygeek/llama-swap/pkg/dialect/internal/core"
)

// Aliases into the canonical model so the codec reads without a package
// qualifier on every type. They are implementation detail: the public surface
// lives in the dialect package.
type (
	ConversionError = core.ConversionError
	Diagnostic      = core.Diagnostic
	Event           = core.Event
	EventKind       = core.EventKind
	Format          = core.Format
	FormatKind      = core.FormatKind
	Frame           = core.Frame
	Message         = core.Message
	Part            = core.Part
	PartKind        = core.PartKind
	ReasoningConfig = core.ReasoningConfig
	Request         = core.Request
	Response        = core.Response
	ResponseFormat  = core.ResponseFormat
	Role            = core.Role
	StreamError     = core.StreamError
	Tool            = core.Tool
	ToolChoice      = core.ToolChoice
	ToolChoiceMode  = core.ToolChoiceMode
	Usage           = core.Usage
	ConvertOptions  = core.ConvertOptions
	StreamOptions   = core.StreamOptions
	Route           = core.Route
)

const (
	OpenAIChat         = core.OpenAIChat
	OpenAIResponses    = core.OpenAIResponses
	ClaudeMessages     = core.ClaudeMessages
	Gemini             = core.Gemini
	GeminiInteractions = core.GeminiInteractions

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
