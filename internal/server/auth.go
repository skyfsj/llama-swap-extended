package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// apiKeyAuthChallenge deliberately advertises bearer credentials to browsers.
// Basic credentials remain accepted by ExtractAPIKey for existing API clients,
// but advertising Basic makes browsers open their own username/password dialog
// on an ordinary API 401, competing with the WebUI's API-key login screen.
const apiKeyAuthChallenge = `Bearer realm="llama-swap"`

// CreateAuthMiddleware returns middleware that validates API keys when the
// config declares any. It accepts the key via Authorization: Bearer,
// Authorization: Basic (password field), or x-api-key. When no keys are
// configured the middleware is a pass-through.
func CreateAuthMiddleware(cfg config.Config) chain.Middleware {
	keys := cfg.RequiredAPIKeys
	return func(next http.Handler) http.Handler {
		if len(keys) == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			provided := swaputil.ExtractAPIKey(r)

			valid := false
			for _, key := range keys {
				if swaputil.ConstantTimeEquals(provided, key) {
					valid = true
					break
				}
			}
			if !valid {
				w.Header().Set("WWW-Authenticate", apiKeyAuthChallenge)
				swaputil.SendResponse(w, r, http.StatusUnauthorized, "unauthorized: invalid or missing API key")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// lookupAPIIdentity validates one presented credential against both legacy
// YAML keys and salted WebUI-managed keys. Keeping this in one helper avoids
// subtle differences between normal requests and the browser session login
// endpoint. Authentication still uses the hash/salt projection; the retained
// raw value is reserved for explicit integrations and is not returned here.
//
// This is the store-direct path: it runs on the per-request probe fallback
// and the session endpoints for servers without a key cache. Servers with a
// cache authenticate from memory instead (see keyCache.validate).
func lookupAPIIdentity(ctx context.Context, cfg config.Config, st *store.Store, provided string) (auth.Identity, bool, error) {
	provided = strings.TrimSpace(provided)
	if provided == "" {
		return auth.Identity{}, false, nil
	}
	for _, key := range cfg.RequiredAPIKeys {
		if swaputil.ConstantTimeEquals(provided, key) {
			return auth.Identity{ID: store.KeyFingerprint(key), Legacy: true}, true, nil
		}
	}
	if st == nil {
		return auth.Identity{}, false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	record, found, err := st.FindAPIKeyBySecret(ctx, provided)
	if err != nil || !found {
		return auth.Identity{}, false, err
	}
	// An access key keeps working only while its management parent is still
	// valid, so resolve the parent row before deriving the identity.
	var parent *store.APIKeyRecord
	if record.Kind == auth.KeyKindAccess && strings.TrimSpace(record.ParentID) != "" {
		parentRecord, parentFound, parentErr := st.FindAPIKeyByID(ctx, record.ParentID)
		if parentErr != nil {
			return auth.Identity{}, false, parentErr
		}
		if parentFound {
			parent = &parentRecord
		}
	}
	return identityForKeyRecord(cfg, record, parent)
}

// CreateRequestContextMiddleware returns middleware that extracts model and
// auth info from the request into the context. Requests where no model can be
// identified are rejected with a 404.
func CreateRequestContextMiddleware(cfg config.Config) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r = markInflightStart(r)
			data, err := swaputil.FetchContext(r, cfg)
			if err != nil {
				swaputil.SendError(w, r, err)
				return
			}
			// Maintenance mode rejects the request here rather than letting the
			// router resolve it: the model keeps its configuration but is not
			// servable, and a 503 that names the reason is far clearer to a
			// client than a 404 from the proxy or an attempt to start it.
			if modelDisabled(cfg, data) {
				swaputil.SendResponse(w, r, http.StatusServiceUnavailable,
					"model "+data.Model+" is disabled (maintenance mode)")
				return
			}
			// Let the outermost request logger know which model this resolved
			// to: that is the layer which writes the incident archive, and a
			// failed request archived without the model name is a 503 nobody
			// can attribute. Publishing from here rather than in the logger
			// works because contexts only ever flow downward.
			swaputil.PublishRequestModel(r.Context(), data.ModelID)
			if data.Metadata == nil {
				data.Metadata = make(map[string]string)
			}
			if identity := identityFromContext(r.Context()); identity.ID != "" && identity.ID != "anonymous" {
				data.Metadata["key_id"] = identity.ID
			}
			for _, header := range cfg.UI.Activity.SessionID {
				if value := strings.TrimSpace(r.Header.Get(header)); value != "" {
					data.Metadata["session_id"] = value
					break
				}
			}
			*r = *r.WithContext(swaputil.SetContext(r.Context(), data))
			next.ServeHTTP(w, r)
		})
	}
}

