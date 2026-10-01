package store

import (
	"context"
	"testing"
	"time"
)

func speedTestEntry(model string, ts int64, input, output int, prefill, decode float64, firstTokenMs int) ActivityLogEntry {
	return ActivityLogEntry{
		Timestamp:    time.Unix(ts, 0),
		Model:        model,
		ReqPath:      "/v1/chat/completions",
		Tokens:       TokenMetrics{InputTokens: input, OutputTokens: output, PromptPerSecond: prefill, TokensPerSecond: decode},
		DurationMs:   1000,
		FirstTokenMs: firstTokenMs,
	}
}

func TestStore_ActivitySpeedReportEmpty(t *testing.T) {
	ctx := context.Background()
	store, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	report, err := store.ActivitySpeedReport(ctx, ActivitySpeedQuery{})
	if err != nil {
		t.Fatalf("ActivitySpeedReport: %v", err)
	}
	if len(report.Series) != 0 || len(report.Context) != 0 {
		t.Fatalf("expected an empty report, got %+v", report)
	}
}

func TestStore_ActivitySpeedReportAveragesAndGaps(t *testing.T) {
	ctx := context.Background()
	store, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	base := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC).Unix()
	entries := []ActivityLogEntry{
		// Two requests in the first hour: prefill/decode/TTFT average to the mean.
		speedTestEntry("m1", base, 1000, 50, 100, 40, 200),
		speedTestEntry("m1", base+60, 2000, 50, 300, 20, 400),
		// Same hour, a backend that reports no speed or TTFT telemetry at all:
		// it counts as a request but must not drag the averages toward zero.
		speedTestEntry("m1", base+120, 3000, 10, -1, -1, -1),
		// Second hour for the same model.
		speedTestEntry("m1", base+3600, 1000, 20, 150, 30, 100),
		// Another model interleaved.
		speedTestEntry("m2", base+30, 8000, 80, 500, 25, 90),
	}
	for _, entry := range entries {
		if _, err := store.InsertActivity(ctx, entry); err != nil {
			t.Fatalf("InsertActivity: %v", err)
		}
	}

	report, err := store.ActivitySpeedReport(ctx, ActivitySpeedQuery{BucketSeconds: 3600})
	if err != nil {
		t.Fatalf("ActivitySpeedReport: %v", err)
	}
	if report.BucketSeconds != 3600 {
		t.Fatalf("bucket seconds = %d, want 3600", report.BucketSeconds)
	}
	if len(report.Series) != 2 {
		t.Fatalf("series = %+v, want 2 models", report.Series)
	}
	// m2 has one request, m1 has four: the busiest model comes first.
	if report.Series[0].Model != "m1" || report.Series[1].Model != "m2" {
		t.Fatalf("series order = %+v", report.Series)
	}

	m1 := report.Series[0]
	if len(m1.Points) != 2 {
		t.Fatalf("m1 points = %+v, want 2 hourly buckets", m1.Points)
	}
	first := m1.Points[0]
	if first.Requests != 3 {
		t.Fatalf("first bucket requests = %d, want 3", first.Requests)
	}
	if first.PrefillTPS != 200 {
		t.Fatalf("first bucket prefill = %v, want 200", first.PrefillTPS)
	}
	if first.DecodeTPS != 30 {
		t.Fatalf("first bucket decode = %v, want 30", first.DecodeTPS)
	}
	if first.TTFTMs != 300 {
		t.Fatalf("first bucket ttft = %v, want 300", first.TTFTMs)
	}
	second := m1.Points[1]
	if second.Requests != 1 || second.PrefillTPS != 150 || second.DecodeTPS != 30 || second.TTFTMs != 100 {
		t.Fatalf("second bucket = %+v", second)
	}
}

