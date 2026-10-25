package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestServer_ResponsesWebSocketProbeAnswers426 pins the transport negotiation
// Codex needs: a WebSocket handshake on /v1/responses must answer 426 Upgrade
// Required, which is the only status that makes it fall back to HTTP. A 405
// makes it retry the handshake until its budget is exhausted.
func TestServer_ResponsesWebSocketProbeAnswers426(t *testing.T) {
	server := newTestServer(nil, nil)

	for _, path := range []string{"/v1/responses", "/v/responses"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Connection", "Upgrade")
		request.Header.Set("Upgrade", "websocket")
		request.Header.Set("Sec-WebSocket-Version", "13")
		request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		recorder := httptest.NewRecorder()

		server.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusUpgradeRequired {
			t.Fatalf("%s websocket probe = %d, want 426: %s", path, recorder.Code, recorder.Body.String())
		}
	}
}

// TestServer_ResponsesCollectionGetIs405 checks the plain GET is still refused
// with the correct method advertisement.
func TestServer_ResponsesCollectionGetIs405(t *testing.T) {
	server := newTestServer(nil, nil)

	request := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("plain GET = %d, want 405: %s", recorder.Code, recorder.Body.String())
	}
	if allow := recorder.Header().Get("Allow"); allow != http.MethodPost {
		t.Fatalf("Allow = %q, want POST", allow)
	}
}
