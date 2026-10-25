package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/gateway"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	protocolrelaykit "github.com/mostlygeek/llama-swap/internal/protocol/relaykit"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

func gatewayTestServer(t *testing.T, cfg config.Config, upstream *httptest.Server) *Server {
	t.Helper()
	if upstream != nil {
		for id, prov := range cfg.Providers {
			prov.Endpoint = upstream.URL
			cfg.Providers[id] = prov
		}
	}
	local := newStubRouter([]string{"local-model"}, `{"local":true}`)
	peer := newStubRouter(nil, "")
	s := &Server{
		cfg:         cfg,
		muxlog:      logmon.NewWriter(io_discard{}),
		proxylog:    logmon.NewWriter(io_discard{}),
		upstreamlog: logmon.NewWriter(io_discard{}),
		inflight:    newInflightTracker(),
		quota:       newQuotaStore(),
		shutdownCtx: context.Background(),
		local:       local,
		peer:        peer,
	}
	if len(cfg.Providers) > 0 {
		gw, err := gateway.New(cfg, s.proxylog)
		if err != nil {
			t.Fatalf("gateway.New: %v", err)
		}
		s.gateway = gw
	}
	s.routes()
	t.Cleanup(func() {
		if gw := s.currentGateway(); gw != nil {
			gw.Shutdown()
		}
	})
	return s
}

type io_discard struct{}

func (io_discard) Write(p []byte) (int, error) { return len(p), nil }

func gatewayTestConfig() config.Config {
	return config.Config{
		Models: map[string]config.ModelConfig{
			"local-model": {Cmd: "echo hi", Proxy: "http://127.0.0.1:1"},
		},
		Providers: config.ProviderDictionaryConfig{
			"prov": {
				Type:     config.ProviderTypeOpenAI,
				Endpoint: "http://placeholder",
				ApiKeys:  []config.ProviderKey{{Value: "k1"}, {Value: "k2"}},
				Models:   []config.ProviderModel{{Local: "remote-chat", Remote: "upstream-chat"}},
			},
		},
	}
}

func TestServer_GatewayDispatchFQN(t *testing.T) {
	var gotAuth atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{"echo":"` + body["model"].(string) + `"}`))
	}))
	defer upstream.Close()

	s := gatewayTestServer(t, gatewayTestConfig(), upstream)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"prov/remote-chat","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.localPeerHandler(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	auth, _ := gotAuth.Load().(string)
	if auth != "Bearer k1" && auth != "Bearer k2" {
		t.Fatalf("upstream Authorization = %q", auth)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["echo"] != "upstream-chat" {
		t.Fatalf("model not rewritten to upstream name: %v", resp)
	}
}

func TestServer_GatewayDispatchBareName(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	s := gatewayTestServer(t, gatewayTestConfig(), upstream)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"remote-chat","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.localPeerHandler(w, req)
	if w.Code != 200 {
		t.Fatalf("bare name should route through gateway, status = %d body = %s", w.Code, w.Body.String())
	}

	// The local model keeps its bare name.
	req2 := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"local-model","messages":[]}`))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	s.localPeerHandler(w2, req2)
	if w2.Code != 200 {
		t.Fatalf("local model dispatch broken: %d", w2.Code)
	}
	if respBody := w2.Body.String(); !strings.Contains(respBody, "local") {
		t.Fatalf("local-model should dispatch to the local router, got %s", respBody)
	}
}

func TestServer_ModelsListIncludesProviderModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	s := gatewayTestServer(t, gatewayTestConfig(), upstream)

	req := httptest.NewRequest("GET", "/v1/models", nil)
	w := httptest.NewRecorder()
	s.handleListModels(w, req)
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	var resp struct {
		Data []struct {
			ID   string `json:"id"`
			Meta map[string]any
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	foundName := false
	for _, m := range resp.Data {
		// The client-facing name is the bare model ID when the provider has no
		// modelPrefix; the legacy provider/model form stays routable but is no
		// longer listed.
		if m.ID == "remote-chat" {
			foundName = true
		}
		if m.ID == "prov/remote-chat" {
			t.Fatalf("listing must show the client-facing name, not the legacy FQN: %s", w.Body.String())
		}
	}
	if !foundName {
		t.Fatalf("provider model missing from /v1/models: %s", w.Body.String())
	}
}

func TestServer_ModelsListUsesModelPrefix(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	cfg := gatewayTestConfig()
	prov := cfg.Providers["prov"]
	prov.ModelPrefix = "go-"
	cfg.Providers["prov"] = prov

	s := gatewayTestServer(t, cfg, upstream)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	w := httptest.NewRecorder()
	s.handleListModels(w, req)
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	found := false
	for _, m := range resp.Data {
		if m.ID == "go-remote-chat" {
			found = true
		}
		if m.ID == "remote-chat" {
			t.Fatalf("a prefixed provider must not also list the bare model ID: %s", w.Body.String())
		}
	}
	if !found {
		t.Fatalf("prefixed model missing from /v1/models: %s", w.Body.String())
	}
}

func TestServer_GatewayReconfigure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := gatewayTestConfig()
	s := gatewayTestServer(t, cfg, upstream)

	if s.currentGateway() == nil {
		t.Fatal("gateway should be active")
	}
	if !s.currentGateway().Handles("prov/remote-chat") {
		t.Fatal("gateway should handle its model")
	}

	// Reconfigure with a different model set.
	next := gatewayTestConfig()
	prov := next.Providers["prov"]
	prov.Models = []config.ProviderModel{{Local: "new-model", Remote: "new-remote"}}
	next.Providers["prov"] = prov

	s.setGateway(func() *gateway.Gateway {
		gw, err := gateway.New(next, s.proxylog)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return gw
	}())

	if s.currentGateway().Handles("prov/remote-chat") {
		t.Fatal("old model should be gone")
	}
	if !s.currentGateway().Handles("prov/new-model") {
		t.Fatal("new model should be handled")
	}

	// Removing all providers drops the gateway.
	s.setGateway(nil)
	if s.currentGateway() != nil {
		t.Fatal("gateway should be nil after removal")
	}
}

func TestServer_ProvidersProbeEndpoint(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"data":[{"id":"m2"},{"id":"m1"}]}`))
	}))
	defer upstream.Close()

	s := gatewayTestServer(t, gatewayTestConfig(), upstream)

	req := httptest.NewRequest("POST", "/api/providers/probe",
		strings.NewReader(fmt.Sprintf(`{"type":"openai","endpoint":%q,"apiKey":"test-key"}`, upstream.URL)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleAPIProvidersProbe(w, req)
	if w.Code != 200 {
		t.Fatalf("probe status = %d body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Models []string `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Models) != 2 || resp.Models[0] != "m1" || resp.Models[1] != "m2" {
		t.Fatalf("models = %v", resp.Models)
	}

	// Auth failure propagates upstream status.
	req2 := httptest.NewRequest("POST", "/api/providers/probe",
		strings.NewReader(fmt.Sprintf(`{"type":"openai","endpoint":%q,"apiKey":"wrong"}`, upstream.URL)))
	w2 := httptest.NewRecorder()
	s.handleAPIProvidersProbe(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized probe status = %d", w2.Code)
	}
}

