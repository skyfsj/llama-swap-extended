package server

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/store"
)

// backfillTestRates is a distinctive rate set so a wrong partition (charging
// cached tokens at the input rate, or failing to clamp the -1 sentinel) changes
// the number visibly.
const (
	backfillInputRate  = 2.0
	backfillOutputRate = 4.0
	backfillReadRate   = 0.5
	backfillWriteRate  = 1.0
	backfillThinkRate  = 3.0
)

// seedPricedModel registers one catalog row so the named model becomes
// priceable through the bare-name lookup.
func seedPricedModel(t *testing.T, s *Server, provider, model string) {
	t.Helper()
	if err := s.store.UpsertPrice(context.Background(), store.Price{
		Provider: provider, Model: model,
		Input: backfillInputRate, Output: backfillOutputRate,
		CacheRead: backfillReadRate, CacheWrite: backfillWriteRate,
		Reasoning: backfillThinkRate,
	}); err != nil {
		t.Fatalf("UpsertPrice %s/%s: %v", provider, model, err)
	}
}

// seedUnpricedRow inserts one activity row with no cost and returns its id.
func seedUnpricedRow(t *testing.T, s *Server, model, reqPath string, tokens store.TokenMetrics, created, reasoning int) int {
	t.Helper()
	entry, err := s.store.InsertActivity(context.Background(), store.ActivityLogEntry{
		Timestamp:           time.Now().Add(-time.Hour),
		Model:               model,
		KeyID:               "key-1",
		ReqPath:             reqPath,
		Tokens:              tokens,
		CacheCreationTokens: created,
		ReasoningTokens:     reasoning,
	})
	if err != nil {
		t.Fatalf("InsertActivity: %v", err)
	}
	return entry.ID
}

// modelCost reads the aggregated persisted cost of one model, exactly as the
// stats endpoint does.
func modelCost(t *testing.T, s *Server, model string) (float64, bool) {
	t.Helper()
	stats, err := s.store.ActivityStats(context.Background(), store.ActivityStatsQuery{Model: model})
	if err != nil {
		t.Fatalf("ActivityStats(%s): %v", model, err)
	}
	return stats.EstimatedCost, stats.CostEstimated
}

func costsEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestServer_CostBackfill prices the historical rows the record path left at
// zero, using the same formula and the same lookup as a live request: the
// OpenAI path subtracts cached tokens from the input charge, the Anthropic
// path does not, and the -1 "unknown" sentinel never becomes a negative
// charge. Each shape gets its own model so the stats readback isolates it.
func TestServer_CostBackfill(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })
	ctx := context.Background()

	for _, model := range []string{"cf-openai", "cf-anthropic", "cf-sentinel"} {
		seedPricedModel(t, s, "deepseek", model)
	}
	s.metrics.configurePricing(s.store, config.Config{})

	// OpenAI shape: input is the total, cached sits inside it.
	seedUnpricedRow(t, s, "cf-openai", "/v1/chat/completions",
		store.TokenMetrics{InputTokens: 1000, OutputTokens: 200, CachedTokens: 400}, 100, 50)
	// Anthropic shape: input is already the uncached portion.
	seedUnpricedRow(t, s, "cf-anthropic", "/v1/messages",
		store.TokenMetrics{InputTokens: 1000, OutputTokens: 200, CachedTokens: 400}, 100, 50)
	// Unknown cache counter: -1 must price like 0, not like a debt.
	seedUnpricedRow(t, s, "cf-sentinel", "/v1/chat/completions",
		store.TokenMetrics{InputTokens: 1000, OutputTokens: 200, CachedTokens: -1}, 0, 0)
	// A model with no catalog entry stays unpriced.
	seedUnpricedRow(t, s, "cf-unpriced", "/v1/chat/completions",
		store.TokenMetrics{InputTokens: 100, OutputTokens: 10}, 0, 0)

	if priced := s.backfillEstimatedCosts(ctx); priced != 3 {
		t.Fatalf("priced = %d, want 3", priced)
	}

	wantOpenAI := 500*backfillInputRate*1e-6 + 400*backfillReadRate*1e-6 +
		100*backfillWriteRate*1e-6 + 200*backfillOutputRate*1e-6 + 50*backfillThinkRate*1e-6
	wantAnthropic := 1000*backfillInputRate*1e-6 + 400*backfillReadRate*1e-6 +
		100*backfillWriteRate*1e-6 + 200*backfillOutputRate*1e-6 + 50*backfillThinkRate*1e-6
	wantSentinel := 1000*backfillInputRate*1e-6 + 200*backfillOutputRate*1e-6

	for _, tc := range []struct {
		model string
		want  float64
	}{{"cf-openai", wantOpenAI}, {"cf-anthropic", wantAnthropic}, {"cf-sentinel", wantSentinel}} {
		cost, estimated := modelCost(t, s, tc.model)
		if !estimated || !costsEqual(cost, tc.want) {
			t.Fatalf("%s: cost=%v estimated=%v, want %v/true", tc.model, cost, estimated, tc.want)
		}
	}
	if cost, estimated := modelCost(t, s, "cf-unpriced"); estimated || cost != 0 {
		t.Fatalf("unpriced model: cost=%v estimated=%v, want 0/false", cost, estimated)
	}

	// The grand total the stats page sums must carry all three amounts.
	stats, err := s.store.ActivityStats(ctx, store.ActivityStatsQuery{})
	if err != nil {
		t.Fatalf("ActivityStats: %v", err)
	}
	if !costsEqual(stats.EstimatedCost, wantOpenAI+wantAnthropic+wantSentinel) {
		t.Fatalf("total cost = %v, want %v", stats.EstimatedCost, wantOpenAI+wantAnthropic+wantSentinel)
	}
	if !stats.CostEstimated {
		t.Fatal("total cost_estimated = false, want true")
	}

	// A second pass is a no-op: priced rows are frozen.
	if priced := s.backfillEstimatedCosts(ctx); priced != 0 {
		t.Fatalf("second pass priced = %d, want 0", priced)
	}
}

