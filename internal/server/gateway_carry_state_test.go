package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// A reload must not move a live conversation to another provider.
//
// The two providers start at equal priority, so the first request binds the
// session to whichever of them the rotation picks. The reload then raises the
// other provider's priority: an unbound session would follow the new order and
// leave, so this fails if the binding was dropped by the gateway rebuild.
func TestServer_GatewayReloadKeepsSessionProvider(t *testing.T) {
	alpha := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"a","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ALPHA"},"finish_reason":"stop"}]}`))
	}))
	defer alpha.Close()
	beta := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"b","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"BETA"},"finish_reason":"stop"}]}`))
	}))
	defer beta.Close()

	// betaPriority is the lever: equal at first so the session binds wherever
	// the rotation lands, higher afterwards so a lost binding is visible.
	providers := func(betaPriority int) config.ProviderDictionaryConfig {
		return config.ProviderDictionaryConfig{
			"alpha": {Endpoint: alpha.URL, ApiKeys: []config.ProviderKey{{Value: "a", Priority: 1}}, Models: []config.ProviderModel{{Local: "shared", Remote: "alpha"}}},
			"beta":  {Endpoint: beta.URL, ApiKeys: []config.ProviderKey{{Value: "b", Priority: betaPriority}}, Models: []config.ProviderModel{{Local: "shared", Remote: "beta"}}},
		}
	}
	cfg := config.Config{Providers: providers(1)}
	cfg.UI.Activity.SessionID = []string{"X-Session-ID"}
	s := multiSurfaceServer(t, cfg)

	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"shared","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Session-ID", "reload-session")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		return w
	}
	body := func(w *httptest.ResponseRecorder) string {
		if w.Code != http.StatusOK {
			t.Fatalf("request failed: %d %s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	bound := ""
	for range 3 {
		answer := strings.ToUpper(body(send()))
		switch {
		case strings.Contains(answer, "ALPHA") && bound != "BETA":
			bound = "ALPHA"
		case strings.Contains(answer, "BETA") && bound != "ALPHA":
			bound = "BETA"
		default:
			t.Fatalf("the session moved between providers during warm-up: %s", answer)
		}
	}

	// The reload edits the provider section (which is what forces the gateway
	// to be rebuilt) and promotes the provider this session is not using.
	edited := providers(5)
	edited["gamma"] = config.ProviderConfig{
		Endpoint: beta.URL, ApiKeys: []config.ProviderKey{{Value: "g"}},
		Models: []config.ProviderModel{{Local: "other", Remote: "gamma"}},
	}
	reloaded := config.Config{LogLevel: "debug", Providers: edited}
	reloaded.UI.Activity.SessionID = []string{"X-Session-ID"}
	if err := s.ReconcileConfig(reloaded); err != nil {
		t.Fatalf("ReconcileConfig: %v", err)
	}
	if gw := s.currentGateway(); gw == nil {
		t.Fatal("gateway disappeared after reload")
	}

	for i := 0; i < 3; i++ {
		answer := body(send())
		if !strings.Contains(strings.ToUpper(answer), strings.ToUpper(bound)) {
			t.Fatalf("request %d left the bound provider (%s): %s", i, bound, answer)
		}
	}
}