func TestServer_ProvidersStatusEndpoint(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	s := gatewayTestServer(t, gatewayTestConfig(), upstream)

	// Simulate a cooldown on one credential.
	route := s.currentGateway().RouteByID("prov/remote-chat")
	gateway.CooldownForTest(route.Members[0], gateway.CooldownAuth, time.Now().Add(time.Hour), "401 test")

	req := httptest.NewRequest("GET", "/api/providers/status", nil)
	w := httptest.NewRecorder()
	s.handleAPIProvidersStatus(w, req)
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"providerID":"prov"`) || !strings.Contains(body, `"id":"prov"`) {
		t.Fatalf("status body missing provider info: %s", body)
	}
	if strings.Contains(body, `Bearer k1`) || strings.Contains(body, `Bearer k2`) || strings.Contains(body, `"value":"k`) {
		t.Fatal("status must never leak key values")
	}
	// maxRetries is an optional config field backed by a pointer; the status
	// endpoint must publish the number, never the pointer's object form.
	var parsed struct {
		Providers []struct {
			ID         string `json:"id"`
			MaxRetries *int   `json:"maxRetries"`
		} `json:"providers"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("status body is not valid JSON: %v", err)
	}
	for _, provider := range parsed.Providers {
		if provider.ID == "prov" {
			if provider.MaxRetries == nil {
				t.Fatalf("provider %s reports no maxRetries: %s", provider.ID, body)
			}
			return
		}
	}
	t.Fatalf("provider prov missing from status: %s", body)
}

// geminiGatewayTestConfig mirrors gatewayTestConfig with a Gemini provider:
// the client speaks the native Gemini API and the FQN in the path must be
// rewritten to the upstream model name.
func geminiGatewayTestConfig() config.Config {
	return config.Config{
		Models: map[string]config.ModelConfig{
			"local-model": {Cmd: "echo hi", Proxy: "http://127.0.0.1:1"},
		},
		Providers: config.ProviderDictionaryConfig{
			"google": {
				Type:     config.ProviderTypeGemini,
				Endpoint: "http://placeholder",
				ApiKeys:  []config.ProviderKey{{Value: "g1"}},
				Models:   []config.ProviderModel{{Local: "gemini-flash", Remote: "gemini-2.5-flash"}},
			},
		},
	}
}

func TestServer_GatewayGeminiGenerateContent(t *testing.T) {
	var gotKey, gotPath, gotQuery atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey.Store(r.Header.Get("x-goog-api-key"))
		gotPath.Store(r.URL.Path)
		gotQuery.Store(r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"hello"}]}}]}`))
	}))
	defer upstream.Close()

	s := gatewayTestServer(t, geminiGatewayTestConfig(), upstream)

	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	req := httptest.NewRequest("POST", "/v1beta/models/google/gemini-flash:generateContent", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// emulate the mux: prepareGeminiRoute extracts model/action path values
	// from the URL before dispatch.
	route, ok := protocolrelaykit.RouteForRequest(req.Method, req.URL.Path)
	if !ok {
		t.Fatalf("request did not match the gemini route")
	}
	req.SetPathValue("model", route.Model)
	req.SetPathValue("geminiPath", route.Model+":"+route.Action)

	w := httptest.NewRecorder()
	s.localPeerHandler(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if key, _ := gotKey.Load().(string); key != "g1" {
		t.Fatalf("upstream x-goog-api-key = %q, want credential", key)
	}
	// The FQN must be rewritten to the upstream name; Google does not accept
	// provider-qualified model names.
	if path, _ := gotPath.Load().(string); path != "/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Fatalf("upstream path = %q", path)
	}
	if query, _ := gotQuery.Load().(string); query != "" {
		t.Fatalf("upstream query = %q", query)
	}
	if !strings.Contains(w.Body.String(), "hello") {
		t.Fatalf("gemini response not passed through: %s", w.Body.String())
	}
}

func TestServer_NormalizeUpstreamModelListGemini(t *testing.T) {
	body := []byte(`{"models":[
		{"name":"models/gemini-2.5-flash","supportedGenerationMethods":["generateContent","streamGenerateContent"]},
		{"name":"models/gemini-embedding-001","supportedGenerationMethods":["embedContent"]},
		{"name":"models/gemini-2.5-pro"}
	]}`)
	models := normalizeUpstreamModelList(body)
	want := []string{"gemini-2.5-flash", "gemini-2.5-pro"}
	if len(models) != len(want) {
		t.Fatalf("models = %v, want %v", models, want)
	}
	for i := range want {
		if models[i] != want[i] {
			t.Errorf("models[%d] = %q, want %q", i, models[i], want[i])
		}
	}
}

func TestServer_GatewaySessionDownstreamHeaderWins(t *testing.T) {
	cfg := gatewayTestConfig()
	cfg.UI.Activity.SessionID = []string{"X-Session-ID"}

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"prov/remote-chat","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("X-Session-ID", "session-abc")
	session := gatewaySession(req, cfg, "remote-chat")

	if session.ID != "session-abc" {
		t.Fatalf("session id = %q, want the downstream value", session.ID)
	}
	if session.Hash == "" {
		t.Fatal("sticky hash must be derived")
	}
}

