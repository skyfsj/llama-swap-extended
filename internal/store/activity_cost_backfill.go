package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"
)

// CostBackfillCursorKey records the last activity id the cost backfill walked,
// so an interrupted run resumes instead of starting over. The bookkeeping lives
// in the shared cache_entries table with a zero TTL, same as the rollup
// backfill cursor: it is rewritten on a known event rather than aged out.
const CostBackfillCursorKey = "activity_cost_backfill_id"

// costBackfillBatch bounds how many unpriced activity rows one backfill step
// reads. Small enough that a step stays well under a second on a cold page
// cache, so the backfill can run alongside live traffic without a visible
// stall.
const costBackfillBatch = 1000

// UnpricedActivityRow is one activity row recorded before a price was
// available, carrying everything the cost formula and the rollup delta need.
type UnpricedActivityRow struct {
	ID            int64
	TsCreated     int64
	Model         string
	Provider      string
	KeyID         string
	ReqPath       string
	Input         int64
	Output        int64
	Cached        int64
	CacheCreation int64
	Reasoning     int64
}

// CostBackfillUpdate prices one previously unpriced activity row and carries
// the rollup identity of the bucket the row folded into.
type CostBackfillUpdate struct {
	ID        int64
	Cost      float64
	TsCreated int64
	Model     string
	KeyID     string
	ReqPath   string
}

// NextUnpricedActivity reads up to limit activity rows recorded without a
// cost, ordered by id. The partial index on (id) WHERE cost_estimated = 0
// keeps this scan confined to the unpriced rows.
func (s *Store) NextUnpricedActivity(ctx context.Context, afterID int64, limit int) ([]UnpricedActivityRow, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = costBackfillBatch
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, ts_created, model_id, provider_id, key_id, req_path,
			input_tokens, output_tokens, cache_tokens, cache_creation_tokens, reasoning_tokens
		FROM activity WHERE id > ? AND cost_estimated = 0 ORDER BY id LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("read unpriced activity: %w", err)
	}
	defer rows.Close()
	out := make([]UnpricedActivityRow, 0, limit)
	for rows.Next() {
		var (
			row                         UnpricedActivityRow
			provider                    sql.NullString
			input, output               sql.NullInt64
			cached, creation, reasoning sql.NullInt64
		)
		if err := rows.Scan(&row.ID, &row.TsCreated, &row.Model, &provider, &row.KeyID, &row.ReqPath,
			&input, &output, &cached, &creation, &reasoning); err != nil {
			return nil, fmt.Errorf("scan unpriced activity: %w", err)
		}
		row.Provider = provider.String
		// The activity columns use -1 for "unknown"; keep the sentinel so the
		// shared cost formula applies the same clamping as the record path.
		row.Input = input.Int64
		row.Output = output.Int64
		row.Cached = cached.Int64
		row.CacheCreation = creation.Int64
		row.Reasoning = reasoning.Int64
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate unpriced activity: %w", err)
	}
	return out, nil
}

// ApplyEstimatedCostBatch prices one batch of previously unpriced activity rows
// and folds the same amounts into their rollup buckets, all in one transaction:
// a row whose cost changed must never be visible without its rollup share or
// vice versa.
//
// The activity update keeps its cost_estimated = 0 guard so a row priced by a
// concurrent writer is skipped rather than double charged, and the rollup delta
// is only applied when the activity row actually changed. A missing rollup row
// means the bucket has not been folded yet; the rollup backfill reads the
// activity row afterwards and picks up the now-priced cost itself.
func (s *Store) ApplyEstimatedCostBatch(ctx context.Context, updates []CostBackfillUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	if err := s.ensureDB(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin cost backfill: %w", err)
	}
	for _, up := range updates {
		result, err := tx.ExecContext(ctx,
			`UPDATE activity SET estimated_cost = ?, cost_estimated = 1 WHERE id = ? AND cost_estimated = 0`,
			up.Cost, up.ID)
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("price activity row %d: %w", up.ID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("price activity row %d: %w", up.ID, err)
		}
		if affected == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE activity_rollup SET estimated_cost = estimated_cost + ?, cost_estimated = 1
			WHERE bucket_ts = ? AND model_id = ? AND key_id = ? AND req_path = ?`,
			up.Cost, rollupBucketFor(up.TsCreated), up.Model, up.KeyID, up.ReqPath); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("rollup cost delta for activity row %d: %w", up.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit cost backfill: %w", err)
	}
	return nil
}

// GetCostBackfillCursor returns the persisted backfill cursor, or 0 when no
// run has started yet.
func (s *Store) GetCostBackfillCursor(ctx context.Context) (int64, error) {
	entry, found, err := s.GetCacheEntry(ctx, CostBackfillCursorKey)
	if err != nil || !found {
		return 0, err
	}
	cursor, err := strconv.ParseInt(string(entry.Data), 10, 64)
	if err != nil {
		return 0, nil
	}
	return cursor, nil
}

// SetCostBackfillCursor persists the backfill cursor.
func (s *Store) SetCostBackfillCursor(ctx context.Context, cursor int64) error {
	return s.SetCacheEntry(ctx, CacheEntry{
		Key:       CostBackfillCursorKey,
		Data:      []byte(strconv.FormatInt(cursor, 10)),
		Timestamp: time.Now(),
	})
}

// ClearCostBackfillCursor drops the cursor after a completed pass, so the next
// trigger starts from the top and can price rows the earlier pass had to skip.
func (s *Store) ClearCostBackfillCursor(ctx context.Context) error {
	return s.DeleteCacheEntry(ctx, CostBackfillCursorKey)
}
