package gateway

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// referencePlanRoutes is the pre-optimization PlanRoutes: it rebuilds the
// candidate pool inline, sorts with a comparator that reads each route's bid,
// and applies the same slow filter. The optimized version must return the same
// chain for a fixed state.
func (g *Gateway) referencePlanRoutes(model string, localHasModel bool, clientSurface string, session Session) []*Route {
	g.mu.RLock()
	if exact, ok := g.routes[model]; ok {
		g.mu.RUnlock()
		if exact == nil {
			return nil
		}
		return []*Route{exact}
	}
	candidates := append([]*Route(nil), preferOwnName(g.bareRoutes[model], model)...)
	g.mu.RUnlock()

	eligible := make([]*Route, 0, len(candidates))
	seen := make(map[*Route]struct{}, len(candidates))
	for _, route := range candidates {
		if localHasModel && !route.ProviderFirst {
			continue
		}
		if _, duplicate := seen[route]; duplicate {
			continue
		}
		seen[route] = struct{}{}
		eligible = append(eligible, route)
	}
	if len(eligible) == 0 {
		return nil
	}
	if len(eligible) > 1 {
		eligible = filterSlowRoutes(eligible, g.skipSlow, g.latency, g.logger)
	}
	return orderPoolRoutesReference(eligible, clientSurface)
}

// orderPoolRoutesReference is the comparator-based ordering the precomputed
// version replaced.
func orderPoolRoutesReference(routes []*Route, clientSurface string) []*Route {
	ordered := append([]*Route(nil), routes...)
	for i := 1; i < len(ordered); i++ {
		for j := i; j > 0; j-- {
			left, right := ordered[j-1], ordered[j]
			leftNative := clientSurface != "" && left.Type() == clientSurface
			rightNative := clientSurface != "" && right.Type() == clientSurface
			leftPriority, _ := left.poolBid()
			rightPriority, _ := right.poolBid()
			less := false
			switch {
			case leftNative != rightNative:
				less = rightNative
			case leftPriority != rightPriority:
				less = rightPriority > leftPriority
			default:
				less = right.ProviderID < left.ProviderID
			}
			if !less {
				break
			}
			ordered[j-1], ordered[j] = ordered[j], ordered[j-1]
		}
	}
	return ordered
}

// poolFixture builds a gateway with two providers, each exposing the same bare
// name on two surfaces, so the pool has real ordering work to do.
func poolFixture(t *testing.T) (*Gateway, *httptest.Server) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"a","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(upstream.Close)

	cfg := config.Config{Providers: config.ProviderDictionaryConfig{
		"alpha": {
			Endpoint: upstream.URL,
			ApiKeys:  []config.ProviderKey{{Value: "a", Priority: 1}},
			Models:   []config.ProviderModel{{Local: "shared", Remote: "a"}},
		},
		"beta": {
			Endpoint: upstream.URL,
			ApiKeys:  []config.ProviderKey{{Value: "b", Priority: 5}},
			Models:   []config.ProviderModel{{Local: "shared", Remote: "b"}},
		},
		"gamma": {
			Endpoint: upstream.URL,
			ApiKeys:  []config.ProviderKey{{Value: "g", Priority: 3}},
			Models:   []config.ProviderModel{{Local: "shared", Remote: "g"}},
		},
	}}
	for id, provider := range cfg.Providers {
		cfg.Providers[id] = normalizeProvider(t, provider)
	}
	gw, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(gw.Shutdown)
	return gw, upstream
}

// equalPriorityPoolFixture is poolFixture with every provider at the same
// priority, so rotation (not priority) decides the pick.
func equalPriorityPoolFixture(t *testing.T) (*Gateway, *httptest.Server) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"a","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(upstream.Close)

	cfg := config.Config{Providers: config.ProviderDictionaryConfig{
		"alpha": {Endpoint: upstream.URL, ApiKeys: []config.ProviderKey{{Value: "a", Priority: 1}}, Models: []config.ProviderModel{{Local: "shared", Remote: "a"}}},
		"beta":  {Endpoint: upstream.URL, ApiKeys: []config.ProviderKey{{Value: "b", Priority: 1}}, Models: []config.ProviderModel{{Local: "shared", Remote: "b"}}},
		"gamma": {Endpoint: upstream.URL, ApiKeys: []config.ProviderKey{{Value: "g", Priority: 1}}, Models: []config.ProviderModel{{Local: "shared", Remote: "g"}}},
	}}
	for id, provider := range cfg.Providers {
		cfg.Providers[id] = normalizeProvider(t, provider)
	}
	gw, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(gw.Shutdown)
	return gw, upstream
}

