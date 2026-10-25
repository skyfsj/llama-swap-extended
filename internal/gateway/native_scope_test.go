package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/conversation"
	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

// pinnedScopeConfig exposes one bare model on two providers, the shape in which
// a normal request may rotate but a pinned scope must not.
func pinnedScopeConfig(t *testing.T, firstURL, secondURL string) config.Config {
	t.Helper()
	cfg := config.Config{
		Providers: config.ProviderDictionaryConfig{
			"first": {
				Type:     config.ProviderTypeOpenAIResponses,
				Endpoint: firstURL,
				ApiKeys:  []config.ProviderKey{{Value: "first-key"}},
				Models:   []config.ProviderModel{{Local: "shared", Remote: "first-upstream"}},
			},
			"second": {
				Type:     config.ProviderTypeOpenAIResponses,
				Endpoint: secondURL,
				// Two keys so the scope has to pin the credential, not just the
				// provider: a provider-owned job belongs to one account.
				ApiKeys: []config.ProviderKey{{Value: "second-key"}, {Value: "second-alt"}},
				Models:  []config.ProviderModel{{Local: "shared", Remote: "second-upstream"}},
			},
		},
	}
	for id, p := range cfg.Providers {
		cfg.Providers[id] = normalizeProvider(t, p)
	}
	return cfg
}

// A provider-owned job can only be read by the account that started it, so a
// pinned scope must reach exactly that provider and credential even when the
// pool would rotate elsewhere.
func TestGateway_ForwardScopeStaysPinned(t *testing.T) {
	var firstCalls, secondCalls atomic.Int32
	var secondAuth atomic.Value
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		w.Write([]byte(`{"id":"from-first","status":"completed","object":"response"}`))
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		secondAuth.Store(r.Header.Get("Authorization"))
		w.Write([]byte(`{"id":"resp_1","status":"completed","object":"response","output":[{"type":"message","content":[{"type":"output_text","text":"DONE"}]}]}`))
	}))
	defer second.Close()

	g, err := New(pinnedScopeConfig(t, first.URL, second.URL), testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	// Several requests ride the pool first: rotation is the normal path, and
	// the pinned call below must ignore whatever it settles on.
	for range 4 {
		req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"shared","input":"hi"}`))
		w := httptest.NewRecorder()
		g.ServeHTTPWithFailover(w, req, g.PlanRoutes("shared", false, "openai-responses", Session{}), Session{})
	}
	firstCalls.Store(0)
	secondCalls.Store(0)

	scope := conversation.Scope{
		Provider:   "second",
		Adapter:    "",
		Model:      "second-upstream",
		Credential: keyID("second", "second-alt"),
		Format:     dialect.OpenAIResponses,
	}
	req := httptest.NewRequest("GET", "/v1/responses/resp_1", nil)
	w := httptest.NewRecorder()
	if err := g.ForwardScope(w, req, scope, Session{}); err != nil {
		t.Fatalf("ForwardScope: %v", err)
	}

	if firstCalls.Load() != 0 || secondCalls.Load() != 1 {
		t.Fatalf("pinned call went first=%d second=%d, want 0/1", firstCalls.Load(), secondCalls.Load())
	}
	if got := w.Body.String(); !strings.Contains(got, "DONE") {
		t.Fatalf("body = %s", got)
	}
	if got, _ := secondAuth.Load().(string); got != "Bearer second-alt" {
		t.Fatalf("auth = %q, want the pinned credential", got)
	}
}

// A source that no longer exists in the configuration will never come back on
// its own, so polling must stop instead of retrying forever.
func TestGateway_ForwardScopeMissingSourceIsPermanent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"resp_1","status":"completed"}`))
	}))
	defer upstream.Close()

	g, err := New(pinnedScopeConfig(t, upstream.URL, upstream.URL), testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	scope := conversation.Scope{
		Provider:   "second",
		Model:      "second-upstream",
		Credential: "gone",
		Format:     dialect.OpenAIResponses,
	}
	err = g.ForwardScope(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/responses/resp_1", nil), scope, Session{})
	if err == nil {
		t.Fatal("a missing credential was accepted")
	}
	if IsTransientScopeError(err) {
		t.Fatalf("a missing credential was reported as transient: %v", err)
	}
}
