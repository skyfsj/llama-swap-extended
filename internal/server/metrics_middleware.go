package server

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// ProviderModelNamer reports the client-facing name of a provider-routed
// model; "" means the name is not served by the gateway.
type ProviderModelNamer func(model string) string

// CreateMetricsMiddleware returns middleware that records token metrics for
// model-dispatched POST requests. It resolves the model, tees the response into
// a buffer, and parses token usage once the upstream handler returns.
//
// providerName canonicalizes a provider model to its client-facing name so the
// usage and request tables show one row per model: the same model can be
// requested by its client-facing name, an alias or the provider-qualified form,
// and the records would otherwise split across spellings. It may be nil.
func CreateMetricsMiddleware(mm *metricsMonitor, cfg config.Config, providerName ProviderModelNamer) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if mm == nil || r.Method != http.MethodPost {
				next.ServeHTTP(w, r)
				return
			}

			// Determine the model-routed endpoint path. Regular routes are
			// already meterable; /upstream/<model>/<path> is metered only when
			// the remaining path matches a model-dispatched endpoint.
			checkPath := r.URL.Path
			if strings.HasPrefix(r.URL.Path, "/upstream/") {
				var found bool
				_, _, checkPath, found = swaputil.FindModelInPath(cfg, strings.TrimPrefix(r.URL.Path, "/upstream"))
				if !found {
					next.ServeHTTP(w, r)
					return
				}
			}

			if !isMetricsRecordPath(checkPath) {
				next.ServeHTTP(w, r)
				return
			}

			// Resolve the model now so downstream dispatch hits the context
			// fast path; FetchContext restores the request body for regular
			// routes and extracts the model from the URL for /upstream routes.
			data, err := swaputil.FetchContext(r, cfg)
			if err != nil {
				swaputil.SendError(w, r, err)
				return
			}

			// Buffer the request body and headers for durable audit before
			// dispatch consumes them.
			var reqBody []byte
			var reqHeaders map[string]string
			if mm.audit.Enabled {
				if r.Body != nil {
					if buffered, err := io.ReadAll(io.LimitReader(r.Body, swaputil.MaxRequestBodySize+1)); err == nil && len(buffered) <= swaputil.MaxRequestBodySize {
						reqBody = buffered
						r.Body.Close()
						swaputil.ReplaceRequestBody(r, reqBody)
					} else if err != nil {
						swaputil.SendResponse(w, r, http.StatusBadRequest, "could not read request body for metrics")
						return
					} else {
						swaputil.SendError(w, r, fmt.Errorf("%w: request body exceeds %d bytes", swaputil.ErrRequestBodyTooLarge, swaputil.MaxRequestBodySize))
						return
					}
				}
				reqHeaders = headerMap(r.Header)
				redactHeaders(reqHeaders)
			}

			// Restrict Accept-Encoding to encodings we can decompress so the
			// buffered response body stays parseable.
			if ae := r.Header.Get("Accept-Encoding"); ae != "" {
				r.Header.Set("Accept-Encoding", filterAcceptEncoding(ae))
			}

			recorder := newBodyCopier(w)
			next.ServeHTTP(recorder, r)
			// A request abandoned before any response was written must not be
			// filed as a successful (empty-body) metric. See #1029.
			swaputil.MarkClientClosed(recorder, r)
			recorded := data.ModelID
			if providerName != nil {
				if canonical := providerName(data.ModelID); canonical != "" {
					recorded = canonical
				}
			}
			mm.record(recorded, r, recorder, reqBody, reqHeaders)
		})
	}
}
