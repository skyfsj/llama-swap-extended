package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	protocolrelaykit "github.com/mostlygeek/llama-swap/internal/protocol/relaykit"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

func TestProtocolAdapterResponsesToChat(t *testing.T) {
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"m": {Backend: config.BackendConfig{Protocol: config.AdapterProtocolResponsesToChat}},
	}}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"hello"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	called := false
	handler := CreateProtocolAdapterMiddleware(cfg, nil, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		restore := rewriteProtocolBackendPath(r)
		defer restore()
		called = true
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("backend path = %q", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var request struct {
			Messages []any `json:"messages"`
		}
		if err := json.Unmarshal(body, &request); err != nil || len(request.Messages) != 1 {
			t.Fatalf("converted chat request = %s (%v)", body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`)
	}))
	handler.ServeHTTP(recorder, request)
	if !called || recorder.Code != http.StatusOK {
		t.Fatalf("called=%t status=%d body=%s", called, recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["object"] != "response" {
		t.Fatalf("response = %v", response)
	}
}

func TestProtocolAdapterAnthropicToChat(t *testing.T) {
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"m": {Backend: config.BackendConfig{Protocol: config.AdapterProtocolResponsesToChat}},
	}}
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m","max_tokens":128,"messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler := CreateProtocolAdapterMiddleware(cfg, nil, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		restore := rewriteProtocolBackendPath(r)
		defer restore()
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("backend path = %q", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var request map[string]any
		if err := json.Unmarshal(body, &request); err != nil || request["messages"] == nil {
			t.Fatalf("converted Chat request = %s (%v)", body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`)
	}))
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["type"] != "message" {
		t.Fatalf("response = %v", response)
	}
}

