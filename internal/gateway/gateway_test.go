package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
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

	g.ServeHTTP(w, req, route, Session{})

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
	g.ServeHTTP(w, req, route, Session{})

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

func TestGateway_GeminiHeaders(t *testing.T) {
	var gotAPIKey, gotAuth, gotPath, gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("x-goog-api-key")
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Write([]byte(`{"candidates":[]}`))
	}))
	defer upstream.Close()

	cfg := gwConfig(t, upstream.URL, config.ProviderTypeGemini)
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("prov/m1", false)
	req := httptest.NewRequest("POST", "/v1beta/models/remote-m1:streamGenerateContent?alt=sse", strings.NewReader(`{"contents":[]}`))
	// A client-supplied Google key must never reach the upstream; the
	// credential replaces it.
	req.Header.Set("x-goog-api-key", "client-key")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, route, Session{})

	if gotAPIKey != "key-a" && gotAPIKey != "key-b" {
		t.Fatalf("x-goog-api-key = %q", gotAPIKey)
	}
	if gotAuth != "" {
		t.Fatalf("Authorization must be stripped, got %q", gotAuth)
	}
	if gotPath != "/v1beta/models/remote-m1:streamGenerateContent" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	if gotQuery != "alt=sse" {
		t.Fatalf("upstream query = %q", gotQuery)
	}
}

func TestGateway_SessionHeadersInjection(t *testing.T) {
	var mu sync.Mutex
	gotHeaders := map[string]string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotHeaders["session"] = r.Header.Get("X-Session-Id")
		gotHeaders["trace"] = r.Header.Get("X-Trace")
		mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := gwConfig(t, upstream.URL, config.ProviderTypeOpenAI)
	p := cfg.Providers["prov"]
	p.SessionHeaders = []string{"x-session-id", "X-Trace"}
	cfg.Providers["prov"] = normalizeProvider(t, p)

	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()
	route := g.RouteFor("prov/m1", false)

	// A session id is echoed under every configured header.
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("X-Session-Id", "client-original")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, route, Session{Hash: "h1", ID: "downstream-42"})
	mu.Lock()
	if gotHeaders["session"] != "downstream-42" {
		t.Fatalf("X-Session-Id = %q, want the derived session id", gotHeaders["session"])
	}
	if gotHeaders["trace"] != "downstream-42" {
		t.Fatalf("X-Trace = %q, want the derived session id", gotHeaders["trace"])
	}
	mu.Unlock()

	// Without a session id the client-supplied header value passes through
	// untouched (no injection, no clobbering).
	req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("X-Session-Id", "client-original")
	w = httptest.NewRecorder()
	g.ServeHTTP(w, req, route, Session{})
	mu.Lock()
	if gotHeaders["session"] != "client-original" {
		t.Fatalf("X-Session-Id = %q, want the client value preserved", gotHeaders["session"])
	}
	if gotHeaders["trace"] != "" {
		t.Fatalf("X-Trace = %q, want no injection", gotHeaders["trace"])
	}
	mu.Unlock()
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
	g.ServeHTTP(w, req, route, Session{})

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
	g.ServeHTTP(w, req, route, Session{})

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

func TestGateway_ExcludedStatusIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		w.Write([]byte(`{"error":"payload too large"}`))
	}))
	defer upstream.Close()

	// 413 is excluded from the default failover set: every credential would
	// refuse the same payload, so the error goes straight back.
	cfg := gwConfig(t, upstream.URL, config.ProviderTypeOpenAI)
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("prov/m1", false)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, route, Session{})

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", w.Code)
	}
	if calls.Load() != 1 {
		t.Fatalf("an excluded status must not retry, calls = %d", calls.Load())
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
	g.ServeHTTP(w, req, route, Session{})

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

// A transient failure spends the credential's retry budget instead of benching
// it: the same key is tried again, and success leaves no cooldown behind.
func TestGateway_TransientErrorRetriedOnSameCredential(t *testing.T) {
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
	// One key, so a retry can only be the same credential: this is the shape
	// the per-credential budget exists for.
	prov := cfg.Providers["prov"]
	prov.ApiKeys = []config.ProviderKey{{Value: "only-key"}}
	prov.MaxRetries = intPointerForTest(2)
	cfg.Providers["prov"] = normalizeProvider(t, prov)

	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("prov/m1", false)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, route, Session{})

	if w.Code != 200 {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2 (the first 502 is retried on the same key)", calls.Load())
	}
	for _, m := range route.Members {
		if until := m.Status().TransientUntil; !until.IsZero() {
			t.Fatalf("a retried-then-successful credential must not stay benched: %s", until)
		}
	}
}

