package store

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"
)

func nilActivityContext() context.Context { return nil }

func TestStore_NilIsInMemorySafe(t *testing.T) {
	var store *Store
	if store.IsInMemory() {
		t.Fatal("nil store reported in-memory")
	}
}

func TestStore_ActivityMethodsOnNilStoreReturnErrors(t *testing.T) {
	var store *Store
	ctx := context.Background()
	entry := ActivityLogEntry{Model: "model"}
	if _, err := store.InsertActivity(ctx, entry); err == nil {
		t.Fatal("InsertActivity on nil store unexpectedly succeeded")
	}
	if _, err := store.ListActivity(ctx, ActivityQuery{Limit: 1, Page: 1}); err == nil {
		t.Fatal("ListActivity on nil store unexpectedly succeeded")
	}
	if _, _, err := store.GetActivity(ctx, 1); err == nil {
		t.Fatal("GetActivity on nil store unexpectedly succeeded")
	}
	if _, err := store.ActivityStats(ctx, ActivityStatsQuery{}); err == nil {
		t.Fatal("ActivityStats on nil store unexpectedly succeeded")
	}
	if err := store.PruneActivity(ctx, 1); err == nil {
		t.Fatal("PruneActivity on nil store unexpectedly succeeded")
	}
}

func TestStore_InsertListAndFilterActivity(t *testing.T) {
	ctx := context.Background()
	store, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	for i, model := range []string{"m1", "m2", "m1"} {
		_, err := store.InsertActivity(ctx, ActivityLogEntry{
			Timestamp: time.Unix(int64(100+i), 0),
			Model:     model,
			ReqPath:   "/v1/chat/completions",
			Tokens: TokenMetrics{
				InputTokens:     i + 1,
				OutputTokens:    i + 2,
				PromptPerSecond: float64(10 + i),
				TokensPerSecond: float64(20 + i),
			},
			Metadata: map[string]string{"trace": model},
		})
		if err != nil {
			t.Fatalf("InsertActivity: %v", err)
		}
	}

	page, err := store.ListActivity(ctx, ActivityQuery{Limit: 2, Page: 1})
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	if page.Total != 3 || page.TotalPages != 2 || len(page.Data) != 2 {
		t.Fatalf("page = %+v", page)
	}
	if page.Data[0].ID <= page.Data[1].ID {
		t.Fatalf("activity is not newest first: %+v", page.Data)
	}
	if page.Data[0].Metadata["trace"] != "m1" {
		t.Fatalf("metadata = %+v", page.Data[0].Metadata)
	}

	filtered, err := store.ListActivity(ctx, ActivityQuery{
		ActivityFilter: ActivityFilter{Models: []string{"m1"}},
		Limit:          10,
		Page:           1,
	})
	if err != nil {
		t.Fatalf("ListActivity filtered: %v", err)
	}
	if filtered.Total != 2 || len(filtered.Data) != 2 {
		t.Fatalf("filtered page = %+v", filtered)
	}
	for _, entry := range filtered.Data {
		if entry.Model != "m1" {
			t.Fatalf("filtered model = %q", entry.Model)
		}
	}
}

func TestStore_ActivityFirstTokenRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	inserted, err := store.InsertActivity(ctx, ActivityLogEntry{
		Model:        "first-token-model",
		FirstTokenMs: 321,
	})
	if err != nil {
		t.Fatalf("InsertActivity: %v", err)
	}
	page, err := store.ListActivity(ctx, ActivityQuery{Limit: 1, Page: 1})
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	if len(page.Data) != 1 || page.Data[0].ID != inserted.ID || page.Data[0].FirstTokenMs != 321 {
		t.Fatalf("first token row = %+v, want id=%d first_token_ms=321", page.Data, inserted.ID)
	}

	defaulted, err := store.InsertActivity(ctx, ActivityLogEntry{Model: "unknown-first-token"})
	if err != nil {
		t.Fatalf("InsertActivity default: %v", err)
	}
	got, found, err := store.GetActivity(ctx, defaulted.ID)
	if err != nil || !found {
		t.Fatalf("GetActivity default: found=%v err=%v", found, err)
	}
	if got.FirstTokenMs != -1 {
		t.Errorf("default FirstTokenMs = %d, want -1", got.FirstTokenMs)
	}
}

