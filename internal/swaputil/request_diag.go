package swaputil

import (
	"context"
	"strings"
)

// RequestDiagnostics carries what the inner layers learned about a request
// back out to the outermost request logger, which is the layer that decides
// whether a request becomes an incident archive.
//
// Contexts flow downward only, so a middleware that resolves the model cannot
// hand the value to the middleware that wrapped it: by the time the inner one
// knows, the outer one's own context has already been built. A shared holder
// placed in the context by the outer layer is the way to publish it, and the
// pointer makes the write visible to the outer layer after the handler
// returns.
//
// The struct is deliberately tiny. The model name and the client-visible
// error text are what an operator needs to read a failed request; request and
// response bodies stay out, so this cannot become a covert copy of the
// traffic flowing through the proxy.
type RequestDiagnostics struct {
	// Model is the configured model ID the request resolved to, empty when the
	// request carried no model selector or none could be resolved.
	Model string
	// Provider is the upstream provider the request was actually served by,
	// empty for a locally hosted model. Adapter names the provider's API
	// surface when the request went through a multi-surface provider.
	//
	// A bare provider ID is not enough to answer "where did this go": the same
	// provider can expose one model on several surfaces (an Anthropic client
	// and an OpenAI client reach different adapters), and the same model name
	// can be served by several providers at once. Both are resolved only deep
	// inside the gateway, which is why they travel back through the holder.
	Provider string
	Adapter  string
	// Local marks a request served by a locally hosted model. It is what lets a
	// reader tell "this ran here" apart from "no upstream answered": both leave
	// Provider empty, and a failed provider request must not be reported as a
	// local one.
	Local bool
}

type requestDiagnosticsKey struct{}

// WithRequestDiagnostics attaches an empty diagnostics holder to ctx. It is
// called by the outermost request logger before the request is dispatched.
func WithRequestDiagnostics(ctx context.Context) context.Context {
	return context.WithValue(ctx, requestDiagnosticsKey{}, &RequestDiagnostics{})
}

// PublishRequestModel records the model the request resolved to. It is a
// no-op when the request carries no holder, so middleware assembled by hand
// (and embedders that skip the request logger) are unaffected.
func PublishRequestModel(ctx context.Context, model string) {
	if ctx == nil {
		return
	}
	diagnostics, ok := ctx.Value(requestDiagnosticsKey{}).(*RequestDiagnostics)
	if !ok || diagnostics == nil {
		return
	}
	if diagnostics.Model == "" {
		diagnostics.Model = strings.TrimSpace(model)
	}
}

// PublishRequestProvider records the provider (and adapter, when the provider
// has several API surfaces) that served the request.
//
// The first publisher wins: a request that failed over between providers keeps
// the one that actually answered, and later layers cannot rewrite it. A
// locally hosted model publishes nothing, so the field stays empty and the UI
// shows the request as local.
func PublishRequestProvider(ctx context.Context, provider, adapter string) {
	if ctx == nil {
		return
	}
	diagnostics, ok := ctx.Value(requestDiagnosticsKey{}).(*RequestDiagnostics)
	if !ok || diagnostics == nil {
		return
	}
	if provider = strings.TrimSpace(provider); provider == "" || diagnostics.Provider != "" || diagnostics.Local {
		return
	}
	diagnostics.Provider = provider
	diagnostics.Adapter = strings.TrimSpace(adapter)
	// The live in-flight row watches the same decision. It cannot read the
	// holder when it renders: the row is built by the middleware that created
	// it, which has already handed control to the gateway, so the value is
	// pushed to it instead — on the request's own goroutine, which keeps the
	// row's copy race-free.
	reportInferenceRoute(ctx, provider, diagnostics.Adapter)
}

// ReportInferenceAttempt tells the in-flight table which upstream the current
// attempt is going to. It is called when a route is picked, before the
// upstream has answered, so a row stays attributed during a slow first token
// instead of only at the end. It deliberately does not touch the diagnostics
// holder: the finished record keeps the route that produced the answer, which
// a failed-over attempt must not overwrite.
func ReportInferenceAttempt(ctx context.Context, provider, adapter string) {
	if ctx == nil {
		return
	}
	if provider = strings.TrimSpace(provider); provider == "" {
		return
	}
	reportInferenceRoute(ctx, provider, strings.TrimSpace(adapter))
}

// PublishLocalRoute records that the request is served by a locally hosted
// model: the record marks it local, and the in-flight row resolves to no
// provider. First publisher wins, exactly as it does for a provider, so a
// request that was handed to a provider first keeps that attribution.
func PublishLocalRoute(ctx context.Context) {
	if ctx == nil {
		return
	}
	if diagnostics, ok := ctx.Value(requestDiagnosticsKey{}).(*RequestDiagnostics); ok && diagnostics != nil {
		if diagnostics.Provider != "" {
			return
		}
		diagnostics.Local = true
	}
	ReportLocalRoute(ctx)
}

// ReportLocalRoute tells the in-flight table that the request is served here
// and therefore has no upstream provider. It is reported at the dispatch point
// rather than inferred from the model name: bare-name arbitration can send a
// name that exists locally to a provider, and a thermally parked model can be
// forwarded to one.
func ReportLocalRoute(ctx context.Context) {
	if ctx == nil {
		return
	}
	reportInferenceRoute(ctx, "", "")
}

func reportInferenceRoute(ctx context.Context, provider, adapter string) {
	if reporter, ok := ctx.Value(inferenceRouteReporterKey{}).(InferenceRouteReporter); ok && reporter != nil {
		reporter(provider, adapter)
	}
}

// InferenceRouteReporter receives the upstream route a request is going to,
// with an empty provider meaning "resolved to no provider" rather than "not
// reported". It exists for observers that must attribute a request while it is
// still running — currently the in-flight table, which otherwise only learns
// where a request went after it finishes.
type InferenceRouteReporter func(provider, adapter string)

type inferenceRouteReporterKey struct{}

// WithInferenceRouteReporter attaches a route reporter to a request context.
// A nil reporter is treated as no reporter, so callers that do not track
// in-flight requests are unaffected.
func WithInferenceRouteReporter(ctx context.Context, reporter InferenceRouteReporter) context.Context {
	if ctx == nil || reporter == nil {
		return ctx
	}
	return context.WithValue(ctx, inferenceRouteReporterKey{}, reporter)
}

// RequestDiagnosticsFrom returns the holder attached to ctx, or nil when the
// request was not wrapped by the request logger.
func RequestDiagnosticsFrom(ctx context.Context) *RequestDiagnostics {
	if ctx == nil {
		return nil
	}
	diagnostics, _ := ctx.Value(requestDiagnosticsKey{}).(*RequestDiagnostics)
	return diagnostics
}

// RequestModel returns the resolved model for a request, or "" when the
// request carried no holder or no model could be resolved.
func RequestModel(ctx context.Context) string {
	diagnostics := RequestDiagnosticsFrom(ctx)
	if diagnostics == nil {
		return ""
	}
	return diagnostics.Model
}