// Once the per-credential budget is spent, the credential is benched with its
// class cooldown and the request continues on the next credential.
func TestGateway_ExhaustedCredentialBenchedThenNextCredential(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`upstream down`))
	}))
	defer upstream.Close()

	cfg := gwConfig(t, upstream.URL, config.ProviderTypeOpenAI)
	prov := cfg.Providers["prov"]
	prov.ApiKeys = []config.ProviderKey{{Value: "k1"}, {Value: "k2"}}
	prov.MaxRetries = intPointerForTest(1)
	cfg.Providers["prov"] = normalizeProvider(t, prov)

	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("prov/m1", false)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, route, Session{})

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", w.Code)
	}
	// maxRetries 1 means two attempts per credential, and both keys are tried.
	if calls.Load() != 4 {
		t.Fatalf("upstream calls = %d, want 4 (2 attempts on each of 2 keys)", calls.Load())
	}
	benched := 0
	for _, m := range route.Members {
		if m.Status().TransientUntil.After(time.Now()) {
			benched++
		}
	}
	if benched != 2 {
		t.Fatalf("both credentials should carry a transient cooldown after their budget, got %d", benched)
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
	g.ServeHTTP(w, req, route, Session{})

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
	go g.ServeHTTP(w1, r1, route, Session{})
	time.Sleep(100 * time.Millisecond) // let it acquire key-a

	// Second request must skip key-a (saturated) and use key-b.
	r2 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	w2 := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		g.ServeHTTP(w2, r2, route, Session{})
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
		g.ServeHTTP(w, req, route, Session{Hash: "session-42"})
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
	g.ServeHTTP(w, req, route, Session{})
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

func TestGateway_AdapterRoutesShareEndpointAndDialect(t *testing.T) {
	var gotPath, gotAuth, gotGoogKey, gotUA, gotSession atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		gotAuth.Store(r.Header.Get("Authorization"))
		gotGoogKey.Store(r.Header.Get("x-goog-api-key"))
		gotUA.Store(r.Header.Get("User-Agent"))
		gotSession.Store(r.Header.Get("x-opencode-session"))
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := config.Config{
		Providers: config.ProviderDictionaryConfig{
			"go": {
				Headers:        map[string]string{"User-Agent": "llama-swap-test"},
				SessionHeaders: []string{"x-opencode-session"},
				ApiKeys:        []config.ProviderKey{{Value: "provider-key"}},
				Adapters: []config.ProviderAdapter{
					{
						Name:     "Chat",
						Type:     config.ProviderTypeOpenAI,
						Endpoint: upstream.URL,
						Models:   []config.ProviderModel{{Local: "kimi-k3", Remote: "kimi-k3"}},
					},
					{
						Name:     "Google",
						Type:     config.ProviderTypeGemini,
						Endpoint: upstream.URL + "/google",
						PathMap:  map[string]string{"/v1beta/models/": "/v1/models/"},
						ApiKeys:  []config.ProviderKey{{Value: "adapter-key"}},
						Models:   []config.ProviderModel{{Local: "gemini-4-argon-contributor", Remote: "gemini-4-argon-contributor"}},
					},
				},
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

	chat := g.RouteFor("go/kimi-k3", false)
	if chat == nil {
		t.Fatal("chat route missing")
	}
	if chat.Adapter() != "Chat" || chat.Type() != config.ProviderTypeOpenAI {
		t.Fatalf("chat route adapter = %q type = %q", chat.Adapter(), chat.Type())
	}
	google := g.RouteFor("go/gemini-4-argon-contributor", false)
	if google == nil {
		t.Fatal("google route missing")
	}
	if google.Adapter() != "Google" || google.Type() != config.ProviderTypeGemini {
		t.Fatalf("google route adapter = %q type = %q", google.Adapter(), google.Type())
	}

	// Provider-level key on the chat adapter (no adapter override); the
	// gemini key must not leak onto the openai surface.
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, chat, Session{ID: "sess-77"})
	if auth, _ := gotAuth.Load().(string); auth != "Bearer provider-key" {
		t.Fatalf("chat upstream auth = %q", auth)
	}
	if key, _ := gotGoogKey.Load().(string); key != "" {
		t.Fatalf("openai adapter must not send x-goog-api-key, got %q", key)
	}
	if ua, _ := gotUA.Load().(string); ua != "llama-swap-test" {
		t.Fatalf("static header not applied: %q", ua)
	}
	if session, _ := gotSession.Load().(string); session != "sess-77" {
		t.Fatalf("session header = %q", session)
	}

	// Gemini adapter: own credential, path rewritten by pathMap, endpoint
	// prefix preserved.
	req = httptest.NewRequest("POST", "/v1beta/models/gemini-4-argon-contributor:generateContent", strings.NewReader(`{}`))
	w = httptest.NewRecorder()
	g.ServeHTTP(w, req, google, Session{})
	if key, _ := gotGoogKey.Load().(string); key != "adapter-key" {
		t.Fatalf("google adapter key = %q, want the adapter-level credential", key)
	}
	if auth, _ := gotAuth.Load().(string); auth != "" {
		t.Fatalf("google upstream must not carry Authorization, got %q", auth)
	}
	if path, _ := gotPath.Load().(string); path != "/google/v1/models/gemini-4-argon-contributor:generateContent" {
		t.Fatalf("google upstream path = %q", path)
	}
}

func TestGateway_OpenAIAuthWinsOverStaticHeaders(t *testing.T) {
	// Static headers cannot set credential headers (config validation rejects
	// them), but a static header sharing a name with a session header must not
	// survive: session headers are applied after.
	var gotSession string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSession = r.Header.Get("X-Session-Id")
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	cfg := gwConfig(t, upstream.URL, config.ProviderTypeOpenAI)
	p := cfg.Providers["prov"]
	p.Headers = map[string]string{"X-Session-Id": "static-value"}
	p.SessionHeaders = []string{"X-Session-Id"}
	cfg.Providers["prov"] = normalizeProvider(t, p)

	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("prov/m1", false)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, route, Session{ID: "session-wins"})
	if gotSession != "session-wins" {
		t.Fatalf("session header = %q, want the session id to win", gotSession)
	}
}

func TestApplyPathMapLongestPrefix(t *testing.T) {
	pathMap := map[string]string{
		"/v1beta/models/": "/v1/models/",
		"/v1beta/":        "/v2/",
	}
	cases := map[string]string{
		"/v1beta/models/x:generateContent": "/v1/models/x:generateContent",
		"/v1beta/other":                    "/v2/other",
		"/v1/chat/completions":             "/v1/chat/completions",
	}
	for input, want := range cases {
		if got := applyPathMap(input, pathMap); got != want {
			t.Errorf("applyPathMap(%q) = %q, want %q", input, got, want)
		}
	}
	if got := applyPathMap("/v1/chat/completions", nil); got != "/v1/chat/completions" {
		t.Errorf("nil pathMap changed the path: %q", got)
	}
}

func TestGateway_AdaptersShareCredentialState(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	cfg := config.Config{
		Providers: config.ProviderDictionaryConfig{
			"go": {
				ApiKeys: []config.ProviderKey{{Value: "shared-key"}},
				Adapters: []config.ProviderAdapter{
					{Name: "Chat", Type: config.ProviderTypeOpenAI, Endpoint: upstream.URL, Models: []config.ProviderModel{{Local: "m1", Remote: "m1"}}},
					{Name: "Google", Type: config.ProviderTypeGemini, Endpoint: upstream.URL, Models: []config.ProviderModel{{Local: "m2", Remote: "m2"}}},
				},
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

	chat := g.RouteFor("go/m1", false)
	google := g.RouteFor("go/m2", false)
	if chat == nil || google == nil {
		t.Fatal("routes missing")
	}
	chatCred := chat.Credentials()[0]
	googleCred := google.Credentials()[0]
	if chatCred != googleCred {
		t.Fatal("adapters of one provider must share the credential object")
	}

	// A cooldown (or quota park) recorded through one adapter must apply to
	// every adapter serving that key.
	chatCred.SetQuotaCooldown(time.Now().Add(time.Hour))
	if google.BestCredential() != nil {
		t.Fatal("a parked credential must be unavailable on every adapter")
	}
	chatCred.ClearQuotaCooldown()
	if google.BestCredential() == nil {
		t.Fatal("clearing the park must release every adapter")
	}
}

// poolConfig builds two providers serving the same bare model name, each on
// its own upstream, so failover can be observed across providers.
func poolConfig(t *testing.T, firstURL, secondURL string, crossProvider *bool, extra config.GatewayFailoverConfig) config.Config {
	t.Helper()
	cfg := config.Config{
		Gateway: config.GatewayConfig{Failover: extra},
		Providers: config.ProviderDictionaryConfig{
			"alpha": {
				Endpoint: firstURL,
				ApiKeys:  []config.ProviderKey{{Value: "alpha-key"}},
				Models:   []config.ProviderModel{{Local: "shared-model", Remote: "alpha-upstream-model"}},
			},
			"beta": {
				Endpoint: secondURL,
				ApiKeys:  []config.ProviderKey{{Value: "beta-key"}},
				Models:   []config.ProviderModel{{Local: "shared-model", Remote: "beta-upstream-model"}},
			},
		},
	}
	if crossProvider != nil {
		cfg.Gateway.Failover.CrossProvider = crossProvider
	}
	for id, p := range cfg.Providers {
		cfg.Providers[id] = normalizeProvider(t, p)
	}
	return cfg
}

func TestGateway_FailoverMovesToAnotherProviderOn4xx(t *testing.T) {
	var alphaCalls, betaCalls atomic.Int32
	var betaModel atomic.Value
	alpha := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		alphaCalls.Add(1)
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"model not available on this plan"}`))
	}))
	defer alpha.Close()
	beta := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		betaCalls.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if model, ok := body["model"].(string); ok {
			betaModel.Store(model)
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer beta.Close()

	g, err := New(poolConfig(t, alpha.URL, beta.URL, nil, config.GatewayFailoverConfig{}), testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	// A bare name resolves to one of the two providers; whichever it picks
	// first, the 404 must move the request to the other one within the same
	// request.
	route := g.RouteFor("shared-model", false)
	if route == nil {
		t.Fatal("route not found")
	}
	chain := g.FailoverRoutes("shared-model", false, route)
	if len(chain) != 2 {
		t.Fatalf("chain = %d routes, want 2", len(chain))
	}
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"shared-model","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTPWithFailover(w, req, chain, Session{})

	if w.Code != 200 {
		t.Fatalf("status = %d body = %s (alpha=%d beta=%d)", w.Code, w.Body.String(), alphaCalls.Load(), betaCalls.Load())
	}
	if alphaCalls.Load() != 1 || betaCalls.Load() != 1 {
		t.Fatalf("calls alpha=%d beta=%d, want one each", alphaCalls.Load(), betaCalls.Load())
	}
	// Each provider receives its own upstream model name.
	winner := "alpha-upstream-model"
	if route.ProviderID == "alpha" {
		winner = "beta-upstream-model"
	}
	if model, _ := betaModel.Load().(string); model != winner {
		t.Fatalf("upstream model = %q, want %q", model, winner)
	}
}

func TestGateway_FailoverOn5xxAndResetOnSuccess(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":"overloaded"}`))
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := gwConfig(t, upstream.URL, config.ProviderTypeOpenAI)
	cfg.Gateway.Failover.CrossProvider = boolPtr(false)
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("prov/m1", false)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"remote-m1","messages":[]}`))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, route, Session{})
	if w.Code != 200 {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

func TestGateway_CrossProviderDisabledStaysPinned(t *testing.T) {
	var alphaCalls, betaCalls atomic.Int32
	alpha := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		alphaCalls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer alpha.Close()
	beta := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		betaCalls.Add(1)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer beta.Close()

	cfg := poolConfig(t, alpha.URL, beta.URL, boolPtr(false), config.GatewayFailoverConfig{})
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("shared-model", false)
	chain := g.FailoverRoutes("shared-model", false, route)
	if len(chain) != 1 {
		t.Fatalf("crossProvider=false must pin the chain, got %d routes", len(chain))
	}
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"shared-model","messages":[]}`))
	w := httptest.NewRecorder()
	g.ServeHTTPWithFailover(w, req, chain, Session{})
	if betaCalls.Load() != 0 {
		t.Fatalf("disabled cross-provider failover still hit the sibling (%d calls)", betaCalls.Load())
	}
}

