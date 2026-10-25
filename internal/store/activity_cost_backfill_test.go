package store

import (
	"context"
	"math"
	"testing"
	"time"
)

// seedUnpricedActivity inserts n activity rows with no cost, all under the
// same model/key/path so their rollup bucket is predictable, and returns the
// ids in insertion order.
func seedUnpricedActivity(t *testing.T, st *Store, n int) []int {
	t.Helper()
	ctx := context.Background()
	base := time.Now().Add(-2 * time.Hour).Truncate(time.Hour)
	ids := make([]int, 0, n)
	for i := 0; i < n; i++ {
		entry, err := st.InsertActivity(ctx, ActivityLogEntry{
			Timestamp: base.Add(time.Duration(i) * time.Second),
			Model:     "m1",
			KeyID:     "key-1",
			ReqPath:   "/v1/chat/completions",
			Tokens: TokenMetrics{
				InputTokens:  1000,
				OutputTokens: 200,
				CachedTokens: 400,
			},
		})
		if err != nil {
			t.Fatalf("InsertActivity %d: %v", i, err)
		}
		ids = append(ids, entry.ID)
	}
	return ids
}

// rollupCost reads the aggregated cost of the seeded bucket.
func rollupCost(t *testing.T, st *Store) (float64, bool) {
	t.Helper()
	var (
		cost      float64
		estimated int64
	)
	err := st.db.QueryRowContext(context.Background(),
		`SELECT COALESCE(SUM(estimated_cost), 0), MAX(cost_estimated) FROM activity_rollup WHERE model_id = 'm1'`).
		Scan(&cost, &estimated)
	if err != nil {
		t.Fatalf("read rollup cost: %v", err)
	}
	return cost, estimated != 0
}

func almostEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestStore_ApplyEstimatedCostBatch_PricesRowsAndRollup is the core backfill
// contract: unpriced activity rows get their cost persisted, and the same
// amount shows up in the rollup the dashboard reads.
func TestStore_ApplyEstimatedCostBatch_PricesRowsAndRollup(t *testing.T) {
	st := newRollupTestStore(t)
	ids := seedUnpricedActivity(t, st, 3)
	ctx := context.Background()

	rows, err := st.NextUnpricedActivity(ctx, 0, 100)
	if err != nil {
		t.Fatalf("NextUnpricedActivity: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("unpriced rows = %d, want 3", len(rows))
	}

	updates := make([]CostBackfillUpdate, 0, len(rows))
	for _, row := range rows {
		updates = append(updates, CostBackfillUpdate{
			ID:        row.ID,
			Cost:      1.25,
			TsCreated: row.TsCreated,
			Model:     row.Model,
			KeyID:     row.KeyID,
			ReqPath:   row.ReqPath,
		})
	}
	if err := st.ApplyEstimatedCostBatch(ctx, updates); err != nil {
		t.Fatalf("ApplyEstimatedCostBatch: %v", err)
	}

	for _, id := range ids {
		var cost float64
		var estimated int64
		if err := st.db.QueryRowContext(ctx,
			`SELECT estimated_cost, cost_estimated FROM activity WHERE id = ?`, id).
			Scan(&cost, &estimated); err != nil {
			t.Fatalf("read activity %d: %v", id, err)
		}
		if estimated != 1 || !almostEqual(cost, 1.25) {
			t.Fatalf("activity %d: cost=%v estimated=%d, want 1.25/1", id, cost, estimated)
		}
	}

	cost, estimated := rollupCost(t, st)
	if !estimated || !almostEqual(cost, 3.75) {
		t.Fatalf("rollup cost=%v estimated=%v, want 3.75/true", cost, estimated)
	}

	// The rows are priced now; the next scan must not see them again.
	rows, err = st.NextUnpricedActivity(ctx, 0, 100)
	if err != nil {
		t.Fatalf("NextUnpricedActivity after batch: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("unpriced rows after batch = %d, want 0", len(rows))
	}
}

// TestStore_ApplyEstimatedCostBatch_SkipsAlreadyPriced guards the concurrency
// contract: a row priced between the read and the write must not be charged
// twice.
func TestStore_ApplyEstimatedCostBatch_SkipsAlreadyPriced(t *testing.T) {
	st := newRollupTestStore(t)
	ids := seedUnpricedActivity(t, st, 1)
	ctx := context.Background()

	// Simulate a concurrent writer pricing the row first.
	if _, err := st.db.ExecContext(ctx,
		`UPDATE activity SET estimated_cost = 9.99, cost_estimated = 1 WHERE id = ?`, ids[0]); err != nil {
		t.Fatalf("pre-price row: %v", err)
	}
	// And fold its contribution into the rollup, as the write path would have.
	if err := st.ApplyEstimatedCostBatch(ctx, nil); err != nil {
		t.Fatalf("ApplyEstimatedCostBatch nil: %v", err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE activity_rollup SET estimated_cost = 9.99, cost_estimated = 1 WHERE model_id = 'm1'`); err != nil {
		t.Fatalf("seed rollup: %v", err)
	}

	// A stale batch that still contains the row must be a no-op.
	err := st.ApplyEstimatedCostBatch(ctx, []CostBackfillUpdate{{
		ID: int64(ids[0]), Cost: 5, TsCreated: time.Now().Unix(), Model: "m1", KeyID: "key-1", ReqPath: "/v1/chat/completions",
	}})
	if err != nil {
		t.Fatalf("ApplyEstimatedCostBatch: %v", err)
	}

	var cost float64
	if err := st.db.QueryRowContext(ctx,
		`SELECT estimated_cost FROM activity WHERE id = ?`, ids[0]).Scan(&cost); err != nil {
		t.Fatalf("read activity: %v", err)
	}
	if !almostEqual(cost, 9.99) {
		t.Fatalf("activity cost=%v, want 9.99 unchanged", cost)
	}
	rollup, estimated := rollupCost(t, st)
	if !estimated || !almostEqual(rollup, 9.99) {
		t.Fatalf("rollup cost=%v estimated=%v, want 9.99/true (no double charge)", rollup, estimated)
	}
}

// TestStore_ApplyEstimatedCostBatch_MissingRollupRow: an activity row whose
// hour bucket has no rollup row yet (rollup not folded that far) must still be
// priced in activity without failing the batch; the rollup backfill picks the
// cost up later.
func TestStore_ApplyEstimatedCostBatch_MissingRollupRow(t *testing.T) {
	st := newRollupTestStore(t)
	ids := seedUnpricedActivity(t, st, 1)
	ctx := context.Background()
	// Remove the rollup row the insert path created.
	if _, err := st.db.ExecContext(ctx, `DELETE FROM activity_rollup WHERE model_id = 'm1'`); err != nil {
		t.Fatalf("delete rollup rows: %v", err)
	}

	rows, err := st.NextUnpricedActivity(ctx, 0, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("NextUnpricedActivity = %d rows, err %v; want 1", len(rows), err)
	}
	row := rows[0]
	err = st.ApplyEstimatedCostBatch(ctx, []CostBackfillUpdate{{
		ID: row.ID, Cost: 2, TsCreated: row.TsCreated, Model: row.Model, KeyID: row.KeyID, ReqPath: row.ReqPath,
	}})
	if err != nil {
		t.Fatalf("ApplyEstimatedCostBatch without rollup row: %v", err)
	}
	var cost float64
	var estimated int64
	if err := st.db.QueryRowContext(ctx,
		`SELECT estimated_cost, cost_estimated FROM activity WHERE id = ?`, ids[0]).
		Scan(&cost, &estimated); err != nil {
		t.Fatalf("read activity: %v", err)
	}
	if estimated != 1 || !almostEqual(cost, 2) {
		t.Fatalf("activity cost=%v estimated=%d, want 2/1", cost, estimated)
	}
}

// TestStore_CostBackfillCursor checks the resume bookkeeping: absent -> 0,
// set -> persisted, clear -> 0 again.
func TestStore_CostBackfillCursor(t *testing.T) {
	st := newRollupTestStore(t)
	ctx := context.Background()

	cursor, err := st.GetCostBackfillCursor(ctx)
	if err != nil || cursor != 0 {
		t.Fatalf("initial cursor = %d, err %v; want 0", cursor, err)
	}
	if err := st.SetCostBackfillCursor(ctx, 4242); err != nil {
		t.Fatalf("SetCostBackfillCursor: %v", err)
	}
	cursor, err = st.GetCostBackfillCursor(ctx)
	if err != nil || cursor != 4242 {
		t.Fatalf("cursor after set = %d, err %v; want 4242", cursor, err)
	}
	if err := st.ClearCostBackfillCursor(ctx); err != nil {
		t.Fatalf("ClearCostBackfillCursor: %v", err)
	}
	cursor, err = st.GetCostBackfillCursor(ctx)
	if err != nil || cursor != 0 {
		t.Fatalf("cursor after clear = %d, err %v; want 0", cursor, err)
	}
}

// TestStore_NextUnpricedActivity_CursorAndLimit: batches resume after the last
// id and respect the limit, so a large backlog walks in order without repeats.
func TestStore_NextUnpricedActivity_CursorAndLimit(t *testing.T) {
	st := newRollupTestStore(t)
	ids := seedUnpricedActivity(t, st, 5)
	ctx := context.Background()

	first, err := st.NextUnpricedActivity(ctx, 0, 2)
	if err != nil || len(first) != 2 {
		t.Fatalf("first batch = %d rows, err %v; want 2", len(first), err)
	}
	if first[0].ID != int64(ids[0]) || first[1].ID != int64(ids[1]) {
		t.Fatalf("first batch ids = %d,%d; want %d,%d", first[0].ID, first[1].ID, ids[0], ids[1])
	}
	second, err := st.NextUnpricedActivity(ctx, first[1].ID, 2)
	if err != nil || len(second) != 2 {
		t.Fatalf("second batch = %d rows, err %v; want 2", len(second), err)
	}
	if second[0].ID != int64(ids[2]) {
		t.Fatalf("second batch starts at %d, want %d", second[0].ID, ids[2])
	}
	third, err := st.NextUnpricedActivity(ctx, second[1].ID, 2)
	if err != nil || len(third) != 1 {
		t.Fatalf("third batch = %d rows, err %v; want 1", len(third), err)
	}
	if third[0].ID != int64(ids[4]) {
		t.Fatalf("third batch id = %d, want %d", third[0].ID, ids[4])
	}
}
