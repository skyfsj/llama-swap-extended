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
)

func TestProtocolAdapterResponsesToChat(t *testing.T) {
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"m": {Backend: config.BackendConfig{Protocol: config.AdapterProtocolResponsesToChat}},
	}}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"hello"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	called := false
	handler := CreateProtocolAdapterMiddleware(cfg, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	handler := CreateProtocolAdapterMiddleware(cfg, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	handler := prepareGeminiRoute(CreateProtocolAdapterMiddleware(cfg, nil)(next))
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
