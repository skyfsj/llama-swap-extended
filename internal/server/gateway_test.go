package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/gateway"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

func gatewayTestServer(t *testing.T, cfg config.Config, upstream *httptest.Server) *Server {
	t.Helper()
	if upstream != nil {
		prov := cfg.Providers["prov"]
		prov.Endpoint = upstream.URL
		cfg.Providers["prov"] = prov
	}
	local := newStubRouter([]string{"local-model"}, `{"local":true}`)
	peer := newStubRouter(nil, "")
	s := &Server{
		cfg:         cfg,
		muxlog:      logmon.NewWriter(io_discard{}),
		proxylog:    logmon.NewWriter(io_discard{}),
		upstreamlog: logmon.NewWriter(io_discard{}),
		inflight:    newInflightTracker(),
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
	foundFQN := false
	for _, m := range resp.Data {
		if m.ID == "prov/remote-chat" {
			foundFQN = true
		}
	}
	if !foundFQN {
		t.Fatalf("provider model missing from /v1/models: %s", w.Body.String())
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