func TestProtocolAdapterGeminiRouteAndStream(t *testing.T) {
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"m": {Backend: config.BackendConfig{Protocol: config.AdapterProtocolNative}},
	}}
	request := httptest.NewRequest(http.MethodPost, "/v1beta/models/m:streamGenerateContent", strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		restore := rewriteProtocolBackendPath(r)
		defer restore()
		called = true
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("backend path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("alt") != "" {
			t.Fatalf("unexpected target query: %s", r.URL.RawQuery)
		}
		body, _ := io.ReadAll(r.Body)
		var request map[string]any
		if err := json.Unmarshal(body, &request); err != nil || request["model"] != "m" {
			t.Fatalf("converted chat request = %s (%v)", body, err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := []byte("data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n")
		_, _ = w.Write(chunk[:31])
		_, _ = w.Write(chunk[31:])
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	handler := prepareGeminiRoute(CreateProtocolAdapterMiddleware(cfg, nil, nil)(next))
	handler.ServeHTTP(recorder, request)
	if !called || recorder.Code != http.StatusOK {
		t.Fatalf("called=%t status=%d body=%s", called, recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Header().Get("Content-Type"), "text/event-stream") || !strings.Contains(recorder.Body.String(), "hello") {
		t.Fatalf("Gemini stream = %q, headers=%v", recorder.Body.String(), recorder.Header())
	}
	if strings.Contains(recorder.Body.String(), "chat.completion.chunk") || strings.Contains(recorder.Body.String(), "[DONE]") {
		t.Fatalf("Chat SSE leaked through Gemini stream: %q", recorder.Body.String())
	}
}

func TestProtocolTargetPathGemini(t *testing.T) {
	path, err := protocolrelaykit.TargetPath(protocolrelaykit.Gemini, "m", true)
	if err != nil || path != "/v1beta/models/m:streamGenerateContent" {
		t.Fatalf("TargetPath() = %q, %v", path, err)
	}
}

func TestProtocolTargetForUsesChatUpstream(t *testing.T) {
	for _, source := range []protocolrelaykit.Format{
		protocolrelaykit.OpenAIChat,
		protocolrelaykit.OpenAIResponses,
		protocolrelaykit.ClaudeMessages,
		protocolrelaykit.Gemini,
	} {
		target, enabled := protocolTargetFor(source, config.AdapterProtocolNative)
		if target != protocolrelaykit.OpenAIChat || enabled != (source != protocolrelaykit.OpenAIChat) {
			t.Errorf("protocolTargetFor(%q) = %q, %t", source, target, enabled)
		}
	}
}

func TestServer_GeminiGenerateContentUsesRelayKitRoute(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	var upstreamPath string
	var upstreamBody []byte
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		upstreamBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`)
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{
		"m": {Backend: config.BackendConfig{Type: "vllm", Protocol: config.AdapterProtocolNative, APIs: []string{"chat"}}},
	}}
	s.routes()

	request := httptest.NewRequest(http.MethodPost, "/v1beta/models/m:generateContent", strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-goog-api-key", "client-key")
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if upstreamPath != "/v1/chat/completions" || !strings.Contains(string(upstreamBody), `"model":"m"`) {
		t.Fatalf("upstream path=%q body=%s", upstreamPath, upstreamBody)
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	candidates, _ := response["candidates"].([]any)
	if len(candidates) != 1 {
		t.Fatalf("Gemini response = %v", response)
	}
}

// withConvertedPath mirrors the real dispatch: the protocol adapter marks the
// target path in the request context and localPeerHandlerWithConfig rewrites
// r.URL.Path from it before forwarding.
func withConvertedPath(t *testing.T, next http.HandlerFunc) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		restore := rewriteProtocolBackendPath(r)
		defer restore()
		next(w, r)
	}
}

// providerDialectStub resolves every model to one upstream API surface.
func providerDialectStub(surface string) ProviderDialectResolver {
	return func(string) (ProviderDialect, bool) {
		return ProviderDialect{Type: surface, Label: "prov/adapter"}, true
	}
}

func TestProtocolProviderChatClientToGeminiUpstream(t *testing.T) {
	var gotPath, gotBody string
	next := withConvertedPath(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"pong"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1,"totalTokenCount":4}}`))
	})
	cfg := config.Config{Models: map[string]config.ModelConfig{}}
	handler := CreateProtocolAdapterMiddleware(cfg, nil, providerDialectStub(config.ProviderTypeGemini))(next)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gemini-4-argon","messages":[{"role":"user","content":"ping"}],"max_tokens":16}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1beta/models/gemini-4-argon:generateContent" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	if !strings.Contains(gotBody, `"contents"`) {
		t.Fatalf("request was not converted to the Gemini dialect: %s", gotBody)
	}
	// The response is converted back into the client's dialect.
	body := w.Body.String()
	if !strings.Contains(body, `"choices"`) || !strings.Contains(body, "pong") {
		t.Fatalf("response was not converted to chat completions: %s", body)
	}
}

func TestProtocolProviderResponsesClientToGeminiUpstream(t *testing.T) {
	var gotPath, gotBody string
	next := withConvertedPath(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"pong"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1,"totalTokenCount":4}}`))
	})
	handler := CreateProtocolAdapterMiddleware(config.Config{Models: map[string]config.ModelConfig{}}, nil,
		providerDialectStub(config.ProviderTypeGemini))(next)

	req := httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"gemini-4-argon","input":"ping","max_output_tokens":16}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1beta/models/gemini-4-argon:generateContent" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	if !strings.Contains(gotBody, `"contents"`) {
		t.Fatalf("request was not converted to Gemini: %s", gotBody)
	}
	if body := w.Body.String(); !strings.Contains(body, `"object":"response"`) || !strings.Contains(body, "pong") {
		t.Fatalf("response was not converted back to Responses: %s", body)
	}
}

func TestProtocolProviderResponsesToChatUpstreamWhenDeclared(t *testing.T) {
	var gotPath, gotBody string
	next := withConvertedPath(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"deepseek-chat","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`))
	})
	// An openai adapter that declares it only speaks chat completions.
	handler := CreateProtocolAdapterMiddleware(config.Config{Models: map[string]config.ModelConfig{}}, nil,
		providerDialectStub(config.ProviderTypeOpenAI))(next)

	req := httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"deepseek-flash","input":"ping","max_output_tokens":16}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	if !strings.Contains(gotBody, `"messages"`) {
		t.Fatalf("request was not converted to chat completions: %s", gotBody)
	}
	if body := w.Body.String(); !strings.Contains(body, `"object":"response"`) || !strings.Contains(body, "pong") {
		t.Fatalf("response was not converted back to Responses: %s", body)
	}
}

