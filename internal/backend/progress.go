package backend

import (
	"strings"
	"unicode"

	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

type ProgressEvent = swaputil.BackendProgressEvent

const (
	maxProgressPhase   = 64
	maxProgressMessage = 2048
)

// Normalize bounds progress returned by a remote backend before it reaches
// the control-plane API. Backend progress is diagnostic input rather than a
// trusted local value: reject invisible/control characters, trim ordinary
// padding, cap strings and keep counters non-negative. The phase fallback
// preserves the existing idle semantics for older adapters that return an
// empty object.
func (p Progress) Normalize() Progress {
	p.Phase = normalizeProgressText(p.Phase, maxProgressPhase)
	if p.Phase == "" {
		p.Phase = "idle"
	}
	p.Message = normalizeProgressText(p.Message, maxProgressMessage)
	p.Error = normalizeProgressText(p.Error, maxProgressMessage)
	if p.Completed < 0 {
		p.Completed = 0
	}
	if p.Total < 0 {
		p.Total = 0
	}
	return p
}

func normalizeProgressText(value string, maxBytes int) string {
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ""
		}
		if unicode.IsSpace(r) && r != ' ' {
			return ""
		}
	}
	value = strings.TrimSpace(value)
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	runes := []rune(value)
	for len(string(runes)) > maxBytes {
		runes = runes[:len(runes)-1]
	}
	return string(runes)
}

func PublishProgress(progress ProgressEvent) {
	if normalized, ok := progress.Normalize(); ok {
		event.Emit(normalized)
	}
}
