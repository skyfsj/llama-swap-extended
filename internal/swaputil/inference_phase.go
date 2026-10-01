package swaputil

import (
	"context"
	"net/http"
)

// InferencePhaseReporter lets the router update the in-flight record that was
// created by the HTTP middleware without importing the server package. The
// callback is intentionally small: phase names and user-facing detail remain
// owned by the caller that understands the request lifecycle.
type InferencePhaseReporter func(phase, message string)

type inferencePhaseReporterKey struct{}

// WithInferencePhaseReporter attaches an in-flight phase reporter to a
// request context. A nil reporter is treated as no reporter.
func WithInferencePhaseReporter(ctx context.Context, reporter InferencePhaseReporter) context.Context {
	if ctx == nil || reporter == nil {
		return ctx
	}
	return context.WithValue(ctx, inferencePhaseReporterKey{}, reporter)
}

// ReportInferencePhase publishes a phase transition when the request carries
// a reporter. Callers that do not run behind the in-flight middleware are
// therefore unaffected.
func ReportInferencePhase(ctx context.Context, phase, message string) {
	if ctx == nil {
		return
	}
	if reporter, ok := ctx.Value(inferencePhaseReporterKey{}).(InferencePhaseReporter); ok && reporter != nil {
		reporter(phase, message)
	}
}

// OperatorStartKey marks a request an operator explicitly started — currently
// the UI's load button, which reaches a model through /upstream/<model>/.
// Maintenance mode refuses inference requests naming a model, so this marker
// is what lets the operator perform the start that clears the state.
type operatorStartKey struct{}

// WithOperatorStart marks a request as an explicit operator start.
func WithOperatorStart(ctx context.Context) context.Context {
	if ctx == nil {
		return ctx
	}
	return context.WithValue(ctx, operatorStartKey{}, true)
}

// IsOperatorStart reports whether the request is an explicit operator start.
func IsOperatorStart(r *http.Request) bool {
	if r == nil {
		return false
	}
	return r.Context().Value(operatorStartKey{}) == true
}