func TestGateway_QualifiedNameStaysPinnedToProvider(t *testing.T) {
	alpha := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer alpha.Close()
	beta := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer beta.Close()

	cfg := poolConfig(t, alpha.URL, beta.URL, nil, config.GatewayFailoverConfig{})
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("alpha/shared-model", false)
	if route == nil {
		t.Fatal("qualified route not found")
	}
	if chain := g.FailoverRoutes("alpha/shared-model", false, route); len(chain) != 1 {
		t.Fatalf("a provider-qualified name must stay pinned, chain = %d", len(chain))
	}
}

func TestGateway_TruncatedSuccessRetries(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			// Promise more bytes than the body delivers, then stop: the
			// client observes a truncated response.
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "4096")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"choices":`))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			// Return without writing the rest: the server closes the body.
			panic(http.ErrAbortHandler)
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
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"remote-m1"}`))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, route, Session{})
	if w.Code != 200 {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("body = %s", w.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2 (truncated response retried)", calls.Load())
	}
}

func TestGateway_EmptySuccessBodyRetries(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusOK) // 200 with no body at all
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
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"remote-m1"}`))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, route, Session{})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2 (empty response retried)", calls.Load())
	}
}

func boolPtr(v bool) *bool { return &v }

func intPointerForTest(v int) *int { return &v }

func TestGateway_PolicyForbiddenFailsOverWithoutCooldown(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":{"type":"RegionError","message":"This model is not available in your country"}}`))
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
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"remote-m1"}`))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, route, Session{})
	if w.Code != 200 {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2 (403 must fail over)", calls.Load())
	}
	// The key that saw the policy rejection must stay schedulable: a region
	// rule for one model says nothing about the rest of the subscription.
	for _, cred := range route.Members {
		if until := cred.Status().AuthUntil; !until.IsZero() {
			t.Fatalf("403 must not bench the credential, got auth cooldown until %s", until)
		}
	}
	if cred := route.BestCredential(); cred == nil {
		t.Fatal("a 403 policy rejection must leave the key available for other requests")
	}
}

func TestGateway_UnauthorizedStillBenchesCredential(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid api key"}`))
	}))
	defer upstream.Close()

	cfg := gwConfig(t, upstream.URL, config.ProviderTypeOpenAI)
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("prov/m1", false)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"remote-m1"}`))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, route, Session{})

	benched := 0
	for _, cred := range route.Members {
		if cred.Status().AuthUntil.After(time.Now()) {
			benched++
		}
	}
	if benched == 0 {
		t.Fatal("401 must still bench the credential")
	}
}

// deadProxy points at a closed port: any request routed through it fails, so
// it distinguishes "this egress was used" from "the request went direct".
const deadProxy = "http://127.0.0.1:9"

func TestGateway_ModelProxyOverridesCredentialProxy(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	// The credential's proxy is dead; the model says "direct", which must win.
	cfg := config.Config{
		Providers: config.ProviderDictionaryConfig{
			"prov": {
				Endpoint: upstream.URL,
				ApiKeys:  []config.ProviderKey{{Value: "k1", Proxy: deadProxy}},
				Models:   []config.ProviderModel{{Local: "m1", Remote: "m1", Proxy: config.ProxyDirect}},
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
	if route.EgressProxy() != config.ProxyDirect {
		t.Fatalf("route proxy = %q, want direct", route.EgressProxy())
	}
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m1"}`))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, route, Session{})
	if w.Code != 200 || calls.Load() != 1 {
		t.Fatalf("model-level direct must bypass the credential's dead proxy: status=%d calls=%d", w.Code, calls.Load())
	}
}

