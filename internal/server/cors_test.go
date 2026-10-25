package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
)

// corsTestHandler exercises the CORS middleware in isolation.
func corsTestHandler(cfg config.Config) http.Handler {
	return chain.New(CreateCORSMiddleware(cfg)).Then(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

func TestCORSMiddleware_LegacyDefault(t *testing.T) {
	handler := corsTestHandler(config.Config{})

	t.Run("origin gets the wildcard", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/running", nil)
		r.Header.Set("Origin", "https://dashboard.example.com")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)

		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
		}
		if got := w.Header().Get("Vary"); got != "Origin" {
			t.Errorf("Vary = %q, want Origin", got)
		}
	})

	t.Run("no origin gets no cors headers", func(t *testing.T) {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/running", nil))

		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Access-Control-Allow-Origin = %q, want empty", got)
		}
	})

	t.Run("preflight mirrors requested headers", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
		r.Header.Set("Origin", "https://dashboard.example.com")
		r.Header.Set("Access-Control-Request-Headers", "X-Custom, Content-Type")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)

		if w.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", w.Code)
		}
		if got := w.Header().Get("Access-Control-Allow-Headers"); got != "X-Custom, Content-Type" {
			t.Errorf("Access-Control-Allow-Headers = %q", got)
		}
		if got := w.Header().Get("Access-Control-Max-Age"); got != "86400" {
			t.Errorf("Access-Control-Max-Age = %q, want 86400", got)
		}
		// A preflight whose response depends on the request's Origin and
		// Access-Control-Request-Headers must say so, or a shared cache can
		// replay it for a different origin.
		vary := w.Header().Get("Vary")
		if !strings.Contains(vary, "Origin") {
			t.Errorf("Vary = %q, want it to include Origin", vary)
		}
		if !strings.Contains(vary, "Access-Control-Request-Headers") {
			t.Errorf("Vary = %q, want it to include Access-Control-Request-Headers", vary)
		}
	})

	t.Run("preflight with a fixed header list varies only by origin", func(t *testing.T) {
		cfg := config.Config{Security: config.SecurityConfig{CORS: config.CORSConfig{
			AllowedOrigins: []string{"*"},
			AllowedHeaders: []string{"Authorization"},
		}}}
		r := httptest.NewRequest(http.MethodOptions, "/api/models", nil)
		r.Header.Set("Origin", "https://anywhere.example")
		r.Header.Set("Access-Control-Request-Headers", "X-Ignored")
		w := httptest.NewRecorder()
		corsTestHandler(cfg).ServeHTTP(w, r)

		vary := w.Header().Get("Vary")
		if !strings.Contains(vary, "Origin") {
			t.Errorf("Vary = %q, want it to include Origin", vary)
		}
		// The echoed list is not consulted, so the response does not vary by it.
		if strings.Contains(vary, "Access-Control-Request-Headers") {
			t.Errorf("Vary = %q, want no Access-Control-Request-Headers", vary)
		}
	})
}

func TestCORSMiddleware_Allowlist(t *testing.T) {
	cfg := config.Config{Security: config.SecurityConfig{CORS: config.CORSConfig{
		AllowedOrigins:   []string{"https://dashboard.example.com"},
		AllowCredentials: true,
	}}}
	handler := corsTestHandler(cfg)

	t.Run("allowed origin is echoed", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/running", nil)
		r.Header.Set("Origin", "https://dashboard.example.com")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)

		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://dashboard.example.com" {
			t.Errorf("Access-Control-Allow-Origin = %q", got)
		}
		if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
			t.Errorf("Access-Control-Allow-Credentials = %q, want true", got)
		}
	})

	t.Run("origin match is case-insensitive and echoes the request spelling", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/running", nil)
		r.Header.Set("Origin", "https://DASHBOARD.example.com")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)

		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://DASHBOARD.example.com" {
			t.Errorf("Access-Control-Allow-Origin = %q, want the request spelling", got)
		}
	})

	t.Run("other origin gets no headers", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/running", nil)
		r.Header.Set("Origin", "https://evil.example")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)

		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Access-Control-Allow-Origin = %q, want empty", got)
		}
	})

	t.Run("disallowed preflight still answers 204 without headers", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
		r.Header.Set("Origin", "https://evil.example")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)

		if w.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", w.Code)
		}
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Access-Control-Allow-Origin = %q, want empty", got)
		}
	})
}