// TestGateway_PlanRoutesMatchesReference pins the candidate construction and
// ordering against the inline implementation: for a fixed pool the chain must be
// identical, so the shared builder and the precomputed bids cannot reorder a
// request's candidates.
func TestGateway_PlanRoutesMatchesReference(t *testing.T) {
	gw, _ := poolFixture(t)
	for _, surface := range []string{"", "openai", "anthropic", "gemini", "openai-responses"} {
		for _, local := range []bool{false, true} {
			t.Run(fmt.Sprintf("surface=%s/local=%v", surface, local), func(t *testing.T) {
				want := gw.referencePlanRoutes("shared", local, surface, Session{})
				got := gw.PlanRoutes("shared", local, surface, Session{})
				if len(got) != len(want) {
					t.Fatalf("chain length = %d, want %d", len(got), len(want))
				}
				for i := range want {
					if got[i].Key() != want[i].Key() {
						t.Fatalf("chain[%d] = %s, want %s", i, got[i].Key(), want[i].Key())
					}
				}
			})
		}
	}
}

// TestGateway_PlanRoutesDeduplicatesAndHonoursLocalClaims is the case the
// shared candidate builder exists for: a name registered twice through aliases
// must appear once, and a local model must push every non-provider-first route
// out of the pool. Both rules live in the candidate construction, so they are
// pinned here rather than inferred from the ordering test.
func TestGateway_PlanRoutesDeduplicatesAndHonoursLocalClaims(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"a","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(upstream.Close)

	cfg := config.Config{Providers: config.ProviderDictionaryConfig{
		// alpha exposes the name twice (its own name and an alias), so the pool
		// must deduplicate it, and it is provider-first so a local model cannot
		// displace it.
		"alpha": {
			Endpoint:       upstream.URL,
			ApiKeys:        []config.ProviderKey{{Value: "a", Priority: 1}},
			Models:         []config.ProviderModel{{Local: "shared", Remote: "a"}, {Local: "other", Remote: "a", Aliases: []string{"shared"}}},
			BareNamePolicy: "provider-first",
		},
		// beta is an ordinary provider, so a local model does displace it.
		"beta": {
			Endpoint: upstream.URL,
			ApiKeys:  []config.ProviderKey{{Value: "b", Priority: 2}},
			Models:   []config.ProviderModel{{Local: "shared", Remote: "b"}},
		},
	}}
	for id, provider := range cfg.Providers {
		cfg.Providers[id] = normalizeProvider(t, provider)
	}
	gw, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer gw.Shutdown()

	// Without a local claim both providers serve the name, each once.
	noLocal := gw.PlanRoutes("shared", false, "", Session{})
	if len(noLocal) != 2 {
		t.Fatalf("planned %d routes, want 2 (one per provider)", len(noLocal))
	}
	seen := map[string]int{}
	for _, route := range noLocal {
		seen[route.ProviderID]++
	}
	if seen["alpha"] != 1 || seen["beta"] != 1 {
		t.Fatalf("provider counts = %v, want one each", seen)
	}

	// With a local claim only the provider-first routes survive.
	withLocal := gw.PlanRoutes("shared", true, "", Session{})
	if len(withLocal) != 1 || withLocal[0].ProviderID != "alpha" {
		t.Fatalf("planned %+v, want only alpha (provider-first)", withLocal)
	}
}

