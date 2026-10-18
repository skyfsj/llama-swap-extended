package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
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
	session := gatewaySession(req, cfg)

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
		return gatewaySession(req, cfg)
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
	session := gatewaySession(req, cfg)
	if session.ID != "" {
		t.Fatalf("session id = %q, want empty when neither header nor body signal exists", session.ID)
	}
	if session.Hash == "" {
		t.Fatal("client-identity fallback hash must still be produced")
	}
}

func TestServer_GatewayGeminiRejectsForeignDialect(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	s := gatewayTestServer(t, geminiGatewayTestConfig(), upstream)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"google/gemini-flash","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.localPeerHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "type gemini") {
		t.Fatalf("error does not explain the dialect mismatch: %s", w.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatal("foreign-dialect request must not reach the gemini upstream")
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

func TestServer_GatewayAdapterDialectGuardNamesAdapter(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	cfg := gatewayTestConfig()
	prov := cfg.Providers["prov"]
	prov.Adapters = []config.ProviderAdapter{
		{Name: "Google", Type: config.ProviderTypeGemini, Endpoint: "http://placeholder", Models: []config.ProviderModel{{Local: "gem-model", Remote: "gem-model"}}},
	}
	prov.Type, prov.Endpoint, prov.Models = "", "", nil
	cfg.Providers["prov"] = prov

	s := gatewayTestServer(t, cfg, upstream)
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"prov/gem-model","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.localPeerHandler(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	// The JSON envelope escapes the quotes around the adapter name.
	message := w.Body.String()
	if !strings.Contains(message, "adapter") || !strings.Contains(message, "Google") || !strings.Contains(message, "type gemini") {
		t.Fatalf("error must name the adapter and dialect: %s", message)
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
	// serveGateway applies the pinning only when the conversion layer rewrote
	// the request, so mirror that decision here.
	chainFor := func(r *http.Request) []*gateway.Route {
		if target := protocolTargetFormat(r); target != "" {
			return sameDialectRoutes(chain, primary)
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
	if kept[0].Type() != primary.Type() {
		t.Fatalf("kept %s (%s), want the primary's dialect %s", kept[0].ProviderID, kept[0].Type(), primary.Type())
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