func TestStore_ActivityMethodsAcceptNilContext(t *testing.T) {
	store, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.InsertActivity(nilActivityContext(), ActivityLogEntry{Model: "nil-context"}); err != nil {
		t.Fatalf("InsertActivity: %v", err)
	}
	if _, err := store.ListActivity(nilActivityContext(), ActivityQuery{Limit: 10, Page: 1}); err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	if _, err := store.ActivityStats(nilActivityContext(), ActivityStatsQuery{Model: "nil-context"}); err != nil {
		t.Fatalf("ActivityStats: %v", err)
	}
	if err := store.PruneActivity(nilActivityContext(), 10); err != nil {
		t.Fatalf("PruneActivity: %v", err)
	}
}

// seedFilterActivity inserts 5 rows with ids 1..5 at ts_created 1000..1004,
// alternating models m1/m2/m1/m2/m1, and returns the store.
func seedFilterActivity(t *testing.T) (*Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	store, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	for i, model := range []string{"m1", "m2", "m1", "m2", "m1"} {
		if _, err := store.InsertActivity(ctx, ActivityLogEntry{
			Timestamp: time.Unix(int64(1000+i), 0),
			Model:     model,
		}); err != nil {
			t.Fatalf("InsertActivity: %v", err)
		}
	}
	return store, ctx
}

// entryIDs returns the ids of a page in the order they were returned.
func entryIDs(entries []ActivityLogEntry) []int {
	ids := make([]int, len(entries))
	for i, entry := range entries {
		ids[i] = entry.ID
	}
	return ids
}

func equalIDs(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestStore_ListActivityFilterTimeRange(t *testing.T) {
	store, ctx := seedFilterActivity(t)

	tests := []struct {
		name   string
		filter ActivityFilter
		want   []int
	}{
		{"start only", ActivityFilter{Start: time.Unix(1002, 0)}, []int{5, 4, 3}},
		{"end only", ActivityFilter{End: time.Unix(1001, 0)}, []int{2, 1}},
		{"both bounds inclusive", ActivityFilter{Start: time.Unix(1001, 0), End: time.Unix(1003, 0)}, []int{4, 3, 2}},
		{"empty range", ActivityFilter{Start: time.Unix(9000, 0)}, []int{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page, err := store.ListActivity(ctx, ActivityQuery{ActivityFilter: tt.filter, Limit: 10, Page: 1})
			if err != nil {
				t.Fatalf("ListActivity: %v", err)
			}
			if got := entryIDs(page.Data); !equalIDs(got, tt.want) {
				t.Fatalf("ids = %v, want %v", got, tt.want)
			}
			if page.Total != len(tt.want) {
				t.Fatalf("Total = %d, want %d", page.Total, len(tt.want))
			}
		})
	}
}

func TestStore_ListActivityFilterIDRange(t *testing.T) {
	store, ctx := seedFilterActivity(t)

	tests := []struct {
		name   string
		filter ActivityFilter
		want   []int
	}{
		{"min only", ActivityFilter{MinID: 4}, []int{5, 4}},
		{"max only", ActivityFilter{MaxID: 2}, []int{2, 1}},
		{"both bounds inclusive", ActivityFilter{MinID: 2, MaxID: 4}, []int{4, 3, 2}},
		{"single row", ActivityFilter{MinID: 3, MaxID: 3}, []int{3}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page, err := store.ListActivity(ctx, ActivityQuery{ActivityFilter: tt.filter, Limit: 10, Page: 1})
			if err != nil {
				t.Fatalf("ListActivity: %v", err)
			}
			if got := entryIDs(page.Data); !equalIDs(got, tt.want) {
				t.Fatalf("ids = %v, want %v", got, tt.want)
			}
			if page.Total != len(tt.want) {
				t.Fatalf("Total = %d, want %d", page.Total, len(tt.want))
			}
		})
	}
}

