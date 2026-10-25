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

// unauthPool builds a bare-name pool whose every provider and every key
// answers 401, and returns the plan chain for its shared model name.
func unauthPool(t *testing.T, providers ...string) (*Gateway, []*Route, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"invalid api key","type":"auth_error"}}`))
	}))
	t.Cleanup(upstream.Close)

	cfg := config.Config{Providers: config.ProviderDictionaryConfig{}}
	for _, id := range providers {
		p := config.ProviderConfig{
			Type:     config.ProviderTypeOpenAI,
			Endpoint: upstream.URL,
			ApiKeys: []config.ProviderKey{
				{Value: id + "-key-a", Weight: 1},
				{Value: id + "-key-b", Weight: 1},
			},
			Models: []config.ProviderModel{{Local: "shared", Remote: "shared-remote"}},
		}
		cfg.Providers[id] = normalizeProvider(t, p)
	}
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(g.Shutdown)
	return g, g.PlanRoutes("shared", false, "", Session{}), &calls
}

// TestGateway_AllSourcesUnauthorizedKeepsRealStatus drives a pool where every
// provider is rejected with 401. The client must keep receiving the upstream's
// own 401 and error body, not the synthetic "no available credential" 502: the
// upstream did refuse the request, and that is the answer a client can act on.
func TestGateway_AllSourcesUnauthorizedKeepsRealStatus(t *testing.T) {
	g, chain, calls := unauthPool(t, "alpha", "beta", "gamma")
	pass := chainCredentialCount(chain)

	for round := 0; round < 3; round++ {
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"shared","messages":[]}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		g.ServeHTTPWithFailover(w, req, chain, Session{})

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("round %d: status = %d, want the upstream's 401", round, w.Code)
		}
		if !strings.Contains(w.Body.String(), "invalid api key") {
			t.Fatalf("round %d: body lost the upstream error: %s", round, w.Body.String())
		}
	}

	// The first request exhausts the chain once; the rest are answered from the
	// recorded upstream answer while every credential cools down, so the pool
	// is not hammered for as long as the cooldowns last.
	if got := int(calls.Load()); got > pass {
		t.Fatalf("upstream calls = %d, want at most one pass over the chain (%d)", got, pass)
	}
}

// TestGateway_BenchedPoolAnswersWithoutUpstream checks that a request arriving
// while every credential is cooling down is answered immediately and does not
// dial the upstream again.
func TestGateway_BenchedPoolAnswersWithoutUpstream(t *testing.T) {
	g, chain, calls := unauthPool(t, "alpha", "beta", "gamma", "delta")
	pass := chainCredentialCount(chain)

	// First request benches every credential for real.
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"shared","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	g.ServeHTTPWithFailover(httptest.NewRecorder(), req, chain, Session{})
	if got := calls.Load(); got != int32(pass) {
		t.Fatalf("first request made %d upstream calls, want one per credential (%d)", got, pass)
	}

	before := calls.Load()
	req = httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"shared","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	start := time.Now()
	g.ServeHTTPWithFailover(w, req, chain, Session{})
	elapsed := time.Since(start)

	if extra := calls.Load() - before; extra != 0 {
		t.Errorf("benched pool dialed the upstream %d more time(s), want 0", extra)
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want the upstream's 401", w.Code)
	}
	if elapsed > 50*time.Millisecond {
		t.Errorf("benched request took %s, want an immediate answer", elapsed)
	}
}

// TestGateway_AuthCooldownStaysBenchedAcrossWindows drives one credential
// through the "every key returns 401" timeline. The first failure gets the
// short probe; every later one must keep the full auth cooldown instead of
// falling back to the probe, which is what left a dead key alternating between
// a 30s and a 10m bench forever.
func TestGateway_AuthCooldownStaysBenchedAcrossWindows(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()

	cfg := gwConfig(t, upstream.URL, config.ProviderTypeOpenAI)
	prov := cfg.Providers["prov"]
	prov.ApiKeys = []config.ProviderKey{{Value: "only-key"}}
	prov.MaxRetries = intPointerForTest(1)
	cfg.Providers["prov"] = normalizeProvider(t, prov)

	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("prov/m1", false)
	cred := route.Credentials()[0]

	// Six consecutive 401s. ClearCooldowns before each one lifts the timestamps
	// so the request can actually be attempted again, which is what an operator
	// retrying right after a 401 does — the recorded auth failure must survive
	// it, otherwise every clear resets the escalation and the key keeps being
	// retried on the short probe.
	benched := make([]time.Duration, 0, 6)
	for i := 0; i < 6; i++ {
		cred.ClearCooldowns()
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"remote-m1"}`))
		g.ServeHTTP(httptest.NewRecorder(), req, route, Session{})
		until := cred.Status().AuthUntil
		if until.IsZero() {
			t.Fatalf("failure %d: credential was not benched at all", i+1)
		}
		benched = append(benched, time.Until(until))
	}

	if got := benched[0]; got > DefaultAuthProbeCooldown+time.Minute {
		t.Errorf("first 401 should get the short probe, got %s", got)
	}
	for i, d := range benched[1:] {
		if d < DefaultAuthCooldown-time.Minute {
			t.Errorf("failure %d benched for %s, want the full auth cooldown (%s)",
				i+2, d, DefaultAuthCooldown)
		}
	}
}
