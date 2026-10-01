package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// CreateModelNameRewriteMiddleware returns middleware that rewrites only the
// model name in a JSON request body — the name an alias requested under is
// replaced by the name the model's engine serves — and applies no other
// filters. The /upstream passthrough uses it so an alias in a JSON body reaches
// the engine, while the rest of that route's body stays exactly as the client
// wrote it.
func CreateModelNameRewriteMiddleware(cfg config.Config) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Media types are case-insensitive (RFC 9110).
			if !strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
				next.ServeHTTP(w, r)
				return
			}

			data, err := swaputil.FetchContext(r, cfg)
			if err != nil {
				// The /upstream passthrough names the model in the URL path, not
				// the body, so a request without a model field is legitimate
				// there and must be forwarded unchanged rather than rejected.
				if errors.Is(err, swaputil.ErrNoModelInContext) && strings.HasPrefix(r.URL.Path, "/upstream/") {
					next.ServeHTTP(w, r)
					return
				}
				swaputil.SendError(w, r, err)
				return
			}

			upstreamName, _, ok := resolveFilters(cfg, data.Model)
			if !ok || upstreamName == "" {
				next.ServeHTTP(w, r)
				return
			}

			if r.Body == nil || r.Body == http.NoBody {
				next.ServeHTTP(w, r)
				return
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, swaputil.MaxRequestBodySize+1))
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}
			_ = r.Body.Close()

			body, err = applyModelNameRewrite(body, data.Model, upstreamName)
			if err != nil {
				swaputil.SendError(w, r, err)
				return
			}

			r.Body = io.NopCloser(bytes.NewReader(body))
			r.Header.Del("Transfer-Encoding")
			r.Header.Set("Content-Length", strconv.Itoa(len(body)))
			r.ContentLength = int64(len(body))

			next.ServeHTTP(w, r)
		})
	}
}

// applyModelNameRewrite replaces the body's model field with the name the
// engine serves, but only when the field currently names the model this request
// resolved through. A body naming something else — a profile replacement the
// client asked for, or an engine name the client already uses — is returned
// unchanged, so the passthrough never rewrites a name it was not asked about.
func applyModelNameRewrite(body []byte, requested, upstreamName string) ([]byte, error) {
	if upstreamName == "" || gjson.GetBytes(body, "model").String() != requested {
		return body, nil
	}
	rewritten, err := sjson.SetBytes(body, "model", upstreamName)
	if err != nil {
		return nil, fmt.Errorf("error rewriting model name in JSON: %w", err)
	}
	return rewritten, nil
}

// CreateFilterMiddleware returns middleware that applies per-model request-body
// filters to JSON requests before they are forwarded upstream:
//
//   - UseModelName rewrite (issue #69)
//   - Alias rewrite to the name the engine serves
//   - StripParams removal (issue #174)
//   - SetParams injection (issue #453)
//   - SetParamsByID per-alias overrides
//
// Non-JSON requests (GET, multipart forms) pass through untouched. The buffered
// body is re-attached with Content-Length / Transfer-Encoding cleanup so the
// downstream reverse proxy forwards the correct bytes (see issue #11).
func CreateFilterMiddleware(cfg config.Config) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Media types are case-insensitive (RFC 9110); matching case
			// sensitively skipped filtering entirely for "Application/JSON".
			if !strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
				next.ServeHTTP(w, r)
				return
			}

			data, err := swaputil.FetchContext(r, cfg)
			if err != nil {
				// The /upstream passthrough names the model in the URL path, not
				// the body, so a request without a model field is legitimate
				// there and must be forwarded unchanged rather than rejected.
				if errors.Is(err, swaputil.ErrNoModelInContext) && strings.HasPrefix(r.URL.Path, "/upstream/") {
					next.ServeHTTP(w, r)
					return
				}
				swaputil.SendError(w, r, err)
				return
			}

			upstreamName, filters, ok := resolveFilters(cfg, data.Model)
			if !ok {
				next.ServeHTTP(w, r)
				return
			}

			if r.Body == nil || r.Body == http.NoBody {
				swaputil.SendResponse(w, r, http.StatusBadRequest, "could not read request body")
				return
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, swaputil.MaxRequestBodySize+1))
			if err != nil {
				swaputil.SendResponse(w, r, http.StatusBadRequest, "could not read request body")
				return
			}
			if len(body) > swaputil.MaxRequestBodySize {
				swaputil.SendError(w, r, fmt.Errorf("%w: request body exceeds %d bytes", swaputil.ErrRequestBodyTooLarge, swaputil.MaxRequestBodySize))
				return
			}
			_ = r.Body.Close()

			body, err = applyFilters(body, data.Model, upstreamName, filters)
			if err != nil {
				swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
				return
			}

			r.Body = io.NopCloser(bytes.NewReader(body))
			r.Header.Del("Transfer-Encoding")
			r.Header.Set("Content-Length", strconv.Itoa(len(body)))
			r.ContentLength = int64(len(body))

			next.ServeHTTP(w, r)
		})
	}
}

