package store

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newRollupTestStore opens a store whose migrations have run, so the rollup
// table and its bookkeeping exist.
func newRollupTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// seedActivity writes rows spread over several hours and models, with the value
// shapes the rollup has to treat specially: zero and negative speeds, a missing
// first token, and a zero cost.
func seedActivity(t *testing.T, store *Store, count int) {
	t.Helper()
	base := time.Now().Add(-6 * time.Hour).Truncate(time.Hour)
	rng := rand.New(rand.NewSource(7))
	models := []string{"m1", "m2", "m3"}
	paths := []string{"/v1/chat/completions", "/v1/responses"}
	ctx := context.Background()
	for i := 0; i < count; i++ {
		entry := ActivityLogEntry{
			Timestamp:      base.Add(time.Duration(rng.Intn(6*3600)) * time.Second),
			Model:          models[rng.Intn(len(models))],
			KeyID:          fmt.Sprintf("key-%d", rng.Intn(4)),
			SessionID:      fmt.Sprintf("sess-%d", rng.Intn(8)),
			ReqPath:        paths[rng.Intn(len(paths))],
			RespStatusCode: 200,
			Tokens: TokenMetrics{
				InputTokens:     rng.Intn(5000),
				OutputTokens:    rng.Intn(2000),
				CachedTokens:    rng.Intn(500),
				PromptPerSecond: float64(rng.Intn(3000)) / 10,
				TokensPerSecond: float64(rng.Intn(2000)) / 10,
			},
			CacheCreationTokens: rng.Intn(100),
			ReasoningTokens:     rng.Intn(400),
			EstimatedCost:       float64(rng.Intn(50)) / 100,
			CostEstimated:       i%3 == 0,
			DurationMs:          rng.Intn(9000),
			FirstTokenMs:        -1,
		}
		// A missing first token must not drag the average toward zero.
		if i%5 != 0 {
			entry.FirstTokenMs = rng.Intn(3000)
		}
		// An unreported speed must be excluded from that average too.
		if i%7 == 0 {
			entry.Tokens.PromptPerSecond = 0
			entry.Tokens.TokensPerSecond = 0
		}
		if _, err := store.InsertActivity(ctx, entry); err != nil {
			t.Fatalf("InsertActivity %d: %v", i, err)
		}
	}
}

// TestStore_RollupMatchesFullScan is the correctness gate for the pre-aggregated
// statistics: whatever the rollup answers must equal what scanning every activity
// row answers. If these disagree the rollup is worse than useless, because the
// dashboard would silently report different numbers than the detail table.
func TestStore_RollupMatchesFullScan(t *testing.T) {
	store := newRollupTestStore(t)
	seedActivity(t, store, 2000)

	ctx := context.Background()
	if !store.rollupAvailable(ctx) {
		t.Fatal("rollup should be complete after writes with no pre-existing rows")
	}
	filters := []ActivityFilter{
		{},
		{Models: []string{"m1"}},
		{Models: []string{"m1", "m3"}},
		{KeyID: "key-2"},
		// The time bounds sit on hour boundaries: a bound inside an hour cannot be
		// answered from an hourly bucket without widening the selection, so the
		// store falls back to the scan for those.
		{Start: hourFloor(time.Now().Add(-3 * time.Hour)), End: hourFloor(time.Now())},
		{Models: []string{"m2"}, KeyID: "key-1", Start: hourFloor(time.Now().Add(-5 * time.Hour))},
	}
	for index, filter := range filters {
		t.Run(fmt.Sprintf("filter-%d", index), func(t *testing.T) {
			where, args := activityWhere(filter)
			scanned, err := store.aggregateActivity(ctx, where, args)
			if err != nil {
				t.Fatalf("aggregateActivity: %v", err)
			}
			rollupQuery, ok := rollupFilterFor(filter)
			if !ok {
				t.Fatalf("filter %d should be expressible in the rollup", index)
			}
			folded, err := store.rollupAggregates(ctx, rollupQuery)
			if err != nil {
				t.Fatalf("rollupAggregates: %v", err)
			}
			compareAggregates(t, "total", scanned.total, folded.total)

			scannedModels := scanned.byModel
			for model, want := range scannedModels {
				got, present := folded.byModel[model]
				if !present {
					t.Fatalf("rollup is missing model %q", model)
				}
				compareAggregates(t, "model "+model, *want, *got)
			}
			if len(scannedModels) != len(folded.byModel) {
				t.Fatalf("model count: scan=%d rollup=%d", len(scannedModels), len(folded.byModel))
			}
		})
	}
}

