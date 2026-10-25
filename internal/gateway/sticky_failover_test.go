package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// cooldownConfigFor builds a single-model pool whose providers each hold one
// key, with cooldowns short enough for a test to observe a bench and its
// recovery.
func cooldownConfigFor(t *testing.T, alphaURL, betaURL string) config.Config {
	t.Helper()
	cfg := config.Config{
		Gateway: config.GatewayConfig{Cooldown: config.GatewayCooldownConfig{
			Transient: "300ms", RateLimit: "300ms", Auth: "300ms",
		}},
		Providers: config.ProviderDictionaryConfig{
			"alpha": {Endpoint: alphaURL, ApiKeys: []config.ProviderKey{{Value: "a", Priority: 1}}, Models: []config.ProviderModel{{Local: "shared", Remote: "alpha"}}},
			"beta":  {Endpoint: betaURL, ApiKeys: []config.ProviderKey{{Value: "b", Priority: 1}}, Models: []config.ProviderModel{{Local: "shared", Remote: "beta"}}},
		},
	}
	for id, p := range cfg.Providers {
		cfg.Providers[id] = normalizeProvider(t, p)
	}
	return cfg
}

// TestGateway_CooldownMovesSessionOnceAndKeepsItThere pins the session
// semantics a provider outage must have:
//
//   - while the bound provider is down, the session moves to another one;
//   - it then stays there across the whole outage instead of rotating;
//   - when the original provider recovers, the session does NOT move back,
//     because the cache it built on the new provider would be discarded.
func TestGateway_CooldownMovesSessionOnceAndKeepsItThere(t *testing.T) {
	var alphaDown atomic.Bool
	var alphaCalls, betaCalls atomic.Int32
	alpha := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		alphaCalls.Add(1)
		if alphaDown.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"overloaded"}`))
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer alpha.Close()
	beta := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		betaCalls.Add(1)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer beta.Close()

	g, err := New(cooldownConfigFor(t, alpha.URL, beta.URL), testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()
	betaRouteKey := ""
	for _, route := range g.AllRoutes() {
		if route.ProviderID == "beta" {
			betaRouteKey = route.Key()
		}
	}
	if betaRouteKey == "" {
		t.Fatal("beta route missing")
	}

	session := Session{Hash: "cooldown-session"}
	send := func() {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
		w := httptest.NewRecorder()
		// The whole pool, the way the server plans it: a provider going down
		// must be able to fail over to a different provider.
		g.ServeHTTPWithFailover(w, req, g.PlanRoutes("shared", false, "openai", session), session)
		if w.Code != http.StatusOK {
			t.Fatalf("request failed: %d %s", w.Code, w.Body.String())
		}
	}

	// Bind the session: whichever provider the rotation picked serves first.
	send()
	boundToAlpha := alphaCalls.Load() == 1 && betaCalls.Load() == 0
	alphaCalls.Store(0)
	betaCalls.Store(0)
	if !boundToAlpha && betaCalls.Load() != 0 {
		t.Fatalf("expected one provider to serve, alpha=%d beta=%d", alphaCalls.Load(), betaCalls.Load())
	}
	if !boundToAlpha {
		// The rotation picked beta. Fail beta instead so the test still
		// exercises "bound provider down, session moves".
		t.Skip("session bound to beta; alpha is not the bound provider")
	}

	// Take the bound provider down. Every request must land on beta: a session
	// that rotates while its provider is cooling down is the bug.
	alphaDown.Store(true)
	for i := 0; i < 4; i++ {
		send()
	}
	if betaCalls.Load() != 4 {
		t.Fatalf("while alpha was down beta served %d of 4 requests", betaCalls.Load())
	}
	// The very first request of the outage must already have moved the binding:
	// leaving it on the dead provider would let a later rotation pull the
	// session back before it built any cache.
	if entry, ok := g.sticky.get(session.affinityKey(), time.Now()); !ok || entry.route != betaRouteKey {
		t.Fatalf("binding after the outage = %+v ok=%v, want %s", entry, ok, betaRouteKey)
	}

	// Recover alpha. The session must stay on beta: it has since built its
	// prompt cache there, and moving back would throw that cache away.
	alphaDown.Store(false)
	for i := 0; i < 3; i++ {
		send()
	}
	if betaCalls.Load() != 7 {
		t.Fatalf("after alpha recovered beta served %d of 7 requests; the session was pulled back", betaCalls.Load())
	}
}
