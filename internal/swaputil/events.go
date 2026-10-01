package swaputil

import (
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const ProcessStateChangeEventID = 0x01
const ConfigFileChangedEventID = 0x03
const ActivityLogEventID = 0x05
const ModelPreloadedEventID = 0x06
const InFlightRequestsEventID = 0x07
const ProfileChangedEventID = 0x08
const BackendProgressEventID = 0x09
const ModelStatusesChangedEventID = 0x0A

// ProcessStateChangeEvent is emitted whenever a process transitions between
// lifecycle states. States are carried as strings so this package stays a leaf
// (no import of internal/process).
type ProcessStateChangeEvent struct {
	ProcessName string
	OldState    string
	NewState    string
}

func (e ProcessStateChangeEvent) Type() uint32 {
	return ProcessStateChangeEventID
}

type ReloadingState int

const (
	ReloadingStateStart ReloadingState = iota
	ReloadingStateEnd
)

type ConfigFileChangedEvent struct {
	State ReloadingState
}

func (e ConfigFileChangedEvent) Type() uint32 {
	return ConfigFileChangedEventID
}

// ModelStatusesChangedEvent tells SSE subscribers that scheduler lifecycle
// statuses changed. It is deliberately distinct from ConfigFileChangedEvent:
// lifecycle transitions fire on every start/stop/restart, not only on config
// reloads, so reusing the reload event mislabeled the traffic.
type ModelStatusesChangedEvent struct{}

func (e ModelStatusesChangedEvent) Type() uint32 {
	return ModelStatusesChangedEventID
}

type ModelPreloadedEvent struct {
	ModelName string
	Success   bool
}

func (e ModelPreloadedEvent) Type() uint32 {
	return ModelPreloadedEventID
}

type InFlightRequestsEvent struct {
	Operation string                 `json:"operation"`
	Requests  []InflightRequestEntry `json:"requests,omitempty"`
	Request   *InflightRequestEntry  `json:"request,omitempty"`
	ID        string                 `json:"id,omitempty"`
}

func (e InFlightRequestsEvent) Type() uint32 {
	return InFlightRequestsEventID
}

type InflightRequestEntry struct {
	ID           string    `json:"id"`
	Timestamp    time.Time `json:"timestamp"`
	Model        string    `json:"model"`
	Phase        string    `json:"phase"`
	PhaseMessage string    `json:"phase_message,omitempty"`
	// OutputPreview is a bounded, in-memory view of the latest streamed text.
	// It is intentionally separate from captures/audit bodies and is omitted
	// when the upstream has not produced recognizable text yet.
	OutputPreview string            `json:"output_preview,omitempty"`
	ReqPath       string            `json:"req_path"`
	Method        string            `json:"method"`
	ReqHeaders    map[string]string `json:"req_headers"`
	RemoteIP      string            `json:"remote_ip"`
	RespHeaders   map[string]string `json:"resp_headers"`
	RespBytes     int64             `json:"resp_bytes"`
	ElapsedMs     int64             `json:"elapsed_ms"`
	// Token telemetry is copied only when the upstream explicitly reports it.
	// Zero means unknown here; the UI must not infer tokens from response text.
	InputTokens     int64             `json:"input_tokens,omitempty"`
	OutputTokens    int64             `json:"output_tokens,omitempty"`
	CachedTokens    int64             `json:"cached_tokens,omitempty"`
	PromptPerSecond float64           `json:"prompt_per_second,omitempty"`
	TokensPerSecond float64           `json:"tokens_per_second,omitempty"`
	FirstTokenMs    int64             `json:"first_token_ms,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
}

type ProfileChangedEvent struct {
	Active string
}

func (e ProfileChangedEvent) Type() uint32 {
	return ProfileChangedEventID
}

// BackendProgressEvent is the common progress envelope for runtime updates,
// downloads, model load/prefill/generation and sleep/wake operations.
type BackendProgressEvent struct {
	Model       string  `json:"model,omitempty"`
	Runtime     string  `json:"runtime,omitempty"`
	OperationID string  `json:"operationId,omitempty"`
	Phase       string  `json:"phase"`
	Progress    float64 `json:"progress"`
	Completed   int64   `json:"completed,omitempty"`
	// Total is -1 when the source did not provide a Content-Length. Keeping
	// that distinction lets the UI show an honest byte counter instead of
	// turning an unknown length into a misleading zero-byte total.
	Total        int64  `json:"total,omitempty"`
	Message      string `json:"message,omitempty"`
	Error        string `json:"error,omitempty"`
	OutputStream string `json:"outputStream,omitempty"`
	// Output carries a bounded chunk of command stdout/stderr. Unlike Message
	// and Error it intentionally preserves newlines so a compiler log remains
	// readable in the runtime manager UI.
	Output string `json:"output,omitempty"`
}

func (e BackendProgressEvent) Type() uint32 { return BackendProgressEventID }

// Normalize bounds progress events before they enter the shared event bus or
// a server's latest-value cache. Runtime providers and remote adapters can
// supply these strings, so an unexpectedly large diagnostic must not turn a
// single backend response into unbounded SSE/memory growth. The bool result is
// false when the event has no addressable model or runtime and should be
// dropped.
func (e BackendProgressEvent) Normalize() (BackendProgressEvent, bool) {
	e.Model = normalizeProgressEventText(e.Model, 256)
	e.Runtime = normalizeProgressEventText(e.Runtime, 256)
	e.OperationID = normalizeProgressEventText(e.OperationID, 128)
	if e.Model == "" && e.Runtime == "" {
		return BackendProgressEvent{}, false
	}
	e.Phase = normalizeProgressEventText(e.Phase, 64)
	if e.Phase == "" {
		e.Phase = "unknown"
	}
	e.Message = normalizeProgressEventText(e.Message, 2048)
	e.Error = normalizeProgressEventText(e.Error, 2048)
	e.OutputStream = normalizeProgressEventText(e.OutputStream, 16)
	if e.Output != "" {
		e.Output = normalizeProgressOutput(e.Output, 32<<10)
		if e.Output != "" && e.OutputStream == "" {
			e.OutputStream = "stdout"
		}
	}
	if math.IsNaN(e.Progress) || math.IsInf(e.Progress, 0) {
		e.Progress = 0
	} else if e.Progress < 0 {
		e.Progress = 0
	} else if e.Progress > 1 {
		e.Progress = 1
	}
	if e.Completed < 0 {
		e.Completed = 0
	}
	if e.Total < -1 {
		e.Total = -1
	}
	if e.Total >= 0 && e.Completed > e.Total {
		e.Completed = e.Total
	}
	return e, true
}

func normalizeProgressOutput(value string, maxBytes int) string {
	var builder strings.Builder
	builder.Grow(len(value))
	for _, r := range value {
		switch r {
		case '\n', '\r', '\t':
			builder.WriteRune(r)
		default:
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				continue
			}
			builder.WriteRune(r)
		}
	}
	return truncateProgressString(builder.String(), maxBytes)
}

func normalizeProgressEventText(value string, maxBytes int) string {
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ""
		}
		if unicode.IsSpace(r) && r != ' ' {
			return ""
		}
	}
	return truncateProgressString(strings.TrimSpace(value), maxBytes)
}

func truncateProgressString(value string, maxBytes int) string {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	// Keep the longest head whose byte length fits the budget by walking
	// runes forward once. Re-encoding the whole remaining string per dropped
	// rune (the previous implementation) is quadratic: trimming a 256KB log
	// tail to 32KB re-encoded gigabytes and stalled the handler for minutes.
	truncated := []rune(value)
	used := 0
	end := 0
	for end < len(truncated) {
		width := utf8.RuneLen(truncated[end])
		if used+width > maxBytes {
			break
		}
		used += width
		end++
	}
	return string(truncated[:end])
}
