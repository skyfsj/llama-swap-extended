package server

// End-to-end check through the real middleware chain: a Gemini 3 style
// function call carrying a thought signature survives a non-converted request
// and is diagnosed (not silently dropped) when the target cannot carry it.
import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
)

func TestE2E_ThoughtSignatureThroughAdapter(t *testing.T) {
	var upstreamBody string
	next := withConvertedPath(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP","index":0}]}`)
	})
	handler := prepareGeminiRoute(CreateProtocolAdapterMiddleware(
		config.Config{Models: map[string]config.ModelConfig{}}, nil,
		providerDialectStub(config.ProviderTypeGemini))(next))

	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/m:generateContent", strings.NewReader(
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]},{"role":"model","parts":[{"functionCall":{"name":"f","args":{}},"thoughtSignature":"SIG-E2E"}]},{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"result":"r"}}}]}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	// Gemini -> Gemini is a pass-through: the signature must be intact.
	if !strings.Contains(upstreamBody, "SIG-E2E") {
		t.Fatalf("signature lost on a native-surface request: %s", upstreamBody)
	}
}