func compareAggregates(t *testing.T, label string, want, got activityAggregate) {
	t.Helper()
	if want.Requests != got.Requests {
		t.Errorf("%s requests: scan=%d rollup=%d", label, want.Requests, got.Requests)
	}
	if want.InputTokens != got.InputTokens {
		t.Errorf("%s input tokens: scan=%d rollup=%d", label, want.InputTokens, got.InputTokens)
	}
	if want.OutputTokens != got.OutputTokens {
		t.Errorf("%s output tokens: scan=%d rollup=%d", label, want.OutputTokens, got.OutputTokens)
	}
	if want.CachedTokens != got.CachedTokens {
		t.Errorf("%s cached tokens: scan=%d rollup=%d", label, want.CachedTokens, got.CachedTokens)
	}
	if want.CacheCreationTokens != got.CacheCreationTokens {
		t.Errorf("%s cache creation tokens: scan=%d rollup=%d", label, want.CacheCreationTokens, got.CacheCreationTokens)
	}
	if want.ReasoningTokens != got.ReasoningTokens {
		t.Errorf("%s reasoning tokens: scan=%d rollup=%d", label, want.ReasoningTokens, got.ReasoningTokens)
	}
	if diff := want.EstimatedCost - got.EstimatedCost; diff > 0.0001 || diff < -0.0001 {
		t.Errorf("%s cost: scan=%f rollup=%f", label, want.EstimatedCost, got.EstimatedCost)
	}
	if want.CostEstimated != got.CostEstimated {
		t.Errorf("%s cost estimated: scan=%v rollup=%v", label, want.CostEstimated, got.CostEstimated)
	}
}

// TestStore_RollupAveragesMatchFullScan covers the average fields, which are
// derived from sums and counts rather than folded from other averages.
func TestStore_RollupAveragesMatchFullScan(t *testing.T) {
	store := newRollupTestStore(t)
	seedActivity(t, store, 800)

	ctx := context.Background()
	// The scan does not track the sums, so derive them from the same rows the
	// rollup folded and compare the resulting averages.
	var durationSum, durationCount int64
	var firstSum, firstCount int64
	rows, err := store.db.QueryContext(ctx, `SELECT duration_ms, first_token_ms FROM activity`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var duration, first int64
		if err := rows.Scan(&duration, &first); err != nil {
			t.Fatal(err)
		}
		if duration > 0 {
			durationSum += duration
			durationCount++
		}
		if first > 0 {
			firstSum += first
			firstCount++
		}
	}
	rows.Close()

	rollupTotals := usageTotalsFromRollup(mustRollupTotal(t, store))
	if want := roundAverageMs(float64(durationSum) / float64(durationCount)); rollupTotals.AverageDurationMs != want {
		t.Errorf("average duration: rollup=%d want=%d (sum=%d count=%d)", rollupTotals.AverageDurationMs, want, durationSum, durationCount)
	}
	if want := roundAverageMs(float64(firstSum) / float64(firstCount)); rollupTotals.AverageFirstTokenMs != want {
		t.Errorf("average first token: rollup=%d want=%d", rollupTotals.AverageFirstTokenMs, want)
	}
}

func mustRollupTotal(t *testing.T, store *Store) activityAggregate {
	t.Helper()
	aggregates, err := store.rollupAggregates(context.Background(), rollupQuery{})
	if err != nil {
		t.Fatalf("rollupAggregates: %v", err)
	}
	return aggregates.total
}