func TestCORSMiddleware_ConfiguredHeadersAndMethods(t *testing.T) {
	cfg := config.Config{Security: config.SecurityConfig{CORS: config.CORSConfig{
		AllowedOrigins: []string{"*"},
		AllowedMethods: []string{"GET", "POST"},
		AllowedHeaders: []string{"Authorization"},
		ExposedHeaders: []string{"X-Request-Id"},
		MaxAge:         120,
	}}}
	handler := corsTestHandler(cfg)

	r := httptest.NewRequest(http.MethodOptions, "/api/models", nil)
	r.Header.Set("Origin", "https://anywhere.example")
	r.Header.Set("Access-Control-Request-Headers", "X-Ignored")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if got := w.Header().Get("Access-Control-Allow-Methods"); got != "GET, POST" {
		t.Errorf("Access-Control-Allow-Methods = %q", got)
	}
	// A configured list wins over echoing the request.
	if got := w.Header().Get("Access-Control-Allow-Headers"); got != "Authorization" {
		t.Errorf("Access-Control-Allow-Headers = %q", got)
	}
	if got := w.Header().Get("Access-Control-Expose-Headers"); got != "X-Request-Id" {
		t.Errorf("Access-Control-Expose-Headers = %q", got)
	}
	if got := w.Header().Get("Access-Control-Max-Age"); got != "120" {
		t.Errorf("Access-Control-Max-Age = %q", got)
	}
}

func TestCORSMiddleware_PrivateNetwork(t *testing.T) {
	t.Run("answered when configured with an explicit origin", func(t *testing.T) {
		cfg := config.Config{Security: config.SecurityConfig{CORS: config.CORSConfig{
			AllowedOrigins:      []string{"https://app.example.com"},
			AllowPrivateNetwork: true,
		}}}
		handler := corsTestHandler(cfg)

		r := httptest.NewRequest(http.MethodOptions, "/api/models", nil)
		r.Header.Set("Origin", "https://app.example.com")
		r.Header.Set("Access-Control-Request-Private-Network", "true")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)

		if got := w.Header().Get("Access-Control-Allow-Private-Network"); got != "true" {
			t.Errorf("Access-Control-Allow-Private-Network = %q, want true", got)
		}
	})

	t.Run("never sent for the wildcard policy", func(t *testing.T) {
		cfg := config.Config{Security: config.SecurityConfig{CORS: config.CORSConfig{
			AllowedOrigins:      []string{"*"},
			AllowPrivateNetwork: true,
		}}}
		handler := corsTestHandler(cfg)

		r := httptest.NewRequest(http.MethodOptions, "/api/models", nil)
		r.Header.Set("Origin", "https://anywhere.example")
		r.Header.Set("Access-Control-Request-Private-Network", "true")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)

		if got := w.Header().Get("Access-Control-Allow-Private-Network"); got != "" {
			t.Errorf("Access-Control-Allow-Private-Network = %q, want empty", got)
		}
	})
}

// The cross-origin management write guard is independent of the CORS policy:
// a wildcard policy must not open /api/ writes to other origins.
func TestCORSMiddleware_ManagementWriteGuardIsIndependent(t *testing.T) {
	for _, origins := range [][]string{nil, {"*"}, {"https://dashboard.example.com"}} {
		cfg := config.Config{Security: config.SecurityConfig{CORS: config.CORSConfig{AllowedOrigins: origins}}}
		handler := corsTestHandler(cfg)

		r := httptest.NewRequest(http.MethodPost, "/api/config/yaml", nil)
		r.Header.Set("Origin", "https://evil.example")
		r.Host = "localhost:8080"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)

		if w.Code != http.StatusForbidden {
			t.Errorf("origins=%v status = %d, want 403", origins, w.Code)
		}
	}
}
