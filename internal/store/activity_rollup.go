package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// activityRollupBucketSeconds is the hour bucket the rollup folds activity rows
// into.
const activityRollupBucketSeconds = 3600

// rollupBackfillBatch bounds how many activity rows one backfill step reads.
// Small enough that a step stays well under a second on a cold page cache, so
// the backfill can run on the request path without a visible stall.
const rollupBackfillBatch = 5000

// rollupBackfillKey records the last activity id folded into the rollup, so an
// interrupted backfill resumes instead of starting over. The bookkeeping lives
// in the shared cache_entries table with a zero TTL: it is rewritten on a known
// event rather than aged out.
const rollupBackfillKey = "activity_rollup_backfill_id"

// rollupCompleteKey records that every activity row has been folded. The id key
// alone cannot tell "caught up" from "interrupted just before the end", and a
// partially built rollup must never be read as if it were complete.
const rollupCompleteKey = "activity_rollup_backfilled"

// rollupStartedKey marks an in-flight from-zero fold. A backfill that resumes
// after its 2s budget must continue where it stopped; the marker is what stops
// it from clearing the buckets the previous slice already filled and folding
// them a second time.
const rollupStartedKey = "activity_rollup_started"

// rollupInput is one activity row expressed as rollup contributions. It exists
// so the same folding code serves both the write path (one row at a time) and
// the backfill (batches of rows) without either side knowing about the other.
type rollupInput struct {
	Bucket          int64
	Model           string
	KeyID           string
	ReqPath         string
	Input           int64
	Output          int64
	Cached          int64
	CacheCreation   int64
	Reasoning       int64
	EstimatedCost   float64
	CostEstimated   bool
	DurationMs      int64
	FirstTokenMs    int64
	PromptPerSecond float64
	TokensPerSecond float64
}

// rollupBucketFor maps a unix timestamp onto its hour bucket.
func rollupBucketFor(unixSeconds int64) int64 {
	return unixSeconds - unixSeconds%activityRollupBucketSeconds
}

// rollupInputFromRow builds a contribution from persisted activity values.
//
// Negative values are the "unknown" sentinel the activity columns use (-1), and
// an unknown counter must contribute nothing rather than subtract from a total.
// The row scan normalizes the same way, so both paths agree.
func rollupInputFromRow(unixSeconds int64, model, keyID, reqPath string,
	input, output, cached, cacheCreation, reasoning int64, cost float64, costEstimated bool,
	durationMs, firstTokenMs int64, promptSpeed, genSpeed float64) rollupInput {
	return rollupInput{
		Bucket:          rollupBucketFor(unixSeconds),
		Model:           model,
		KeyID:           keyID,
		ReqPath:         reqPath,
		Input:           boundedCounter(input),
		Output:          boundedCounter(output),
		Cached:          boundedCounter(cached),
		CacheCreation:   boundedCounter(cacheCreation),
		Reasoning:       boundedCounter(reasoning),
		EstimatedCost:   cost,
		CostEstimated:   costEstimated,
		DurationMs:      durationMs,
		FirstTokenMs:    firstTokenMs,
		PromptPerSecond: promptSpeed,
		TokensPerSecond: genSpeed,
	}
}

// rollupContribution derives one activity row's contribution.
func rollupContribution(entry ActivityLogEntry) rollupInput {
	return rollupInputFromRow(
		entry.Timestamp.Unix(), entry.Model, entry.KeyID, entry.ReqPath,
		int64(entry.Tokens.InputTokens), int64(entry.Tokens.OutputTokens), int64(entry.Tokens.CachedTokens),
		int64(entry.CacheCreationTokens), int64(entry.ReasoningTokens),
		entry.EstimatedCost, entry.CostEstimated,
		int64(entry.DurationMs), int64(entry.FirstTokenMs),
		entry.Tokens.PromptPerSecond, entry.Tokens.TokensPerSecond,
	)
}