func TestProtocolProviderOpenAIResponsesSurfacePassesThrough(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("openai-responses surface must forward /v1/responses untouched, got %q", r.URL.Path)
		}
		w.Write([]byte(`{"object":"response"}`))
	})
	handler := CreateProtocolAdapterMiddleware(config.Config{Models: map[string]config.ModelConfig{}}, nil,
		providerDialectStub(config.ProviderTypeOpenAIResponses))(next)

	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"grok-4.7","input":"ping"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if !called {
		t.Fatalf("request must be forwarded: status=%d body=%s", w.Code, w.Body.String())
	}
	if w.Body.String() != `{"object":"response"}` {
		t.Fatalf("response must pass through untouched: %s", w.Body.String())
	}
}

func TestProtocolProviderStreamingConversion(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		for _, event := range []string{
			`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"po"}]},"index":0}]}` + "\n\n",
			`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"ng"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":2,"totalTokenCount":4}}` + "\n\n",
		} {
			w.Write([]byte(event))
			if flusher != nil {
				flusher.Flush()
			}
		}
	})
	handler := CreateProtocolAdapterMiddleware(config.Config{Models: map[string]config.ModelConfig{}}, nil,
		providerDialectStub(config.ProviderTypeGemini))(next)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gemini-4-argon","messages":[{"role":"user","content":"ping"}],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "data: ") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("stream was not converted to chat SSE: %s", body)
	}
	if !strings.Contains(body, `"chat.completion.chunk"`) {
		t.Fatalf("chunks are not chat completions: %s", body)
	}
	if strings.Contains(body, `"candidates"`) {
		t.Fatalf("gemini frames leaked to the client: %s", body)
	}
}