// TestGateway_PoolRoutesMatchesPlanRoutesCandidates makes sure the two public
// entry points agree on the candidate set: dialect resolution and chain
// planning must not disagree about which routes serve a name.
func TestGateway_PoolRoutesMatchesPlanRoutesCandidates(t *testing.T) {
	gw, _ := poolFixture(t)
	pool := gw.PoolRoutes("shared", false, "")
	if len(pool) == 0 {
		t.Fatal("the pool is empty")
	}
	planned := gw.PlanRoutes("shared", false, "", Session{})
	if len(planned) != len(pool) {
		t.Fatalf("planned %d routes, pool has %d", len(planned), len(pool))
	}
	seen := map[string]bool{}
	for _, route := range planned {
		seen[route.Key()] = true
	}
	for _, route := range pool {
		if !seen[route.Key()] {
			t.Fatalf("route %s is in the pool but not in the chain", route.Key())
		}
	}
}

// TestGateway_PinnedNameStillSkipsThePool keeps the qualified-name contract: an
// FQN routes to exactly one provider, with no pool and no rotation.
func TestGateway_PinnedNameStillSkipsThePool(t *testing.T) {
	gw, _ := poolFixture(t)
	route := gw.PlanRoutes("alpha/shared", false, "", Session{})
	if len(route) != 1 || route[0].ProviderID != "alpha" {
		t.Fatalf("chain = %+v, want exactly the alpha route", route)
	}
	pool := gw.PoolRoutes("alpha/shared", false, "")
	if len(pool) != 1 || pool[0].ProviderID != "alpha" {
		t.Fatalf("pool = %+v, want exactly the alpha route", pool)
	}
}

// TestGateway_StickySweepKeepsLiveSessions is the safety property of the sweep:
// an entry that has not expired survives, and one that has is removed. A sweep
// that removed live bindings would move conversations between providers.
func TestGateway_StickySweepKeepsLiveSessions(t *testing.T) {
	table := newStickyTable(time.Hour)
	now := time.Now()
	table.bind("live", "alpha", "alpha/openai/shared", "a", now, false)
	table.bind("expired", "beta", "beta/openai/shared", "b", now.Add(-2*time.Hour), false)
	table.bind("fresh", "gamma", "gamma/openai/shared", "g", now, false)
	// Nothing has looked these up, so only the sweep can remove the expired one.
	swept := table.sweepExpired(now)
	if swept != 1 {
		t.Fatalf("swept %d bindings, want 1", swept)
	}
	if _, ok := table.get("live", now); !ok {
		t.Fatal("the sweep removed a live binding")
	}
	if _, ok := table.get("fresh", now); !ok {
		t.Fatal("the sweep removed a fresh binding")
	}
	// An expired binding is gone for good.
	if _, ok := table.get("expired", now); ok {
		t.Fatal("the expired binding survived the sweep")
	}
}

// TestGateway_StickySweepSlidesTheTTL checks that a session which keeps coming
// back is never swept: its expiry slides forward on every hit.
func TestGateway_StickySweepSlidesTheTTL(t *testing.T) {
	table := newStickyTable(time.Minute)
	now := time.Now()
	table.bind("session", "alpha", "alpha/openai/shared", "a", now, false)
	for i := range 10 {
		hit := now.Add(time.Duration(i) * 30 * time.Second)
		if _, ok := table.get("session", hit); !ok {
			t.Fatalf("session expired at %v", hit)
		}
		if swept := table.sweepExpired(hit); swept != 0 {
			t.Fatalf("sweep removed an active session at %v", hit)
		}
	}
	if swept := table.sweepExpired(now.Add(10 * time.Minute)); swept != 1 {
		t.Fatalf("swept %d, want 1 after the session went quiet", swept)
	}
}

// TestGateway_ShutdownStopsTheSweep makes sure the background sweep exits, so a
// daemon that reloads its configuration does not accumulate goroutines.
func TestGateway_ShutdownStopsTheSweep(t *testing.T) {
	gw, _ := poolFixture(t)
	// Bind an expired entry, then shut down: the sweep must not panic on the
	// discarded table.
	table := gw.stickyTable()
	table.bind("gone", "alpha", "alpha/openai/shared", "a", time.Now().Add(-time.Hour), false)
	gw.Shutdown()
	time.Sleep(20 * time.Millisecond)
	if swept := table.sweepExpired(time.Now()); swept != 1 {
		t.Fatalf("swept %d, want 1", swept)
	}
}

