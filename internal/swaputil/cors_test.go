package swaputil

import (
	"net/http"
	"testing"
)

func TestStripUpstreamCORSHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET")
	h.Set("Access-Control-Allow-Headers", "X")
	h.Set("Access-Control-Allow-Credentials", "true")
	h.Set("Access-Control-Expose-Headers", "X")
	h.Set("Access-Control-Max-Age", "1")
	h.Set("Access-Control-Allow-Private-Network", "true")
	h.Set("Content-Type", "application/json")

	StripUpstreamCORSHeaders(h)

	for _, name := range corsResponseHeaders {
		if got := h.Get(name); got != "" {
			t.Errorf("%s = %q, want it stripped", name, got)
		}
	}
	if got := h.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, unrelated headers must survive", got)
	}
}

// TestStripUpstreamCORSHeaders_LowercaseKeys checks the helper works on headers
// built with non-canonical keys, which is how a raw map literal spells them.
func TestStripUpstreamCORSHeaders_LowercaseKeys(t *testing.T) {
	h := http.Header{}
	h["access-control-allow-origin"] = []string{"*"}
	h["ACCESS-CONTROL-ALLOW-METHODS"] = []string{"GET"}
	StripUpstreamCORSHeaders(h)
	if len(h) != 0 {
		t.Errorf("header map = %v, want empty", h)
	}
}

func TestStripUpstreamCORSHeaders_EmptyHeader(t *testing.T) {
	h := http.Header{}
	StripUpstreamCORSHeaders(h)
	if len(h) != 0 {
		t.Errorf("header map = %v, want empty", h)
	}
}

func TestStripBrowserRequestHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Origin", "https://app.example.com")
	h.Set("Referer", "https://app.example.com/")
	h.Set("Access-Control-Request-Method", "POST")
	h.Set("Access-Control-Request-Headers", "Authorization")
	h.Set("Access-Control-Request-Private-Network", "true")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Site", "cross-site")
	h.Set("Authorization", "Bearer key")
	h.Set("Content-Type", "application/json")

	StripBrowserRequestHeaders(h)

	for _, name := range []string{
		"Origin", "Referer",
		"Access-Control-Request-Method",
		"Access-Control-Request-Headers",
		"Access-Control-Request-Private-Network",
		"Sec-Fetch-Mode", "Sec-Fetch-Site",
	} {
		if got := h.Get(name); got != "" {
			t.Errorf("%s = %q, want it stripped", name, got)
		}
	}
	if got := h.Get("Authorization"); got != "Bearer key" {
		t.Errorf("Authorization = %q, credentials must survive", got)
	}
	if got := h.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, must survive", got)
	}
}