// providerMatrixCases describe one client dialect and the wire shape its
// requests/responses use, so a table test can assert the conversion happened.
var (
	clientRequests = map[protocolrelaykit.Format]string{
		protocolrelaykit.OpenAIChat:      `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`,
		protocolrelaykit.OpenAIResponses: `{"model":"m","input":"hi","max_output_tokens":16}`,
		protocolrelaykit.ClaudeMessages:  `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		protocolrelaykit.Gemini:          `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":16}}`,
	}
	clientPaths = map[protocolrelaykit.Format]string{
		protocolrelaykit.OpenAIChat:      "/v1/chat/completions",
		protocolrelaykit.OpenAIResponses: "/v1/responses",
		protocolrelaykit.ClaudeMessages:  "/v1/messages",
		protocolrelaykit.Gemini:          "/v1beta/models/m:generateContent",
	}
	// The streaming form of each client path: for Gemini the action suffix
	// decides, so a streaming client must use streamGenerateContent.
	clientStreamPaths = map[protocolrelaykit.Format]string{
		protocolrelaykit.OpenAIChat:      "/v1/chat/completions",
		protocolrelaykit.OpenAIResponses: "/v1/responses",
		protocolrelaykit.ClaudeMessages:  "/v1/messages",
		protocolrelaykit.Gemini:          "/v1beta/models/m:streamGenerateContent",
	}
	// Upstream responses in each dialect (what the provider returns).
	upstreamResponses = map[protocolrelaykit.Format]string{
		protocolrelaykit.OpenAIChat:      `{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		protocolrelaykit.OpenAIResponses: `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[{"id":"item_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"pong","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`,
		protocolrelaykit.ClaudeMessages:  `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
		protocolrelaykit.Gemini:          `{"candidates":[{"content":{"role":"model","parts":[{"text":"pong"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`,
	}
	// A dialect's own markers: the converted request/response must use them.
	upstreamMarkers = map[protocolrelaykit.Format]string{
		protocolrelaykit.OpenAIChat:      `"messages"`,
		protocolrelaykit.OpenAIResponses: `"input"`,
		protocolrelaykit.ClaudeMessages:  `"max_tokens"`,
		protocolrelaykit.Gemini:          `"contents"`,
	}
	clientResponseMarkers = map[protocolrelaykit.Format]string{
		protocolrelaykit.OpenAIChat:      `"choices"`,
		protocolrelaykit.OpenAIResponses: `"object":"response"`,
		protocolrelaykit.ClaudeMessages:  `"type":"message"`,
		protocolrelaykit.Gemini:          `"candidates"`,
	}
)

// TestProtocolProviderConversionMatrix drives every client dialect against
// every upstream dialect: the request must arrive in the upstream's dialect and
// the response must come back in the client's.
func TestProtocolProviderConversionMatrix(t *testing.T) {
	targets := []protocolrelaykit.Format{
		protocolrelaykit.OpenAIChat,
		protocolrelaykit.OpenAIResponses,
		protocolrelaykit.ClaudeMessages,
		protocolrelaykit.Gemini,
	}
	// One provider type per upstream API surface.
	surfaces := map[protocolrelaykit.Format]string{
		protocolrelaykit.OpenAIChat:      config.ProviderTypeOpenAI,
		protocolrelaykit.OpenAIResponses: config.ProviderTypeOpenAIResponses,
		protocolrelaykit.ClaudeMessages:  config.ProviderTypeAnthropic,
		protocolrelaykit.Gemini:          config.ProviderTypeGemini,
	}
	for client, requestBody := range clientRequests {
		for _, upstream := range targets {
			name := string(client) + "_client__" + string(upstream) + "_upstream"
			t.Run(name, func(t *testing.T) {
				var gotBody, gotPath string
				var targetFormat protocolrelaykit.Format
				next := func(w http.ResponseWriter, r *http.Request) {
					restore := rewriteProtocolBackendPath(r)
					defer restore()
					targetFormat = protocolTargetFormat(r)
					body, _ := io.ReadAll(r.Body)
					gotBody = string(body)
					gotPath = r.URL.Path
					w.Header().Set("Content-Type", "application/json")
					w.Write([]byte(upstreamResponses[upstream]))
				}
				resolver := func(string) (ProviderDialect, bool) {
					return ProviderDialect{Type: surfaces[upstream], Label: "prov"}, true
				}
				var handler http.Handler = CreateProtocolAdapterMiddleware(
					config.Config{Models: map[string]config.ModelConfig{}}, nil, resolver)(http.HandlerFunc(next))
				if client == protocolrelaykit.Gemini {
					// The mux normally extracts the model from the Gemini path
					// before the chain runs.
					handler = prepareGeminiRoute(handler)
				}

				req := httptest.NewRequest("POST", clientPaths[client], strings.NewReader(requestBody))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, req)

				if w.Code != 200 {
					t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
				}
				sameDialect := client == upstream
				if sameDialect {
					// Nothing to convert: the request must arrive untouched.
					if targetFormat != "" {
						t.Fatalf("same-dialect request was converted to %q", targetFormat)
					}
					if gotBody != requestBody {
						t.Fatalf("same-dialect request was rewritten: %s", gotBody)
					}
					if body := w.Body.String(); body != upstreamResponses[upstream] {
						t.Fatalf("same-dialect response was rewritten: %s", body)
					}
					return
				}
				if targetFormat != upstream {
					t.Fatalf("conversion target = %q, want %q", targetFormat, upstream)
				}
				if !strings.Contains(gotBody, upstreamMarkers[upstream]) {
					t.Fatalf("converted request lacks the %s shape (%s): %s", upstream, upstreamMarkers[upstream], gotBody)
				}
				if !strings.Contains(gotBody, `"hi"`) {
					t.Fatalf("prompt text lost during conversion: %s", gotBody)
				}
				body := w.Body.String()
				if !strings.Contains(body, clientResponseMarkers[client]) {
					t.Fatalf("response is not in the client dialect %s: %s", client, body)
				}
				if !strings.Contains(body, "pong") {
					t.Fatalf("assistant text lost during conversion: %s", body)
				}
				_ = gotPath
			})
		}
	}
}

func TestProtocolProviderStreamingMatrix(t *testing.T) {
	// Streaming deltas must survive every client/server dialect pair that the
	// non-streaming matrix covers.
	upstreamStreams := map[protocolrelaykit.Format][]string{
		protocolrelaykit.OpenAIChat: {
			`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"po"}}]}`,
			`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"ng"},"finish_reason":"stop"}]}`,
		},
		protocolrelaykit.OpenAIResponses: {
			`data: {"type":"response.output_text.delta","delta":"po"}`,
			`data: {"type":"response.output_text.delta","delta":"ng"}`,
		},
		protocolrelaykit.ClaudeMessages: {
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"po"}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ng"}}`,
		},
		protocolrelaykit.Gemini: {
			`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"po"}]},"index":0}]}`,
			`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"ng"}]},"finishReason":"STOP","index":0}]}`,
		},
	}
	surfaces := map[protocolrelaykit.Format]string{
		protocolrelaykit.OpenAIChat:      config.ProviderTypeOpenAI,
		protocolrelaykit.OpenAIResponses: config.ProviderTypeOpenAIResponses,
		protocolrelaykit.ClaudeMessages:  config.ProviderTypeAnthropic,
		protocolrelaykit.Gemini:          config.ProviderTypeGemini,
	}
	streamingRequests := map[protocolrelaykit.Format]string{
		protocolrelaykit.OpenAIChat:      `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":16}`,
		protocolrelaykit.OpenAIResponses: `{"model":"m","input":"hi","stream":true,"max_output_tokens":16}`,
		protocolrelaykit.ClaudeMessages:  `{"model":"m","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		protocolrelaykit.Gemini:          `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":16}}`,
	}
	for client, requestBody := range streamingRequests {
		for upstream, deltas := range upstreamStreams {
			if client == upstream {
				continue
			}
			t.Run(string(client)+"_client__"+string(upstream)+"_upstream", func(t *testing.T) {
				next := func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(200)
					flusher, _ := w.(http.Flusher)
					for _, delta := range deltas {
						w.Write([]byte(delta + "\n\n"))
						if flusher != nil {
							flusher.Flush()
						}
					}
				}
				resolver := func(string) (ProviderDialect, bool) {
					return ProviderDialect{Type: surfaces[upstream], Label: "prov"}, true
				}
				var handler http.Handler = CreateProtocolAdapterMiddleware(
					config.Config{Models: map[string]config.ModelConfig{}}, nil, resolver)(http.HandlerFunc(next))
				if client == protocolrelaykit.Gemini {
					handler = prepareGeminiRoute(handler)
				}
				req := httptest.NewRequest("POST", clientStreamPaths[client], strings.NewReader(requestBody))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, req)

				if w.Code != 200 {
					t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
				}
				body := w.Body.String()
				if !strings.Contains(body, "po") || !strings.Contains(body, "ng") {
					t.Fatalf("stream deltas lost converting %s -> %s: %s", client, upstream, body)
				}
				if !strings.Contains(body, clientResponseMarkers[client]) && client != protocolrelaykit.OpenAIResponses {
					t.Fatalf("stream is not in the client dialect %s: %s", client, body)
				}
			})
		}
	}
}

