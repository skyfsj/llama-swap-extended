package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/store"
)

func TestServer_SanitizeAccessControlRequestHeaders(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Content-Type, Authorization", "Content-Type, Authorization"},
		{"  X-Custom ,  Accept ", "X-Custom, Accept"},
		{"Valid, Bad Header", "Valid"},
		{"Bad@Header", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := sanitizeAccessControlRequestHeaderValues(c.in); got != c.want {
			t.Errorf("sanitize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestServer_IsTokenChar(t *testing.T) {
	for _, r := range "abcXYZ0129!#$%&'*+-.^_`|~" {
		if !isTokenChar(r) {
			t.Errorf("isTokenChar(%q) = false, want true", r)
		}
	}
	for _, r := range " @()/\t\"" {
		if isTokenChar(r) {
			t.Errorf("isTokenChar(%q) = true, want false", r)
		}
	}
}

func TestServer_RequestContextMiddleware(t *testing.T) {
	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			"llama3": {},
		},
	}

	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mw := CreateRequestContextMiddleware(cfg)

	t.Run("known model passes through", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"llama3"}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mw(final).ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}
	})

	t.Run("missing model returns 404", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mw(final).ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", w.Code)
		}
	})
}

// TestServer_RequestContextMiddleware_Disabled covers maintenance mode at the
// request path: the model keeps its configuration but is not servable, so the
// request is rejected with a 503 naming the reason instead of being routed.
func TestServer_RequestContextMiddleware_Disabled(t *testing.T) {
	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			"live":   {},
			"paused": {Disabled: true},
			// An alias resolves through the same lookup, so naming a disabled
			// model by one of its aliases is rejected too.
			"aliased": {Disabled: true, Aliases: []string{"alt"}},
		},
	}

	reached := false
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	mw := CreateRequestContextMiddleware(cfg)

	t.Run("disabled model returns 503 without reaching the router", func(t *testing.T) {
		reached = false
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"paused"}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mw(final).ServeHTTP(w, r)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", w.Code)
		}
		if reached {
			t.Error("the request reached the router, want it rejected at the gate")
		}
		if body := w.Body.String(); !strings.Contains(body, "maintenance mode") {
			t.Errorf("body = %q, want a reason naming maintenance mode", body)
		}
	})

	t.Run("alias of a disabled model returns 503", func(t *testing.T) {
		reached = false
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alt"}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mw(final).ServeHTTP(w, r)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", w.Code)
		}
		if reached {
			t.Error("the request reached the router, want it rejected at the gate")
		}
	})

	t.Run("enabled model still passes through", func(t *testing.T) {
		reached = false
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"live"}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mw(final).ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}
		if !reached {
			t.Error("the request did not reach the router")
		}
	})
}

func TestServer_AuthMiddleware(t *testing.T) {
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	t.Run("no keys configured passes through", func(t *testing.T) {
		mw := CreateAuthMiddleware(config.Config{})
		w := httptest.NewRecorder()
		mw(final).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}
	})

	cfg := config.Config{RequiredAPIKeys: []string{"secret"}}

	t.Run("valid key", func(t *testing.T) {
		mw := CreateAuthMiddleware(cfg)
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", "Bearer secret")
		w := httptest.NewRecorder()
		mw(final).ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}
	})

	t.Run("invalid key", func(t *testing.T) {
		mw := CreateAuthMiddleware(cfg)
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", "Bearer wrong")
		w := httptest.NewRecorder()
		mw(final).ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", w.Code)
		}
		if got := w.Header().Get("WWW-Authenticate"); got != apiKeyAuthChallenge {
			t.Errorf("WWW-Authenticate = %q, want %q", got, apiKeyAuthChallenge)
		}
	})
}

