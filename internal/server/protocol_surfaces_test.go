package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/gateway"
	"github.com/mostlygeek/llama-swap/internal/store"
)

// surfaceRecorder captures what one upstream surface received.
type surfaceRecorder struct {
	mu     sync.Mutex
	calls  int
	paths  []string
	bodies []string
	auths  []string
	keys   []string
}

func (s *surfaceRecorder) record(r *http.Request) string {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.paths = append(s.paths, r.URL.Path)
	s.bodies = append(s.bodies, string(body))
	s.auths = append(s.auths, r.Header.Get("Authorization"))
	s.keys = append(s.keys, r.Header.Get("x-api-key"))
	return string(body)
}

func (s *surfaceRecorder) snapshot() (int, []string, []string, []string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls,
		append([]string(nil), s.paths...),
		append([]string(nil), s.bodies...),
		append([]string(nil), s.auths...),
		append([]string(nil), s.keys...)
}

func (s *surfaceRecorder) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

const (
	claudeSurfaceResponse = `{"id":"msg_1","type":"message","role":"assistant","model":"haiku-messages","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	responsesSurfaceBody  = `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"haiku-responses","output":[{"id":"item_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"pong","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
)

// multiSurfaceConfig exposes one model on two adapters with different API
// surfaces, the configuration the gateway treats as a pool of surfaces. The
// adapters are listed with Messages first so a test can prove the client's own
// dialect wins over configuration order.
func multiSurfaceConfig(responsesURL, messagesURL string) config.Config {
	return config.Config{
		Models: map[string]config.ModelConfig{
			"local-model": {Cmd: "echo hi", Proxy: "http://127.0.0.1:1"},
		},
		Providers: config.ProviderDictionaryConfig{
			"ocg": {
				ApiKeys: []config.ProviderKey{{Value: "k-test"}},
				Adapters: []config.ProviderAdapter{
					{
						Name:     "Messages",
						Type:     config.ProviderTypeAnthropic,
						Endpoint: messagesURL,
						Models:   []config.ProviderModel{{Local: "claude-haiku-5-5", Remote: "haiku-messages"}},
					},
					{
						Name:     "Responses",
						Type:     config.ProviderTypeOpenAIResponses,
						Endpoint: responsesURL,
						Models:   []config.ProviderModel{{Local: "claude-haiku-5-5", Remote: "haiku-responses"}},
					},
				},
			},
		},
	}
}

func multiSurfaceServer(t *testing.T, cfg config.Config) *Server {
	t.Helper()
	s := newTestServer(newStubRouter([]string{"local-model"}, `{"local":true}`), newStubRouter(nil, ""))
	s.cfg = cfg
	gw, err := gateway.New(cfg, s.proxylog)
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	s.gateway = gw
	s.routes()
	t.Cleanup(gw.Shutdown)
	return s
}

func postJSON(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	return w
}

// TestServer_MultiSurfacePrefersClientDialect pins the surface order: a client
// reaches the adapter that speaks its own dialect, whatever the adapter order
// in the configuration is.
func TestServer_MultiSurfacePrefersClientDialect(t *testing.T) {
	messages := &surfaceRecorder{}
	responses := &surfaceRecorder{}
	messagesUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		messages.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, claudeSurfaceResponse)
	}))
	defer messagesUpstream.Close()
	responsesUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		responses.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, responsesSurfaceBody)
	}))
	defer responsesUpstream.Close()

	s := multiSurfaceServer(t, multiSurfaceConfig(responsesUpstream.URL, messagesUpstream.URL))

	t.Run("anthropic client", func(t *testing.T) {
		w := postJSON(t, s, "/v1/messages",
			`{"model":"claude-haiku-5-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
		}
		if responses.callCount() != 0 {
			t.Fatalf("an Anthropic client must not reach the Responses surface")
		}
		calls, paths, bodies, auths, keys := messages.snapshot()
		if calls != 1 || paths[0] != "/v1/messages" {
			t.Fatalf("messages surface calls = %d paths = %v", calls, paths)
		}
		if !strings.Contains(bodies[0], `"model":"haiku-messages"`) || !strings.Contains(bodies[0], `"max_tokens"`) {
			t.Fatalf("request did not arrive in the Anthropic dialect: %s", bodies[0])
		}
		if auths[0] != "" || keys[0] != "k-test" {
			t.Fatalf("anthropic auth: Authorization=%q x-api-key=%q", auths[0], keys[0])
		}
		if !strings.Contains(w.Body.String(), `"type":"message"`) {
			t.Fatalf("response is not Anthropic: %s", w.Body.String())
		}
	})

	t.Run("responses client", func(t *testing.T) {
		w := postJSON(t, s, "/v1/responses", `{"model":"claude-haiku-5-5","input":"hi","max_output_tokens":16}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
		}
		if messages.callCount() != 1 {
			t.Fatalf("a Responses client must not reach the Messages surface (calls = %d)", messages.callCount())
		}
		calls, paths, bodies, auths, keys := responses.snapshot()
		if calls != 1 || paths[0] != "/v1/responses" {
			t.Fatalf("responses surface calls = %d paths = %v", calls, paths)
		}
		if !strings.Contains(bodies[0], `"model":"haiku-responses"`) || !strings.Contains(bodies[0], `"input"`) {
			t.Fatalf("request did not arrive in the Responses dialect: %s", bodies[0])
		}
		if auths[0] != "Bearer k-test" || keys[0] != "" {
			t.Fatalf("bearer auth: Authorization=%q x-api-key=%q", auths[0], keys[0])
		}
		if !strings.Contains(w.Body.String(), `"object":"response"`) || !strings.Contains(w.Body.String(), "pong") {
			t.Fatalf("response is not Responses: %s", w.Body.String())
		}
	})

	t.Run("chat client converts to the first surface", func(t *testing.T) {
		w := postJSON(t, s, "/v1/chat/completions",
			`{"model":"claude-haiku-5-5","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
		}
		// No surface matches chat completions, so the configuration order
		// decides: Messages is listed first.
		if messages.callCount() != 2 {
			t.Fatalf("chat client should reach the Messages surface (calls = %d)", messages.callCount())
		}
		_, paths, bodies, _, _ := messages.snapshot()
		if paths[1] != "/v1/messages" || !strings.Contains(bodies[1], `"max_tokens"`) {
			t.Fatalf("converted request = %s %s", paths[1], bodies[1])
		}
		if !strings.Contains(w.Body.String(), `"choices"`) || !strings.Contains(w.Body.String(), "pong") {
			t.Fatalf("response was not converted to chat completions: %s", w.Body.String())
		}
	})
}

// TestServer_MultiSurfaceFallsBackOnRejection is the auto-conflict case: the
// upstream that owns the client's preferred surface refuses the request, so the
// other adapter of the same model serves it instead of the client seeing an
// error.
func TestServer_MultiSurfaceFallsBackOnRejection(t *testing.T) {
	var mu sync.Mutex
	var tried []string
	responses := &surfaceRecorder{}
	messages := &surfaceRecorder{}
	messagesUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		messages.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, claudeSurfaceResponse)
	}))
	defer messagesUpstream.Close()
	responsesUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := responses.record(r)
		mu.Lock()
		tried = append(tried, "responses:"+body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"unsupported parameter max_tokens","type":"invalid_request_error"}}`)
	}))
	defer responsesUpstream.Close()

	cfg := multiSurfaceConfig(responsesUpstream.URL, messagesUpstream.URL)
	// Responses is listed first here: a chat client has no matching surface, so
	// that adapter is tried first and has to hand the request over.
	cfg.Providers["ocg"] = config.ProviderConfig{
		ApiKeys: []config.ProviderKey{{Value: "k-test"}},
		Adapters: []config.ProviderAdapter{
			{
				Name:     "Responses",
				Type:     config.ProviderTypeOpenAIResponses,
				Endpoint: responsesUpstream.URL,
				Models:   []config.ProviderModel{{Local: "claude-haiku-5-5", Remote: "haiku-responses"}},
			},
			{
				Name:     "Messages",
				Type:     config.ProviderTypeAnthropic,
				Endpoint: messagesUpstream.URL,
				Models:   []config.ProviderModel{{Local: "claude-haiku-5-5", Remote: "haiku-messages"}},
			},
		},
	}
	s := multiSurfaceServer(t, cfg)

	w := postJSON(t, s, "/v1/chat/completions",
		`{"model":"claude-haiku-5-5","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if responses.callCount() != 1 {
		t.Fatalf("the first surface should have been tried once (calls = %d)", responses.callCount())
	}
	if messages.callCount() != 1 {
		t.Fatalf("the request should have continued on the second surface (calls = %d)", messages.callCount())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"choices"`) || !strings.Contains(body, "pong") {
		t.Fatalf("the fallback response must be converted back to chat: %s", body)
	}
	if strings.Contains(body, "unsupported parameter") {
		t.Fatalf("the rejected surface's error leaked to the client: %s", body)
	}
	var decoded map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("client response is not JSON: %v", err)
	}
}