func TestGateway_ModelProxyAppliesWithoutCredentialProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := config.Config{
		Providers: config.ProviderDictionaryConfig{
			"prov": {
				Endpoint: upstream.URL,
				ApiKeys:  []config.ProviderKey{{Value: "k1"}},
				Models:   []config.ProviderModel{{Local: "m1", Remote: "m1", Proxy: deadProxy}},
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
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m1"}`))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, route, Session{})
	if w.Code == 200 {
		t.Fatal("a dead model-level proxy must be used (request cannot succeed direct)")
	}
}

func TestGateway_AdapterProxyAppliesAndModelBeatsAdapter(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := config.Config{
		Providers: config.ProviderDictionaryConfig{
			"prov": {
				ApiKeys: []config.ProviderKey{{Value: "k1"}},
				Adapters: []config.ProviderAdapter{
					{
						Name:     "DeadEgress",
						Type:     config.ProviderTypeOpenAI,
						Endpoint: upstream.URL,
						Proxy:    deadProxy, // inherits down to its models
						Models:   []config.ProviderModel{{Local: "inherits", Remote: "inherits"}},
					},
					{
						Name:     "Explicit",
						Type:     config.ProviderTypeOpenAI,
						Endpoint: upstream.URL,
						Proxy:    deadProxy,
						Models: []config.ProviderModel{
							{Local: "overrides", Remote: "overrides", Proxy: config.ProxyDirect},
						},
					},
				},
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

	// The adapter-level dead proxy reaches its models.
	inherits := g.RouteFor("prov/inherits", false)
	overridesRoute := g.RouteFor("prov/overrides", false)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"inherits"}`))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, inherits, Session{})
	if w.Code == 200 {
		t.Fatal("adapter-level proxy must apply to its models")
	}

	// The failed attempt above cooled the shared credential (transport error);
	// release it so the second half tests the proxy decision, not the cooldown.
	for _, cred := range inherits.Credentials() {
		cred.ClearCooldowns()
	}
	for _, cred := range overridesRoute.Credentials() {
		cred.ClearCooldowns()
	}

	// A model-level proxy beats the adapter's.
	overrides := overridesRoute
	if overrides.EgressProxy() != config.ProxyDirect {
		t.Fatalf("model proxy = %q, want direct over the adapter", overrides.EgressProxy())
	}
	req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"overrides"}`))
	w = httptest.NewRecorder()
	g.ServeHTTP(w, req, overrides, Session{})
	if w.Code != 200 {
		t.Fatalf("model-level direct must beat the adapter's dead proxy: status=%d", w.Code)
	}
}

func TestGateway_ModelPrefixExposesPrefixedName(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := gwConfig(t, upstream.URL, config.ProviderTypeOpenAI)
	prov := cfg.Providers["prov"]
	prov.ModelPrefix = "go-"
	cfg.Providers["prov"] = prov
	for id, p := range cfg.Providers {
		cfg.Providers[id] = normalizeProvider(t, p)
	}
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	// The prefixed name routes and carries the exposed name.
	route := g.RouteFor("go-m1", false)
	if route == nil {
		t.Fatal("prefixed name must route")
	}
	if route.ExposedName != "go-m1" || route.ModelID != "m1" {
		t.Fatalf("route = %+v", route)
	}
	// The legacy provider/model form still resolves to the same route.
	if legacy := g.RouteFor("prov/m1", false); legacy != route {
		t.Fatal("legacy provider/model name must keep routing")
	}
	if chain := g.FailoverRoutes("go-m1", false, route); len(chain) != 1 {
		t.Fatalf("chain = %d, want the primary only (no sibling providers)", len(chain))
	}
}

func TestGateway_PrefixedPoolFailsOverAcrossProviders(t *testing.T) {
	var alphaCalls, betaCalls atomic.Int32
	alpha := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		alphaCalls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer alpha.Close()
	beta := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		betaCalls.Add(1)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer beta.Close()

	cfg := poolConfig(t, alpha.URL, beta.URL, nil, config.GatewayFailoverConfig{})
	for id, p := range cfg.Providers {
		p.ModelPrefix = "shared-"
		cfg.Providers[id] = normalizeProvider(t, p)
	}
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("shared-shared-model", false)
	if route == nil {
		t.Fatal("prefixed pool name must route")
	}
	chain := g.FailoverRoutes("shared-shared-model", false, route)
	if len(chain) != 2 {
		t.Fatalf("chain = %d routes, want the two providers sharing the prefixed name", len(chain))
	}
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"shared-shared-model"}`))
	w := httptest.NewRecorder()
	g.ServeHTTPWithFailover(w, req, chain, Session{})
	if w.Code != 200 {
		t.Fatalf("status = %d (404 must fail over to the sibling provider)", w.Code)
	}
	// The model name the upstream saw is the route's remote name.
	if alphaCalls.Load()+betaCalls.Load() != 2 {
		t.Fatalf("calls alpha=%d beta=%d, want one each", alphaCalls.Load(), betaCalls.Load())
	}
}

func TestGateway_FirstUnauthorizedBenchesBriefly(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"Go requires a credential"}`))
	}))
	defer upstream.Close()

	cfg := gwConfig(t, upstream.URL, config.ProviderTypeOpenAI)
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("prov/m1", false)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"remote-m1"}`))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, route, Session{})

	// A transient upstream 401 must not park the credential for the configured
	// auth cooldown (10m by default): the next request should be able to try it
	// again within the probe window.
	soon := time.Now().Add(DefaultAuthProbeCooldown + time.Second)
	for _, cred := range route.Members {
		until := cred.Status().AuthUntil
		if until.IsZero() {
			continue
		}
		if until.After(time.Now().Add(cc1authTimeout())) {
			t.Fatalf("first 401 benched the credential too long: until %s", until)
		}
		if !until.Before(soon.Add(time.Minute)) {
			t.Fatalf("probe cooldown looks wrong: until %s", until)
		}
	}
}

// cc1authTimeout is the default configured auth cooldown minus a margin, so the
// assertion fails only when the probe path was skipped.
func cc1authTimeout() time.Duration {
	return DefaultAuthCooldown - time.Minute
}

func TestGateway_RepeatedUnauthorizedEscalates(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()

	cfg := gwConfig(t, upstream.URL, config.ProviderTypeOpenAI)
	// One credential, so every attempt hits the same credential and the second
	// 401 arrives inside the escalation window.
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
	// First 401: probe cooldown.
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"remote-m1"}`))
	g.ServeHTTP(httptest.NewRecorder(), req, route, Session{})
	first := cred.Status().AuthUntil
	if first.IsZero() || first.After(time.Now().Add(DefaultAuthProbeCooldown+5*time.Second)) {
		t.Fatalf("first 401 cooldown = %s, want a short probe", first)
	}

	// A second 401 while the failure is still recent escalates to the
	// configured auth cooldown. ClearCooldowns only lifts the timestamps: the
	// recorded auth failure stays, like an operator retrying right after a 401.
	cred.ClearCooldowns()
	req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"remote-m1"}`))
	g.ServeHTTP(httptest.NewRecorder(), req, route, Session{})
	second := cred.Status().AuthUntil
	if second.Before(time.Now().Add(DefaultAuthCooldown - time.Minute)) {
		t.Fatalf("repeated 401 must escalate to the configured cooldown, got %s", second)
	}
}

func TestGateway_PaymentRequiredKeepsFullCooldown(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
	}))
	defer upstream.Close()

	cfg := gwConfig(t, upstream.URL, config.ProviderTypeOpenAI)
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("prov/m1", false)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"remote-m1"}`))
	g.ServeHTTP(httptest.NewRecorder(), req, route, Session{})

	benched := false
	for _, cred := range route.Members {
		if cred.Status().AuthUntil.After(time.Now().Add(DefaultAuthCooldown - time.Minute)) {
			benched = true
		}
	}
	if !benched {
		t.Fatal("402 (payment required) must keep the full auth cooldown")
	}
}

func TestGateway_ProviderCooldownDisabled(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid key"}`))
	}))
	defer upstream.Close()

	cfg := gwConfig(t, upstream.URL, config.ProviderTypeOpenAI)
	prov := cfg.Providers["prov"]
	prov.Cooldown = config.ProviderCooldownConfig{Policy: config.ProviderCooldownDisabled}
	cfg.Providers["prov"] = normalizeProvider(t, prov)

	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("prov/m1", false)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"remote-m1"}`))
	g.ServeHTTP(httptest.NewRecorder(), req, route, Session{})

	// Cooldown disabled: the failure is recorded, but nothing is benched, so a
	// follow-up request still reaches the upstream.
	for _, cred := range route.Members {
		st := cred.Status()
		if !st.AuthUntil.IsZero() || !st.RateLimitUntil.IsZero() || !st.TransientUntil.IsZero() {
			t.Fatalf("cooldown disabled but credential %s is benched: %+v", cred.ID, st)
		}
	}
	if route.BestCredential() == nil {
		t.Fatal("cooldown disabled must leave the credential schedulable")
	}
	before := calls.Load()
	req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"remote-m1"}`))
	g.ServeHTTP(httptest.NewRecorder(), req, route, Session{})
	if calls.Load() <= before {
		t.Fatal("a follow-up request must reach the upstream while cooldowns are disabled")
	}
	if last := route.Members[0].Status().LastError; last == "" {
		t.Fatal("the failure must still be recorded for observability")
	}
}

