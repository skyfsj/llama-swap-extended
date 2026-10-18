package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// TestProtocol_UpstreamSendsJSONWhenSSEWasRequested guards the fallback for an
// upstream that ignores alt=sse and answers with one bare JSON body: the client
// must receive the converted answer, not an empty stream.
func TestProtocol_UpstreamSendsJSONWhenSSEWasRequested(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// No "data:" prefix, no blank-line framing: plain JSON.
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"pong"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":2,"totalTokenCount":9}}`)
	})
	handler := CreateProtocolAdapterMiddleware(config.Config{Models: map[string]config.ModelConfig{}}, nil,
		providerDialectStub(config.ProviderTypeGemini))(next)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"m","input":"hi","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "pong") {
		t.Fatalf("bare JSON answer was dropped: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "response.completed") {
		t.Fatalf("no terminal frame for the Responses client: %s", w.Body.String())
	}
}
