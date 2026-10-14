package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

func TestBackendCapabilityForPath(t *testing.T) {
	tests := []struct {
		method, path, want string
	}{
		{http.MethodPost, "/v1/chat/completions", "chat"},
		{http.MethodPost, "/v1/responses", "responses"},
		{http.MethodPost, "/v1/audio/transcriptions", "transcriptions"},
		{http.MethodPost, "/v1/rerank", "rerank"},
		{http.MethodPost, "/v1/pooling", "pooling"},
		{http.MethodPost, "/v1/generative_scoring", "generative_scoring"},
		{http.MethodPost, "/v1/messages/count_tokens", "anthropic"},
		{http.MethodPost, "/v1/audio/translations", "translations"},
		{http.MethodGet, "/v1/realtime", "realtime"},
		{http.MethodGet, "/api/backends/m/cache", ""},
	}
	for _, test := range tests {
		if got := backendCapabilityForPath(test.method, test.path); got != test.want {
			t.Errorf("backendCapabilityForPath(%q, %q) = %q, want %q", test.method, test.path, got, test.want)
		}
	}
}

func TestBackendProtocolCapabilityTargetsChat(t *testing.T) {
	for _, test := range []struct {
		path       string
		capability string
	}{
		{"/v1/chat/completions", "chat"},
		{"/v1/responses", "responses"},
		{"/v1/messages", "anthropic"},
		{"/v1beta/models/m:generateContent", "gemini"},
	} {
		if got := backendProtocolCapability(config.AdapterProtocolNative, http.MethodPost, test.path, test.capability); got != "chat" {
			t.Errorf("backendProtocolCapability(%q) = %q, want chat", test.path, got)
		}
	}
	if got := backendProtocolCapability(config.AdapterProtocolNative, http.MethodPost, "/v1/messages/count_tokens", "anthropic"); got != "anthropic" {
		t.Fatalf("count_tokens capability = %q, want anthropic", got)
	}
}

func TestBackendCapabilityMiddlewareExplicitAllowlist(t *testing.T) {
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"m": {Backend: config.BackendConfig{APIs: []string{"chat"}}},
	}}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := CreateBackendCapabilityMiddleware(cfg)(next)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m"}`))
	request = request.WithContext(swaputil.SetContext(request.Context(), swaputil.ReqContextData{ModelID: "m", Model: "m"}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("Responses-to-Chat target status = %d, body=%s", recorder.Code, recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	request = request.WithContext(swaputil.SetContext(request.Context(), swaputil.ReqContextData{ModelID: "m", Model: "m"}))
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("enabled capability status = %d, body=%s", recorder.Code, recorder.Body.String())
	}

	nativeCfg := config.Config{Models: map[string]config.ModelConfig{
		"m": {Backend: config.BackendConfig{Protocol: config.AdapterProtocolNative, APIs: []string{"chat"}}},
	}}
	nativeHandler := CreateBackendCapabilityMiddleware(nativeCfg)(next)
	request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m"}`))
	request = request.WithContext(swaputil.SetContext(request.Context(), swaputil.ReqContextData{ModelID: "m", Model: "m"}))
	recorder = httptest.NewRecorder()
	nativeHandler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("Chat-only backend capability status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestBackendCapabilityAllowlistWildcard(t *testing.T) {
	if !backendAPIAllowed([]string{" all "}, "responses") || !backendAPIAllowed([]string{"*"}, "chat") {
		t.Fatal("wildcard capability allowlist was not accepted")
	}
	if backendAPIAllowed([]string{"chat"}, "responses") {
		t.Fatal("unlisted capability was accepted")
	}
}