func TestServer_GatewaySessionFallsBackToPrefix(t *testing.T) {
	cfg := gatewayTestConfig()
	cfg.UI.Activity.SessionID = []string{"X-Session-ID"}

	sessionFor := func(firstMessage string) gateway.Session {
		body := `{"model":"prov/remote-chat","messages":[{"role":"user","content":"` + firstMessage + `"}]}`
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		return gatewaySession(req, cfg, "remote-chat")
	}

	first := sessionFor("how do I configure the gateway")
	if first.ID == "" {
		t.Fatal("a prefix-derived session id is required when no header is sent")
	}
	// The same conversation prefix keeps the same id across turns (the body
	// grows, the first message does not change).
	if again := sessionFor("how do I configure the gateway"); again.ID != first.ID {
		t.Fatalf("prefix-derived id not stable: %q != %q", again.ID, first.ID)
	}
	if other := sessionFor("a different conversation"); other.ID == first.ID {
		t.Fatal("different prefixes must not share one session id")
	}
	// The injected value is a hash of the prefix, never the content itself.
	if strings.Contains(first.ID, "gateway") {
		t.Fatalf("prefix content leaked into the session id: %q", first.ID)
	}
}

func TestServer_GatewaySessionNoSignalInjectsNothing(t *testing.T) {
	cfg := gatewayTestConfig()
	cfg.UI.Activity.SessionID = []string{"X-Session-ID"}

	req := httptest.NewRequest("GET", "/v1/models", nil)
	session := gatewaySession(req, cfg, "remote-chat")
	if session.ID != "" {
		t.Fatalf("session id = %q, want empty when neither header nor body signal exists", session.ID)
	}
	if session.Hash != "" {
		t.Fatal("a request with no conversation signal must have no affinity: hashing the caller alone would pin every request it makes to one provider")
	}
}