// rollupUpsert folds one contribution into its bucket. It is an upsert rather
// than a read-modify-write so a concurrent writer cannot lose a contribution to
// a race between the two statements.
const rollupUpsert = `
INSERT INTO activity_rollup (
    bucket_ts, model_id, key_id, req_path,
    requests, input_tokens, output_tokens, cache_tokens, cache_creation_tokens, reasoning_tokens,
    estimated_cost, cost_estimated,
    duration_ms_sum, duration_count, first_token_ms_sum, first_token_count,
    prompt_speed_sum, prompt_speed_count, gen_speed_sum, gen_speed_count
) VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (bucket_ts, model_id, key_id, req_path) DO UPDATE SET
    requests = requests + 1,
    input_tokens = input_tokens + excluded.input_tokens,
    output_tokens = output_tokens + excluded.output_tokens,
    cache_tokens = cache_tokens + excluded.cache_tokens,
    cache_creation_tokens = cache_creation_tokens + excluded.cache_creation_tokens,
    reasoning_tokens = reasoning_tokens + excluded.reasoning_tokens,
    estimated_cost = estimated_cost + excluded.estimated_cost,
    cost_estimated = MAX(cost_estimated, excluded.cost_estimated),
    duration_ms_sum = duration_ms_sum + excluded.duration_ms_sum,
    duration_count = duration_count + excluded.duration_count,
    first_token_ms_sum = first_token_ms_sum + excluded.first_token_ms_sum,
    first_token_count = first_token_count + excluded.first_token_count,
    prompt_speed_sum = prompt_speed_sum + excluded.prompt_speed_sum,
    prompt_speed_count = prompt_speed_count + excluded.prompt_speed_count,
    gen_speed_sum = gen_speed_sum + excluded.gen_speed_sum,
    gen_speed_count = gen_speed_count + excluded.gen_speed_count`

func rollupUpsertArgs(in rollupInput) []any {
	costEstimated := int64(0)
	if in.CostEstimated {
		costEstimated = 1
	}
	// A value that was never reported must not drag an average toward zero, so
	// it contributes to neither the sum nor the count. 0 means "measured"; the
	// activity columns use -1 for "unknown", which lands here as <= 0.
	durationSum, durationCount := int64(0), int64(0)
	if in.DurationMs > 0 {
		durationSum, durationCount = in.DurationMs, 1
	}
	firstTokenSum, firstTokenCount := int64(0), int64(0)
	if in.FirstTokenMs > 0 {
		firstTokenSum, firstTokenCount = in.FirstTokenMs, 1
	}
	promptSum, promptCount := float64(0), int64(0)
	if in.PromptPerSecond > 0 {
		promptSum, promptCount = in.PromptPerSecond, 1
	}
	genSum, genCount := float64(0), int64(0)
	if in.TokensPerSecond > 0 {
		genSum, genCount = in.TokensPerSecond, 1
	}
	return []any{
		in.Bucket, in.Model, in.KeyID, in.ReqPath,
		in.Input, in.Output, in.Cached, in.CacheCreation, in.Reasoning,
		in.EstimatedCost, costEstimated,
		durationSum, durationCount, firstTokenSum, firstTokenCount,
		promptSum, promptCount, genSum, genCount,
	}
}

// applyRollup folds contributions using an existing executor. The caller decides
// the transaction boundary, so the rollup stays consistent with the row it
// describes: a row that failed to insert must not leave its contribution behind.
func applyRollup(ctx context.Context, tx rollupExecer, inputs []rollupInput) error {
	if len(inputs) == 0 {
		return nil
	}
	for _, in := range inputs {
		if _, err := tx.ExecContext(ctx, rollupUpsert, rollupUpsertArgs(in)...); err != nil {
			return fmt.Errorf("fold activity rollup: %w", err)
		}
	}
	return nil
}

// rollupExecer is the subset of *sql.Tx and *sql.DB the rollup needs, so tests
// can supply a fake.
type rollupExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sqlResult, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// sqlResult is the subset of sql.Result the rollup reads.
type sqlResult = sql.Result

// rollupAvailable reports whether the rollup can answer a query. It is false
// until the backfill has folded every row that predates the table.
func (s *Store) rollupAvailable(ctx context.Context) bool {
	if err := s.ensureDB(); err != nil {
		return false
	}
	if s.rollupBackfillComplete(ctx) {
		return true
	}
	// A store whose rows were all written after the rollup existed never runs a
	// backfill, so it carries no marker even though every row was folded on its
	// way in. The cursor answers that case: nothing is waiting to be folded when
	// it is at or past the highest id in activity AND the rollup already holds
	// what that cursor claims.
	//
	// The population check is what makes the cursor trustworthy. A cursor that
	// ran ahead of the folding (an older build advanced it on every insert) sits
	// at max(id) while the table holds almost nothing, and treating that as
	// "caught up" hides the entire history from every statistics view.
	cursor := s.rollupCursor(ctx)
	var maxID int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM activity`).Scan(&maxID); err != nil {
		return false
	}
	if cursor < maxID {
		return false
	}
	// The cursor claims everything is folded; verify the rollup actually holds
	// the work before trusting it. Comparing totals cannot answer that (buckets
	// do not sum to the row count), so look for a row the fold never reached:
	// its bucket has no rollup entry at all.
	var unfolded int64
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM activity a
		WHERE NOT EXISTS (
			SELECT 1 FROM activity_rollup r
			WHERE r.bucket_ts = (a.ts_created/3600)*3600
			  AND r.model_id = a.model_id
			  AND r.key_id = COALESCE(a.key_id,'')
			  AND r.req_path = COALESCE(a.req_path,'')
		)`).Scan(&unfolded); err != nil {
		return false
	}
	return unfolded == 0
}