// CreateCORSMiddleware returns middleware that answers OPTIONS preflight
// requests with permissive CORS headers (see issues #81, #77, #42). Non-OPTIONS
// requests pass through untouched, except state-changing requests aimed at the
// management API (/api/) that carry a cross-origin browser Origin header:
// those are rejected. The web UI is same-origin, so no legitimate client can
// make such a request, while a malicious web page can drive a keyless or
// session-cookie deployment (the permissive preflight makes /api/ writes
// preflightable). Cross-origin inference paths keep their API contract.
func CreateCORSMiddleware() chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodOptions {
				if isCrossOriginManagementWrite(r) {
					swaputil.SendResponse(w, r, http.StatusForbidden, "cross-origin management requests are not allowed")
					return
				}
				next.ServeHTTP(w, r)
				return
			}

			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			if headers := r.Header.Get("Access-Control-Request-Headers"); headers != "" {
				w.Header().Set("Access-Control-Allow-Headers", sanitizeAccessControlRequestHeaderValues(headers))
			} else {
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Accept, X-Requested-With")
			}
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
		})
	}
}

// isCrossOriginManagementWrite reports whether the request is a state-changing
// browser request aimed at the management API from a different origin.
func isCrossOriginManagementWrite(r *http.Request) bool {
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		return false
	}
	return !sameOrigin(origin, r.Host)
}

// sameOrigin compares an Origin header value against the request's Host,
// tolerating the omitted default port browsers use in Origin.
func sameOrigin(origin, host string) bool {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return false
	}
	return normalizeHostPort(parsed.Host, parsed.Scheme) == normalizeHostPort(host, parsed.Scheme)
}

func normalizeHostPort(h, scheme string) string {
	if h == "" {
		return ""
	}
	// An explicit port (or an IPv6 literal) is compared as-is; browsers omit
	// only the scheme's default port.
	if strings.Contains(h, ":") && !strings.HasPrefix(h, "[") {
		return h
	}
	switch scheme {
	case "https":
		if strings.HasSuffix(h, ":443") {
			return h
		}
		return h + ":443"
	case "http":
		if strings.HasSuffix(h, ":80") {
			return h
		}
		return h + ":80"
	}
	return h
}

func isTokenChar(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z':
	case r >= 'A' && r <= 'Z':
	case r >= '0' && r <= '9':
	case strings.ContainsRune("!#$%&'*+-.^_`|~", r):
	default:
		return false
	}
	return true
}

// sanitizeAccessControlRequestHeaderValues drops any header names that contain
// characters outside the HTTP token grammar before echoing them back.
func sanitizeAccessControlRequestHeaderValues(headerValues string) string {
	parts := strings.Split(headerValues, ",")
	valid := make([]string, 0, len(parts))

	for _, p := range parts {
		v := strings.TrimSpace(p)
		if v == "" {
			continue
		}

		validPart := true
		for _, c := range v {
			if !isTokenChar(c) {
				validPart = false
				break
			}
		}
		if validPart {
			valid = append(valid, v)
		}
	}

	return strings.Join(valid, ", ")
}

// modelDisabled reports whether the requested model is in maintenance mode. The
// check resolves the request through the same real-name lookup the router uses,
// so an alias or a selector target pointing at a disabled model is rejected
// too. A selector that merely names a disabled model among its targets is not
// disabled itself: only the model that actually resolves is.
func modelDisabled(cfg config.Config, data swaputil.ReqContextData) bool {
	modelID, found := cfg.RealModelName(data.ModelID)
	if !found {
		return false
	}
	model, exists := cfg.Models[modelID]
	return exists && model.Disabled
}