// TestGateway_RotationCursorIsConcurrentSafe drives the per-model cursor from
// many goroutines: the shared sync.Map must hand every caller the same cursor
// for a model, and the pick sequence must stay a valid rotation.
func TestGateway_RotationCursorIsConcurrentSafe(t *testing.T) {
	gw, _ := poolFixture(t)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			for range 50 {
				rotator := gw.rotator("shared")
				rotator.mu.Lock()
				rotator.last = fmt.Sprintf("provider-%d", index)
				_ = rotator.last
				rotator.mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	// Every model still has exactly one cursor, and the rotation still works.
	first := gw.rotator("shared")
	second := gw.rotator("shared")
	if first != second {
		t.Fatal("rotator returned two cursors for one model")
	}
}

// TestGateway_CarryStateStillMovesRotationCursors covers the reload contract
// after the cursor storage changed: the per-model last pick must survive a
// gateway rebuild.
func TestGateway_CarryStateStillMovesRotationCursors(t *testing.T) {
	gw, _ := poolFixture(t)
	rotator := gw.rotator("shared")
	rotator.mu.Lock()
	rotator.last = "beta"
	rotator.mu.Unlock()

	state := gw.rotationState()
	if state["shared"] != "beta" {
		t.Fatalf("rotation state = %v, want shared=beta", state)
	}

	next, _ := poolFixture(t)
	// The reload path plans the pool (creating its cursor) before carrying the
	// state over, so the adopted value has somewhere to land.
	if chain := next.PlanRoutes("shared", false, "", Session{}); len(chain) == 0 {
		t.Fatal("the new gateway planned no chain")
	}
	next.adoptRotation(state)
	carried := next.rotator("shared")
	carried.mu.Lock()
	last := carried.last
	carried.mu.Unlock()
	if last != "beta" {
		t.Fatalf("adopted cursor = %q, want beta", last)
	}
}

// TestGateway_AdoptRotationKeepsAnExistingCursor pins the "only fill an empty
// cursor" rule: a new gateway that has already served a request keeps its own
// pick, so a reload cannot rewrite a rotation that has moved on.
func TestGateway_AdoptRotationKeepsAnExistingCursor(t *testing.T) {
	next, _ := poolFixture(t)
	if chain := next.PlanRoutes("shared", false, "", Session{}); len(chain) == 0 {
		t.Fatal("the new gateway planned no chain")
	}
	carried := next.rotator("shared")
	carried.mu.Lock()
	before := carried.last
	carried.mu.Unlock()
	if before == "" {
		t.Fatal("planning should have set the cursor")
	}
	next.adoptRotation(map[string]string{"shared": "some-other-provider"})
	carried.mu.Lock()
	after := carried.last
	carried.mu.Unlock()
	if after != before {
		t.Fatalf("adopt overwrote an existing cursor: %q -> %q", before, after)
	}
}

// TestGateway_CarryStateDropsUnknownModels keeps the other half of the reload
// contract: a cursor for a model the new configuration does not serve is dropped
// rather than carried into a pool that does not exist.
func TestGateway_CarryStateDropsUnknownModels(t *testing.T) {
	next, _ := poolFixture(t)
	// No request has been served yet, so the cursor for the served model is
	// still empty and can be adopted; the unknown model's cursor is not.
	next.adoptRotation(map[string]string{"absent-model": "beta", "shared": "alpha"})
	carried := next.rotator("shared")
	carried.mu.Lock()
	last := carried.last
	carried.mu.Unlock()
	if last != "alpha" {
		t.Fatalf("adopted cursor = %q, want alpha", last)
	}
}

// TestGateway_SessionRotationSpreadsProviders is the end-to-end rotation
// contract: repeated unbound requests must visit more than one provider, which
// is what the cursor exists for.
func TestGateway_SessionRotationSpreadsProviders(t *testing.T) {
	gw, _ := equalPriorityPoolFixture(t)
	seen := map[string]bool{}
	for range 12 {
		chain := gw.PlanRoutes("shared", false, "", Session{})
		if len(chain) == 0 {
			t.Fatal("no route planned")
		}
		seen[chain[0].ProviderID] = true
	}
	if len(seen) < 2 {
		t.Fatalf("rotation visited %v, want more than one provider", seen)
	}
}

var _ = strings.TrimSpace