func TestServer_GatewayGeminiAdapterConvertsForeignDialect(t *testing.T) {
	var gotPath, gotBody atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotPath.Store(r.URL.Path)
		gotBody.Store(string(body))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"pong"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1,"totalTokenCount":4}}`))
	}))
	defer upstream.Close()

	cfg := geminiGatewayTestConfig()
	cfg.Providers["google"] = config.ProviderConfig{
		ApiKeys: []config.ProviderKey{{Value: "g"}},
		Adapters: []config.ProviderAdapter{{
			Name: "Google", Type: config.ProviderTypeGemini, Endpoint: upstream.URL,
			Models: []config.ProviderModel{{Local: "gemini-flash", Remote: "gemini-2.5-flash"}},
		}},
	}
	s := gatewayTestServer(t, cfg, nil)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"google/gemini-flash","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.localPeerHandler(w, req)

	// A chat client reaches a Gemini-only pool by conversion. Forwarding the
	// client's path unchanged would have produced a confusing upstream 404;
	// the attempt is converted to the pool's dialect instead.
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if path, _ := gotPath.Load().(string); !strings.HasPrefix(path, "/v1beta/models/") {
		t.Fatalf("upstream path = %q, want the Gemini generateContent path", path)
	}
	body, _ := gotBody.Load().(string)
	if !strings.Contains(body, `"contents"`) || !strings.Contains(body, "hi") {
		t.Fatalf("upstream body is not the converted Gemini request: %s", body)
	}
	if !strings.Contains(w.Body.String(), `"choices"`) || !strings.Contains(w.Body.String(), "pong") {
		t.Fatalf("client response was not converted back to chat: %s", w.Body.String())
	}
}

func TestServer_ProvidersStatusReportsAdapters(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	cfg := gatewayTestConfig()
	prov := cfg.Providers["prov"]
	prov.Adapters = []config.ProviderAdapter{
		{Name: "Chat", Type: config.ProviderTypeOpenAI, Endpoint: "http://placeholder", Models: []config.ProviderModel{{Local: "chat-model", Remote: "chat-model"}}},
		{Name: "Google", Type: config.ProviderTypeGemini, Endpoint: "http://placeholder", Models: []config.ProviderModel{{Local: "gem-model", Remote: "gem-model"}}},
	}
	prov.Type, prov.Endpoint, prov.Models = "", "", nil
	prov.Models = nil
	cfg.Providers["prov"] = prov

	s := gatewayTestServer(t, cfg, upstream)
	req := httptest.NewRequest("GET", "/api/providers/status", nil)
	w := httptest.NewRecorder()
	s.handleAPIProvidersStatus(w, req)
	if w.Code != 200 {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Providers []struct {
			ID         string `json:"id"`
			Type       string `json:"type"`
			Endpoint   string `json:"endpoint"`
			ModelCount int    `json:"modelCount"`
			Adapters   []struct {
				Name       string `json:"name"`
				Type       string `json:"type"`
				Endpoint   string `json:"endpoint"`
				ModelCount int    `json:"modelCount"`
			} `json:"adapters"`
		} `json:"providers"`
		Routes []struct {
			Adapter     string `json:"adapter"`
			AdapterType string `json:"adapterType"`
			ModelID     string `json:"modelID"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	var provider *struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Endpoint   string `json:"endpoint"`
		ModelCount int    `json:"modelCount"`
		Adapters   []struct {
			Name       string `json:"name"`
			Type       string `json:"type"`
			Endpoint   string `json:"endpoint"`
			ModelCount int    `json:"modelCount"`
		} `json:"adapters"`
	}
	for i := range resp.Providers {
		if resp.Providers[i].ID == "prov" {
			provider = &resp.Providers[i]
		}
	}
	if provider == nil {
		t.Fatalf("provider missing from status: %s", w.Body.String())
	}
	if provider.Type != "multi" || provider.Endpoint != "" {
		t.Errorf("multi-adapter provider must report type=multi and no endpoint, got %q %q", provider.Type, provider.Endpoint)
	}
	if provider.ModelCount != 2 || len(provider.Adapters) != 2 {
		t.Fatalf("modelCount = %d adapters = %d, want 2/2", provider.ModelCount, len(provider.Adapters))
	}
	if provider.Adapters[0].Name != "Chat" || provider.Adapters[0].Type != "openai" || provider.Adapters[0].ModelCount != 1 {
		t.Errorf("adapter0 = %+v", provider.Adapters[0])
	}
	byModel := map[string]string{}
	for _, route := range resp.Routes {
		byModel[route.ModelID] = route.Adapter + ":" + route.AdapterType
	}
	if byModel["chat-model"] != "Chat:openai" || byModel["gem-model"] != "Google:gemini" {
		t.Errorf("routes = %+v", byModel)
	}
}