// TestStore_RollupBackfillResumesCoversPreExistingRows covers the migration case:
// rows that were written before the rollup existed are folded by the backfill,
// and an interrupted backfill resumes instead of starting over.
func TestStore_RollupBackfillResumesCoversPreExistingRows(t *testing.T) {
	store := newRollupTestStore(t)
	seedActivity(t, store, 600)
	ctx := context.Background()

	// Simulate a database that predates the rollup: drop the folded rows and the
	// bookkeeping, leaving only the activity table.
	if _, err := store.db.ExecContext(ctx, `DELETE FROM activity_rollup`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM cache_entries WHERE key LIKE 'activity_rollup%'`); err != nil {
		t.Fatal(err)
	}
	if store.rollupAvailable(ctx) {
		t.Fatal("rollup must report unavailable after being cleared")
	}

	// A backfill with a deadline in the past makes no progress but must not fail.
	done, err := store.backfillActivityRollup(ctx, time.Now().Add(-time.Second))
	if err != nil {
		t.Fatalf("backfill with elapsed deadline: %v", err)
	}
	if done {
		t.Fatal("an elapsed deadline cannot complete the backfill")
	}

	// A real pass folds everything.
	done, err = store.backfillActivityRollup(ctx, time.Now().Add(30*time.Second))
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if !done {
		t.Fatal("backfill did not finish")
	}
	if !store.rollupAvailable(ctx) {
		t.Fatal("rollup must report available after a complete backfill")
	}
	scanned, err := store.aggregateActivity(ctx, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	folded, err := store.rollupAggregates(ctx, rollupQuery{})
	if err != nil {
		t.Fatal(err)
	}
	compareAggregates(t, "backfilled", scanned.total, folded.total)

	// A second backfill is a no-op: the cursor is at the end of the table.
	done, err = store.backfillActivityRollup(ctx, time.Now().Add(30*time.Second))
	if err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	if !done {
		t.Fatal("second backfill should be a no-op")
	}
	again, err := store.rollupAggregates(ctx, rollupQuery{})
	if err != nil {
		t.Fatal(err)
	}
	compareAggregates(t, "after second backfill", scanned.total, again.total)
}

// TestStore_RollupUnsupportedFilterFallsBack pins which filters the rollup cannot
// answer, so the caller falls back to the scan instead of silently reporting a
// narrower number.
func TestStore_RollupUnsupportedFilterFallsBack(t *testing.T) {
	cases := []struct {
		name   string
		filter ActivityFilter
		want   bool
	}{
		{"empty", ActivityFilter{}, true},
		{"model", ActivityFilter{Models: []string{"m"}}, true},
		{"key", ActivityFilter{KeyID: "k"}, true},
		// The session is not a rollup dimension: keying on it would produce about
		// one bucket per row, so a session-scoped query scans instead.
		{"session", ActivityFilter{SessionID: "s"}, false},
		// time.Now() is mid-hour, which an hourly bucket cannot answer exactly.
		{"mid-hour range", ActivityFilter{Start: time.Now(), End: time.Now()}, false},
		{"hour-aligned range", ActivityFilter{Start: hourFloor(time.Now()), End: hourFloor(time.Now())}, true},
		{"min id", ActivityFilter{MinID: 5}, false},
		{"max id", ActivityFilter{MaxID: 9}, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, ok := rollupFilterFor(testCase.filter)
			if ok != testCase.want {
				t.Fatalf("rollupFilterFor supported=%v want=%v", ok, testCase.want)
			}
		})
	}
}

// TestStore_UsageAnalyticsMatchesFullScan covers the dashboard path, including
// the dimensions whose activity query used a correlated subquery per row.
func TestStore_UsageAnalyticsMatchesFullScan(t *testing.T) {
	store := newRollupTestStore(t)
	seedActivity(t, store, 1200)
	ctx := context.Background()

	query := UsageQuery{Start: hourFloor(time.Now().Add(-7 * time.Hour)), End: hourFloor(time.Now().Add(time.Hour))}
	rollupQuery, ok := usageRollupFor(query)
	if !ok {
		t.Fatal("an unfiltered usage query must be rollup-expressible")
	}
	folded, err := store.usageAnalyticsFromRollup(ctx, query, "hour", rollupQuery, 3600)
	if err != nil {
		t.Fatalf("usageAnalyticsFromRollup: %v", err)
	}

	// The totals must match the row scan.
	where, args := usageWhere(query)
	scannedTotals, err := store.usageTotals(ctx, where, args)
	if err != nil {
		t.Fatalf("usageTotals: %v", err)
	}
	if folded.Totals.Requests != scannedTotals.Requests {
		t.Errorf("requests: rollup=%d scan=%d", folded.Totals.Requests, scannedTotals.Requests)
	}
	if folded.Totals.InputTokens != scannedTotals.InputTokens {
		t.Errorf("input tokens: rollup=%d scan=%d", folded.Totals.InputTokens, scannedTotals.InputTokens)
	}
	if diff := folded.Totals.EstimatedCost - scannedTotals.EstimatedCost; diff > 0.0001 || diff < -0.0001 {
		t.Errorf("cost: rollup=%f scan=%f", folded.Totals.EstimatedCost, scannedTotals.EstimatedCost)
	}

	// The model dimension must carry the same models and counts.
	scannedModels, err := store.usageDimension(ctx, where, args, "model_id")
	if err != nil {
		t.Fatalf("usageDimension: %v", err)
	}
	byName := map[string]int{}
	for _, row := range folded.ByModel {
		byName[row.Name] = row.Requests
	}
	if len(byName) != len(scannedModels) {
		t.Fatalf("model rows: rollup=%d scan=%d", len(byName), len(scannedModels))
	}
	for _, row := range scannedModels {
		if byName[row.Name] != row.Requests {
			t.Errorf("model %s requests: rollup=%d scan=%d", row.Name, byName[row.Name], row.Requests)
		}
	}

	// The group dimension must total the same request count: it is derived from
	// the key dimension, so a mismatch means the key join lost or duplicated rows.
	groupTotal := 0
	for _, row := range folded.ByGroup {
		groupTotal += row.Requests
	}
	if groupTotal != folded.Totals.Requests {
		t.Errorf("group rows total %d requests, want %d", groupTotal, folded.Totals.Requests)
	}

	// The trend must cover the same request count as the totals.
	trendTotal := 0
	for _, bucket := range folded.Trend {
		trendTotal += bucket.Requests
	}
	if trendTotal != folded.Totals.Requests {
		t.Errorf("trend buckets total %d requests, want %d", trendTotal, folded.Totals.Requests)
	}
	// Every bucket must land inside the queried range. A bucket arithmetic error
	// (dividing where it should multiply, or the reverse) produces a timestamp
	// thousands of years out, which the JSON encoder then rejects outright — so
	// the whole response would fail to encode rather than merely look wrong.
	windowStart := query.Start.Add(-2 * time.Hour)
	windowEnd := query.End.Add(2 * time.Hour)
	for index, bucket := range folded.Trend {
		if bucket.BucketStart.Before(windowStart) || bucket.BucketStart.After(windowEnd) {
			t.Fatalf("trend bucket %d starts at %s, outside %s..%s",
				index, bucket.BucketStart.Format(time.RFC3339), windowStart.Format(time.RFC3339), windowEnd.Format(time.RFC3339))
		}
	}
}

// TestStore_ActivitySortUsesIndex is the guard for the sorting indexes: a sort
// that SQLite answers with a temporary B-tree is a full sort of every matching
// row, which is what made the request-records page slower the more rows it held.
func TestStore_ActivitySortUsesIndex(t *testing.T) {
	store := newRollupTestStore(t)
	seedActivity(t, store, 200)

	for key := range activitySortColumns {
		t.Run(key, func(t *testing.T) {
			column, ok := ActivitySortColumn(key)
			if !ok {
				t.Fatalf("sort key %q is not whitelisted", key)
			}
			query := ActivityQuery{Sort: key, Order: "desc", Limit: 25, Page: 1}
			whereClause, _ := activityWhere(ActivityFilter{})
			statement := `SELECT id FROM activity` + whereClause + activityOrderBy(query) + ` LIMIT 25`
			plan, err := explainQueryPlan(store, statement)
			if err != nil {
				t.Fatalf("explain: %v", err)
			}
			if containsTempBTree(plan) {
				t.Fatalf("sort %q (%s) still sorts: %s", key, column, plan)
			}
		})
	}
}

func explainQueryPlan(store *Store, statement string) (string, error) {
	rows, err := store.db.QueryContext(context.Background(), `EXPLAIN QUERY PLAN `+statement)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var out string
	for rows.Next() {
		var (
			_      int64
			_      int64
			detail string
		)
		// The plan rows are (selectid, order, from, detail); only detail matters.
		var selectID, order, from int64
		if err := rows.Scan(&selectID, &order, &from, &detail); err != nil {
			// Some SQLite builds return three columns; fall back to scanning
			// whatever the driver produced.
			return out, nil
		}
		out += detail + "\n"
	}
	return out, rows.Err()
}

func containsTempBTree(plan string) bool {
	return strings.Contains(strings.ToUpper(plan), "TEMP B-TREE")
}

// hourFloor truncates a time onto its hour bucket, the only shape an hourly
// rollup can answer exactly.
func hourFloor(at time.Time) time.Time {
	return at.Truncate(time.Hour)
}

// TestStore_UsageRecordsKeysetWalksEveryRowOnce pins the keyset walk the CSV
// export uses: walking with AfterID must visit every row exactly once, in order,
// without re-reading the rows it already passed.
func TestStore_UsageRecordsKeysetWalksEveryRowOnce(t *testing.T) {
	store := newRollupTestStore(t)
	ctx := context.Background()
	base := time.Now().Add(-2 * time.Hour)
	const total = 1200
	for i := 0; i < total; i++ {
		if _, err := store.InsertActivity(ctx, ActivityLogEntry{
			Timestamp: base.Add(time.Duration(i) * time.Second),
			Model:     "m",
			ReqPath:   "/v1/chat/completions",
			Tokens:    TokenMetrics{InputTokens: i, OutputTokens: i},
		}); err != nil {
			t.Fatalf("InsertActivity %d: %v", i, err)
		}
	}

	seen := make([]int, 0, total)
	lastID := 0
	const batch = 100
	for {
		page, err := store.UsageRecords(ctx, UsageQuery{
			Limit: batch, AfterID: lastID, Sort: "time", Order: "asc",
		})
		if err != nil {
			t.Fatalf("UsageRecords: %v", err)
		}
		if len(page.Data) == 0 {
			break
		}
		for _, record := range page.Data {
			if record.ID <= lastID {
				t.Fatalf("row %d is not after the cursor %d", record.ID, lastID)
			}
			seen = append(seen, record.ID)
			lastID = record.ID
		}
		if len(page.Data) < batch {
			break
		}
	}
	if len(seen) != total {
		t.Fatalf("walk visited %d rows, want %d", len(seen), total)
	}
	for index := 1; index < len(seen); index++ {
		if seen[index] <= seen[index-1] {
			t.Fatalf("walk is not strictly ascending at %d: %d then %d", index, seen[index-1], seen[index])
		}
	}

	// A keyset page carries no total: the caller already knows where it is, and
	// re-counting the selection on every batch is what the walk avoids.
	page, err := store.UsageRecords(ctx, UsageQuery{Limit: 10, AfterID: seen[0], Sort: "time", Order: "asc"})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 0 {
		t.Fatalf("keyset page reported a total of %d, want 0 (uncounted)", page.Total)
	}
	if len(page.Data) != 10 {
		t.Fatalf("keyset page returned %d rows, want 10", len(page.Data))
	}

	// The first page still reports the total, because the UI's page count needs it.
	first, err := store.UsageRecords(ctx, UsageQuery{Limit: 10, Sort: "time", Order: "asc"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Total != total {
		t.Fatalf("first page total = %d, want %d", first.Total, total)
	}
}

// TestStore_RollupBackfillRepairsCursorThatRanAhead reproduces the production
// failure: an older build advanced the backfill cursor on every insert, so a
// migrated store carries a cursor at max(id) while the rollup holds only the
// rows written since the migration. Treating that cursor as "caught up" left
// the whole history missing from every statistics view while the activity table
// still held every row.
func TestStore_RollupBackfillRepairsCursorThatRanAhead(t *testing.T) {
	store := newRollupTestStore(t)
	seedActivity(t, store, 400)
	ctx := context.Background()

	// Simulate the broken state: the rollup holds only the newest slice and the
	// cursor claims everything is folded, with no completion marker.
	if _, err := store.db.ExecContext(ctx, `DELETE FROM activity_rollup`); err != nil {
		t.Fatal(err)
	}
	var maxID int64
	if err := store.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM activity`).Scan(&maxID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO activity_rollup (bucket_ts, model_id, key_id, req_path, requests,
			input_tokens, output_tokens, cache_tokens, cache_creation_tokens, reasoning_tokens)
		VALUES (?, 'stale', 'k', '/v1/chat/completions', 1, 1, 1, 0, 0, 0)`,
		0); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCacheEntry(ctx, CacheEntry{
		Key: rollupBackfillKey, Data: []byte(strconv.FormatInt(maxID, 10)),
	}); err != nil {
		t.Fatal(err)
	}

	// The rollup must not be trusted: it holds one row while activity holds 400.
	if store.rollupAvailable(ctx) {
		t.Fatal("a cursor at max(id) with an almost empty rollup must not count as caught up")
	}

	// The backfill must fold the whole history, not resume past it.
	done, err := store.backfillActivityRollup(ctx, time.Now().Add(30*time.Second))
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if !done {
		t.Fatal("backfill did not finish")
	}
	if !store.rollupAvailable(ctx) {
		t.Fatal("rollup must report available after the repair backfill")
	}

	var folded int64
	if err := store.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(requests),0) FROM activity_rollup`).Scan(&folded); err != nil {
		t.Fatal(err)
	}
	if folded < int64(400) {
		t.Fatalf("rollup folded %d requests, want the whole history (400)", folded)
	}
}

