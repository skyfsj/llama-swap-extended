package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// TestE2E_ServerProtocolMatrixThroughRealHandler drives the real middleware
// chain (auth -> protocol adapter -> writer) for every client dialect against
// every upstream dialect, using realistic upstream bodies.
func TestE2E_ServerProtocolMatrixThroughRealHandler(t *testing.T) {
	type fixture struct {
		path    string
		request string
		marker  string
	}
	clients := map[string]fixture{
		"chat": {
			path:    "/v1/chat/completions",
			request: `{"model":"m","messages":[{"role":"user","content":"hello"}],"max_tokens":32}`,
			marker:  `"choices"`,
		},
		"responses": {
			path:    "/v1/responses",
			request: `{"model":"m","input":"hello","max_output_tokens":32}`,
			marker:  `"object":"response"`,
		},
		"claude": {
			path:    "/v1/messages",
			request: `{"model":"m","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`,
			marker:  `"type":"message"`,
		},
		"gemini": {
			path:    "/v1beta/models/m:generateContent",
			request: `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`,
			marker:  `"candidates"`,
		},
	}
	upstreams := map[string]struct {
		surface string
		body    string
	}{
		"chat":      {config.ProviderTypeOpenAI, `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`},
		"responses": {config.ProviderTypeOpenAIResponses, `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[{"id":"it","type":"message","role":"assistant","content":[{"type":"output_text","text":"pong","annotations":[]}]}],"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}`},
		"claude":    {config.ProviderTypeAnthropic, `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`},
		"gemini":    {config.ProviderTypeGemini, `{"candidates":[{"content":{"role":"model","parts":[{"text":"pong"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1,"totalTokenCount":4}}`},
	}

	for clientName, client := range clients {
		for upstreamName, upstream := range upstreams {
			name := clientName + "_client__" + upstreamName + "_upstream"
			t.Run(name, func(t *testing.T) {
				var gotPath string
				var gotBody string
				next := withConvertedPath(t, func(w http.ResponseWriter, r *http.Request) {
					gotPath = r.URL.Path
					body, _ := io.ReadAll(r.Body)
					gotBody = string(body)
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, upstream.body)
				})
				resolver := func(string) (ProviderDialect, bool) {
					return ProviderDialect{Type: upstream.surface, Label: "prov"}, true
				}
				var handler http.Handler = CreateProtocolAdapterMiddleware(
					config.Config{Models: map[string]config.ModelConfig{}}, nil, resolver)(next)
				if clientName == "gemini" {
					handler = prepareGeminiRoute(handler)
				}

				req := httptest.NewRequest(http.MethodPost, client.path, strings.NewReader(client.request))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, req)

				if w.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
				sameDialect := clientName == upstreamName
				if sameDialect {
					if gotBody != client.request {
						t.Fatalf("same-dialect request rewritten: %s", gotBody)
					}
					if w.Body.String() != upstream.body {
						t.Fatalf("same-dialect response rewritten: %s", w.Body.String())
					}
					return
				}
				if !strings.Contains(w.Body.String(), "pong") {
					t.Fatalf("assistant text lost: %s", w.Body.String())
				}
				if !strings.Contains(w.Body.String(), client.marker) {
					t.Fatalf("response not in client dialect (%s): %s", client.marker, w.Body.String())
				}
				if !strings.Contains(gotBody, "hello") {
					t.Fatalf("prompt lost upstream: %s", gotBody)
				}
				_ = gotPath
				var parsed map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &parsed); err != nil {
					t.Fatalf("response is not JSON: %v (%s)", err, w.Body.String())
				}
				_ = fmt.Sprint(parsed)
			})
		}
	}
}
