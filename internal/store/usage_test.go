package store

import (
	"context"
	"testing"
	"time"
)

func TestStore_UsageAnalyticsAndRecords(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	usageTestKey(t, s, "mgmt", "", "platform", nil)
	usageTestKey(t, s, "child-a", "mgmt", "team-a", nil)
	usageTestKey(t, s, "child-b", "mgmt", "team-b", []string{"gpt-4"})

	now := time.Now().UTC()
	rows := []ActivityLogEntry{
		{Timestamp: now.Add(-3 * time.Hour), Model: "gpt-4", KeyID: "child-a", ReqPath: "/v1/chat/completions", ClientIP: "203.0.113.7",
			Tokens:              TokenMetrics{InputTokens: 100, OutputTokens: 50, CachedTokens: 40},
			CacheCreationTokens: 10, EstimatedCost: 0.5, CostEstimated: true, DurationMs: 2000},
		{Timestamp: now.Add(-2 * time.Hour), Model: "gpt-4", KeyID: "child-a", ReqPath: "/v1/chat/completions", ClientIP: "203.0.113.7",
			Tokens:              TokenMetrics{InputTokens: 200, OutputTokens: 80, CachedTokens: 60},
			CacheCreationTokens: 20, EstimatedCost: 0.75, CostEstimated: true, DurationMs: 4000},
		{Timestamp: now.Add(-1 * time.Hour), Model: "claude", KeyID: "child-b", ReqPath: "/v1/responses", ClientIP: "10.0.0.5",
			Tokens:        TokenMetrics{InputTokens: 50, OutputTokens: 10},
			EstimatedCost: 0.1, CostEstimated: true, DurationMs: 5000, FirstTokenMs: -1},
		// An anonymous row must still aggregate under a stable label.
		{Timestamp: now, Model: "claude", ReqPath: "/v1/responses", ClientIP: "10.0.0.9",
			Tokens: TokenMetrics{InputTokens: 10, OutputTokens: 5}, DurationMs: 100},
	}
	for _, row := range rows {
		if _, err := s.InsertActivity(ctx, row); err != nil {
			t.Fatalf("insert activity: %v", err)
		}
	}

	analytics, err := s.UsageAnalytics(ctx, UsageQuery{}, UsageGranularityHour)
	if err != nil {
		t.Fatalf("usage analytics: %v", err)
	}
	if analytics.Granularity != UsageGranularityHour {
		t.Fatalf("granularity = %q", analytics.Granularity)
	}
	if analytics.Totals.Requests != 4 {
		t.Fatalf("requests = %d, want 4", analytics.Totals.Requests)
	}
	if analytics.Totals.InputTokens != 360 || analytics.Totals.OutputTokens != 145 {
		t.Fatalf("totals = %#v", analytics.Totals)
	}
	if analytics.Totals.CachedTokens != 100 || analytics.Totals.CacheCreationTokens != 30 {
		t.Fatalf("cache totals = %#v", analytics.Totals)
	}
	if analytics.Totals.TotalTokens != 505 {
		t.Fatalf("total tokens = %d, want 505", analytics.Totals.TotalTokens)
	}
	// Duration averages cover every row: (2000 + 4000 + 5000 + 100) / 4.
	if analytics.Totals.AverageDurationMs != 2775 {
		t.Fatalf("average duration = %d, want 2775", analytics.Totals.AverageDurationMs)
	} // First-token averages only cover rows that reported a boundary; every row
	// here has -1.
	if analytics.Totals.AverageFirstTokenMs != 0 {
		t.Fatalf("average first token = %d, want 0", analytics.Totals.AverageFirstTokenMs)
	}
	if len(analytics.ByModel) != 2 {
		t.Fatalf("by model = %#v", analytics.ByModel)
	}
	// Both models tie on request count, so the secondary name ordering decides.
	if analytics.ByModel[0].Name != "claude" || analytics.ByModel[1].Name != "gpt-4" {
		t.Fatalf("by model = %#v", analytics.ByModel)
	}
	modelCounts := map[string]int{}
	for _, row := range analytics.ByModel {
		modelCounts[row.Name] = row.Requests
	}
	if modelCounts["gpt-4"] != 2 || modelCounts["claude"] != 2 {
		t.Fatalf("by model request counts = %#v", analytics.ByModel)
	}
	// Groups come from the key row, so the anonymous row lands in "ungrouped".
	groups := map[string]int{}
	for _, row := range analytics.ByGroup {
		groups[row.Name] = row.Requests
	}
	if groups["team-a"] != 2 || groups["team-b"] != 1 || groups["ungrouped"] != 1 {
		t.Fatalf("by group = %#v", analytics.ByGroup)
	}
	endpoints := map[string]int{}
	for _, row := range analytics.ByEndpoint {
		endpoints[row.Name] = row.Requests
	}
	if endpoints["/v1/chat/completions"] != 2 || endpoints["/v1/responses"] != 2 {
		t.Fatalf("by endpoint = %#v", analytics.ByEndpoint)
	}
	// Key dimension prefers the managed key's display name.
	keys := map[string]int{}
	for _, row := range analytics.ByKey {
		keys[row.Name] = row.Requests
	}
	if keys["child-a-name"] != 2 || keys["anonymous"] != 1 {
		t.Fatalf("by key = %#v", analytics.ByKey)
	}
	if len(analytics.Trend) == 0 {
		t.Fatal("trend has no buckets")
	}
	for _, bucket := range analytics.Trend {
		if bucket.BucketStart.IsZero() {
			t.Fatalf("trend bucket without start: %#v", bucket)
		}
	}

	// Granularity validation.
	if _, err := s.UsageAnalytics(ctx, UsageQuery{}, "fortnight"); err == nil {
		t.Fatal("unsupported granularity accepted")
	}

	// A model filter narrows every aggregate and dimension.
	filtered, err := s.UsageAnalytics(ctx, UsageQuery{Models: []string{"gpt-4"}}, UsageGranularityDay)
	if err != nil {
		t.Fatalf("filtered analytics: %v", err)
	}
	if filtered.Totals.Requests != 2 || len(filtered.ByModel) != 1 || filtered.ByModel[0].Name != "gpt-4" {
		t.Fatalf("filtered analytics = %#v", filtered)
	}
	// Group filtering reaches through the api_keys join.
	grouped, err := s.UsageAnalytics(ctx, UsageQuery{Groups: []string{"team-a"}}, UsageGranularityDay)
	if err != nil {
		t.Fatalf("group filtered analytics: %v", err)
	}
	if grouped.Totals.Requests != 2 {
		t.Fatalf("group filtered totals = %#v", grouped.Totals)
	}
	// Key filtering is by key id.
	keyFiltered, err := s.UsageAnalytics(ctx, UsageQuery{Keys: []string{"child-b"}}, UsageGranularityDay)
	if err != nil {
		t.Fatalf("key filtered analytics: %v", err)
	}
	if keyFiltered.Totals.Requests != 1 {
		t.Fatalf("key filtered totals = %#v", keyFiltered.Totals)
	}
	// Endpoint filtering.
	endpointFiltered, err := s.UsageAnalytics(ctx, UsageQuery{Endpoints: []string{"/v1/responses"}}, UsageGranularityDay)
	if err != nil {
		t.Fatalf("endpoint filtered analytics: %v", err)
	}
	if endpointFiltered.Totals.Requests != 2 {
		t.Fatalf("endpoint filtered totals = %#v", endpointFiltered.Totals)
	}

	records, err := s.UsageRecords(ctx, UsageQuery{Limit: 2})
	if err != nil {
		t.Fatalf("usage records: %v", err)
	}
	if len(records.Data) != 2 || records.Total != 4 || records.TotalPages != 2 {
		t.Fatalf("usage records = %#v", records)
	}
	if records.Data[0].Model == "" || records.Data[0].Timestamp.IsZero() {
		t.Fatalf("usage record row = %#v", records.Data[0])
	}
	// Newest first by default, and the join supplies the key name.
	if !records.Data[0].Timestamp.After(records.Data[1].Timestamp) {
		t.Fatalf("records not ordered newest first: %#v", records.Data)
	}
	if records.Data[0].KeyName == "" && records.Data[0].KeyID != "" {
		t.Fatalf("key name not joined: %#v", records.Data[0])
	}
	ascending, err := s.UsageRecords(ctx, UsageQuery{Limit: 2, Sort: "cost", Order: "asc"})
	if err != nil {
		t.Fatalf("ascending records: %v", err)
	}
	// The anonymous row carries no cost estimate, so it sorts first.
	if len(ascending.Data) != 2 || ascending.Data[0].EstimatedCost != 0 {
		t.Fatalf("cost ordering = %#v", ascending.Data)
	}

	options, err := s.UsageFilterOptions(ctx, UsageQuery{})
	if err != nil {
		t.Fatalf("usage filter options: %v", err)
	}
	if len(options.Keys) < 3 {
		t.Fatalf("filter keys = %#v", options.Keys)
	}
	if len(options.Models) != 2 || len(options.Endpoints) != 2 {
		t.Fatalf("filter models=%#v endpoints=%#v", options.Models, options.Endpoints)
	}
	groupsSeen := map[string]bool{}
	for _, group := range options.Groups {
		groupsSeen[group] = true
	}
	if !groupsSeen["team-a"] || !groupsSeen["ungrouped"] {
		t.Fatalf("filter groups = %#v", options.Groups)
	}
}