func TestStore_ActivitySpeedReportEmptyBucketGap(t *testing.T) {
	ctx := context.Background()
	store, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	base := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC).Unix()
	// Two requests three days apart with a one-day bucket leave the middle
	// buckets empty; those must stay distinguishable from measured zeros.
	for _, ts := range []int64{base, base + 3*86400} {
		if _, err := store.InsertActivity(ctx, speedTestEntry("m1", ts, 1000, 10, 100, 20, 50)); err != nil {
			t.Fatalf("InsertActivity: %v", err)
		}
	}

	report, err := store.ActivitySpeedReport(ctx, ActivitySpeedQuery{BucketSeconds: 86400})
	if err != nil {
		t.Fatalf("ActivitySpeedReport: %v", err)
	}
	points := report.Series[0].Points
	if len(points) != 4 {
		t.Fatalf("points = %+v, want 4 daily buckets", points)
	}
	for i, point := range points {
		switch i {
		case 0, 3:
			if point.Requests != 1 || point.DecodeTPS != 20 {
				t.Fatalf("point %d = %+v, want the measured request", i, point)
			}
		default:
			if point.Requests != 0 || point.PrefillTPS >= 0 || point.DecodeTPS >= 0 || point.TTFTMs >= 0 {
				t.Fatalf("point %d = %+v, want an unknown-value gap", i, point)
			}
		}
	}
}

func TestStore_ActivitySpeedReportContextBuckets(t *testing.T) {
	ctx := context.Background()
	store, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	base := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC).Unix()
	entries := []ActivityLogEntry{
		speedTestEntry("m1", base, 4096, 100, 900, 30, 100),
		speedTestEntry("m1", base+1, 4097, 300, 700, 20, 200),
		speedTestEntry("m1", base+2, 1024*1024, 50, 200, 40, 300),
		speedTestEntry("m1", base+3, 1024*1024+1, 50, 100, 10, 400),
		// Zero prompt tokens carry no length information and are skipped.
		speedTestEntry("m1", base+4, 0, 50, 500, 25, 500),
	}
	for _, entry := range entries {
		if _, err := store.InsertActivity(ctx, entry); err != nil {
			t.Fatalf("InsertActivity: %v", err)
		}
	}

	report, err := store.ActivitySpeedReport(ctx, ActivitySpeedQuery{BucketSeconds: 3600})
	if err != nil {
		t.Fatalf("ActivitySpeedReport: %v", err)
	}
	if len(report.Context) != 1 {
		t.Fatalf("context series = %+v, want 1 model", report.Context)
	}
	buckets := report.Context[0].Buckets
	wantLabels := []string{"<=4k", "4k-8k", "512k-1M", ">1M"}
	if len(buckets) != len(wantLabels) {
		t.Fatalf("buckets = %+v, want %v", buckets, wantLabels)
	}
	for i, bucket := range buckets {
		if bucket.Label != wantLabels[i] {
			t.Fatalf("bucket %d label = %q, want %q", i, bucket.Label, wantLabels[i])
		}
	}
	// Exactly 4096 prompt tokens is a 4k context, so it lands in the first
	// bucket and 4097 in the next one.
	if buckets[0].Requests != 1 || buckets[0].PrefillTPS != 900 || buckets[0].AvgInputTokens != 4096 {
		t.Fatalf("<=4k bucket = %+v", buckets[0])
	}
	if buckets[1].Requests != 1 || buckets[1].PrefillTPS != 700 || buckets[1].AvgInputTokens != 4097 {
		t.Fatalf("4k-8k bucket = %+v", buckets[1])
	}
	// Exactly 1M tokens stays inside the 512k-1M bucket; one more crosses over.
	if buckets[2].Requests != 1 || buckets[2].PrefillTPS != 200 || buckets[2].MaxTokens != 1024*1024 {
		t.Fatalf("512k-1M bucket = %+v", buckets[2])
	}
	if buckets[3].Requests != 1 || buckets[3].PrefillTPS != 100 || buckets[3].MaxTokens != 0 {
		t.Fatalf(">1M bucket = %+v", buckets[3])
	}
}