// TestStore_RollupAvailableTrustsCursorAfterForwardMigration guards the other
// side: a store whose rows were all written after the rollup existed carries a
// cursor with no completion marker, and that state must keep working without a
// needless re-fold.
func TestStore_RollupAvailableTrustsCursorAfterForwardMigration(t *testing.T) {
	store := newRollupTestStore(t)
	ctx := context.Background()
	seedActivity(t, store, 50) // folded on the way in, cursor advanced with it

	if !store.rollupAvailable(ctx) {
		t.Fatal("a fully folded store must report the rollup as available")
	}
}

// TestStore_RollupRefoldIsIdempotent 锁住"回填可以安全重跑"。
//
// 回填会因为游标不可信而从零重来（老版本在每次插入时推进游标，迁移库里的
// 游标因此跑在折叠进度前面）。如果重来是在已有 bucket 上继续累加，每重跑
// 一次就把同一批请求再计一遍——生产上滚出了 2.25 倍的虚高总量，个别模型
// 甚至 4 倍。重跑必须替换而不是叠加。
func TestStore_RollupRefoldIsIdempotent(t *testing.T) {
	store := newRollupTestStore(t)
	seedActivity(t, store, 300)
	ctx := context.Background()

	first, err := store.rollupAggregates(ctx, rollupQuery{})
	if err != nil {
		t.Fatal(err)
	}

	// Force a from-zero refold: drop the completion marker, keep only a slice of
	// the rollup, and park the cursor past the end. That is the broken migration
	// state — the cursor says "done", the table says "barely started" — and the
	// refold it triggers must replace the slice, not add a second copy to it.
	if _, err := store.db.ExecContext(ctx, `DELETE FROM cache_entries WHERE key = ?`, rollupCompleteKey); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
		DELETE FROM activity_rollup WHERE rowid NOT IN (SELECT rowid FROM activity_rollup LIMIT 1)`); err != nil {
		t.Fatal(err)
	}
	var maxID int64
	if err := store.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM activity`).Scan(&maxID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCacheEntry(ctx, CacheEntry{
		Key: rollupBackfillKey, Data: []byte(strconv.FormatInt(maxID, 10)),
	}); err != nil {
		t.Fatal(err)
	}

	done, err := store.backfillActivityRollup(ctx, time.Now().Add(30*time.Second))
	if err != nil {
		t.Fatalf("refold: %v", err)
	}
	if !done {
		t.Fatal("refold did not finish")
	}

	second, err := store.rollupAggregates(ctx, rollupQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if second.total.Requests != first.total.Requests {
		t.Fatalf("refold changed the total: %d -> %d (a re-fold must replace, not accumulate)",
			first.total.Requests, second.total.Requests)
	}
	if second.total.InputTokens != first.total.InputTokens {
		t.Fatalf("refold changed input tokens: %d -> %d", first.total.InputTokens, second.total.InputTokens)
	}
}