func TestStore_ListActivityFilterModels(t *testing.T) {
	store, ctx := seedFilterActivity(t)

	tests := []struct {
		name   string
		models []string
		want   []int
	}{
		{"single", []string{"m1"}, []int{5, 3, 1}},
		{"multiple", []string{"m1", "m2"}, []int{5, 4, 3, 2, 1}},
		{"blank entries ignored", []string{"m2", "  "}, []int{4, 2}},
		{"all blank matches everything", []string{"", " "}, []int{5, 4, 3, 2, 1}},
		{"unknown model", []string{"nope"}, []int{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page, err := store.ListActivity(ctx, ActivityQuery{
				ActivityFilter: ActivityFilter{Models: tt.models},
				Limit:          10,
				Page:           1,
			})
			if err != nil {
				t.Fatalf("ListActivity: %v", err)
			}
			if got := entryIDs(page.Data); !equalIDs(got, tt.want) {
				t.Fatalf("ids = %v, want %v", got, tt.want)
			}
		})
	}
}

// Combined filters must AND together, and Total/TotalPages must describe the
// filtered set rather than the whole table.
func TestStore_ListActivityFilterCombinedPaging(t *testing.T) {
	store, ctx := seedFilterActivity(t)

	page, err := store.ListActivity(ctx, ActivityQuery{
		ActivityFilter: ActivityFilter{
			Models: []string{"m1"},
			Start:  time.Unix(1001, 0),
			MinID:  2,
			MaxID:  5,
		},
		Limit: 1,
		Page:  1,
	})
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	// m1 rows are ids 1,3,5; the time and id bounds each drop id 1, leaving
	// 3 and 5.
	if page.Total != 2 || page.TotalPages != 2 {
		t.Fatalf("Total = %d, TotalPages = %d, want 2 and 2", page.Total, page.TotalPages)
	}
	if got := entryIDs(page.Data); !equalIDs(got, []int{5}) {
		t.Fatalf("page 1 ids = %v, want [5]", got)
	}
}

func TestStore_ListActivitySort(t *testing.T) {
	ctx := context.Background()
	store, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	// Insert rows whose output_tokens ordering differs from insertion (id) order.
	outputs := []int{30, 10, 20}
	for i, out := range outputs {
		if _, err := store.InsertActivity(ctx, ActivityLogEntry{
			Timestamp: time.Unix(int64(100+i), 0),
			Model:     "m1",
			Tokens:    TokenMetrics{OutputTokens: out},
		}); err != nil {
			t.Fatalf("InsertActivity: %v", err)
		}
	}

	asc, err := store.ListActivity(ctx, ActivityQuery{Limit: 10, Page: 1, Sort: "generated", Order: "asc"})
	if err != nil {
		t.Fatalf("ListActivity asc: %v", err)
	}
	gotAsc := []int{}
	for _, e := range asc.Data {
		gotAsc = append(gotAsc, e.Tokens.OutputTokens)
	}
	if len(gotAsc) != 3 || gotAsc[0] != 10 || gotAsc[1] != 20 || gotAsc[2] != 30 {
		t.Fatalf("ascending generated sort = %v", gotAsc)
	}

	desc, err := store.ListActivity(ctx, ActivityQuery{Limit: 10, Page: 1, Sort: "generated", Order: "desc"})
	if err != nil {
		t.Fatalf("ListActivity desc: %v", err)
	}
	gotDesc := []int{}
	for _, e := range desc.Data {
		gotDesc = append(gotDesc, e.Tokens.OutputTokens)
	}
	if len(gotDesc) != 3 || gotDesc[0] != 30 || gotDesc[1] != 20 || gotDesc[2] != 10 {
		t.Fatalf("descending generated sort = %v", gotDesc)
	}

	// Unknown sort keys fall back to id ordering (newest first).
	fallback, err := store.ListActivity(ctx, ActivityQuery{Limit: 10, Page: 1, Sort: "bogus"})
	if err != nil {
		t.Fatalf("ListActivity fallback: %v", err)
	}
	if fallback.Data[0].ID <= fallback.Data[len(fallback.Data)-1].ID {
		t.Fatalf("fallback sort is not newest first: %+v", fallback.Data)
	}
}