func TestStore_UsageOnNilStoreReturnsErrors(t *testing.T) {
	var store *Store
	ctx := context.Background()
	if _, err := store.UsageAnalytics(ctx, UsageQuery{}, UsageGranularityDay); err == nil {
		t.Fatal("UsageAnalytics on nil store unexpectedly succeeded")
	}
	if _, err := store.UsageRecords(ctx, UsageQuery{}); err == nil {
		t.Fatal("UsageRecords on nil store unexpectedly succeeded")
	}
	if _, err := store.UsageFilterOptions(ctx, UsageQuery{}); err == nil {
		t.Fatal("UsageFilterOptions on nil store unexpectedly succeeded")
	}
}

func TestStore_UsageRecordsClientIPPersisted(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if _, err := s.InsertActivity(ctx, ActivityLogEntry{
		Model: "gpt-4", KeyID: "mgmt", ReqPath: "/v1/chat/completions",
		ClientIP: "203.0.113.7", DurationMs: 1000,
	}); err != nil {
		t.Fatalf("insert activity: %v", err)
	}
	page, err := s.UsageRecords(ctx, UsageQuery{Limit: 10})
	if err != nil {
		t.Fatalf("usage records: %v", err)
	}
	if len(page.Data) != 1 || page.Data[0].ClientIP != "203.0.113.7" {
		t.Fatalf("client ip = %#v", page.Data)
	}
	// The activity read path surfaces the same value.
	activity, err := s.ListActivity(ctx, ActivityQuery{Limit: 1, Page: 1})
	if err != nil || len(activity.Data) != 1 || activity.Data[0].ClientIP != "203.0.113.7" {
		t.Fatalf("activity read = %#v err=%v", activity, err)
	}
}