// CreateFormFilterMiddleware returns middleware that applies the UseModelName
// rewrite (issue #69) to multipart/form-data requests before they are forwarded
// upstream. JSON-body filters (StripParams, SetParams) do not apply to form
// endpoints; only the "model" field is rewritten.
//
// Non-multipart requests pass through untouched. When a rewrite is needed the
// form is reconstructed and re-attached with Content-Type / Content-Length
// cleanup so the downstream reverse proxy forwards the correct bytes.
func CreateFormFilterMiddleware(cfg config.Config) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.Contains(r.Header.Get("Content-Type"), "multipart/form-data") {
				next.ServeHTTP(w, r)
				return
			}

			data, err := swaputil.FetchContext(r, cfg)
			if err != nil {
				swaputil.SendError(w, r, err)
				return
			}

			upstreamName, _, ok := resolveFilters(cfg, data.Model)
			if !ok || upstreamName == "" {
				next.ServeHTTP(w, r)
				return
			}

			updated, err := swaputil.ReplaceRequestModel(r, data.Model, upstreamName)
			if err != nil {
				sendModelRewriteError(w, r, err)
				return
			}

			// UseModelName changes only the model name sent upstream. Keep the
			// original request context so routing and metrics still identify
			// the configured model.
			updated = updated.WithContext(r.Context())
			next.ServeHTTP(w, updated)
		})
	}
}

// upstreamModelName returns the name the model's engine actually serves, so a
// request that arrived under an alias can be rewritten to it before forwarding.
//
// The engines serve one name each: whatever the operator configured with
// useModelName, or the canonical model ID when that is unset. An alias is a
// llama-swap-side concept that the upstream process knows nothing about, so
// forwarding it unchanged makes the engine answer "The model X does not exist"
// even though routing resolved it correctly. useModelName keeps priority
// because the engine is started with it. Peers return "" because they choose
// their own naming and a name rewrite must not fight the remote server.
func upstreamModelName(cfg config.Config, requested string) (name string, ok bool) {
	if realName, found := cfg.RealModelName(requested); found {
		mc := cfg.Models[realName]
		if override := strings.TrimSpace(mc.UseModelName); override != "" {
			return override, true
		}
		return realName, true
	}
	if peerID, _, found := cfg.ResolvePeerModel(requested); found {
		_ = peerID
		return "", true
	}
	return "", false
}

// resolveFilters returns the filter settings and the upstream name for a
// requested model. The upstream name is empty when the request must be
// forwarded exactly as received (peers, and a local request that already names
// the model the engine serves).
func resolveFilters(cfg config.Config, requested string) (upstream string, filters config.Filters, ok bool) {
	if realName, found := cfg.RealModelName(requested); found {
		mc := cfg.Models[realName]
		name, _ := upstreamModelName(cfg, requested)
		if name == requested {
			name = ""
		}
		return name, mc.Filters.Filters, true
	}
	if peerID, _, found := cfg.ResolvePeerModel(requested); found {
		return "", cfg.Peers[peerID].Filters, true
	}
	return "", config.Filters{}, false
}

// applyFilters rewrites the JSON body in place. Order matches the legacy
// ProxyManager: the upstream model name, stripParams, setParams, then
// setParamsByID (which can override setParams). requested keeps the name the
// client used, because setParamsByID variants are keyed by it.
//
// The model field is only rewritten when it names the model this request
// resolved through — the alias or canonical ID. A body naming something else
// (a profile replacement the client asked for, or an engine name it already
// uses) is left alone, matching ReplaceRequestModel's rule for forms and the
// /upstream passthrough.
func applyFilters(body []byte, requested, upstreamName string, f config.Filters) ([]byte, error) {
	var err error

	if upstreamName != "" && gjson.GetBytes(body, "model").String() == requested {
		if body, err = sjson.SetBytes(body, "model", upstreamName); err != nil {
			return nil, fmt.Errorf("error rewriting model name in JSON: %w", err)
		}
	}
	for _, param := range f.SanitizedStripParams() {
		if body, err = sjson.DeleteBytes(body, param); err != nil {
			return nil, fmt.Errorf("error stripping parameter %s from request", param)
		}
	}

	setParams, setKeys := f.SanitizedSetParams()
	for _, key := range setKeys {
		if body, err = sjson.SetBytes(body, key, setParams[key]); err != nil {
			return nil, fmt.Errorf("error setting parameter %s in request", key)
		}
	}

	byID, byIDKeys := f.SanitizedSetParamsByID(requested)
	for _, key := range byIDKeys {
		if body, err = sjson.SetBytes(body, key, byID[key]); err != nil {
			return nil, fmt.Errorf("error setting parameter %s in request", key)
		}
	}

	return body, nil
}
