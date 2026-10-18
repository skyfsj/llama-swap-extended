package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServer_SecurityHeaders(t *testing.T) {
	handler := CreateSecurityHeadersMiddleware(true)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	cases := []struct {
		path        string
		wantCSP     bool
		description string
	}{
		{"/ui/", true, "the SPA gets a content security policy"},
		{"/api/config", false, "API responses need the other headers but no CSP"},
		{"/health", false, "health is a plain text probe"},
	}
	for _, c := range cases {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, c.path, nil))
		if got := recorder.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options=%q", c.path, got)
		}
		if got := recorder.Header().Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("%s: X-Frame-Options=%q", c.path, got)
		}
		if got := recorder.Header().Get("Referrer-Policy"); got != "no-referrer" {
			t.Errorf("%s: Referrer-Policy=%q", c.path, got)
		}
		if got := recorder.Header().Get("Permissions-Policy"); got == "" {
			t.Errorf("%s: Permissions-Policy missing", c.path)
		}
		csp := recorder.Header().Get("Content-Security-Policy")
		if c.wantCSP && csp == "" {
			t.Errorf("%s: Content-Security-Policy missing (%s)", c.path, c.description)
		}
		if !c.wantCSP && csp != "" {
			t.Errorf("%s: unexpected Content-Security-Policy %q", c.path, csp)
		}
	}
}

func TestServer_SecurityHeadersPassthroughAndDisabled(t *testing.T) {
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	// Reverse-proxied applications own their own headers.
	for _, path := range []string{"/upstream/model/v1/models", "/comfyui/object_info"} {
		recorder := httptest.NewRecorder()
		CreateSecurityHeadersMiddleware(true)(final).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if got := recorder.Header().Get("X-Frame-Options"); got != "" {
			t.Errorf("%s: passthrough added X-Frame-Options=%q", path, got)
		}
	}

	// The operator can turn the whole set off.
	recorder := httptest.NewRecorder()
	CreateSecurityHeadersMiddleware(false)(final).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/ui/", nil))
	if got := recorder.Header().Get("X-Content-Type-Options"); got != "" {
		t.Errorf("disabled middleware added X-Content-Type-Options=%q", got)
	}
}

func TestServer_SecurityHeadersUIPolicyDeniesFramingAndInlineScript(t *testing.T) {
	recorder := httptest.NewRecorder()
	CreateSecurityHeadersMiddleware(true)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})).
		ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/ui/", nil))
	policy := recorder.Header().Get("Content-Security-Policy")
	for _, want := range []string{"frame-ancestors 'none'", "object-src 'none'", "script-src 'self'"} {
		if !strings.Contains(policy, want) {
			t.Errorf("policy %q is missing %q", policy, want)
		}
	}
	// Inline script must stay forbidden; only styles need the exception.
	if strings.Contains(policy, "script-src 'self' 'unsafe-inline'") {
		t.Errorf("policy allows inline script: %q", policy)
	}
}