// TestProtocolConvertedGeminiRequestCarriesUpstreamModel guards the path-value
// handoff: the model rewrite downstream reads the model from the Gemini path,
// so a converted request that misses it would forward the local name upstream.
func TestProtocolConvertedGeminiRequestCarriesUpstreamModel(t *testing.T) {
	var upstreamPath string
	next := func(w http.ResponseWriter, r *http.Request) {
		restore := rewriteProtocolBackendPath(r)
		defer restore()
		// The dispatch's model rewrite: local name -> upstream name.
		rewritten, err := swaputil.ReplaceRequestModel(r, "gemini-4-argon", "gemini-4-argon-contributor")
		if err != nil {
			t.Errorf("ReplaceRequestModel: %v", err)
		}
		upstreamPath = rewritten.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"pong"}]},"finishReason":"STOP","index":0}]}`))
	}
	resolver := func(string) (ProviderDialect, bool) {
		return ProviderDialect{Type: config.ProviderTypeGemini, Label: "prov/Google"}, true
	}
	handler := CreateProtocolAdapterMiddleware(config.Config{Models: map[string]config.ModelConfig{}}, nil,
		resolver)(http.HandlerFunc(next))

	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gemini-4-argon","input":"hi","max_output_tokens":16}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if upstreamPath != "/v1beta/models/gemini-4-argon-contributor:generateContent" {
		t.Fatalf("upstream path = %q, want the upstream model name", upstreamPath)
	}
}

func TestProtocolProviderOpenAIAdapterConvertsForeignDialects(t *testing.T) {
	// A Claude client against an openai-type adapter has no chance upstream:
	// the OpenAI family is passed through, everything else becomes chat.
	for _, tc := range []struct {
		name    string
		path    string
		body    string
		mustHas string
	}{
		{"claude client", "/v1/messages", `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, `"messages"`},
		{"gemini client", "/v1beta/models/m:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, `"messages"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotBody string
			next := func(w http.ResponseWriter, r *http.Request) {
				restore := rewriteProtocolBackendPath(r)
				defer restore()
				gotPath = r.URL.Path
				body, _ := io.ReadAll(r.Body)
				gotBody = string(body)
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(upstreamResponses[protocolrelaykit.OpenAIChat]))
			}
			resolver := func(string) (ProviderDialect, bool) {
				return ProviderDialect{Type: config.ProviderTypeOpenAI, Label: "prov/Chat"}, true
			}
			var handler http.Handler = CreateProtocolAdapterMiddleware(
				config.Config{Models: map[string]config.ModelConfig{}}, nil, resolver)(http.HandlerFunc(next))
			if strings.HasPrefix(tc.path, "/v1beta") {
				handler = prepareGeminiRoute(handler)
			}
			req := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != 200 {
				t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
			}
			if gotPath != "/v1/chat/completions" {
				t.Fatalf("upstream path = %q, want chat completions", gotPath)
			}
			if !strings.Contains(gotBody, tc.mustHas) || !strings.Contains(gotBody, `"hi"`) {
				t.Fatalf("converted body = %s", gotBody)
			}
			// The client still receives its own dialect.
			if !strings.Contains(w.Body.String(), clientResponseMarkers[map[bool]protocolrelaykit.Format{true: protocolrelaykit.ClaudeMessages, false: protocolrelaykit.Gemini}[strings.HasPrefix(tc.path, "/v1/messages")]]) {
				t.Fatalf("response not converted back: %s", w.Body.String())
			}
		})
	}
}