func TestGateway_ProviderCooldownDurationsOverride(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	cfg := gwConfig(t, upstream.URL, config.ProviderTypeOpenAI)
	prov := cfg.Providers["prov"]
	prov.Cooldown = config.ProviderCooldownConfig{
		Policy:    config.ProviderCooldownEnabled,
		Transient: "45s",
	}
	cfg.Providers["prov"] = normalizeProvider(t, prov)

	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("prov/m1", false)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"remote-m1"}`))
	g.ServeHTTP(httptest.NewRecorder(), req, route, Session{})

	wait := time.Duration(0)
	for _, cred := range route.Members {
		if until := cred.Status().TransientUntil; until.After(time.Now()) {
			if d := time.Until(until); d > wait {
				wait = d
			}
		}
	}
	if wait < 40*time.Second || wait > 50*time.Second {
		t.Fatalf("provider transient cooldown = %s, want ~45s", wait)
	}
}

func TestGateway_CompressedErrorSnippetIsDecoded(t *testing.T) {
	payload := `{"error":{"message":"Insufficient Balance","type":"unknown_error","code":"invalid_request_error"}}`
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write([]byte(payload)); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	writer.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusBadRequest)
		w.Write(compressed.Bytes())
	}))
	defer upstream.Close()

	cfg := gwConfig(t, upstream.URL, config.ProviderTypeOpenAI)
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("prov/m1", false)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"remote-m1"}`))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, route, Session{})

	body := w.Body.String()
	if !json.Valid([]byte(body)) {
		t.Fatalf("error envelope is not valid JSON (raw compressed bytes leaked): %q", body)
	}
	if !strings.Contains(body, "Insufficient Balance") {
		t.Fatalf("decoded upstream message missing from the envelope: %s", body)
	}
	if strings.Contains(body, "\\u001f") || strings.Contains(body, "\x1f\x8b") {
		t.Fatalf("compressed bytes leaked into the envelope: %q", body)
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestSwaputil_PaymentRequiredEnvelope(t *testing.T) {
	envelope := swaputil.NewErrorEnvelope(http.StatusPaymentRequired, "Insufficient Balance", "")
	if envelope.Error.Type != swaputil.ErrorTypeInsufficientQuota || envelope.Error.Code != "insufficient_quota" {
		t.Fatalf("402 envelope = %+v", envelope.Error)
	}
}

func TestGateway_ModelAliasesRouteAndHideFromListing(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := gwConfig(t, upstream.URL, config.ProviderTypeOpenAI)
	prov := cfg.Providers["prov"]
	prov.Models = []config.ProviderModel{{
		Local:   "short-name",
		Remote:  "remote-m1",
		Aliases: []string{"alias-one", "alias-two"},
	}}
	cfg.Providers["prov"] = normalizeProvider(t, prov)

	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	primary := g.RouteFor("short-name", false)
	if primary == nil {
		t.Fatal("the primary name must route")
	}
	for _, alias := range []string{"alias-one", "alias-two", "prov/alias-one"} {
		route := g.RouteFor(alias, false)
		if route != primary {
			t.Fatalf("%q must resolve to the same route", alias)
		}
	}
	// The listing shows one entry per configured model entry.
	if ids := g.ModelIDs(); len(ids) != 1 {
		t.Fatalf("ModelIDs = %v, want only the primary name", ids)
	}
	// Failover treats an alias request as belonging to this pool.
	if chain := g.FailoverRoutes("alias-one", false, primary); len(chain) != 1 {
		t.Fatalf("chain = %d routes", len(chain))
	}
}

// A chunked streaming response must reach the client as the upstream produces
// it: buffering the head until a full read buffer is available would hold back
// the first token of every provider answer.
func TestGateway_StreamingSuccessIsForwardedWithoutBuffering(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }
	firstFrame := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		// One small frame, then a long pause before the rest: nothing here
		// fills the gateway's read buffer.
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		close(firstFrame)
		<-release
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstream.Close()
	defer releaseUpstream()

	cfg := gwConfig(t, upstream.URL, config.ProviderTypeOpenAI)
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("prov/m1", false)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"remote-m1","stream":true}`))

	var sawFirst atomic.Bool
	done := make(chan struct{})
	client := &streamSpyWriter{
		onWrite: func(data []byte) {
			if strings.Contains(string(data), `"content":"a"`) {
				sawFirst.Store(true)
			}
		},
		onDone: done,
	}
	go func() {
		g.ServeHTTP(client, req, route, Session{})
		close(done)
	}()

	select {
	case <-firstFrame:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never produced the first frame")
	}
	// The gateway must forward the frame while the upstream is still holding
	// the rest of the stream open.
	deadline := time.Now().Add(3 * time.Second)
	for !sawFirst.Load() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !sawFirst.Load() {
		t.Fatal("the first streaming frame was not forwarded before the upstream finished")
	}
	releaseUpstream()
	<-done
}

// streamSpyWriter records writes and optionally calls back on each one.
type streamSpyWriter struct {
	mu      sync.Mutex
	status  int
	body    bytes.Buffer
	onWrite func([]byte)
	onDone  chan struct{}
}

func (w *streamSpyWriter) Header() http.Header { return http.Header{} }

func (w *streamSpyWriter) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = status
	}
}

func (w *streamSpyWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.body.Write(data)
	w.mu.Unlock()
	if w.onWrite != nil {
		w.onWrite(data)
	}
	return len(data), nil
}

func (w *streamSpyWriter) Flush() {}

// A name claimed by a model and by another model's alias resolves to the model:
// the alias is a second way to reach the same upstream, never a replacement for
// the real entry.
func TestGateway_ModelNameBeatsAlias(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := config.Config{
		Providers: config.ProviderDictionaryConfig{
			"prov": {
				Type:     config.ProviderTypeOpenAI,
				Endpoint: upstream.URL,
				ApiKeys:  []config.ProviderKey{{Value: "key-a"}},
				Models: []config.ProviderModel{
					{Local: "flash", Remote: "flash-upstream", Aliases: []string{"pro"}},
					{Local: "pro", Remote: "pro-upstream"},
				},
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

	if route := g.RouteFor("pro", false); route == nil || route.ModelID != "pro" {
		t.Fatalf("route for %q = %+v, want the model that declares it", "pro", route)
	}
	if route := g.RouteFor("prov/pro", false); route == nil || route.ModelID != "pro" {
		t.Fatalf("qualified route for %q = %+v, want the declared model", "prov/pro", route)
	}
	// The alias keeps working for its own model.
	if route := g.RouteFor("flash", false); route == nil || route.ModelID != "flash" {
		t.Fatalf("route for %q = %+v, want flash", "flash", route)
	}
	// Both declarations stay listed exactly once each.
	ids := g.ModelIDs()
	if len(ids) != 2 {
		t.Fatalf("ModelIDs = %v, want one entry per model", ids)
	}
}

// Every upstream read must be flushed to the client: without the flush, an SSE
// answer that is smaller than net/http's write buffer arrives as one lump when
// the handler returns.
func TestGateway_StreamingFramesArriveIncrementally(t *testing.T) {
	secondFrameReleased := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: first\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		// Hold the stream open until the test has seen the first frame: the
		// second frame must reach the client even though nothing else is
		// coming yet.
		<-secondFrameReleased
		_, _ = w.Write([]byte("data: second\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
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
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"remote-m1","stream":true}`))

	frames := make(chan string, 8)
	var sawFirst sync.WaitGroup
	sawFirst.Add(1)
	client := &frameFlushingWriter{
		onFrame: func(line string) {
			frames <- line
			if strings.Contains(line, "first") {
				sawFirst.Done()
			}
		},
	}
	done := make(chan struct{})
	go func() {
		g.ServeHTTP(client, req, route, Session{})
		close(done)
	}()

	waitFrame := func(contains string) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case line := <-frames:
				if strings.Contains(line, contains) {
					return
				}
			case <-deadline:
				t.Fatalf("frame %q never reached the client", contains)
			}
		}
	}
	waitFrame("first")
	close(secondFrameReleased)
	waitFrame("second")
	waitFrame("[DONE]")
	<-done
}

// frameFlushingWriter mirrors net/http's buffered response writer: written
// bytes stay buffered until Flush, which is what makes a missing flush visible
// to the test.
type frameFlushingWriter struct {
	mu        sync.Mutex
	pending   strings.Builder
	delivered strings.Builder
	onFrame   func(string)
}

func (w *frameFlushingWriter) Header() http.Header { return http.Header{} }

func (w *frameFlushingWriter) WriteHeader(int) {}

func (w *frameFlushingWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending.Write(data)
	return len(data), nil
}

