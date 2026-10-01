package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// normalizeProvider applies the same defaults/validation as config loading.
// It re-parses through YAML to reuse the unexported normalize path.
func normalizeProvider(t *testing.T, p config.ProviderConfig) config.ProviderConfig {
	t.Helper()
	node := map[string]config.ProviderConfig{"p": p}
	data, err := yaml.Marshal(node)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]config.ProviderConfig
	if err := yaml.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out["p"]
}

func testLogger() *logmon.Monitor {
	return logmon.NewWriter(testDiscard{})
}

type testDiscard struct{}

func (testDiscard) Write(p []byte) (int, error) { return len(p), nil }

func gwConfig(t *testing.T, upstreamURL, providerType string) config.Config {
	t.Helper()
	cfg := config.Config{
		Providers: config.ProviderDictionaryConfig{
			"prov": {
				Type:     providerType,
				Endpoint: upstreamURL,
				ApiKeys: []config.ProviderKey{
					{Value: "key-a", Weight: 1},
					{Value: "key-b", Weight: 1},
				},
				Models: []config.ProviderModel{{Local: "m1", Remote: "remote-m1"}},
			},
		},
	}
	for id, p := range cfg.Providers {
		cfg.Providers[id] = normalizeProvider(t, p)
	}
	return cfg
}

func TestGateway_EndToEndAuthHeaderAndModelRewrite(t *testing.T) {
	var gotAuth, gotAPIKey, gotModel, gotVersion string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if m, ok := body["model"].(string); ok {
			gotModel = m
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := gwConfig(t, upstream.URL, config.ProviderTypeOpenAI)
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("prov/m1", false)
	if route == nil {
		t.Fatal("route not found")
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"prov/m1","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	// Model rewrite is the caller's job (swaputil.ReplaceRequestModel in the
	// server path); emulate it here.
	body, _ := json.Marshal(map[string]any{"model": "remote-m1", "messages": []any{}})
	req.Body = http.NoBody
	req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")

	g.ServeHTTP(w, req, route, "")

	if w.Code != 200 {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if gotAuth != "Bearer key-a" && gotAuth != "Bearer key-b" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotAPIKey != "" {
		t.Fatalf("x-api-key must be removed for openai type, got %q", gotAPIKey)
	}
	if gotModel != "remote-m1" {
		t.Fatalf("upstream model = %q", gotModel)
	}
	_ = gotVersion
}

func TestGateway_AnthropicHeaders(t *testing.T) {
	var gotAPIKey, gotAuth, gotVersion string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("x-api-key")
		gotAuth = r.Header.Get("Authorization")
		gotVersion = r.Header.Get("anthropic-version")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := gwConfig(t, upstream.URL, config.ProviderTypeAnthropic)
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("prov/m1", false)
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, route, "")

	if gotAPIKey != "key-a" && gotAPIKey != "key-b" {
		t.Fatalf("x-api-key = %q", gotAPIKey)
	}
	if gotAuth != "" {
		t.Fatalf("Authorization must be stripped, got %q", gotAuth)
	}
	if gotVersion != "2023-06-01" {
		t.Fatalf("anthropic-version = %q", gotVersion)
	}
}

func TestGateway_RetryOn401SwitchesKey(t *testing.T) {
	var calls atomic.Int32
	var secondAuth atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"bad key"}`))
			return
		}
		secondAuth.Store(r.Header.Get("Authorization"))
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := gwConfig(t, upstream.URL, config.ProviderTypeOpenAI)
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("prov/m1", false)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, route, "")

	if w.Code != 200 {
		t.Fatalf("expected retry to succeed, status = %d", w.Code)
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls.Load())
	}
	// Exactly one of the two credentials carries the auth cooldown; the
	// scheduler picked it, cooled it, and retried on the other.
	cooled := 0
	for _, m := range route.Members {
		if m.Status().AuthUntil.After(time.Now()) {
			cooled++
		}
	}
	if cooled != 1 {
		t.Fatalf("exactly one credential should be in auth cooldown, got %d", cooled)
	}
}

func TestGateway_AuthCooldownExhaustedReturnsLastUpstreamError(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid_api_key"}`))
	}))
	defer upstream.Close()

	cfg := gwConfig(t, upstream.URL, config.ProviderTypeOpenAI)
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("prov/m1", false)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, route, "")

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 passthrough of last upstream error", w.Code)
	}
	if !strings.Contains(w.Body.String(), "invalid_api_key") {
		t.Fatalf("body should carry the upstream error: %s", w.Body.String())
	}
	// 2 keys, maxRetries 2 -> up to 3 attempts, but only 2 credentials.
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2 (each key once)", calls.Load())
	}
}