// rollupCovers reports whether every activity row up to the cursor has a
// rollup bucket. A cursor that ran ahead of the folding fails this check and
// forces a re-fold from the start.
func (s *Store) rollupCovers(ctx context.Context, cursor int64) bool {
	var unfolded int64
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM activity a
		WHERE a.id <= ? AND NOT EXISTS (
			SELECT 1 FROM activity_rollup r
			WHERE r.bucket_ts = (a.ts_created/3600)*3600
			  AND r.model_id = a.model_id
			  AND r.key_id = COALESCE(a.key_id,'')
			  AND r.req_path = COALESCE(a.req_path,'')
		)`, cursor).Scan(&unfolded); err != nil {
		return false
	}
	return unfolded == 0
}

// rollupBackfillStarted reports whether a from-zero fold is already in flight.
// While it is, a resume must keep folding rather than clearing what the earlier
// slice produced.
func (s *Store) rollupBackfillStarted(ctx context.Context) bool {
	entry, found, err := s.GetCacheEntry(ctx, rollupStartedKey)
	return err == nil && found && string(entry.Data) == "1"
}

// markRollupBackfillComplete records that the history is fully folded and closes
// the in-flight marker, so a later reset starts a clean from-zero pass.
func (s *Store) markRollupBackfillComplete(ctx context.Context) error {
	if err := s.SetCacheEntry(ctx, CacheEntry{Key: rollupCompleteKey, Data: []byte("1")}); err != nil {
		return fmt.Errorf("mark rollup backfill complete: %w", err)
	}
	if err := s.SetCacheEntry(ctx, CacheEntry{Key: rollupStartedKey, Data: []byte("0")}); err != nil {
		return fmt.Errorf("clear rollup backfill started marker: %w", err)
	}
	return nil
}

func (s *Store) rollupCursor(ctx context.Context) int64 {
	entry, found, err := s.GetCacheEntry(ctx, rollupBackfillKey)
	if err != nil || !found {
		return 0
	}
	parsed, convErr := strconv.ParseInt(strings.TrimSpace(string(entry.Data)), 10, 64)
	if convErr != nil || parsed < 0 {
		return 0
	}
	return parsed
}

// backfillActivityRollup folds the activity rows the rollup has not seen yet.
// It is resumable: the last folded id is persisted with every batch, so a
// restart continues from there instead of redoing the table.
//
// done is false when the deadline stopped the work before the table was caught
// up; the next call continues from the persisted cursor.
func (s *Store) backfillActivityRollup(ctx context.Context, deadline time.Time) (bool, error) {
	if err := s.ensureDB(); err != nil {
		return false, err
	}
	after := int64(0)
	if entry, found, err := s.GetCacheEntry(ctx, rollupBackfillKey); err == nil && found {
		if parsed, convErr := strconv.ParseInt(string(entry.Data), 10, 64); convErr == nil && parsed > 0 {
			after = parsed
		}
	} else if err != nil {
		return false, fmt.Errorf("read rollup backfill cursor: %w", err)
	}
	// Re-fold from the beginning when the cursor claims more progress than the
	// rollup holds. An older build advanced this cursor on every insert, so a
	// store migrated with it carries a cursor at max(id) while the table holds a
	// single hour. Resuming from that cursor would fold nothing and then mark the
	// backfill complete, leaving the history permanently missing.
	restart := after > 0 && !s.rollupBackfillComplete(ctx) && !s.rollupCovers(ctx, after)
	if restart {
		after = 0
	}
	if restart || (after == 0 && !s.rollupBackfillStarted(ctx)) {
		// A fold that starts from the beginning replaces what is there instead
		// of adding to it, and the started marker keeps a resumed pass from
		// clearing the work an interrupted pass already loaded. Without that
		// marker every resume of a partial backfill (its cursor still behind the
		// end) would clear and refold, and the two passes would double every
		// bucket — a store ended up with exactly twice the real totals.
		if _, err := s.db.ExecContext(ctx, `DELETE FROM activity_rollup`); err != nil {
			return false, fmt.Errorf("clear rollup before refold: %w", err)
		}
		if err := s.SetCacheEntry(ctx, CacheEntry{Key: rollupStartedKey, Data: []byte("1")}); err != nil {
			return false, fmt.Errorf("mark rollup backfill started: %w", err)
		}
	}
	for {
		if !deadline.After(time.Now()) {
			return false, nil
		}
		inputs, last, err := s.readActivityForRollup(ctx, after)
		if err != nil {
			return false, err
		}
		if len(inputs) == 0 {
			if err := s.markRollupBackfillComplete(ctx); err != nil {
				return false, err
			}
			return true, nil
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return false, fmt.Errorf("begin rollup backfill: %w", err)
		}
		if err := applyRollup(ctx, tx, inputs); err != nil {
			_ = tx.Rollback()
			return false, err
		}
		cursor := strconv.FormatInt(last, 10)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO cache_entries (key,data,expires_at,updated_at) VALUES (?,?,0,?)
			ON CONFLICT (key) DO UPDATE SET data=excluded.data, updated_at=excluded.updated_at`,
			rollupBackfillKey, []byte(cursor), time.Now().Unix()); err != nil {
			_ = tx.Rollback()
			return false, fmt.Errorf("persist rollup backfill cursor: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("commit rollup backfill: %w", err)
		}
		after = last
		if len(inputs) < rollupBackfillBatch {
			if err := s.markRollupBackfillComplete(ctx); err != nil {
				return false, err
			}
			return true, nil
		}
	}
}