func (w *frameFlushingWriter) Flush() {
	w.mu.Lock()
	flushed := w.pending.String()
	w.pending.Reset()
	w.delivered.WriteString(flushed)
	body := w.delivered.String()
	var frames []string
	for {
		idx := strings.Index(body, "\n\n")
		if idx < 0 {
			break
		}
		frames = append(frames, body[:idx])
		body = body[idx+2:]
	}
	w.delivered.Reset()
	w.delivered.WriteString(body)
	w.mu.Unlock()
	if w.onFrame != nil {
		for _, frame := range frames {
			w.onFrame(frame)
		}
	}
}

// 供应商归属：请求经过网关后，活动记录必须能说出它是被哪个供应商应答的。
// 一个模型名可能同时挂在多个供应商上（裸名池）、一个供应商也可能有多个
// API 表面，只有在网关内部才知道最终走的是哪一条，所以这里验证它确实把
// 该信息发布到了请求诊断里。
func TestGateway_PublishesServingProvider(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
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

	// 诊断 holder 由最外层请求日志中间件创建，这里直接放一个并在请求后读取。
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"remote-m1","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(swaputil.WithRequestDiagnostics(req.Context()))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, route, Session{})
	if w.Code != 200 {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}

	diagnostics := swaputil.RequestDiagnosticsFrom(req.Context())
	if diagnostics == nil {
		t.Fatal("诊断 holder 丢失")
	}
	if diagnostics.Provider != "prov" {
		t.Fatalf("Provider = %q，期望 %q", diagnostics.Provider, "prov")
	}
	// 单适配器供应商没有适配器名，界面此时只显示供应商 ID。
	if diagnostics.Adapter != "" {
		t.Fatalf("Adapter = %q，单适配器供应商应为空", diagnostics.Adapter)
	}
}

// 上游以错误状态应答（凭证级失败，重试耗尽）时也要记录供应商：
// 运维排查"某个供应商开始报错"时，失败的正是要看的那一行。
func TestGateway_PublishesProviderOnExhaustedFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"message":"Insufficient Balance"}}`))
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

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"remote-m1","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(swaputil.WithRequestDiagnostics(req.Context()))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, route, Session{})
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d，期望 402", w.Code)
	}

	diagnostics := swaputil.RequestDiagnosticsFrom(req.Context())
	if diagnostics == nil || diagnostics.Provider != "prov" {
		t.Fatalf("失败请求未记录供应商：%+v", diagnostics)
	}
}

// 多表面转发时，网关把一个失败候选交还给调用方继续试下一个之前，不得写入
// 归属：holder 是"首个发布者胜出"，先失败的候选一旦抢先发布，真正答出
// 200 的候选就再也写不进去，活动记录会把失败者当成服务器。
// 这是"记录显示 opencode-go、实际由 DeepSeek 官方答出"的回归守卫。
func TestGateway_HandedOffFailureLeavesRecordForTheAnsweringRoute(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"message":"Insufficient Balance"}}`))
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

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"remote-m1","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(swaputil.WithRequestDiagnostics(req.Context()))
	w := httptest.NewRecorder()
	opts := AttemptOptions{StopOnRequestScoped: true, StopOnUnavailable: true}
	outcome := g.ServeHTTPAttempt(w, req, []*Route{route}, Session{}, opts)
	if outcome.Committed {
		t.Fatal("交还给下一候选的失败不得提交响应")
	}
	if outcome.Status != http.StatusPaymentRequired {
		t.Fatalf("outcome.Status = %d，期望 402", outcome.Status)
	}

	diagnostics := swaputil.RequestDiagnosticsFrom(req.Context())
	if diagnostics == nil {
		t.Fatal("诊断 holder 丢失")
	}
	if diagnostics.Provider != "" {
		t.Fatalf("Provider = %q，交还给下一候选的失败不得写入归属", diagnostics.Provider)
	}
}

// 整条链的凭据都已冷却时同样不得写入归属：调用方会拿这条链最近一次上游
// 答复去试别的候选，归属属于最终答出来的那条链，而不是冷却中的这条。
func TestGateway_BenchedChainLeavesRecordForTheAnsweringRoute(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"message":"Insufficient Balance"}}`))
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

	newReq := func() *http.Request {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"remote-m1","messages":[]}`))
		req.Header.Set("Content-Type", "application/json")
		return req.WithContext(swaputil.WithRequestDiagnostics(req.Context()))
	}

	// 第一条请求把每个 key 都打成 402：auth 失败触发默认 10 分钟冷却，
	// 此后整条链不可调度。
	first := newReq()
	firstRecorder := httptest.NewRecorder()
	g.ServeHTTP(firstRecorder, first, route, Session{})
	if firstRecorder.Code != http.StatusPaymentRequired {
		t.Fatalf("第一条请求 status = %d，期望 402", firstRecorder.Code)
	}

	second := newReq()
	secondRecorder := httptest.NewRecorder()
	opts := AttemptOptions{StopOnRequestScoped: true, StopOnUnavailable: true}
	outcome := g.ServeHTTPAttempt(secondRecorder, second, []*Route{route}, Session{}, opts)
	if outcome.Committed {
		t.Fatal("冷却链交还给下一候选时不得提交响应")
	}
	if outcome.Status != http.StatusPaymentRequired {
		t.Fatalf("outcome.Status = %d，期望链上最近一次上游答复 402", outcome.Status)
	}

	diagnostics := swaputil.RequestDiagnosticsFrom(second.Context())
	if diagnostics == nil {
		t.Fatal("诊断 holder 丢失")
	}
	if diagnostics.Provider != "" {
		t.Fatalf("Provider = %q，冷却链的交还不得写入归属", diagnostics.Provider)
	}
}

func TestGateway_ReportsAttemptBeforeTheAnswer(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hold the response so the assertions below run while the attempt is
		// still in flight.
		<-release
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

	// Unblock the upstream even when an assertion fails: httptest.Server.Close
	// waits for outstanding handlers, so a held request would hang the test.
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()

	reported := make(chan [2]string, 4)
	ctx := swaputil.WithRequestDiagnostics(context.Background())
	ctx = swaputil.WithInferenceRouteReporter(ctx, func(provider, adapter string) {
		reported <- [2]string{provider, adapter}
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"remote-m1","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		g.ServeHTTP(w, req, route, Session{})
	}()

	select {
	case got := <-reported:
		if got[0] != "prov" {
			t.Fatalf("reported provider = %q, want prov", got[0])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("attempt was not reported before the upstream answered")
	}

	// The live report must not attribute the record: a failover still has to
	// be able to name the route that produced the answer.
	if diagnostics := swaputil.RequestDiagnosticsFrom(ctx); diagnostics.Provider != "" {
		t.Fatalf("record provider = %q, want the record left for the answering route", diagnostics.Provider)
	}

	unblock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("gateway did not finish after the upstream answered")
	}
	if diagnostics := swaputil.RequestDiagnosticsFrom(ctx); diagnostics.Provider != "prov" {
		t.Fatalf("record provider after the answer = %q, want prov", diagnostics.Provider)
	}
}

// A bare-name pool with equal priorities must actually rotate between its
// providers across requests. Building a scheduler per request used to reset
// the rotation cursor, so every request picked the same sorted-first provider
// and the other providers never saw traffic.
func TestGateway_PoolRotatesBetweenProvidersAcrossRequests(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := poolConfig(t, upstream.URL, upstream.URL, nil, config.GatewayFailoverConfig{})
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	counts := map[string]int{}
	for i := 0; i < 20; i++ {
		routes := g.PlanRoutes("shared-model", false, "", Session{})
		if len(routes) != 2 {
			t.Fatalf("pass %d: routes = %d, want both providers in the chain", i, len(routes))
		}
		counts[routes[0].ProviderID]++
	}
	if counts["alpha"] == 0 || counts["beta"] == 0 {
		t.Fatalf("equal-priority pool did not rotate: %v", counts)
	}
	if counts["alpha"] < 6 || counts["beta"] < 6 {
		t.Fatalf("rotation is lopsided: %v", counts)
	}
}

// Priority still wins over rotation: the lower tier only appears when the high
// tier has nothing usable left, and the chain keeps it as a failover target.
func TestGateway_PoolPriorityTierBeatsRotation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := poolConfig(t, upstream.URL, upstream.URL, nil, config.GatewayFailoverConfig{})
	alpha := cfg.Providers["alpha"]
	alpha.ApiKeys = []config.ProviderKey{{Value: "alpha-key", Priority: 10}}
	cfg.Providers["alpha"] = normalizeProvider(t, alpha)

	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	for i := 0; i < 10; i++ {
		routes := g.PlanRoutes("shared-model", false, "", Session{})
		if len(routes) != 2 {
			t.Fatalf("routes = %d, want the high tier plus its failover", len(routes))
		}
		if routes[0].ProviderID != "alpha" {
			t.Fatalf("pass %d: head = %s, want the higher priority provider", i, routes[0].ProviderID)
		}
		if routes[1].ProviderID != "beta" {
			t.Fatalf("pass %d: tail = %s, want the other provider as failover", i, routes[1].ProviderID)
		}
	}
}