func TestStore_UsageTotalsAverageIsNotAnInteger(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	// SQLite's AVG always returns REAL, so a fractional average must scan into
	// the reported millisecond fields instead of failing the whole query.
	for _, duration := range []int{1, 2} {
		if _, err := s.InsertActivity(ctx, ActivityLogEntry{Model: "gpt-4", DurationMs: duration}); err != nil {
			t.Fatalf("insert activity: %v", err)
		}
	}
	analytics, err := s.UsageAnalytics(ctx, UsageQuery{}, UsageGranularityDay)
	if err != nil {
		t.Fatalf("usage analytics: %v", err)
	}
	// (1 + 2) / 2 is 1.5, which rounds up to 2 rather than truncating to 1.
	if analytics.Totals.AverageDurationMs != 2 {
		t.Fatalf("average duration = %d, want 2 (rounded from 1.5)", analytics.Totals.AverageDurationMs)
	}
	if _, err := s.InsertActivity(ctx, ActivityLogEntry{Model: "gpt-4", DurationMs: 1}); err != nil {
		t.Fatalf("insert activity: %v", err)
	}
	analytics, err = s.UsageAnalytics(ctx, UsageQuery{}, UsageGranularityDay)
	if err != nil {
		t.Fatalf("usage analytics with three rows: %v", err)
	}
	// (1 + 2 + 1) / 3 is 1.33, which rounds to 1.
	if analytics.Totals.AverageDurationMs != 1 {
		t.Fatalf("average duration = %d, want 1", analytics.Totals.AverageDurationMs)
	}
}

// A filtered UsageFilterOptions request used to fail with "near WHERE:
// syntax error": distinct() embedded its own WHERE after usageWhere's.
func TestStore_UsageFilterOptionsWithFilters(t *testing.T) {
	ctx := context.Background()
	s, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()
	entries := []ActivityLogEntry{
		{Timestamp: time.Unix(1700000000, 0), Model: "m1", ReqPath: "/v1/chat/completions", Tokens: TokenMetrics{InputTokens: 10, OutputTokens: 5}, KeyID: "key-a"},
		{Timestamp: time.Unix(1700000100, 0), Model: "m2", ReqPath: "/v1/responses", Tokens: TokenMetrics{InputTokens: 10, OutputTokens: 5}, KeyID: "key-a"},
	}
	for _, entry := range entries {
		if _, err := s.InsertActivity(ctx, entry); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	query := UsageQuery{Models: []string{"m1"}, Endpoints: []string{"/v1/chat/completions"}}
	options, err := s.UsageFilterOptions(ctx, query)
	if err != nil {
		t.Fatalf("filtered options: %v", err)
	}
	if len(options.Models) != 1 || options.Models[0] != "m1" {
		t.Fatalf("models = %v, want [m1]", options.Models)
	}
	// Unfiltered must keep working too.
	options, err = s.UsageFilterOptions(ctx, UsageQuery{})
	if err != nil {
		t.Fatalf("unfiltered options: %v", err)
	}
	if len(options.Models) != 2 {
		t.Fatalf("models = %v, want both", options.Models)
	}
}
