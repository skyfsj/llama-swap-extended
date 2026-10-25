package process

import (
	"encoding/json"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// maxLoadingMessageLen caps the loading message an upstream reports, in
// characters. It is shown in the UI and resent on every progress update.
const maxLoadingMessageLen = 1024

// maxProgressBodyLen is the largest health check body parsed for loading
// progress. A progress report is a few dozen bytes; anything much bigger is
// not one.
const maxProgressBodyLen = 64 * 1024

// LoadingProgress is what an upstream reported about its own startup in the
// body of a not-yet-ready health check response.
type LoadingProgress struct {
	// Progress is the fraction loaded, from 0 to 1. Nil when the upstream
	// sent only a message.
	Progress *float64
	// Message describes the current loading step. Empty when the upstream
	// sent only a progress value.
	Message string
}

func (lp *LoadingProgress) equal(o *LoadingProgress) bool {
	if lp == nil || o == nil {
		return lp == o
	}
	if lp.Message != o.Message || (lp.Progress == nil) != (o.Progress == nil) {
		return false
	}
	return lp.Progress == nil || *lp.Progress == *o.Progress
}

// parseLoadingProgress reads loading progress from a health check body shaped
// like {"progress": 0.42, "message": "loading tensors"}. Both fields are
// optional, but a body with neither, or one that is not a JSON object, is not
// a progress report. llama-server's 503 body while loading is one such body.
func parseLoadingProgress(body []byte) (LoadingProgress, bool) {
	if len(body) == 0 || len(body) > maxProgressBodyLen {
		return LoadingProgress{}, false
	}
	var raw struct {
		Progress *float64 `json:"progress"`
		Message  *string  `json:"message"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return LoadingProgress{}, false
	}

	var lp LoadingProgress
	if raw.Progress != nil {
		v := min(max(*raw.Progress, 0), 1)
		lp.Progress = &v
	}
	if raw.Message != nil {
		lp.Message = truncateRunes(strings.TrimSpace(*raw.Message), maxLoadingMessageLen)
	}
	if lp.Progress == nil && lp.Message == "" {
		return LoadingProgress{}, false
	}
	return lp, true
}

func truncateRunes(s string, n int) string {
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// reportLoading publishes one loading-progress update on the shared backend
// progress stream. The process may have moved on while the health check was
// in flight, so a report that arrives after the start finished is dropped: it
// would otherwise overwrite the ready/idle state the UI already shows.
func (p *ProcessCommand) reportLoading(lp LoadingProgress) bool {
	if p.State() != StateStarting {
		return false
	}
	progress := 0.0
	if lp.Progress != nil {
		progress = *lp.Progress
	}
	event.Emit(swaputil.BackendProgressEvent{
		Model:    p.id,
		Phase:    "loading",
		Progress: progress,
		Message:  lp.Message,
	})
	return true
}