// TestServer_CostBackfillVendorArbitration: when several catalog rows carry
// the same model name, the official vendor's row prices the backfill, matching
// what the live record path would have paid.
func TestServer_CostBackfillVendorArbitration(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })
	ctx := context.Background()

	// deepseek-flash is also listed by a reseller at absurd rates; the vendor
	// prefix table maps deepseek -> deepseek, so the vendor row must win.
	seedPricedModel(t, s, "deepseek", "deepseek-flash")
	if err := s.store.UpsertPrice(ctx, store.Price{
		Provider: "some-reseller", Model: "deepseek-flash",
		Input: 99, Output: 99, CacheRead: 99, CacheWrite: 99, Reasoning: 99,
	}); err != nil {
		t.Fatalf("UpsertPrice reseller: %v", err)
	}
	s.metrics.configurePricing(s.store, config.Config{})

	seedUnpricedRow(t, s, "deepseek-flash", "/v1/chat/completions",
		store.TokenMetrics{InputTokens: 1000, OutputTokens: 200, CachedTokens: 400}, 100, 50)

	if priced := s.backfillEstimatedCosts(ctx); priced != 1 {
		t.Fatalf("priced = %d, want 1", priced)
	}
	want := 500*backfillInputRate*1e-6 + 400*backfillReadRate*1e-6 +
		100*backfillWriteRate*1e-6 + 200*backfillOutputRate*1e-6 + 50*backfillThinkRate*1e-6
	cost, estimated := modelCost(t, s, "deepseek-flash")
	if !estimated || !costsEqual(cost, want) {
		t.Fatalf("cost=%v estimated=%v, want vendor price %v/true", cost, estimated, want)
	}
}

// TestServer_CostBackfillSingleFlight: a pass already running rejects a second
// trigger, and the rejected call must not release the running pass's guard.
func TestServer_CostBackfillSingleFlight(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })

	if !s.costBackfillRunning.CompareAndSwap(false, true) {
		t.Fatal("guard should start free")
	}
	if priced := s.backfillEstimatedCosts(context.Background()); priced != 0 {
		t.Fatalf("rejected pass priced = %d, want 0", priced)
	}
	if !s.costBackfillRunning.Load() {
		t.Fatal("rejected pass released the running pass's guard")
	}
	s.costBackfillRunning.Store(false)
	if !s.costBackfillIdle() {
		t.Fatal("guard did not release after the pass finished")
	}
}

// TestServer_CostBackfillSkipsUnpricedWithoutStalling: rows without a catalog
// entry are skipped but advance the cursor, so a backlog of unpriceable rows
// cannot loop forever; a completed pass clears the cursor so the next trigger
// re-examines them in case a price appeared meanwhile.
func TestServer_CostBackfillSkipsUnpricedWithoutStalling(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })

	for i := 0; i < 5; i++ {
		seedUnpricedRow(t, s, "no-such-model", "/v1/chat/completions",
			store.TokenMetrics{InputTokens: 100, OutputTokens: 10}, 0, 0)
	}

	finished := make(chan int64, 1)
	go func() { finished <- s.backfillEstimatedCosts(context.Background()) }()
	select {
	case priced := <-finished:
		if priced != 0 {
			t.Fatalf("priced = %d, want 0", priced)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("backfill stalled on unpriceable rows")
	}

	cursor, err := s.store.GetCostBackfillCursor(context.Background())
	if err != nil || cursor != 0 {
		t.Fatalf("cursor = %d, err %v; want 0 after a completed pass", cursor, err)
	}
}

