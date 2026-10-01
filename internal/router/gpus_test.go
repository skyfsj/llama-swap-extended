package router

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
)

// newTestGpus builds a Gpus router from supplied processes, bypassing
// NewGpus's call to process.New.
func newTestGpus(t *testing.T, cards map[string][]string, processes map[string]process.Process) *Gpus {
	t.Helper()
	models := make(map[string]config.ModelConfig, len(processes))
	for model := range processes {
		models[model] = config.ModelConfig{}
	}
	gpus := &config.GpusConfig{Cards: cards}
	if err := config.ValidateGpus(gpus, models); err != nil {
		t.Fatalf("ValidateGpus: %v", err)
	}
	conf := config.Config{
		HealthCheckTimeout: 5,
		Routing: config.RoutingConfig{
			Router: config.RouterConfig{
				Use:      "gpus",
				Settings: config.RouterSettings{Gpus: gpus},
			},
		},
	}

	logger := logmon.NewWriter(io.Discard)
	swapper := &gpusSwapper{gpus: gpus, logger: logger}
	base, err := newBaseRouter("gpus", conf, processes, logger, swapper)
	if err != nil {
		t.Fatalf("newBaseRouter: %v", err)
	}
	base.testProcessed = make(chan struct{}, 64)
	r := &Gpus{baseRouter: base}
	go base.run()
	t.Cleanup(func() {
		if !r.shuttingDown.Load() {
			_ = r.Shutdown(time.Second)
		}
	})
	return r
}

// TestGpus_SwapEvictsConflicting verifies that loading a model evicts every
// running model that shares a card with it.
func TestGpus_SwapEvictsConflicting(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	go a.Run(0)

	b := newFakeProcess("b")
	b.autoReady = true

	// a and b share card 0, so loading b must evict a.
	r := newTestGpus(t, map[string][]string{"0": {"a", "b"}}, map[string]process.Process{"a": a, "b": b})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, newRequest("b"))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if got := a.stopCalls.Load(); got != 1 {
		t.Errorf("a.stopCalls=%d want 1", got)
	}
	if got := b.runCalls.Load(); got != 1 {
		t.Errorf("b.runCalls=%d want 1", got)
	}
}

// TestGpus_CoexistOnDisjointCards verifies that a model on another card does
// not evict the running model.
func TestGpus_CoexistOnDisjointCards(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	go a.Run(0)

	b := newFakeProcess("b")
	b.autoReady = true

	r := newTestGpus(t, map[string][]string{"0": {"a"}, "1": {"b"}}, map[string]process.Process{"a": a, "b": b})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, newRequest("b"))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if got := a.stopCalls.Load(); got != 0 {
		t.Errorf("a.stopCalls=%d want 0 (b is on a different card)", got)
	}
	if got := b.runCalls.Load(); got != 1 {
		t.Errorf("b.runCalls=%d want 1", got)
	}
}

// TestGpus_MultiCardModelEvictsAllSharedCards verifies that a model listed on
// several cards evicts the occupants of all of them.
func TestGpus_MultiCardModelEvictsAllSharedCards(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	go a.Run(0)

	b := newFakeProcess("b")
	b.markReady()
	go b.Run(0)

	c := newFakeProcess("c")
	c.autoReady = true

	// c occupies both cards, so loading c must evict both a and b.
	r := newTestGpus(t, map[string][]string{"0": {"a", "c"}, "1": {"b", "c"}}, map[string]process.Process{"a": a, "b": b, "c": c})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, newRequest("c"))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if got := a.stopCalls.Load(); got != 1 {
		t.Errorf("a.stopCalls=%d want 1", got)
	}
	if got := b.stopCalls.Load(); got != 1 {
		t.Errorf("b.stopCalls=%d want 1", got)
	}
	if got := c.runCalls.Load(); got != 1 {
		t.Errorf("c.runCalls=%d want 1", got)
	}
}

// TestGpus_UnlistedModelCoexists verifies that a model not listed on any card
// evicts nothing and runs alongside everything.
func TestGpus_UnlistedModelCoexists(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	go a.Run(0)

	b := newFakeProcess("b")
	b.autoReady = true

	// b appears on no card, so it never shares a card with a.
	r := newTestGpus(t, map[string][]string{"0": {"a"}}, map[string]process.Process{"a": a, "b": b})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, newRequest("b"))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if got := a.stopCalls.Load(); got != 0 {
		t.Errorf("a.stopCalls=%d want 0 (b occupies no card)", got)
	}
	if got := b.runCalls.Load(); got != 1 {
		t.Errorf("b.runCalls=%d want 1", got)
	}
}

// TestGpusSwapper_EvictionForUnit verifies the pure eviction decision.
func TestGpusSwapper_EvictionForUnit(t *testing.T) {
	gpus := &config.GpusConfig{Cards: map[string][]string{
		"0": {"a", "b"},
		"1": {"c"},
		"2": {"b", "c"},
	}}
	s := &gpusSwapper{gpus: gpus, logger: logmon.NewWriter(io.Discard)}

	// b shares card 0 with a and card 2 with c; x shares nothing.
	got := s.EvictionFor("b", []string{"a", "c", "x"})
	want := []string{"a", "c"}
	if len(got) != len(want) {
		t.Fatalf("EvictionFor = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("EvictionFor = %v, want %v", got, want)
		}
	}
	// x occupies no card: evicts nothing.
	if got := s.EvictionFor("x", []string{"a", "c"}); len(got) != 0 {
		t.Errorf("EvictionFor(x) = %v, want empty", got)
	}
	// The same (target, running) pair is served from the decision cache.
	if got := s.EvictionFor("b", []string{"a", "c", "x"}); len(got) != len(want) {
		t.Errorf("cached EvictionFor = %v, want %v", got, want)
	}
	// A ready target is part of the running set and shares its own card; the
	// target must never evict itself, or every request to an already-loaded
	// model would stop and restart its own backend.
	if got := s.EvictionFor("a", []string{"a", "x"}); len(got) != 0 {
		t.Errorf("EvictionFor(a, running includes a) = %v, want empty (no self-eviction)", got)
	}
	if got := s.EvictionFor("b", []string{"a", "b", "c"}); len(got) != 2 {
		t.Errorf("EvictionFor(b, running includes b) = %v, want exactly [a c]", got)
	}
}
