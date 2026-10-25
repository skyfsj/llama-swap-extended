package store

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// seedLargeActivity writes a realistic volume of request records: enough that a
// full table scan is measurably slower than an indexed one, but small enough
// that the test stays under a few seconds.
func seedLargeActivity(t *testing.T, store *Store, count int) {
	t.Helper()
	base := time.Now().Add(-24 * time.Hour).Truncate(time.Hour)
	rng := rand.New(rand.NewSource(11))
	models := []string{"deepseek-flash", "gpt-6-luna", "claude-haiku-5-5", "gemini-4-argon", "glm-5.3-flash"}
	paths := []string{"/v1/chat/completions", "/v1/responses", "/v1/messages", "/v1beta/models/x:generateContent"}
	ctx := context.Background()
	// The rows go through the same insert path production uses, so the rollup is
	// folded exactly as it is for a real request. One row at a time is slower to
	// seed than a batch insert, but it cannot drift from the write path the test
	// is measuring.
	for i := 0; i < count; i++ {
		entry := ActivityLogEntry{
			Timestamp:      base.Add(time.Duration(rng.Intn(24*3600)) * time.Second),
			Model:          models[rng.Intn(len(models))],
			KeyID:          fmt.Sprintf("key-%d", rng.Intn(20)),
			SessionID:      fmt.Sprintf("sess-%d", rng.Intn(2000)),
			ReqPath:        paths[rng.Intn(len(paths))],
			RespStatusCode: 200,
			Tokens: TokenMetrics{
				InputTokens:     rng.Intn(20000),
				OutputTokens:    rng.Intn(8000),
				CachedTokens:    rng.Intn(4000),
				PromptPerSecond: float64(rng.Intn(30000)) / 10,
				TokensPerSecond: float64(rng.Intn(20000)) / 10,
			},
			CacheCreationTokens: rng.Intn(2000),
			ReasoningTokens:     rng.Intn(4000),
			EstimatedCost:       float64(rng.Intn(2000)) / 1000,
			CostEstimated:       i%2 == 0,
			DurationMs:          rng.Intn(60000),
			FirstTokenMs:        rng.Intn(8000),
		}
		if _, err := store.InsertActivity(ctx, entry); err != nil {
			t.Fatalf("InsertActivity %d: %v", i, err)
		}
	}
}

// TestStore_ActivityScaling reports how the record page and the statistics
// behave as the table grows. It is a benchmark in test form so a regression
// shows up as a failing assertion rather than as a number somebody has to
// notice.
func TestStore_ActivityScaling(t *testing.T) {
	store := newRollupTestStore(t)
	seedLargeActivity(t, store, 60_000)
	ctx := context.Background()

	// Sorting by an unindexed column used to force a full sort of every row.
	for _, sort := range []string{"id", "duration", "input_tokens", "estimated_cost", "resp_status_code"} {
		started := time.Now()
		page, err := store.ListActivity(ctx, ActivityQuery{Limit: 25, Page: 1, Sort: sort, Order: "desc"})
		if err != nil {
			t.Fatalf("ListActivity sort=%s: %v", sort, err)
		}
		if len(page.Data) != 25 {
			t.Fatalf("sort=%s returned %d rows", sort, len(page.Data))
		}
		t.Logf("sort=%-16s page  %s", sort, time.Since(started).Round(time.Millisecond))
	}

	// A deep page is where OFFSET shows its cost.
	started := time.Now()
	if _, err := store.ListActivity(ctx, ActivityQuery{Limit: 100, Page: 900, Sort: "id", Order: "desc"}); err != nil {
		t.Fatal(err)
	}
	t.Logf("deep page (900x100)  %s", time.Since(started).Round(time.Millisecond))

	// Statistics must fold buckets, not rows.
	started = time.Now()
	stats, err := store.ActivityStats(ctx, ActivityStatsQuery{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("activity stats       %s (requests=%d)", time.Since(started).Round(time.Millisecond), stats.TotalRequests)
	if stats.TotalRequests != 60_000 {
		t.Fatalf("stats requests = %d, want 60000", stats.TotalRequests)
	}

	started = time.Now()
	analytics, err := store.UsageAnalytics(ctx, UsageQuery{}, "hour")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("usage analytics      %s (trend buckets=%d)", time.Since(started).Round(time.Millisecond), len(analytics.Trend))
	if analytics.Totals.Requests != 60_000 {
		t.Fatalf("analytics requests = %d, want 60000", analytics.Totals.Requests)
	}
}