// TestServer_CostBackfillResumesFromCursor: an interrupted pass resumes after
// its cursor instead of re-walking the earlier rows, and the completed pass
// clears the cursor so a later trigger can price what an earlier one had to
// skip.
func TestServer_CostBackfillResumesFromCursor(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })
	ctx := context.Background()

	seedPricedModel(t, s, "deepseek", "cf-resume")
	s.metrics.configurePricing(s.store, config.Config{})

	first := seedUnpricedRow(t, s, "cf-resume", "/v1/chat/completions",
		store.TokenMetrics{InputTokens: 1000, OutputTokens: 100}, 0, 0)
	seedUnpricedRow(t, s, "cf-resume", "/v1/chat/completions",
		store.TokenMetrics{InputTokens: 1000, OutputTokens: 100}, 0, 0)

	// Simulate a pass that walked up to (and past) the first row and died
	// before pricing the second.
	if err := s.store.SetCostBackfillCursor(ctx, int64(first)); err != nil {
		t.Fatalf("SetCostBackfillCursor: %v", err)
	}

	if priced := s.backfillEstimatedCosts(ctx); priced != 1 {
		t.Fatalf("resumed pass priced = %d, want 1 (only the row after the cursor)", priced)
	}
	// The first row stays unpriced for this pass; the completed pass cleared
	// the cursor, so the next trigger picks it up.
	if _, err := s.store.NextUnpricedActivity(ctx, int64(first)-1, 1); err != nil {
		t.Fatalf("NextUnpricedActivity: %v", err)
	}
	if priced := s.backfillEstimatedCosts(ctx); priced != 1 {
		t.Fatalf("follow-up pass priced = %d, want 1 (the skipped row)", priced)
	}
	cost, estimated := modelCost(t, s, "cf-resume")
	if !estimated || costsEqual(cost, 0) {
		t.Fatalf("cost=%v estimated=%v, want both rows priced", cost, estimated)
	}
}

// TestServer_StartCostBackfillRespectsConfig: the off switch silences the
// trigger; the default (absent key) runs a pass.
func TestServer_StartCostBackfillRespectsConfig(t *testing.T) {
	disabled := false
	s := newTestServerWithConfig(config.Config{
		Pricing: config.PricingConfig{
			ModelsDev: config.ModelsDevPricingConfig{CostBackfillOnSync: &disabled},
		},
	}, newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })

	seedUnpricedRow(t, s, "no-such-model", "/v1/chat/completions",
		store.TokenMetrics{InputTokens: 100, OutputTokens: 10}, 0, 0)
	s.startCostBackfill()
	if !s.costBackfillIdle() {
		t.Fatal("startCostBackfill launched a pass despite costBackfillOnSync=false")
	}

	// The default (absent key) is enabled: a triggered pass prices the row.
	enabled := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = enabled.Shutdown(0) })
	seedPricedModel(t, enabled, "deepseek", "cf-trigger")
	enabled.metrics.configurePricing(enabled.store, config.Config{})
	seedUnpricedRow(t, enabled, "cf-trigger", "/v1/chat/completions",
		store.TokenMetrics{InputTokens: 1000, OutputTokens: 100}, 0, 0)

	enabled.startCostBackfill()
	deadline := time.Now().Add(5 * time.Second)
	for {
		cost, estimated := modelCost(t, enabled, "cf-trigger")
		if estimated && cost > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("triggered pass never priced the row")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !enabled.costBackfillIdle() {
		t.Fatal("triggered pass never finished")
	}
}

// TestServer_CostBackfillConcurrentWriter: a live writer inserting rows while
// a pass runs must not produce an error or leave anything unpriced after the
// follow-up pass.
func TestServer_CostBackfillConcurrentWriter(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })
	ctx := context.Background()

	seedPricedModel(t, s, "deepseek", "cf-concurrent")
	s.metrics.configurePricing(s.store, config.Config{})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			if _, err := s.store.InsertActivity(ctx, store.ActivityLogEntry{
				Timestamp: time.Now().Add(-time.Hour),
				Model:     "cf-concurrent",
				KeyID:     "key-1",
				ReqPath:   "/v1/chat/completions",
				Tokens:    store.TokenMetrics{InputTokens: 1000, OutputTokens: 100},
			}); err != nil {
				t.Errorf("InsertActivity %d: %v", i, err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		s.backfillEstimatedCosts(ctx)
	}()
	wg.Wait()

	// Rows inserted after the concurrent pass's final read are caught by this
	// one; nothing may remain unpriced afterwards.
	if priced := s.backfillEstimatedCosts(ctx); priced < 0 {
		t.Fatalf("final pass priced = %d", priced)
	}
	rows, err := s.store.NextUnpricedActivity(ctx, 0, 1000)
	if err != nil {
		t.Fatalf("NextUnpricedActivity: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("%d rows left unpriced after the final pass", len(rows))
	}
}
