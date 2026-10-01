package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/protocol/anthropiccache"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

type anthropicCacheTelemetry struct {
	Applied    bool
	Detected   bool
	Enabled    bool
	PrefixHash string
	Transforms []anthropiccache.TransformResult
	Anomalies  []string
}

type anthropicCacheTelemetryKey struct{}

func cacheTelemetryFromContext(r *http.Request) anthropicCacheTelemetry {
	telemetry, _ := r.Context().Value(anthropicCacheTelemetryKey{}).(anthropicCacheTelemetry)
	return telemetry
}

// CreateAnthropicCacheMiddleware applies the cache-fix pipeline only to the
// Anthropic Messages endpoint. It runs after request-context extraction so the
// model router still sees the original model and before legacy body filters.
func CreateAnthropicCacheMiddleware(cfg config.Config) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || (r.URL.Path != "/v1/messages" && r.URL.Path != "/v/messages") || !strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
				next.ServeHTTP(w, r)
				return
			}
			if r.Body == nil || r.Body == http.NoBody {
				swaputil.SendResponse(w, r, http.StatusBadRequest, "could not read Anthropic request body")
				return
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, swaputil.MaxRequestBodySize+1))
			if err != nil {
				swaputil.SendResponse(w, r, http.StatusBadRequest, "could not read Anthropic request body")
				return
			}
			if len(body) > swaputil.MaxRequestBodySize {
				swaputil.SendError(w, r, fmt.Errorf("%w: Anthropic request body exceeds %d bytes", swaputil.ErrRequestBodyTooLarge, swaputil.MaxRequestBodySize))
				return
			}
			_ = r.Body.Close()
			result, err := anthropiccache.Apply(body, r.Header, anthropiccache.Options{
				Mode: anthropiccache.Mode(cfg.Anthropic.CacheFix.Mode),
				Transforms: anthropiccache.TransformConfig{
					FingerprintStrip:      cfg.Anthropic.CacheFix.Transforms.FingerprintStrip,
					SortStabilization:     cfg.Anthropic.CacheFix.Transforms.SortStabilization,
					FreshSessionSort:      cfg.Anthropic.CacheFix.Transforms.FreshSessionSort,
					IdentityNormalization: cfg.Anthropic.CacheFix.Transforms.IdentityNormalization,
					CacheControlNormalize: cfg.Anthropic.CacheFix.Transforms.CacheControlNormalize,
					TTLManagement:         cfg.Anthropic.CacheFix.Transforms.TTLManagement,
					ThinkingSanitize:      cfg.Anthropic.CacheFix.Transforms.ThinkingSanitize,
					CCVersionNormalize:    cfg.Anthropic.CacheFix.Transforms.CCVersionNormalize,
					HighRisk:              cfg.Anthropic.CacheFix.Transforms.HighRisk,
				},
			})
			if err != nil {
				if cfg.Anthropic.CacheFix.Mode == "force" {
					swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				next.ServeHTTP(w, r)
				return
			}
			if result.Enabled {
				body = result.Body
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.Header.Del("Transfer-Encoding")
			r.Header.Set("Content-Length", strconv.Itoa(len(body)))
			r.ContentLength = int64(len(body))
			telemetry := anthropicCacheTelemetry{
				Applied:    result.Changed,
				Detected:   result.Detected,
				Enabled:    result.Enabled,
				PrefixHash: result.PrefixHash,
				Transforms: append([]anthropiccache.TransformResult(nil), result.Transforms...),
				Anomalies:  append([]string(nil), result.Anomalies...),
			}
			r = r.WithContext(withAnthropicCacheTelemetry(r.Context(), telemetry))
			next.ServeHTTP(w, r)
		})
	}
}

func withAnthropicCacheTelemetry(ctx context.Context, telemetry anthropicCacheTelemetry) context.Context {
	return context.WithValue(ctx, anthropicCacheTelemetryKey{}, telemetry)
}
