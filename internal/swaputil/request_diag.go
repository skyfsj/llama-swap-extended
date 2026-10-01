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