// Session affinity is per conversation *and* per upstream surface: one
// conversation keeps hitting the same provider/adapter, while different
// conversations still spread across the pool.
func TestGateway_SessionAffinitySticksToSurfacePerSession(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]map[string]int{} // session -> provider
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		session := r.Header.Get("x-session-id")
		if hits[session] == nil {
			hits[session] = map[string]int{}
		}
		hits[session][r.Host]++
		mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := poolConfig(t, upstream.URL, upstream.URL, nil, config.GatewayFailoverConfig{})
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	// Each session runs several turns. The first turn picks whatever the pool
	// rotates to; every later turn must follow that binding.
	bound := map[string]string{}
	for turn := 0; turn < 4; turn++ {
		for _, session := range []string{"s1", "s2", "s3", "s4", "s5", "s6"} {
			routes := g.PlanRoutes("shared-model", false, "", Session{Hash: session, Model: "shared-model", ID: session})
			if len(routes) == 0 {
				t.Fatalf("no routes for %s", session)
			}
			if want, ok := bound[session]; ok && routes[0].ProviderID != want {
				t.Fatalf("%s turn %d moved from %s to %s", session, turn, want, routes[0].ProviderID)
			}
			bound[session] = routes[0].ProviderID

			// Serve it so the binding is recorded and the rotation advances.
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
			w := httptest.NewRecorder()
			g.ServeHTTP(w, req, routes[0], Session{Hash: session, Model: "shared-model", ID: session})
			if w.Code != 200 {
				t.Fatalf("%s: status = %d", session, w.Code)
			}
		}
	}
	distinct := map[string]struct{}{}
	for _, provider := range bound {
		distinct[provider] = struct{}{}
	}
	if len(distinct) < 2 {
		t.Fatalf("all sessions stuck to one provider (%v): affinity must not disable rotation", bound)
	}
}

// The two adapters of one provider are one upstream account: a bare-name pool
// must rotate between *providers*, not give a multi-adapter provider a bigger
// share just because it exposes the model on more surfaces.
func TestGateway_PoolRotationCountsProvidersNotAdapters(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := config.Config{
		Providers: config.ProviderDictionaryConfig{
			"multi": {
				ApiKeys: []config.ProviderKey{{Value: "m1"}},
				Adapters: []config.ProviderAdapter{
					{Name: "Chat", Type: config.ProviderTypeOpenAI, Endpoint: upstream.URL,
						Models: []config.ProviderModel{{Local: "shared-model", Remote: "shared-model"}}},
					{Name: "Messages", Type: config.ProviderTypeAnthropic, Endpoint: upstream.URL,
						Models: []config.ProviderModel{{Local: "shared-model", Remote: "shared-model"}}},
				},
			},
			"single": {
				Type: config.ProviderTypeOpenAI, Endpoint: upstream.URL,
				ApiKeys: []config.ProviderKey{{Value: "s1"}},
				Models:  []config.ProviderModel{{Local: "shared-model", Remote: "shared-model"}},
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

	multiHeads, singleHeads := 0, 0
	for i := 0; i < 20; i++ {
		routes := g.PlanRoutes("shared-model", false, "openai", Session{})
		if len(routes) == 0 {
			t.Fatal("no routes")
		}
		switch routes[0].ProviderID {
		case "multi":
			multiHeads++
		case "single":
			singleHeads++
		}
	}
	if multiHeads == 0 || singleHeads == 0 {
		t.Fatalf("rotation must visit both providers: multi=%d single=%d", multiHeads, singleHeads)
	}
	if multiHeads > 14 {
		t.Fatalf("multi-adapter provider took %d/20 heads: adapters must not each count as a pool member", multiHeads)
	}
}

// A provider-level "direct" must beat a process-wide proxy: an operator who
// marks one provider as direct is saying that upstream must never go through
// the shared egress, which is the toggle the providers page exposes.
func TestGateway_ProviderProxyDirectBeatsGlobalProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", deadProxy)
	t.Setenv("HTTPS_PROXY", deadProxy)

	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := config.Config{
		Providers: config.ProviderDictionaryConfig{
			"direct-provider": {
				Type:     config.ProviderTypeOpenAI,
				Endpoint: upstream.URL,
				Proxy:    config.ProxyDirect,
				ApiKeys:  []config.ProviderKey{{Value: "k"}},
				Models:   []config.ProviderModel{{Local: "m1", Remote: "m1"}},
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

	route := g.RouteFor("direct-provider/m1", false)
	if route == nil {
		t.Fatal("route missing")
	}
	// A provider-level proxy is resolved onto its credentials (the provider is
	// one level above the key), so the credential is where the route reads it.
	var resolved string
	if len(route.Members) > 0 {
		resolved = route.egressProxy(route.Members[0])
	}
	if resolved != config.ProxyDirect {
		t.Fatalf("resolved egress proxy = %q, want %q", resolved, config.ProxyDirect)
	}
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m1"}`))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, route, Session{})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s (a direct provider must not use the dead proxy)", w.Code, w.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

// TestGateway_WholePoolExhaustedReportsRealUpstreamError pins what a client
// sees when every credential of a bare-name pool is cooling down.
//
// This is the operator-visible shape of a provider outage: the last upstream
// answer is reported verbatim (status and body), not a synthetic 502, because
// the request did reach a real provider that refused it. The synthetic error is
// reserved for a pool that never heard from any upstream at all — every key
// disabled, or an unset env var — where there is genuinely nothing to report.
//
// Both branches are asserted because they differ only in whether the chain has
// a recorded upstream answer, which is exactly the state an operator needs to
// distinguish "my keys are bad" from "my keys are unconfigured".
func TestGateway_WholePoolExhaustedReportsRealUpstreamError(t *testing.T) {
	alpha := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":{"code":"upstream_timeout","message":"upstream timed out: rotation stopped"}}`))
	}))
	defer alpha.Close()
	beta := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":{"code":"upstream_timeout","message":"upstream timed out: rotation stopped"}}`))
	}))
	defer beta.Close()

	g, err := New(poolConfig(t, alpha.URL, beta.URL, nil, config.GatewayFailoverConfig{}), testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("shared-model", false)
	if route == nil {
		t.Fatal("route not found")
	}
	chain := g.FailoverRoutes("shared-model", false, route)
	if len(chain) != 2 {
		t.Fatalf("chain = %d routes, want both providers", len(chain))
	}

	// One request exhausts both providers' keys: each answers 503, so each is
	// benched with a transient cooldown and the chain has a recorded answer.
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"shared-model","messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	g.ServeHTTPWithFailover(w, req, chain, Session{})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body = %s, want the upstream's own 503", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "upstream_timeout") {
		t.Fatalf("body = %s, want the upstream's own error, not a synthetic one", w.Body.String())
	}

	// The next request finds the whole pool benched. It must report the same
	// real upstream answer rather than "no available credential".
	req2 := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"shared-model","messages":[{"role":"user","content":"hi again"}]}`))
	w2 := httptest.NewRecorder()
	g.ServeHTTPWithFailover(w2, req2, chain, Session{})
	if w2.Code != http.StatusServiceUnavailable {
		t.Fatalf("exhausted-pool status = %d body = %s, want the recorded upstream 503",
			w2.Code, w2.Body.String())
	}
	if !strings.Contains(w2.Body.String(), "upstream_timeout") {
		t.Fatalf("exhausted-pool body = %s, want the recorded upstream error", w2.Body.String())
	}
	if strings.Contains(w2.Body.String(), "no available credential") {
		t.Fatalf("exhausted-pool body = %s: a pool that heard from upstream must not report a synthetic error",
			w2.Body.String())
	}
}

// TestGateway_AllCoolingDownTriesShortestBench 覆盖"全冷却"的兜底：
//
// 一个池子的每个 key 都在冷却时，请求不该直接失败。冷却是对上游状态的预测，
// 不是对凭据的判决：最快要解冻的那把 key 就是当下最好的猜测，试一次要么成功，
// 要么拿回上游真实的错误——都比合成的 "all keys are cooling down" 有用。
//
// 这个形态在生产上真实出现过：突发流量把两家供应商的 transient 冷却同时打满，
// 客户端看到的是一条与真实原因无关的 502。
func TestGateway_AllCoolingDownTriesShortestBench(t *testing.T) {
	var (
		mu    sync.Mutex
		calls []string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := gwConfig(t, upstream.URL, config.ProviderTypeOpenAI)
	prov := cfg.Providers["prov"]
	prov.ApiKeys = []config.ProviderKey{
		{Value: "long-bench"},
		{Value: "short-bench"},
	}
	prov.MaxRetries = intPointerForTest(1)
	cfg.Providers["prov"] = normalizeProvider(t, prov)

	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("prov/m1", false)
	if route == nil {
		t.Fatal("route missing")
	}
	creds := route.Credentials()
	if len(creds) != 2 {
		t.Fatalf("credentials = %d, want 2", len(creds))
	}

	// Bench both keys, one for far longer than the other, as a burst of
	// transient failures does.
	now := time.Now()
	CooldownForTest(creds[0], CooldownTransient, now.Add(4*time.Minute), "long")
	CooldownForTest(creds[1], CooldownTransient, now.Add(2*time.Second), "short")

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"remote-m1","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, route, Session{})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s: a benched pool must be tried, not failed", w.Code, w.Body.String())
	}

	// It must be the key whose bench ends soonest, not the first in order.
	mu.Lock()
	defer mu.Unlock()
	if len(calls) == 0 {
		t.Fatal("upstream was never dialed")
	}
	if !strings.Contains(calls[0], "short-bench") {
		t.Fatalf("first attempt used %q, want the shortest-remaining bench", calls[0])
	}
}

// TestGateway_AllCoolingDownNeverRetriesTriedCredential 守住"本次请求没试过的"
// 这个条件：兜底不能把已经失败过的 key 再来一遍，否则一次请求会把同一把坏 key
// 打好几次。
func TestGateway_AllCoolingDownNeverRetriesTriedCredential(t *testing.T) {
	var (
		mu        sync.Mutex
		callCount int
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"down"}`))
	}))
	defer upstream.Close()

	cfg := gwConfig(t, upstream.URL, config.ProviderTypeOpenAI)
	prov := cfg.Providers["prov"]
	prov.ApiKeys = []config.ProviderKey{{Value: "k1"}, {Value: "k2"}}
	prov.MaxRetries = intPointerForTest(5)
	cfg.Providers["prov"] = normalizeProvider(t, prov)

	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("prov/m1", false)
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"remote-m1","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	g.ServeHTTP(httptest.NewRecorder(), req, route, Session{})

	// Two keys with maxRetries 5: each key gets its own budget (6 attempts),
	// and the fallback must never exceed that by cycling back to a key whose
	// budget is already spent.
	mu.Lock()
	defer mu.Unlock()
	want := 2 * (5 + 1)
	if callCount > want {
		t.Fatalf("upstream called %d times, want at most %d (one budget per key)", callCount, want)
	}
}