func TestServer_GatewayAdapterDialectConversionNamesAdapter(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"pong"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1,"totalTokenCount":4}}`))
	}))
	defer upstream.Close()

	cfg := gatewayTestConfig()
	prov := cfg.Providers["prov"]
	prov.Adapters = []config.ProviderAdapter{
		{Name: "Google", Type: config.ProviderTypeGemini, Endpoint: upstream.URL, Models: []config.ProviderModel{{Local: "gem-model", Remote: "gem-remote"}}},
	}
	prov.Type, prov.Endpoint, prov.Models = "", "", nil
	cfg.Providers["prov"] = prov

	s := gatewayTestServer(t, cfg, nil)
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"prov/gem-model","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.localPeerHandler(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if !strings.HasPrefix(gotPath, "/v1beta/models/") {
		t.Fatalf("upstream path = %q, want the adapter's Gemini path", gotPath)
	}
	// Gemini carries the model in the path, not the body.
	if !strings.Contains(gotPath, "gem-remote") {
		t.Fatalf("upstream path = %q, want the adapter's remote model name", gotPath)
	}
	if !strings.Contains(w.Body.String(), `"choices"`) {
		t.Fatalf("response was not converted back to chat: %s", w.Body.String())
	}
}

func TestServer_AccessKeyAllowlistAcceptsExposedAndLegacyName(t *testing.T) {
	cfg := gatewayTestConfig()
	prov := cfg.Providers["prov"]
	prov.ModelPrefix = "go-"
	cfg.Providers["prov"] = prov

	identity := auth.Identity{ID: "key-1", Models: []string{"prov/remote-chat"}}
	// The key allows the legacy form; the exposed (prefixed) name must pass.
	if !modelAllowedForIdentity(cfg, identity, "go-remote-chat") {
		t.Fatal("an allowlist entry in the legacy form must authorize the exposed name")
	}
	if !modelAllowedForIdentity(cfg, identity, "prov/remote-chat") {
		t.Fatal("the legacy form itself must stay authorized")
	}

	other := auth.Identity{ID: "key-2", Models: []string{"go-remote-chat"}}
	if !modelAllowedForIdentity(cfg, other, "go-remote-chat") {
		t.Fatal("an allowlist entry in the exposed form must authorize it")
	}

	stranger := auth.Identity{ID: "key-3", Models: []string{"something-else"}}
	if modelAllowedForIdentity(cfg, stranger, "go-remote-chat") {
		t.Fatal("an unrelated allowlist must not authorize provider models")
	}
}

func TestServer_ConvertedRequestPinsFailoverToSameDialect(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			"local-model": {Cmd: "echo hi", Proxy: "http://127.0.0.1:1"},
		},
		Providers: config.ProviderDictionaryConfig{
			"alpha": {
				Type:     config.ProviderTypeOpenAI,
				Endpoint: upstream.URL,
				ApiKeys:  []config.ProviderKey{{Value: "a"}},
				Models:   []config.ProviderModel{{Local: "shared", Remote: "shared"}},
			},
			"gamma": {
				Type:     config.ProviderTypeGemini,
				Endpoint: upstream.URL,
				ApiKeys:  []config.ProviderKey{{Value: "g"}},
				Models:   []config.ProviderModel{{Local: "shared", Remote: "shared"}},
			},
		},
	}
	s := gatewayTestServer(t, cfg, upstream)
	gw := s.currentGateway()
	primary := gw.RouteFor("shared", false)
	if primary == nil {
		t.Fatal("route missing")
	}
	chain := gw.FailoverRoutes("shared", false, primary)
	if len(chain) != 2 {
		t.Fatalf("chain = %d routes, want both providers", len(chain))
	}
	// serveGateway narrows the pool to the converted surface only when the
	// conversion layer already rewrote the request, so mirror that here.
	chainFor := func(r *http.Request) []*gateway.Route {
		if target := protocolTargetFormat(r); target != "" {
			return sameSurfaceRoutes(chain, target)
		}
		return chain
	}

	// Without a conversion the whole pool is available.
	passthrough := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"shared"}`))
	if kept := chainFor(passthrough); len(kept) != 2 {
		t.Fatalf("kept = %d, want both providers for an unconverted request", len(kept))
	}

	// A converted request stays on providers speaking the same dialect.
	converted := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"shared"}`))
	converted = converted.WithContext(context.WithValue(converted.Context(), protocolBackendPathKey{},
		protocolBackendPath{Source: protocolrelaykit.OpenAIChat, Path: "/v1beta/models/shared:generateContent", Format: protocolrelaykit.Gemini, Stream: false}))
	kept := chainFor(converted)
	if len(kept) != 1 {
		t.Fatalf("kept = %d, want only same-dialect providers", len(kept))
	}
	if kept[0].Type() != config.ProviderTypeGemini {
		t.Fatalf("kept %s (%s), want only the Gemini provider the request was converted for", kept[0].ProviderID, kept[0].Type())
	}
	if len(kept) == 2 {
		t.Fatal("a different-dialect sibling must be dropped")
	}
}

// TestServer_ProvidersProbeReachesHTTP2OnlyUpstream is the regression guard for
// a probe against an upstream that speaks HTTP/2 only (api.xiaomimimo.com is
// one): the process default transport is pinned to HTTP/1.1 so the network
// policy decides every connection, and the probe used to ride on it. The
// handshake then still negotiated h2 (net/http leaves "h2" in the transport's
// TLS config), so the server's HTTP/2 frames surfaced as
// `malformed HTTP response` with a binary SETTINGS frame.
func TestServer_ProvidersProbeReachesHTTP2OnlyUpstream(t *testing.T) {
	sawProtocol := make(chan string, 1)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		select {
		case sawProtocol <- r.Proto:
		default:
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"mimo-1"}]}`))
	}))
	upstream.EnableHTTP2 = true
	// HTTP/2 only: an ALPN offer without h2 has no overlap to fall back on.
	upstream.TLS = &tls.Config{NextProtos: []string{"h2"}}
	upstream.StartTLS()
	defer upstream.Close()

	// The probe builds its own transport, so the test wraps the real builder
	// (HTTP/2 stays its decision) and only swaps in the test server's CA.
	pool := x509.NewCertPool()
	pool.AddCert(upstream.Certificate())
	previous := probeTransportBuilder
	t.Cleanup(func() { probeTransportBuilder = previous })
	probeTransportBuilder = func(proxyValue string) (http.RoundTripper, error) {
		roundTripper, err := buildProbeTransport(proxyValue)
		if err != nil {
			return nil, err
		}
		transport, ok := roundTripper.(*http.Transport)
		if !ok {
			t.Fatalf("probe transport is %T, want *http.Transport", roundTripper)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool}
		return transport, nil
	}

	s := gatewayTestServer(t, gatewayTestConfig(), upstream)
	req := httptest.NewRequest("POST", "/api/providers/probe",
		strings.NewReader(fmt.Sprintf(`{"type":"openai","endpoint":%q,"timeout":20}`, upstream.URL)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleAPIProvidersProbe(w, req)
	if w.Code != 200 {
		t.Fatalf("probe status = %d body = %s", w.Code, w.Body.String())
	}
	if protocol := <-sawProtocol; protocol != "HTTP/2.0" {
		t.Fatalf("upstream saw %q, want HTTP/2.0", protocol)
	}
}

// TestServer_ExhaustedPoolReportsUpstreamErrorInsteadOfShortCircuit is the
// regression guard for the dispatcher's own 502.
//
// RouteFor answers "can any route serve this right now" and returns nil once
// every credential of the pool is cooling down. The dispatcher used to answer
// that nil itself with a synthetic 502 before serveGateway ever ran, so the
// gateway's failover loop — the thing that knows the chain and the last real
// upstream answer — never got a chance to run.
//
// Answering here is wrong for two reasons: a client sees a synthetic message
// instead of the upstream's own error, and the decision is made by a single
// route lookup rather than by the pool plan. This pins that the dispatcher
// still routes to the gateway when the pool is fully benched, and that the
// client receives the last upstream answer rather than "no credential is
// currently available".
func TestServer_ExhaustedPoolReportsUpstreamErrorInsteadOfShortCircuit(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":{"code":"upstream_timeout","message":"upstream timed out"}}`))
	}))
	defer upstream.Close()

	cfg := config.Config{
		Providers: config.ProviderDictionaryConfig{
			"alpha": {
				Endpoint: upstream.URL,
				ApiKeys:  []config.ProviderKey{{Value: "alpha-key"}},
				Models:   []config.ProviderModel{{Local: "shared-model", Remote: "alpha-upstream"}},
				Cooldown: config.ProviderCooldownConfig{Policy: "enabled", Transient: "60s"},
			},
		},
	}
	s := gatewayTestServer(t, cfg, nil)
	gw := s.currentGateway()

	// Request one answers 503 and, after spending the key's retry budget
	// (maxRetries defaults to 2, so three attempts), benches the only key in
	// the pool.
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"shared-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.localPeerHandlerWithConfig(w, req, cfg)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("first request status = %d body = %s", w.Code, w.Body.String())
	}
	if calls.Load() != 3 {
		t.Fatalf("upstream calls = %d, want 3 (one attempt plus two retries on the same key)", calls.Load())
	}

	// The whole pool is now benched: RouteFor must report nil here, or the
	// scenario does not reproduce.
	if route := gw.RouteFor("shared-model", false); route != nil {
		t.Fatalf("RouteFor resolved %s; the pool should be exhausted", route.ProviderID)
	}

	// Request two must still be handed to the gateway. The dispatcher must not
	// answer on its own.
	req2 := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"shared-model","messages":[{"role":"user","content":"hi again"}]}`))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	s.localPeerHandlerWithConfig(w2, req2, cfg)

	if strings.Contains(w2.Body.String(), "no credential is currently available") {
		t.Fatalf("body = %s: the dispatcher short-circuited instead of reporting the upstream answer",
			w2.Body.String())
	}
	if w2.Code != http.StatusServiceUnavailable {
		t.Fatalf("exhausted-pool status = %d body = %s, want the recorded upstream 503",
			w2.Code, w2.Body.String())
	}
	if !strings.Contains(w2.Body.String(), "upstream_timeout") {
		t.Fatalf("body = %s, want the real upstream error", w2.Body.String())
	}
}

// 两个供应商共享同一个裸名，第一家额度耗尽(402)后请求转到第二家并成功：
// 活动记录的供应商列必须落在真正答出 200 的那家。holder 是首个发布者胜出，
// 失败候选曾在交还给下一候选之前抢先发布，把记录钉死在失败者上（用户实测
// 的"记录显示 opencode-go、实际由 DeepSeek 官方答出"）。
func TestServer_FailoverAttributionFollowsTheAnsweringProvider(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"message":"Insufficient Balance"}}`))
	}))
	defer failing.Close()
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer healthy.Close()

	cfg := config.Config{
		Providers: config.ProviderDictionaryConfig{
			// 高 priority 让失败供应商恒为链首，测试顺序不靠轮转掷硬币。
			"opencode-go": {
				Type:     config.ProviderTypeOpenAI,
				Endpoint: failing.URL,
				ApiKeys:  []config.ProviderKey{{Value: "k", Priority: 10}},
				Models:   []config.ProviderModel{{Local: "flash", Remote: "flash"}},
			},
			"deepseek": {
				Type:     config.ProviderTypeOpenAI,
				Endpoint: healthy.URL,
				ApiKeys:  []config.ProviderKey{{Value: "k"}},
				Models:   []config.ProviderModel{{Local: "flash", Remote: "flash"}},
			},
		},
	}
	s := gatewayTestServer(t, cfg, nil)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"flash","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(swaputil.WithRequestDiagnostics(req.Context()))
	w := httptest.NewRecorder()
	s.localPeerHandler(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}

	diagnostics := swaputil.RequestDiagnosticsFrom(req.Context())
	if diagnostics == nil {
		t.Fatal("诊断 holder 丢失")
	}
	if diagnostics.Provider != "deepseek" {
		t.Fatalf("Provider = %q，failover 后归属必须跟随真正答出 200 的供应商", diagnostics.Provider)
	}
}