// TestServer_MultiSurfaceStreamFallsBack covers the streaming form of the same
// handover: the client must receive a single converted SSE stream from the
// surface that served it.
func TestServer_MultiSurfaceStreamFallsBack(t *testing.T) {
	responses := &surfaceRecorder{}
	messages := &surfaceRecorder{}
	responsesUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		responses.record(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"streaming is not enabled for this account","type":"invalid_request_error"}}`)
	}))
	defer responsesUpstream.Close()
	messagesUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		messages.record(r)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, frame := range []string{
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"po"}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ng"}}`,
		} {
			_, _ = io.WriteString(w, frame+"\n\n")
		}
	}))
	defer messagesUpstream.Close()

	cfg := multiSurfaceConfig(responsesUpstream.URL, messagesUpstream.URL)
	cfg.Providers["ocg"] = config.ProviderConfig{
		ApiKeys: []config.ProviderKey{{Value: "k-test"}},
		Adapters: []config.ProviderAdapter{
			{
				Name:     "Responses",
				Type:     config.ProviderTypeOpenAIResponses,
				Endpoint: responsesUpstream.URL,
				Models:   []config.ProviderModel{{Local: "claude-haiku-5-5", Remote: "haiku-responses"}},
			},
			{
				Name:     "Messages",
				Type:     config.ProviderTypeAnthropic,
				Endpoint: messagesUpstream.URL,
				Models:   []config.ProviderModel{{Local: "claude-haiku-5-5", Remote: "haiku-messages"}},
			},
		},
	}
	s := multiSurfaceServer(t, cfg)

	w := postJSON(t, s, "/v1/chat/completions",
		`{"model":"claude-haiku-5-5","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":16}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "chat.completion.chunk") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("client did not receive a chat SSE stream: %s", body)
	}
	if !strings.Contains(body, `"content":"po"`) || !strings.Contains(body, `"content":"ng"`) {
		t.Fatalf("streamed text was lost in the conversion: %s", body)
	}
	if strings.Contains(body, "content_block_delta") || strings.Contains(body, "streaming is not enabled") {
		t.Fatalf("upstream dialect or error leaked to the client: %s", body)
	}
	if messages.callCount() != 1 {
		t.Fatalf("fallback surface calls = %d", messages.callCount())
	}
}

// TestServer_MultiSurfaceFallbackKeepsUpstreamBody is the production shape of
// the fallback: models are declared without a remote name, so no per-adapter
// model rewrite runs and the converted body is the only thing that may reach
// the upstream. A body/content-length mismatch here is invisible to a client
// until the transport fails.
func TestServer_MultiSurfaceFallbackKeepsUpstreamBody(t *testing.T) {
	responses := &surfaceRecorder{}
	messages := &surfaceRecorder{}
	responsesUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		responses.record(r)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"streaming is not enabled for this account","type":"invalid_request_error"}}`)
	}))
	defer responsesUpstream.Close()
	messagesUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		messages.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, claudeSurfaceResponse)
	}))
	defer messagesUpstream.Close()

	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			"local-model": {Cmd: "echo hi", Proxy: "http://127.0.0.1:1"},
		},
		Providers: config.ProviderDictionaryConfig{
			"ocg": {
				ApiKeys: []config.ProviderKey{{Value: "k-test"}},
				Adapters: []config.ProviderAdapter{
					{Name: "Responses", Type: config.ProviderTypeOpenAIResponses, Endpoint: responsesUpstream.URL, Models: []config.ProviderModel{{Local: "claude-haiku-5-5", Remote: "claude-haiku-5-5"}}},
					{Name: "Messages", Type: config.ProviderTypeAnthropic, Endpoint: messagesUpstream.URL, Models: []config.ProviderModel{{Local: "claude-haiku-5-5", Remote: "claude-haiku-5-5"}}},
				},
			},
		},
	}
	s := multiSurfaceServer(t, cfg)

	w := postJSON(t, s, "/v1/chat/completions",
		`{"model":"claude-haiku-5-5","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	calls, paths, bodies, _, keys := messages.snapshot()
	if calls != 1 || paths[0] != "/v1/messages" {
		t.Fatalf("messages surface calls = %d paths = %v", calls, paths)
	}
	if !strings.Contains(bodies[0], `"max_tokens"`) || !strings.Contains(bodies[0], `"hi"`) {
		t.Fatalf("upstream did not receive the converted body: %s", bodies[0])
	}
	if keys[0] != "k-test" {
		t.Fatalf("x-api-key = %q", keys[0])
	}
	if !strings.Contains(w.Body.String(), `"choices"`) {
		t.Fatalf("client response = %s", w.Body.String())
	}
}

// TestServer_ProviderModelRecordedUnderClientName pins the recorded model name:
// the same provider model can be requested by its client-facing name, an alias
// or the provider-qualified form, and the usage and request tables must not
// split it across spellings.
func TestServer_ProviderModelRecordedUnderClientName(t *testing.T) {
	upstream := &surfaceRecorder{}
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"glm-5.1","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstreamServer.Close()

	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			"local-model": {Cmd: "echo hi", Proxy: "http://127.0.0.1:1"},
		},
		Providers: config.ProviderDictionaryConfig{
			"opencode-go": {
				Type:        config.ProviderTypeOpenAI,
				Endpoint:    upstreamServer.URL,
				ModelPrefix: "go-",
				ApiKeys:     []config.ProviderKey{{Value: "k-test"}},
				Models: []config.ProviderModel{
					{Local: "glm-5.1", Remote: "glm-5.1", Aliases: []string{"glm"}},
				},
			},
		},
	}
	s := multiSurfaceServer(t, cfg)

	for _, requested := range []string{"go-glm-5.1", "go-glm", "opencode-go/glm-5.1"} {
		w := postJSON(t, s, "/v1/chat/completions",
			`{"model":"`+requested+`","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d body = %s", requested, w.Code, w.Body.String())
		}
	}

	entries, err := s.store.ListActivity(context.Background(), store.ActivityQuery{Limit: 50})
	if err != nil {
		t.Fatalf("list activity: %v", err)
	}
	if len(entries.Data) != 3 {
		t.Fatalf("activity rows = %d, want 3", len(entries.Data))
	}
	for _, entry := range entries.Data {
		if entry.Model != "go-glm-5.1" {
			t.Fatalf("recorded model = %q, want the client-facing name go-glm-5.1", entry.Model)
		}
	}
}