// TestGateway_FallbackSkipsAuthBenchedCredentials 守住例外：401/402 是对凭据的
// 判决，不是对上游状态的预测。用被拒绝的 key 重试不可能成功，还会给账号招来
// 风控，所以兜底绝不碰它。
func TestGateway_FallbackSkipsAuthBenchedCredentials(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer upstream.Close()

	cfg := gwConfig(t, upstream.URL, config.ProviderTypeOpenAI)
	prov := cfg.Providers["prov"]
	prov.ApiKeys = []config.ProviderKey{{Value: "k1"}}
	prov.MaxRetries = intPointerForTest(1)
	cfg.Providers["prov"] = normalizeProvider(t, prov)

	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("prov/m1", false)
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"remote-m1","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	g.ServeHTTP(httptest.NewRecorder(), req, route, Session{})
	first := calls.Load()

	// The key is now auth-benched. A second request must be answered from the
	// recorded upstream state without dialing again.
	req = httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"remote-m1","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, route, Session{})

	if extra := calls.Load() - first; extra != 0 {
		t.Fatalf("auth-benched key was retried %d time(s); a 401 is a verdict, not a wait", extra)
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want the recorded upstream 401", w.Code)
	}
}

// 禁用的密钥不参与调度，但额度卡片仍然要能读到它的订阅：ProviderCredentials
// 给出全量凭据（含停用），Route.Members 仍然只有可调度的那些。
func TestGateway_ProviderCredentialsIncludeDisabledKeys(t *testing.T) {
	cfg := gwConfig(t, "http://192.0.2.1", config.ProviderTypeOpenAI)
	prov := cfg.Providers["prov"]
	prov.ApiKeys = []config.ProviderKey{
		{Value: "key-live", Weight: 1},
		{Value: "key-parked", Weight: 1, Disabled: true},
	}
	cfg.Providers["prov"] = normalizeProvider(t, prov)

	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	credentials := g.ProviderCredentials("prov")
	if len(credentials) != 2 {
		t.Fatalf("credentials = %d, want every configured key", len(credentials))
	}
	disabled := 0
	for _, cred := range credentials {
		if cred.Disabled {
			disabled++
		}
	}
	if disabled != 1 {
		t.Fatalf("disabled credentials = %d, want the configured one", disabled)
	}

	route := g.RouteFor("prov/m1", false)
	if route == nil {
		t.Fatal("route missing")
	}
	if len(route.Credentials()) != 1 {
		t.Fatalf("route members = %d, a disabled key must never be schedulable", len(route.Credentials()))
	}
	if got := route.Credentials()[0].ID; got != CredentialID("prov", config.ProviderKey{Value: "key-live"}) {
		t.Fatalf("route member = %s, want the live key", got)
	}
}

// 整体停用的供应商没有任何路由，但它的密钥仍然要能被额度卡片找到。
func TestGateway_DisabledProviderStillExposesCredentials(t *testing.T) {
	cfg := gwConfig(t, "http://192.0.2.1", config.ProviderTypeOpenAI)
	prov := cfg.Providers["prov"]
	prov.Disabled = true
	cfg.Providers["prov"] = normalizeProvider(t, prov)

	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	if g.Handles("prov/m1") {
		t.Fatal("disabled provider must not route")
	}
	if got := len(g.ProviderCredentials("prov")); got != 2 {
		t.Fatalf("credentials = %d, want both configured keys", got)
	}
}
