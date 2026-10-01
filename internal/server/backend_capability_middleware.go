package server

import (
	"net/http"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/route"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

var backendCapabilityRegistry = func() *route.Registry {
	registry, err := route.NewRegistry(route.DefaultDescriptors())
	if err != nil {
		return nil
	}
	return registry
}()

// CreateBackendCapabilityMiddleware enforces an explicitly configured backend
// API allowlist. Discovery is deliberately not consulted here: a failed or
// stale probe must never make a configured inference route disappear, while an
// operator's explicit apis list must always win over discovery.
func CreateBackendCapabilityMiddleware(cfg config.Config) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capability := backendCapabilityForPath(r.Method, r.URL.Path)
			if capability == "" {
				next.ServeHTTP(w, r)
				return
			}
			data, ok := swaputil.ReadContext(r.Context())
			if !ok || data.ModelID == "" {
				// Request-context middleware normally runs before this middleware.
				// Keep the fallback permissive so a custom embedding that orders
				// middleware differently does not reject a valid request here.
				next.ServeHTTP(w, r)
				return
			}
			modelConfig, exists := cfg.Models[data.ModelID]
			if !exists || len(modelConfig.Backend.APIs) == 0 || backendAPIAllowed(modelConfig.Backend.APIs, capability) {
				next.ServeHTTP(w, r)
				return
			}
			swaputil.SendResponse(w, r, http.StatusNotImplemented, "backend capability "+capability+" is not enabled for model "+data.ModelID)
		})
	}
}

func backendAPIAllowed(apis []string, capability string) bool {
	capability = strings.ToLower(strings.TrimSpace(capability))
	for _, api := range apis {
		api = strings.ToLower(strings.TrimSpace(api))
		if api == capability || api == "*" || api == "all" {
			return true
		}
	}
	return false
}

// backendCapabilityForPath maps only model-routed inference paths. The
// control-plane endpoints are protected by their own scopes and must not be
// affected by a model's API allowlist.
func backendCapabilityForPath(method, path string) string {
	method = strings.ToUpper(strings.TrimSpace(method))
	path = strings.TrimSpace(path)
	if backendCapabilityRegistry != nil {
		if descriptor, _, ok := backendCapabilityRegistry.Match(method, path); ok && descriptor.Capability != "" {
			return descriptor.Capability
		}
	}
	// WebSocket upgrades arrive as HTTP GET requests, while the registry uses a
	// dedicated WEBSOCKET method to distinguish frame-based model extraction.
	// Keep this small compatibility branch until the HTTP upgrade middleware can
	// pass the pseudo-method through the descriptor matcher.
	if method == http.MethodGet && (path == "/v1/realtime" || path == "/v/realtime") {
		return "realtime"
	}
	if method == http.MethodPost {
		switch path {
		case "/v1/chat/completions", "/v1/chat/completions/batch", "/v1/batches", "/v/chat/completions":
			return "chat"
		case "/v1/responses", "/v/responses":
			return "responses"
		case "/v1/completions", "/v/completions", "/completion", "/infill":
			return "completions"
		case "/v1/embeddings", "/v/embeddings":
			return "embeddings"
		case "/v1/audio/transcriptions":
			return "transcriptions"
		case "/v1/audio/translations":
			return "translations"
		case "/v1/audio/speech":
			return "speech"
		case "/v1/images/generations", "/v1/images/edits", "/sdapi/v1/txt2img", "/sdapi/v1/img2img":
			return "images"
		case "/v1/messages", "/v1/messages/count_tokens", "/v/messages", "/v/messages/count_tokens":
			return "anthropic"
		case "/v1/classify", "/classify":
			return "classify"
		case "/v1/pooling", "/pooling":
			return "pooling"
		case "/v1/generative_scoring", "/generative_scoring":
			return "generative_scoring"
		case "/v1/embed", "/v2/embed", "/embed":
			return "embed"
		case "/v1/rerank", "/v2/rerank", "/v1/reranking", "/rerank", "/reranking":
			return "rerank"
		case "/v1/score", "/score":
			return "score"
		}
	}
	if method == http.MethodGet {
		switch path {
		case "/v1/audio/voices":
			return "speech"
		case "/v1/realtime", "/v/realtime":
			return "realtime"
		}
	}
	return ""
}