// A provider model declares its capabilities like a local one, so the
// playground can file it under chat, image or translation tooling.
func TestServer_ProviderModelCapabilitiesAreListed(t *testing.T) {
	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			"local-model": {Cmd: "echo hi", Proxy: "http://127.0.0.1:1"},
		},
		Providers: config.ProviderDictionaryConfig{
			"img": {
				Type:     config.ProviderTypeOpenAI,
				Endpoint: "https://api.example.com",
				ApiKeys:  []config.ProviderKey{{Value: "k-test"}},
				Models: []config.ProviderModel{
					{Local: "gpt-image-2", Remote: "gpt-image-2", Capabilities: config.ModelCapConfig{In: []string{"text"}, Out: []string{"image"}}},
					{Local: "glm-5.1", Remote: "glm-5.1"},
				},
			},
		},
	}
	gw, err := gateway.New(cfg, nil)
	if err != nil {
		t.Fatalf("gateway: %v", err)
	}
	defer gw.Shutdown()

	route := gw.RouteFor("gpt-image-2", false)
	if route == nil || route.Capabilities.Empty() {
		t.Fatalf("route = %+v, want the declared capabilities", route)
	}
	// The listing turns them into the same capability map local models use.
	if got := route.Capabilities.Out; len(got) != 1 || got[0] != "image" {
		t.Fatalf("capabilities = %+v", route.Capabilities)
	}
	if plain := gw.RouteFor("glm-5.1", false); plain == nil || !plain.Capabilities.Empty() {
		t.Fatalf("a model without a declaration must stay a plain chat model: %+v", plain)
	}
}