// readActivityForRollup reads one backfill batch after the given id.
func (s *Store) readActivityForRollup(ctx context.Context, after int64) ([]rollupInput, int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, ts_created, model_id, key_id, req_path,
			input_tokens, output_tokens, cache_tokens, cache_creation_tokens, reasoning_tokens,
			estimated_cost, cost_estimated, duration_ms, first_token_ms, prompt_per_second, tokens_per_second
		FROM activity WHERE id > ? ORDER BY id LIMIT ?`, after, rollupBackfillBatch)
	if err != nil {
		return nil, after, fmt.Errorf("read activity for rollup backfill: %w", err)
	}
	defer rows.Close()
	inputs := make([]rollupInput, 0, rollupBackfillBatch)
	last := after
	for rows.Next() {
		var (
			id, ts                   int64
			model, keyID, reqPath    string
			input, output, cached    sql.NullInt64
			creation, reasoning      sql.NullInt64
			cost                     sql.NullFloat64
			costEstimated            sql.NullInt64
			durationMs, firstTokenMs sql.NullInt64
			promptSpeed, genSpeed    sql.NullFloat64
		)
		if err := rows.Scan(&id, &ts, &model, &keyID, &reqPath,
			&input, &output, &cached, &creation, &reasoning,
			&cost, &costEstimated, &durationMs, &firstTokenMs, &promptSpeed, &genSpeed); err != nil {
			return nil, after, fmt.Errorf("scan activity for rollup backfill: %w", err)
		}
		inputs = append(inputs, rollupInputFromRow(
			ts, model, keyID, reqPath,
			input.Int64, output.Int64, cached.Int64, creation.Int64, reasoning.Int64,
			cost.Float64, costEstimated.Int64 != 0,
			durationMs.Int64, firstTokenMs.Int64,
			promptSpeed.Float64, genSpeed.Float64,
		))
		last = id
	}
	if err := rows.Err(); err != nil {
		return nil, after, fmt.Errorf("iterate activity for rollup backfill: %w", err)
	}
	return inputs, last, nil
}

// rollupReady reports whether the rollup is usable, running a bounded backfill
// pass first when it is not. Read paths call it so a fresh install, or a
// database migrated from a version without the rollup, converges without an
// operator action. A backfill that cannot finish in this call leaves the table
// unavailable and the caller falls back to scanning activity.
func (s *Store) rollupReady(ctx context.Context) bool {
	if s.rollupAvailable(ctx) {
		return true
	}
	// A short budget keeps the first statistics request from blocking for the
	// whole backfill; the next request continues where this one stopped.
	done, err := s.backfillActivityRollup(ctx, time.Now().Add(2*time.Second))
	if err != nil {
		return false
	}
	return done
}

// boundedCounter maps a stored counter onto its contribution. A negative value is
// the "unknown" sentinel and contributes nothing; a value past the platform int
// range saturates, because the Go-side fold exists precisely to avoid SQLite's
// int64 overflow error.
func boundedCounter(value int64) int64 {
	if value <= 0 {
		return 0
	}
	maxInt := int64(^uint(0) >> 1)
	if value > maxInt {
		return maxInt
	}
	return value
}