func TestGateway_RequestScopedErrorNoRetryNoCooldown(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"bad parameter"}`))
	}))
	defer upstream.Close()

	cfg := gwConfig(t, upstream.URL, config.ProviderTypeOpenAI)
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("prov/m1", false)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, route, "")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", w.Code)
	}
	if calls.Load() != 1 {
		t.Fatalf("request-scoped errors must not retry, calls = %d", calls.Load())
	}
	// The credential that served the request records the error for
	// observability; the idle one stays clean.
	recorded := 0
	for _, c := range route.Members {
		if c.Status().LastError != "" {
			recorded++
		}
	}
	if recorded == 0 {
		t.Fatal("lastError should be recorded for observability")
	}
}

func TestGateway_RateLimitRespectsRetryAfter(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"rate limited"}`))
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := gwConfig(t, upstream.URL, config.ProviderTypeOpenAI)
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("prov/m1", false)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, route, "")

	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	// The rate-limited credential carries the Retry-After-extended cooldown.
	found := false
	for _, m := range route.Members {
		if until := m.Status().RateLimitUntil; until.After(time.Now().Add(100 * time.Second)) {
			found = true
		}
	}
	if !found {
		t.Fatal("rate-limit cooldown should honor Retry-After (120s)")
	}
}

func TestGateway_TransientErrorCooldownAndRetry(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte(`upstream hiccup`))
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := gwConfig(t, upstream.URL, config.ProviderTypeOpenAI)
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("prov/m1", false)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, route, "")

	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	cooled := 0
	for _, m := range route.Members {
		if m.Status().TransientUntil.After(time.Now()) {
			cooled++
		}
	}
	if cooled != 1 {
		t.Fatalf("502 should apply a transient cooldown to the failed credential, got %d", cooled)
	}
}

func TestGateway_StreamBootstrapRetry(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			// SSE headers sent but the stream errors before any data.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			panic(http.ErrAbortHandler) // kill the connection mid-stream
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte("data: {\"delta\":\"hi\"}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer upstream.Close()

	cfg := gwConfig(t, upstream.URL, config.ProviderTypeOpenAI)
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("prov/m1", false)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, route, "")

	// The bootstrap buffer saw a connection error before any byte reached
	// the client, so the retry must have succeeded with the full SSE body.
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "delta") {
		t.Fatalf("client should receive the retried stream: %q", w.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls.Load())
	}
}

func TestGateway_ConcurrentCapSkipsSaturatedKey(t *testing.T) {
	block := make(chan struct{})
	var started atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started.Add(1)
		<-block
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := gwConfig(t, upstream.URL, config.ProviderTypeOpenAI)
	// key-a capped at 1, key-b unlimited
	prov := cfg.Providers["prov"]
	prov.ApiKeys[0].MaxConcurrent = 1
	cfg.Providers["prov"] = prov
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("prov/m1", false)

	// First request saturates key-a.
	r1 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	w1 := httptest.NewRecorder()
	go g.ServeHTTP(w1, r1, route, "")
	time.Sleep(100 * time.Millisecond) // let it acquire key-a

	// Second request must skip key-a (saturated) and use key-b.
	r2 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	w2 := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		g.ServeHTTP(w2, r2, route, "")
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)

	if started.Load() != 2 {
		t.Fatalf("second request should reach upstream on key-b, started = %d", started.Load())
	}
	close(block)
	<-done
	<-time.After(50 * time.Millisecond)
}