func TestStore_ActivitySpeedReportFiltersAndAutoBucket(t *testing.T) {
	ctx := context.Background()
	store, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	base := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC).Unix()
	rows := []ActivityLogEntry{
		speedTestEntry("m1", base, 1000, 10, 100, 20, 50),
		speedTestEntry("m1", base+3600, 1000, 10, 100, 20, 50),
		speedTestEntry("m2", base+7200, 2000, 10, 300, 30, 60),
	}
	for _, entry := range rows {
		if _, err := store.InsertActivity(ctx, entry); err != nil {
			t.Fatalf("InsertActivity: %v", err)
		}
	}

	scoped, err := store.ActivitySpeedReport(ctx, ActivitySpeedQuery{
		ActivityFilter: ActivityFilter{Models: []string{"m2"}},
	})
	if err != nil {
		t.Fatalf("ActivitySpeedReport: %v", err)
	}
	if len(scoped.Series) != 1 || scoped.Series[0].Model != "m2" {
		t.Fatalf("scoped series = %+v, want only m2", scoped.Series)
	}
	if len(scoped.Context) != 1 || scoped.Context[0].Model != "m2" {
		t.Fatalf("scoped context = %+v, want only m2", scoped.Context)
	}

	windowed, err := store.ActivitySpeedReport(ctx, ActivitySpeedQuery{
		ActivityFilter: ActivityFilter{Start: time.Unix(base+1800, 0), End: time.Unix(base+7200, 0)},
	})
	if err != nil {
		t.Fatalf("ActivitySpeedReport: %v", err)
	}
	if len(windowed.Series) != 2 {
		t.Fatalf("windowed series = %+v, want m1 and m2", windowed.Series)
	}
	for _, series := range windowed.Series {
		for _, point := range series.Points {
			if !point.Timestamp.Equal(time.Unix(base+3600, 0)) && !point.Timestamp.Equal(time.Unix(base+7200, 0)) {
				t.Fatalf("%s point %s outside the requested window", series.Model, point.Timestamp)
			}
		}
	}

	// An unbounded filter auto-selects the bucket width: a two-hour span picks
	// five-minute buckets, which is 24 points rather than one collapsed point.
	if autoBucketSeconds(time.Unix(base, 0), time.Unix(base+7200, 0)) != 300 {
		t.Fatalf("auto bucket for a 2h span = %ds, want 300", autoBucketSeconds(time.Unix(base, 0), time.Unix(base+7200, 0)))
	}
	if got := autoBucketSeconds(time.Time{}, time.Time{}); got != 3600 {
		t.Fatalf("auto bucket for an empty span = %ds, want 3600", got)
	}

	// Explicit widths are clamped into the store's supported range.
	auto, err := store.ActivitySpeedReport(ctx, ActivitySpeedQuery{BucketSeconds: 1})
	if err != nil {
		t.Fatalf("ActivitySpeedReport: %v", err)
	}
	if auto.BucketSeconds != 60 {
		t.Fatalf("bucket seconds = %d, want the 60s floor", auto.BucketSeconds)
	}
	wide, err := store.ActivitySpeedReport(ctx, ActivitySpeedQuery{BucketSeconds: 30 * 86400})
	if err != nil {
		t.Fatalf("ActivitySpeedReport: %v", err)
	}
	if wide.BucketSeconds != 86400 {
		t.Fatalf("bucket seconds = %d, want the 1d ceiling", wide.BucketSeconds)
	}
}