// 全部候选都失败时，归属仍是最后一个尝试的供应商——客户端看到的就是它的
// 错误体。放开失败候选的抢先发布之后，这条兜底（publishRouteAttribution）
// 必须继续生效。
func TestServer_ExhaustedCandidatesAttributeTheLastProviderTried(t *testing.T) {
	refuse := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = w.Write([]byte(`{"error":{"message":"Insufficient Balance"}}`))
		}))
	}
	first, second := refuse(), refuse()
	defer first.Close()
	defer second.Close()

	cfg := config.Config{
		Providers: config.ProviderDictionaryConfig{
			"opencode-go": {
				Type:     config.ProviderTypeOpenAI,
				Endpoint: first.URL,
				ApiKeys:  []config.ProviderKey{{Value: "k", Priority: 10}},
				Models:   []config.ProviderModel{{Local: "flash", Remote: "flash"}},
			},
			"deepseek": {
				Type:     config.ProviderTypeOpenAI,
				Endpoint: second.URL,
				ApiKeys:  []config.ProviderKey{{Value: "k"}},
				Models:   []config.ProviderModel{{Local: "flash", Remote: "flash"}},
			},
		},
	}
	s := gatewayTestServer(t, cfg, nil)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"flash","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(swaputil.WithRequestDiagnostics(req.Context()))
	w := httptest.NewRecorder()
	s.localPeerHandler(w, req)
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}

	diagnostics := swaputil.RequestDiagnosticsFrom(req.Context())
	if diagnostics == nil {
		t.Fatal("诊断 holder 丢失")
	}
	if diagnostics.Provider != "deepseek" {
		t.Fatalf("Provider = %q，全失败时应记录最后一个尝试的供应商", diagnostics.Provider)
	}
}