func TestGateway_SessionAffinityBindsCredential(t *testing.T) {
	var mu atomic.Value
	mu.Store(map[string]int{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := mu.Load().(map[string]int)
		m[r.Header.Get("Authorization")]++
		mu.Store(m)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := gwConfig(t, upstream.URL, config.ProviderTypeOpenAI)
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("prov/m1", false)
	for i := 0; i < 6; i++ {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
		w := httptest.NewRecorder()
		g.ServeHTTP(w, req, route, "session-42")
		if w.Code != 200 {
			t.Fatalf("request %d failed: %d", i, w.Code)
		}
	}
	m := mu.Load().(map[string]int)
	if len(m) != 1 {
		t.Fatalf("sticky session must hit one credential, got %v", m)
	}
}

func TestGateway_BareNamePoolCrossProvider(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := config.Config{
		Providers: config.ProviderDictionaryConfig{
			"alpha": {
				Type: config.ProviderTypeOpenAI, Endpoint: upstream.URL,
				ApiKeys:  []config.ProviderKey{{Value: "k1", Priority: 10}},
				Models:   []config.ProviderModel{{Local: "shared", Remote: "alpha-shared"}},
				Timeouts: defaultTestTimeouts(),
			},
			"beta": {
				Type: config.ProviderTypeOpenAI, Endpoint: upstream.URL,
				ApiKeys:  []config.ProviderKey{{Value: "k2", Priority: 0}},
				Models:   []config.ProviderModel{{Local: "shared", Remote: "beta-shared"}},
				Timeouts: defaultTestTimeouts(),
			},
		},
	}
	for id, p := range cfg.Providers {
		cfg.Providers[id] = normalizeProvider(t, p)
	}

	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	if !g.Handles("shared") || !g.Handles("alpha/shared") || !g.Handles("beta/shared") {
		t.Fatal("both FQNs and the bare name must be handled")
	}

	// Highest priority provider wins the bare name.
	route := g.RouteFor("shared", false)
	if route == nil || route.ProviderID != "alpha" {
		t.Fatalf("bare name should route to the higher priority provider, got %+v", route)
	}

	// Excluding alpha's credential pushes the pick to beta.
	alpha := g.RouteByID("alpha/shared")
	alpha.Members[0].cooldown(time.Now(), CooldownAuth, time.Now().Add(time.Hour), "401")
	route = g.RouteFor("shared", false)
	if route == nil || route.ProviderID != "beta" {
		t.Fatalf("after alpha cooldown the bare name should route to beta, got %+v", route)
	}

	// With a local model claiming the bare name and both providers on the
	// default local-first policy, the pool yields to the local model.
	route = g.RouteFor("shared", true)
	if route != nil {
		t.Fatalf("local-first pool must yield to a local model, got %+v", route)
	}
}

func TestGateway_ProviderFirstBeatsLocal(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := config.Config{
		Providers: config.ProviderDictionaryConfig{
			"fast": {
				Type: config.ProviderTypeOpenAI, Endpoint: upstream.URL,
				ApiKeys:        []config.ProviderKey{{Value: "k"}},
				Models:         []config.ProviderModel{{Local: "hybrid", Remote: "remote-hybrid"}},
				BareNamePolicy: "provider-first",
				Timeouts:       defaultTestTimeouts(),
			},
		},
	}
	for id, p := range cfg.Providers {
		cfg.Providers[id] = normalizeProvider(t, p)
	}
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("hybrid", true)
	if route == nil {
		t.Fatal("provider-first route must win against a local model")
	}
	// local-first loses against the local model
	fast := cfg.Providers["fast"]
	fast.BareNamePolicy = "local-first"
	cfg.Providers["fast"] = fast
	g2, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g2.Shutdown()
	if g2.RouteFor("hybrid", true) != nil {
		t.Fatal("local-first route must lose against a local model")
	}
	if g2.RouteFor("hybrid", false) == nil {
		t.Fatal("local-first route must serve when no local model claims the name")
	}
}

func TestGateway_EnvUnresolvedCredentialSkipped(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	t.Setenv("GW_TEST_MISSING_KEY", "")
	cfg := config.Config{
		Providers: config.ProviderDictionaryConfig{
			"prov": {
				Type: config.ProviderTypeOpenAI, Endpoint: upstream.URL,
				ApiKeys: []config.ProviderKey{
					{ValueEnv: "GW_TEST_DEFINITELY_UNSET_12345"},
					{Value: "working-key"},
				},
				Models:   []config.ProviderModel{{Local: "m1"}},
				Timeouts: defaultTestTimeouts(),
			},
		},
	}
	for id, p := range cfg.Providers {
		cfg.Providers[id] = normalizeProvider(t, p)
	}
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("prov/m1", false)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, route, "")
	if w.Code != 200 {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
}

func TestGateway_DisabledProviderNotHandled(t *testing.T) {
	cfg := gwConfig(t, "http://192.0.2.1", config.ProviderTypeOpenAI)
	prov := cfg.Providers["prov"]
	prov.Disabled = true
	cfg.Providers["prov"] = prov
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()
	if g.Handles("prov/m1") || g.Handles("m1") {
		t.Fatal("disabled provider must not handle anything")
	}
}

func TestGateway_TransportCaching(t *testing.T) {
	g, err := New(gwConfig(t, "http://192.0.2.1", config.ProviderTypeOpenAI), testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()
	route := g.RouteFor("prov/m1", false)

	tr1, err := g.transports.get("", route)
	if err != nil {
		t.Fatal(err)
	}
	tr2, err := g.transports.get("", route)
	if err != nil {
		t.Fatal(err)
	}
	if tr1 != tr2 {
		t.Fatal("same proxy+timeouts must reuse the transport")
	}
	tr3, err := g.transports.get("http://127.0.0.1:1", route)
	if err != nil {
		t.Fatal(err)
	}
	if tr1 == tr3 {
		t.Fatal("different proxy must build a new transport")
	}
}

func TestGateway_ProxyDirectBuildsNoProxyTransport(t *testing.T) {
	tr, err := buildTransport("direct", transportTimeouts{connect: 5, keepAlive: 5, responseHeader: 10, tlsHandshake: 5, idleConn: 30})
	if err != nil {
		t.Fatal(err)
	}
	if tr.Proxy != nil {
		t.Fatal("direct must not set a proxy")
	}
	tr2, err := buildTransport("", transportTimeouts{connect: 5, keepAlive: 5, responseHeader: 10, tlsHandshake: 5, idleConn: 30})
	if err != nil {
		t.Fatal(err)
	}
	if tr2.Proxy == nil {
		t.Fatal("empty proxy must inherit from environment")
	}
}

func defaultTestTimeouts() config.TimeoutsConfig {
	return config.TimeoutsConfig{
		Connect: 5, KeepAlive: 5, ResponseHeader: 30, TLSHandshake: 5, ExpectContinue: 1, IdleConn: 30,
	}
}

var _ = fmt.Sprintf