func TestStore_ActivityStats(t *testing.T) {
	ctx := context.Background()
	store, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	entries := []ActivityLogEntry{
		{
			Timestamp: time.Unix(1, 0),
			Model:     "m1",
			Tokens: TokenMetrics{
				CachedTokens:    2,
				InputTokens:     10,
				OutputTokens:    20,
				PromptPerSecond: 100,
				TokensPerSecond: 50,
			},
		},
		{
			Timestamp: time.Unix(2, 0),
			Model:     "m1",
			Tokens: TokenMetrics{
				CachedTokens:    -1,
				InputTokens:     5,
				OutputTokens:    8,
				PromptPerSecond: 200,
				TokensPerSecond: 100,
			},
		},
		{
			Timestamp: time.Unix(3, 0),
			Model:     "m2",
			Tokens: TokenMetrics{
				InputTokens:     7,
				OutputTokens:    9,
				PromptPerSecond: 300,
			},
		},
	}
	for _, entry := range entries {
		if _, err := store.InsertActivity(ctx, entry); err != nil {
			t.Fatalf("InsertActivity: %v", err)
		}
	}

	stats, err := store.ActivityStats(ctx, ActivityStatsQuery{Model: "m1"})
	if err != nil {
		t.Fatalf("ActivityStats: %v", err)
	}
	if stats.TotalRequests != 2 || stats.TotalInputTokens != 15 || stats.TotalOutputTokens != 28 || stats.TotalCacheTokens != 2 {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.PromptHistogram == nil || stats.GenerationHistogram == nil {
		t.Fatalf("expected histograms: %+v", stats)
	}
}

func TestStore_ActivityStatsIncludesCacheCreationRatio(t *testing.T) {
	store, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.InsertActivity(context.Background(), ActivityLogEntry{
		Model:               "cache-model",
		Tokens:              TokenMetrics{InputTokens: 10, CachedTokens: 20},
		CacheCreationTokens: 5,
	}); err != nil {
		t.Fatal(err)
	}
	stats, err := store.ActivityStats(context.Background(), ActivityStatsQuery{Model: "cache-model"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stats.CacheCreationRatio, 5.0/35.0; math.Abs(got-want) > 1e-9 {
		t.Fatalf("cache creation ratio=%v, want %v", got, want)
	}
}

func TestStore_ActivityStatsModelAllowlist(t *testing.T) {
	store, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, entry := range []ActivityLogEntry{
		{Model: "m1", Tokens: TokenMetrics{InputTokens: 2}},
		{Model: "m2", Tokens: TokenMetrics{InputTokens: 9}},
	} {
		if _, err := store.InsertActivity(context.Background(), entry); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := store.ActivityStats(context.Background(), ActivityStatsQuery{Models: []string{"m1"}})
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalRequests != 1 || stats.TotalInputTokens != 2 {
		t.Fatalf("allowlisted stats=%+v", stats)
	}
}

func TestStore_ActivityStatsSaturatesMalformedCounters(t *testing.T) {
	store, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	maxInt := int(^uint(0) >> 1)
	for _, entry := range []ActivityLogEntry{
		{Model: "extreme", Tokens: TokenMetrics{InputTokens: maxInt, OutputTokens: maxInt, CachedTokens: maxInt}, CacheCreationTokens: maxInt, ReasoningTokens: maxInt},
		{Model: "extreme", Tokens: TokenMetrics{InputTokens: maxInt, OutputTokens: -10, CachedTokens: -1}, CacheCreationTokens: -1, ReasoningTokens: maxInt},
	} {
		if _, err := store.InsertActivity(context.Background(), entry); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := store.ActivityStats(context.Background(), ActivityStatsQuery{Model: "extreme"})
	if err != nil {
		t.Fatalf("ActivityStats: %v", err)
	}
	if stats.TotalRequests != 2 || stats.TotalInputTokens != maxInt || stats.TotalOutputTokens != maxInt || stats.TotalCacheTokens != maxInt || stats.TotalCacheCreationTokens != maxInt || stats.TotalReasoningTokens != maxInt {
		t.Fatalf("saturated stats=%+v", stats)
	}
}

func TestStore_PruneActivity(t *testing.T) {
	ctx := context.Background()
	store, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	for i := 0; i < 5; i++ {
		if _, err := store.InsertActivity(ctx, ActivityLogEntry{Timestamp: time.Unix(int64(i), 0), Model: "m"}); err != nil {
			t.Fatalf("InsertActivity: %v", err)
		}
	}
	if err := store.PruneActivity(ctx, 2); err != nil {
		t.Fatalf("PruneActivity: %v", err)
	}
	page, err := store.ListActivity(ctx, ActivityQuery{Limit: 10, Page: 1})
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	if page.Total != 2 || len(page.Data) != 2 {
		t.Fatalf("page = %+v", page)
	}
	if page.Data[0].ID != 5 || page.Data[1].ID != 4 {
		t.Fatalf("kept IDs = %+v", page.Data)
	}
}

func TestStore_NewFilePersistsActivity(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "llama-swap.sqlite")

	store, err := New(path)
	if err != nil {
		t.Fatalf("New file store: %v", err)
	}
	if _, err := store.InsertActivity(ctx, ActivityLogEntry{Timestamp: time.Unix(1, 0), Model: "m"}); err != nil {
		t.Fatalf("InsertActivity: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	store, err = New(path)
	if err != nil {
		t.Fatalf("reopen file store: %v", err)
	}
	defer store.Close()
	page, err := store.ListActivity(ctx, ActivityQuery{Limit: 10, Page: 1})
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	if page.Total != 1 || len(page.Data) != 1 || page.Data[0].Model != "m" {
		t.Fatalf("page = %+v", page)
	}
}

func TestStore_NewFileUsesWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "llama-swap.sqlite")
	store, err := New(path)
	if err != nil {
		t.Fatalf("New file store: %v", err)
	}
	defer store.Close()

	var mode string
	if err := store.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
}

func TestStore_CalculateHistogramDropsNonFiniteValues(t *testing.T) {
	histogram := calculateHistogramData([]float64{math.NaN(), math.Inf(1), 10, 20})
	if histogram == nil {
		t.Fatal("finite histogram samples were discarded")
	}
	if histogram.Min != 10 || histogram.Max != 20 {
		t.Fatalf("histogram bounds = %+v", histogram)
	}
	total := 0
	for _, count := range histogram.Bins {
		total += count
	}
	if total != 2 {
		t.Fatalf("histogram retained non-finite samples: bins=%v", histogram.Bins)
	}
}

func TestStore_CalculateHistogramReturnsNilForOnlyNonFiniteValues(t *testing.T) {
	if histogram := calculateHistogramData([]float64{math.NaN(), math.Inf(-1)}); histogram != nil {
		t.Fatalf("histogram for only non-finite values = %+v", histogram)
	}
}

func TestStore_CalculateHistogramClipsExtremeSpeedOutlier(t *testing.T) {
	values := []float64{100, 100, 100, 100, 100, 100, 100, 100, 8_697_384.1}
	histogram := calculateHistogramData(values)
	if histogram == nil {
		t.Fatal("expected histogram")
	}
	if histogram.Max >= 1_000 {
		t.Fatalf("histogram max = %v, want the extreme outlier clipped", histogram.Max)
	}
	total := 0
	for _, count := range histogram.Bins {
		total += count
	}
	if total != len(values) {
		t.Fatalf("histogram count = %d, want %d", total, len(values))
	}
}

// TestStore_ActivityQueryClampsOffsetArithmetic pins the guard against an
// unbounded page from the query string. (Page-1)*Limit overflows int for a
// large enough page and wraps to a negative OFFSET, which SQLite reads as 0 —
// so a request for an absurd page silently returned page 1 instead of an empty
// page.
func TestStore_ActivityQueryClampsOffsetArithmetic(t *testing.T) {
	for _, tc := range []struct {
		name  string
		page  int
		limit int
	}{
		{name: "max int page", page: math.MaxInt, limit: 999},
		{name: "max int page, odd limit", page: math.MaxInt, limit: 25},
		{name: "page just past the bound", page: maxActivityPage + 1, limit: 25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeActivityQuery(ActivityQuery{Page: tc.page, Limit: tc.limit})
			if got.Page > maxActivityPage {
				t.Fatalf("page = %d, want it clamped to %d", got.Page, maxActivityPage)
			}
			if got.Limit > maxActivityLimit {
				t.Fatalf("limit = %d, want it clamped to %d", got.Limit, maxActivityLimit)
			}
			if offset := (got.Page - 1) * got.Limit; offset < 0 {
				t.Fatalf("offset = %d, want non-negative (a negative offset is read as 0, answering page 1)", offset)
			}
		})
	}
}