func TestStore_ActivitySpeedReportKeepsNewestPoints(t *testing.T) {
	ctx := context.Background()
	store, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	// A five-day span at five-minute buckets is 1441 points, past
	// maxSpeedPoints, so only the most recent buckets are charted.
	base := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC).Unix()
	for hour := 0; hour < 5*24+1; hour++ {
		if _, err := store.InsertActivity(ctx, speedTestEntry("m1", base+int64(hour)*3600, 1000, 10, 100, 20, 50)); err != nil {
			t.Fatalf("InsertActivity: %v", err)
		}
	}

	report, err := store.ActivitySpeedReport(ctx, ActivitySpeedQuery{BucketSeconds: 300})
	if err != nil {
		t.Fatalf("ActivitySpeedReport: %v", err)
	}
	points := report.Series[0].Points
	if len(points) != maxSpeedPoints {
		t.Fatalf("points = %d, want the %d cap", len(points), maxSpeedPoints)
	}
	newest := points[len(points)-1]
	if !newest.Timestamp.Equal(time.Unix(base+5*24*3600, 0)) {
		t.Fatalf("newest point = %s, want the latest request", newest.Timestamp)
	}
	oldest := points[0]
	if !oldest.Timestamp.Equal(time.Unix(base+5*24*3600-int64(maxSpeedPoints-1)*300, 0)) {
		t.Fatalf("oldest point = %s, want the newest window of points", oldest.Timestamp)
	}
	// The window can start on a boundary before the first request it contains,
	// which is an empty bucket rather than a measured zero.
	if oldest.Requests == 0 && (oldest.PrefillTPS >= 0 || oldest.DecodeTPS >= 0 || oldest.TTFTMs >= 0) {
		t.Fatalf("oldest point = %+v, want unknown metrics for an empty bucket", oldest)
	}
}

func TestStore_ActivitySpeedReportOnNilStoreReturnsError(t *testing.T) {
	var store *Store
	if _, err := store.ActivitySpeedReport(context.Background(), ActivitySpeedQuery{}); err == nil {
		t.Fatal("ActivitySpeedReport on nil store unexpectedly succeeded")
	}
}

// Physically impossible rates (collapsed timing windows, buggy usage frames)
// recorded before the ingest-time clamps existed must not skew the speed
// averages; negative rates remain the "not measured" sentinel.
func TestStore_ActivitySpeedReportExcludesImplausibleRates(t *testing.T) {
	ctx := context.Background()
	s, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	entries := []ActivityLogEntry{
		speedTestEntry("m", 1700000000, 100, 50, 400, 45, 1200),
		// Absurd decode rate from the collapsed-window bug: excluded from the
		// report entirely.
		speedTestEntry("m", 1700000010, 100, 50, 400, 418713, 2500),
		// A prompt rate above the cache-hit ceiling: excluded.
		speedTestEntry("m", 1700000020, 100, 50, 71310, 200001, 1200),
		// A cache-hit prompt rate inside the cap stays.
		speedTestEntry("m", 1700000030, 100, 50, 70000, 45, 1200),
		// Not-measured sentinels stay (requests count, averages ignore).
		speedTestEntry("m", 1700000040, 100, 50, -1, -1, -1),
	}
	for _, entry := range entries {
		if _, err := s.InsertActivity(ctx, entry); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	report, err := s.ActivitySpeedReport(ctx, ActivitySpeedQuery{BucketSeconds: 3600})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(report.Series) != 1 {
		t.Fatalf("series = %d, want 1", len(report.Series))
	}
	buckets := report.Series[0].Points
	// The two rows above the caps drop out of the report entirely; the other
	// three (including the not-measured sentinel) count.
	if len(buckets) == 0 || buckets[0].Requests != 3 {
		t.Fatalf("requests = %+v, want 3 rows counted", buckets)
	}
	for _, bucket := range buckets {
		if bucket.DecodeTPS > 2000 {
			t.Fatalf("decode avg %v exceeds the cap; absurd rows leaked in", bucket.DecodeTPS)
		}
		if bucket.PrefillTPS > 150000 {
			t.Fatalf("prefill avg %v exceeds the cap; absurd rows leaked in", bucket.PrefillTPS)
		}
	}
	// Decode average: 45 and 45 (418713 and 200001 excluded, -1 ignored).
	if got := buckets[0].DecodeTPS; got > 0 && (got < 44 || got > 46) {
		t.Fatalf("decode avg = %v, want ~45", got)
	}
	// Prefill average: 400 and 70000 (71310 dropped with its row, -1 ignored).
	if got := buckets[0].PrefillTPS; got > 0 && (got < 35000 || got > 35400) {
		t.Fatalf("prefill avg = %v, want ~35200", got)
	}
}