// A body that is not valid JSON must say what and where: the offset and an
// escaped preview are what let a client author find the content that broke the
// encoding.
func TestJSONErrorDetail_PointsAtTheInvalidEscape(t *testing.T) {
	body := []byte(`{"model":"m","input":"path C:\Program Files\now"}`)
	_, err := withoutPreviousResponseID(body)
	if err == nil {
		t.Fatal("an invalid escape must be rejected")
	}
	message := err.Error()
	if !strings.Contains(message, "not valid JSON") {
		t.Fatalf("message = %q", message)
	}
	if !strings.Contains(message, "invalid character 'P' in string escape code") {
		t.Fatalf("the underlying parse error must survive: %q", message)
	}
	if !strings.Contains(message, "byte ") || !strings.Contains(message, "near ") {
		t.Fatalf("message must carry an offset and a preview: %q", message)
	}
	// The preview keeps the offending content visible (with the backslash
	// escaped by %q) without dumping the whole body.
	if !strings.Contains(message, "Program Files") {
		t.Fatalf("preview = %q", message)
	}

	// Control bytes stay escaped instead of breaking the message.
	withEscape := []byte("{\"input\":\"a\x1bb\"}")
	if _, err := withoutPreviousResponseID(withEscape); err == nil {
		t.Fatal("a raw ESC in an escape sequence must be rejected")
	} else if !strings.Contains(err.Error(), `\x1b`) {
		t.Fatalf("control bytes must be escaped in the preview: %q", err.Error())
	}

	// A legal body with escaped backslashes still converts.
	valid := []byte(`{"model":"m","input":"path C:\\Program Files","previous_response_id":"resp-1"}`)
	converted, err := withoutPreviousResponseID(valid)
	if err != nil {
		t.Fatalf("valid body: %v", err)
	}
	if string(converted) == "" || strings.Contains(string(converted), "previous_response_id") {
		t.Fatalf("previous_response_id must be stripped: %s", converted)
	}
}