func TestServer_APIAuthSessionCookie(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()
	s := &Server{cfg: config.Config{RequiredAPIKeys: []string{"legacy;key"}}, store: st}

	request := httptest.NewRequest(http.MethodPost, "/api/auth/session", strings.NewReader(`{"key":"legacy;key"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	s.handleAPIAuthSession(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", response.Code, response.Body.String())
	}
	var payload map[string]bool
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || !payload["authenticated"] {
		t.Fatalf("login payload=%s err=%v", response.Body.String(), err)
	}
	cookieHeader := response.Header().Get("Set-Cookie")
	if !strings.Contains(cookieHeader, "llama_swap_api_key=") || !strings.Contains(cookieHeader, "HttpOnly") || !strings.Contains(cookieHeader, "SameSite=Strict") {
		t.Fatalf("session cookie=%q", cookieHeader)
	}

	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies=%v", cookies)
	}
	protected := CreateAuthMiddleware(s.cfg)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	protectedRequest := httptest.NewRequest(http.MethodGet, "/", nil)
	protectedRequest.AddCookie(cookies[0])
	protectedResponse := httptest.NewRecorder()
	protected.ServeHTTP(protectedResponse, protectedRequest)
	if protectedResponse.Code != http.StatusNoContent {
		t.Fatalf("cookie auth status=%d body=%s", protectedResponse.Code, protectedResponse.Body.String())
	}

	logout := httptest.NewRecorder()
	s.handleAPIAuthSession(logout, httptest.NewRequest(http.MethodDelete, "/api/auth/session", nil))
	if logout.Code != http.StatusNoContent || !strings.Contains(logout.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("logout status=%d cookie=%q", logout.Code, logout.Header().Get("Set-Cookie"))
	}
}

func TestServer_APIAuthSessionRejectsInvalidKey(t *testing.T) {
	s := &Server{cfg: config.Config{RequiredAPIKeys: []string{"expected"}}}
	request := httptest.NewRequest(http.MethodPost, "/api/auth/session", strings.NewReader(`{"key":"wrong"}`))
	response := httptest.NewRecorder()
	s.handleAPIAuthSession(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("invalid login status=%d body=%s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("WWW-Authenticate"); got != apiKeyAuthChallenge {
		t.Errorf("WWW-Authenticate = %q, want %q", got, apiKeyAuthChallenge)
	}
	if response.Header().Get("Set-Cookie") != "" {
		t.Fatal("invalid login unexpectedly set a cookie")
	}
}

func TestServer_APIAuthSessionAcceptsManagedKey(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()
	secret := "sk-session-managed"
	salt, digest, err := auth.GenerateSalted(secret)
	if err != nil {
		t.Fatalf("GenerateSalted: %v", err)
	}
	if err := st.UpsertAPIKey(context.Background(), store.APIKeyRecord{ID: "managed", KeyHash: digest, KeySalt: salt, Scopes: []string{auth.ScopeKeysAdmin}, AllowManagementLogin: true}); err != nil {
		t.Fatalf("UpsertAPIKey: %v", err)
	}
	s := &Server{cfg: config.Config{}, store: st}
	request := httptest.NewRequest(http.MethodPost, "/api/auth/session", strings.NewReader(`{"key":"`+secret+`"}`))
	response := httptest.NewRecorder()
	s.handleAPIAuthSession(response, request)
	if response.Code != http.StatusOK || len(response.Result().Cookies()) != 1 {
		t.Fatalf("managed login status=%d cookies=%v body=%s", response.Code, response.Result().Cookies(), response.Body.String())
	}
	protected := CreateScopedAuthMiddleware(s.cfg, st, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity := identityFromContext(r.Context())
		if identity.ID != "managed" || !identity.Has(auth.ScopeKeysAdmin) {
			t.Errorf("identity=%+v", identity)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	protectedRequest := httptest.NewRequest(http.MethodGet, "/", nil)
	protectedRequest.AddCookie(response.Result().Cookies()[0])
	protectedResponse := httptest.NewRecorder()
	protected.ServeHTTP(protectedResponse, protectedRequest)
	if protectedResponse.Code != http.StatusNoContent {
		t.Fatalf("managed cookie auth status=%d body=%s", protectedResponse.Code, protectedResponse.Body.String())
	}
}

func TestServer_APIAuthSessionRejectsManagedKeyWithoutManagementLogin(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()
	secret := "sk-inference-only"
	salt, digest, err := auth.GenerateSalted(secret)
	if err != nil {
		t.Fatalf("GenerateSalted: %v", err)
	}
	if err := st.UpsertAPIKey(context.Background(), store.APIKeyRecord{ID: "inference-only", KeyHash: digest, KeySalt: salt, Scopes: []string{auth.ScopeInference}}); err != nil {
		t.Fatalf("UpsertAPIKey: %v", err)
	}
	s := &Server{cfg: config.Config{}, store: st}
	request := httptest.NewRequest(http.MethodPost, "/api/auth/session", strings.NewReader(`{"key":"`+secret+`"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	s.handleAPIAuthSession(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("managed login status=%d body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get("Set-Cookie") != "" {
		t.Fatal("management-panel-disabled key unexpectedly set a cookie")
	}
	statusRequest := httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	statusRequest.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: url.QueryEscape(secret)})
	statusResponse := httptest.NewRecorder()
	s.handleAPIAuthSession(statusResponse, statusRequest)
	var statusPayload map[string]bool
	if err := json.Unmarshal(statusResponse.Body.Bytes(), &statusPayload); err != nil || statusPayload["authenticated"] {
		t.Fatalf("disabled management status payload=%s err=%v", statusResponse.Body.String(), err)
	}
}

func TestServer_UIAssetsDoNotRequireAPIKey(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), nil)
	defer s.store.Close()
	s.cfg = config.Config{RequiredAPIKeys: []string{"required"}}
	s.routes()

	response := httptest.NewRecorder()
	s.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ui/", nil))
	// Plain test binaries do not embed UI assets, so a 404 is expected here.
	// The contract is that the login shell is not blocked with a 401 first.
	if response.Code == http.StatusUnauthorized {
		t.Fatalf("UI status=%d, want a non-auth response", response.Code)
	}
}

// TestServer_CORSGuardsCrossOriginManagementWrites verifies the CSRF guard:
// state-changing browser requests to the management API from another origin
// are rejected, while same-origin, non-browser (no Origin), and cross-origin
// inference requests pass through.
func TestServer_CORSGuardsCrossOriginManagementWrites(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := chain.New(CreateCORSMiddleware()).Then(next)

	tests := []struct {
		name   string
		method string
		path   string
		origin string
		want   int
	}{
		{"cross-origin management write rejected", http.MethodPost, "/api/config/yaml", "https://evil.example", http.StatusForbidden},
		{"cross-origin delete rejected", http.MethodDelete, "/api/keys/k1", "https://evil.example", http.StatusForbidden},
		{"same-origin management write passes", http.MethodPost, "/api/config/yaml", "http://localhost:8080", http.StatusOK},
		{"non-browser write without origin passes", http.MethodPost, "/api/config/yaml", "", http.StatusOK},
		{"cross-origin inference write passes", http.MethodPost, "/v1/chat/completions", "https://playground.example", http.StatusOK},
		{"cross-origin read passes", http.MethodGet, "/api/models", "https://evil.example", http.StatusOK},
		{"preflight still permissive", http.MethodOptions, "/v1/chat/completions", "https://playground.example", http.StatusNoContent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, tt.path, nil)
			if tt.origin != "" {
				r.Header.Set("Origin", tt.origin)
			}
			// The UI is served from the same host:port the browser requests.
			r.Host = "localhost:8080"
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tt.want {
				t.Fatalf("status=%d want %d", w.Code, tt.want)
			}
		})
	}
}
