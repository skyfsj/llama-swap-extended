package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// TestGateway_CarryStateSurvivesReload pins the reload contract: a
// configuration rebuild must not move a live conversation to another provider.
// The new instance starts with fresh cooldowns and fresh credentials, which is
// correct, but which upstream a session was using is not something the config
// file decides.
func TestGateway_CarryStateSurvivesReload(t *testing.T) {
	var firstCalls, secondCalls atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer second.Close()

	// Both providers serve the bare name "shared", so an unbound session would
	// rotate between them. The first is bound on the request below.
	cfg := config.Config{Providers: config.ProviderDictionaryConfig{
		"alpha": {Endpoint: first.URL, ApiKeys: []config.ProviderKey{{Value: "a"}}, Models: []config.ProviderModel{{Local: "shared", Remote: "a"}}},
		"beta":  {Endpoint: second.URL, ApiKeys: []config.ProviderKey{{Value: "b"}}, Models: []config.ProviderModel{{Local: "shared", Remote: "b"}}},
	}}
	for id, p := range cfg.Providers {
		cfg.Providers[id] = normalizeProvider(t, p)
	}
	previous, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer previous.Shutdown()

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	if route := previous.RouteFor("shared", false); route != nil {
		previous.ServeHTTP(httptest.NewRecorder(), req, route, Session{Hash: "session-42"})
	}
	if firstCalls.Load() == 0 {
		t.Fatal("the first provider did not serve the session")
	}
	firstCalls.Store(0)
	secondCalls.Store(0)

	// The reload: same configuration, brand-new Gateway.
	next, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer next.Shutdown()
	if adopted := next.CarryState(previous); adopted != 1 {
		t.Fatalf("carried %d bindings, want 1", adopted)
	}

	for range 3 {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
		if route := next.RouteFor("shared", false); route != nil {
			next.ServeHTTP(httptest.NewRecorder(), req, route, Session{Hash: "session-42"})
		}
	}
	if secondCalls.Load() != 0 || firstCalls.Load() != 3 {
		t.Fatalf("after reload first=%d second=%d, want 3/0 (the session kept its provider)", firstCalls.Load(), secondCalls.Load())
	}
}

// A reload that removes a surface must not carry a binding for it: the route is
// gone, so nothing can honor the binding and it would only be dropped on first
// use.
func TestGateway_CarryStateDropsRemovedRoutes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	oldCfg := config.Config{Providers: config.ProviderDictionaryConfig{
		"alpha": {Endpoint: upstream.URL, ApiKeys: []config.ProviderKey{{Value: "a"}}, Models: []config.ProviderModel{{Local: "shared", Remote: "a"}}},
	}}
	for id, p := range oldCfg.Providers {
		oldCfg.Providers[id] = normalizeProvider(t, p)
	}
	previous, err := New(oldCfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer previous.Shutdown()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	if route := previous.RouteFor("shared", false); route != nil {
		previous.ServeHTTP(httptest.NewRecorder(), req, route, Session{Hash: "session-42"})
	}

	// The new configuration renames the model, so the old binding's route key
	// no longer exists.
	newCfg := config.Config{Providers: config.ProviderDictionaryConfig{
		"alpha": {Endpoint: upstream.URL, ApiKeys: []config.ProviderKey{{Value: "a"}}, Models: []config.ProviderModel{{Local: "renamed", Remote: "a"}}},
	}}
	for id, p := range newCfg.Providers {
		newCfg.Providers[id] = normalizeProvider(t, p)
	}
	next, err := New(newCfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer next.Shutdown()
	if adopted := next.CarryState(previous); adopted != 0 {
		t.Fatalf("carried %d bindings for a removed route, want 0", adopted)
	}
}