// A provider request larger than the session-fingerprint peek window must reach
// the upstream whole. Truncating it to the window used to break every long
// conversation: the cross-dialect conversion then parsed a body cut mid-string
// and answered 400 with a JSON error pointing at the cut.
func TestServer_ProviderRequestBodyAbovePeekWindowIsForwardedWhole(t *testing.T) {
	upstream := &surfaceRecorder{}
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"glm-5.1","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstreamServer.Close()

	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			"local-model": {Cmd: "echo hi", Proxy: "http://127.0.0.1:1"},
		},
		Providers: config.ProviderDictionaryConfig{
			"p": {
				Type:     config.ProviderTypeOpenAI,
				Endpoint: upstreamServer.URL,
				ApiKeys:  []config.ProviderKey{{Value: "k-test"}},
				Models:   []config.ProviderModel{{Local: "glm-5.1", Remote: "glm-5.1"}},
			},
		},
	}
	s := multiSurfaceServer(t, cfg)

	// 1.5 MiB of prompt content, with a unique marker at the very end: a body
	// cut at the peek window loses it.
	filler := strings.Repeat("x", 1536*1024)
	const marker = "END-OF-THE-LONG-CONVERSATION"
	body, err := json.Marshal(map[string]any{
		"model": "glm-5.1",
		"input": filler + marker,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(body) <= 1<<20 {
		t.Fatalf("fixture body = %d bytes, want > 1 MiB", len(body))
	}

	w := postJSON(t, s, "/v1/responses", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	calls, _, bodies, _, _ := upstream.snapshot()
	if calls != 1 {
		t.Fatalf("upstream calls = %d", calls)
	}
	if len(bodies[0]) <= 1<<20 {
		t.Fatalf("upstream body = %d bytes, want the whole request (> 1 MiB)", len(bodies[0]))
	}
	if !strings.Contains(bodies[0], marker) {
		t.Fatalf("the tail of the conversation was lost: body is %d bytes", len(bodies[0]))
	}
	// The conversion rewrote it into the chat dialect, keeping the content.
	if !strings.Contains(bodies[0], `"messages"`) {
		t.Fatalf("converted body = %s", bodies[0][:120])
	}
}

// The same body on a model that several adapters expose: the multi-surface path
// converts per surface from the request body, so a truncated body fails the
// conversion itself and the client sees a JSON parse error (this is the shape
// of the report: "byte 1048576 of 1048576").
func TestServer_MultiSurfaceLongBodyStillConverts(t *testing.T) {
	messages := &surfaceRecorder{}
	messagesUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		messages.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, claudeSurfaceResponse)
	}))
	defer messagesUpstream.Close()
	responses := &surfaceRecorder{}
	responsesUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		responses.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, responsesSurfaceBody)
	}))
	defer responsesUpstream.Close()

	s := multiSurfaceServer(t, multiSurfaceConfig(responsesUpstream.URL, messagesUpstream.URL))

	filler := strings.Repeat("y", 1400*1024)
	const marker = "TAIL-OF-THE-LONG-TURN"
	body, err := json.Marshal(map[string]any{
		"model": "claude-haiku-5-5",
		"input": filler + marker,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	w := postJSON(t, s, "/v1/responses", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	// The Responses surface owns this dialect, so it serves the request whole.
	calls, _, bodies, _, _ := responses.snapshot()
	if calls != 1 {
		t.Fatalf("responses surface calls = %d", calls)
	}
	if len(bodies[0]) <= 1<<20 || !strings.Contains(bodies[0], marker) {
		t.Fatalf("upstream body = %d bytes, want the whole request", len(bodies[0]))
	}

	// A chat client matches no surface here, so the request is converted for
	// the first one: a body cut at the peek window fails that conversion and
	// the client sees the parse error instead of an answer.
	chatBody, err := json.Marshal(map[string]any{
		"model":      "claude-haiku-5-5",
		"messages":   []map[string]any{{"role": "user", "content": strings.Repeat("z", 1400*1024) + "CHAT-TAIL-MARKER"}},
		"max_tokens": 16,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	w2 := postJSON(t, s, "/v1/chat/completions", string(chatBody))
	if w2.Code != http.StatusOK {
		t.Fatalf("chat client status = %d body = %s", w2.Code, w2.Body.String())
	}
	calls2, _, bodies2, _, _ := messages.snapshot()
	if calls2 != 1 {
		t.Fatalf("messages surface calls = %d", calls2)
	}
	if len(bodies2[0]) <= 1<<20 || !strings.Contains(bodies2[0], "CHAT-TAIL-MARKER") {
		t.Fatalf("converted upstream body = %d bytes, want the whole request", len(bodies2[0]))
	}
	if !strings.Contains(bodies2[0], `"max_tokens"`) {
		t.Fatalf("converted body is not Anthropic: %s", bodies2[0][:120])
	}
}
