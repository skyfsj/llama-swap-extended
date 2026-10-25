package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// rollupQuery describes which rollup buckets a statistics read needs. It mirrors
// the part of ActivityFilter the rollup can express: filters on dimensions that
// are not rollup keys (min_id/max_id, and any future column-level predicate) have
// no rollup equivalent and must fall back to scanning activity.
type rollupQuery struct {
	Models []string
	KeyID  string
	Start  time.Time
	End    time.Time
}

// rollupSupports reports whether a filter can be answered from the rollup. A
// filter the rollup cannot express is served by the activity scan instead, so
// the two paths never disagree about what was counted.
func rollupSupports(filter ActivityFilter) bool {
	// An id-range filter has no rollup equivalent: the rollup keys on time, not
	// on the row id, so it must be served by the activity scan instead.
	if filter.MinID > 0 || filter.MaxID > 0 {
		return false
	}
	// A session filter has no rollup equivalent either: the session is not a
	// rollup dimension, because keying on it would produce about one bucket per
	// row and cost more than the scan it replaces.
	if strings.TrimSpace(filter.SessionID) != "" {
		return false
	}
	// A time bound that falls inside an hour cannot be expressed by an hourly
	// bucket without widening the selection: the bucket that contains the bound
	// also holds rows on the other side of it. Asking for the scan instead keeps
	// the rollup's answer identical to the detail table's, which matters more
	// than the speedup on a range that starts mid-hour.
	return rollupBoundsAlign(filter)
}

// rollupBoundsAlign reports whether the filter's time bounds sit on bucket
// boundaries, so a bucket-level range selects exactly the rows a row-level range
// would.
func rollupBoundsAlign(filter ActivityFilter) bool {
	if !filter.Start.IsZero() && filter.Start.Unix()%activityRollupBucketSeconds != 0 {
		return false
	}
	if !filter.End.IsZero() {
		// The end bound is inclusive, so the last hour it may include is the one
		// containing it: that bucket must be whole, meaning the bound sits at the
		// end of an hour.
		end := filter.End.Unix()
		if end%activityRollupBucketSeconds != 0 && end%activityRollupBucketSeconds != activityRollupBucketSeconds-1 {
			return false
		}
	}
	return true
}

// rollupFilterFor derives the rollup query from an activity filter, and reports
// whether the rollup can serve it.
func rollupFilterFor(filter ActivityFilter) (rollupQuery, bool) {
	if !rollupSupports(filter) {
		return rollupQuery{}, false
	}
	return rollupQuery{
		Models: filter.Models,
		KeyID:  filter.KeyID,
		Start:  filter.Start,
		End:    filter.End,
	}, true
}

// rollupWhere builds the WHERE clause for a rollup read. Every predicate maps to
// a rollup key, so each one is index-assisted.
func rollupWhere(query rollupQuery) (string, []any) {
	var conditions []string
	var args []any
	if len(query.Models) > 0 {
		conditions = append(conditions, "model_id IN ("+placeholders(len(query.Models))+")")
		for _, model := range query.Models {
			args = append(args, model)
		}
	}
	if keyID := strings.TrimSpace(query.KeyID); keyID != "" {
		conditions = append(conditions, "key_id = ?")
		args = append(args, keyID)
	}
	// The bucket is the hour that contains the timestamp, so a bucket is in range
	// when it can still hold a row inside it: from the bucket of the start to the
	// bucket of the end.
	if !query.Start.IsZero() {
		conditions = append(conditions, "bucket_ts >= ?")
		args = append(args, rollupBucketFor(query.Start.Unix()))
	}
	if !query.End.IsZero() {
		conditions = append(conditions, "bucket_ts <= ?")
		args = append(args, rollupBucketFor(query.End.Unix()))
	}
	if len(conditions) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

func placeholders(count int) string {
	if count <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

// rollupAggregates folds the rollup into the same shape the activity scan
// produces, so every caller of aggregateActivity keeps working unchanged. The
// fold is over buckets, not rows: the row count it replaces is what made the
// statistics pages slower the longer the server ran.
func (s *Store) rollupAggregates(ctx context.Context, query rollupQuery) (activityAggregates, error) {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return activityAggregates{}, err
	}
	where, args := rollupWhere(query)
	rows, err := s.db.QueryContext(ctx, `
		SELECT model_id, requests, input_tokens, output_tokens, cache_tokens,
			cache_creation_tokens, reasoning_tokens, estimated_cost, cost_estimated,
			duration_ms_sum, duration_count, first_token_ms_sum, first_token_count,
			prompt_speed_sum, prompt_speed_count, gen_speed_sum, gen_speed_count
		FROM activity_rollup`+where, args...)
	if err != nil {
		return activityAggregates{}, fmt.Errorf("read activity rollup: %w", err)
	}
	defer rows.Close()
	aggregates := activityAggregates{byModel: make(map[string]*activityAggregate)}
	for rows.Next() {
		var (
			model                           string
			requests, input, output, cached int64
			creation, reasoning             int64
			cost                            float64
			costEstimated                   int64
			durationSum, durationCount      int64
			firstTokenSum, firstTokenCount  int64
			promptSum                       float64
			promptCount                     int64
			genSum                          float64
			genCount                        int64
		)
		if err := rows.Scan(&model, &requests, &input, &output, &cached,
			&creation, &reasoning, &cost, &costEstimated,
			&durationSum, &durationCount, &firstTokenSum, &firstTokenCount,
			&promptSum, &promptCount, &genSum, &genCount); err != nil {
			return activityAggregates{}, fmt.Errorf("scan activity rollup: %w", err)
		}
		foldRollupInto(&aggregates.total, requests, input, output, cached, creation, reasoning, cost, costEstimated != 0,
			durationSum, durationCount, firstTokenSum, firstTokenCount, promptSum, promptCount, genSum, genCount)
		byModel := aggregates.byModel[model]
		if byModel == nil {
			byModel = &activityAggregate{}
			aggregates.byModel[model] = byModel
		}
		foldRollupInto(byModel, requests, input, output, cached, creation, reasoning, cost, costEstimated != 0,
			durationSum, durationCount, firstTokenSum, firstTokenCount, promptSum, promptCount, genSum, genCount)
	}
	if err := rows.Err(); err != nil {
		return activityAggregates{}, fmt.Errorf("iterate activity rollup: %w", err)
	}
	return aggregates, nil
}

// foldRollupInto adds one bucket's contributions to an aggregate. Counts are
// saturated rather than wrapped: the Go-side aggregation exists precisely because
// SQLite raises on int64 overflow, and folding buckets must keep that property.
func foldRollupInto(aggregate *activityAggregate,
	requests, input, output, cached, creation, reasoning int64, cost float64, costEstimated bool,
	durationSum, durationCount, firstTokenSum, firstTokenCount int64,
	promptSum float64, promptCount int64, genSum float64, genCount int64) {
	if aggregate == nil {
		return
	}
	aggregate.Requests = saturatingAdd(aggregate.Requests, int(requests))
	aggregate.InputTokens = saturatingAdd(aggregate.InputTokens, int(input))
	aggregate.OutputTokens = saturatingAdd(aggregate.OutputTokens, int(output))
	aggregate.CachedTokens = saturatingAdd(aggregate.CachedTokens, int(cached))
	aggregate.CacheCreationTokens = saturatingAdd(aggregate.CacheCreationTokens, int(creation))
	aggregate.ReasoningTokens = saturatingAdd(aggregate.ReasoningTokens, int(reasoning))
	aggregate.EstimatedCost += cost
	if costEstimated {
		aggregate.CostEstimated = true
	}
	aggregate.DurationMsSum = saturatingAdd(aggregate.DurationMsSum, int(durationSum))
	aggregate.DurationCount = saturatingAdd(aggregate.DurationCount, int(durationCount))
	aggregate.FirstTokenMsSum = saturatingAdd(aggregate.FirstTokenMsSum, int(firstTokenSum))
	aggregate.FirstTokenCount = saturatingAdd(aggregate.FirstTokenCount, int(firstTokenCount))
	aggregate.PromptSpeedSum += promptSum
	aggregate.PromptSpeedCount = saturatingAdd(aggregate.PromptSpeedCount, int(promptCount))
	aggregate.GenerationSpeedSum += genSum
	aggregate.GenerationSpeedCount = saturatingAdd(aggregate.GenerationSpeedCount, int(genCount))
}

func saturatingAdd(current, addition int) int {
	sum := int64(current) + int64(addition)
	if sum > int64(maxIntValue) {
		return maxIntValue
	}
	if sum < 0 {
		return maxIntValue
	}
	return int(sum)
}

const maxIntValue = int(^uint(0) >> 1)

// rollupDimensions folds the rollup grouped by one dimension. It replaces the
// per-row correlated subqueries the activity scan used for the key and group
// dimensions: the rollup already carries key_id as a grouping key, and the key
// table is small enough to join in Go.
func (s *Store) rollupDimensions(ctx context.Context, query rollupQuery, dimension string) ([]rollupDimensionRow, error) {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	where, args := rollupWhere(query)
	// req_path is a rollup key but not a page dimension; grouping by it is only
	// meaningful for the endpoint dimension, which maps onto it.
	column := dimension
	switch dimension {
	case "endpoint":
		column = "req_path"
	case "model_id", "key_id", "req_path":
		column = dimension
	default:
		return nil, fmt.Errorf("unsupported rollup dimension %q", dimension)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+column+` AS dimension,
			SUM(requests), SUM(input_tokens), SUM(output_tokens), SUM(cache_tokens),
			SUM(cache_creation_tokens), SUM(reasoning_tokens), SUM(estimated_cost), MAX(cost_estimated),
			SUM(duration_ms_sum), SUM(duration_count), SUM(first_token_ms_sum), SUM(first_token_count)
		FROM activity_rollup`+where+`
		GROUP BY dimension
		ORDER BY SUM(requests) DESC, dimension ASC
		LIMIT 50`, args...)
	if err != nil {
		return nil, fmt.Errorf("read activity rollup dimensions: %w", err)
	}
	defer rows.Close()
	out := []rollupDimensionRow{}
	for rows.Next() {
		var (
			row                      rollupDimensionRow
			requests, input, output  int64
			cached, creation         int64
			reasoning                int64
			cost                     float64
			costEstimated            int64
			durationSum, durationCnt int64
			firstSum, firstCount     int64
		)
		if err := rows.Scan(&row.Dimension, &requests, &input, &output, &cached,
			&creation, &reasoning, &cost, &costEstimated,
			&durationSum, &durationCnt, &firstSum, &firstCount); err != nil {
			return nil, fmt.Errorf("scan activity rollup dimension: %w", err)
		}
		row.Requests = int(requests)
		row.InputTokens = int(input)
		row.OutputTokens = int(output)
		row.CachedTokens = int(cached)
		row.CacheCreationTokens = int(creation)
		row.ReasoningTokens = int(reasoning)
		row.EstimatedCost = cost
		row.CostEstimated = costEstimated != 0
		row.DurationMsSum = durationSum
		row.DurationCount = durationCnt
		row.FirstTokenMsSum = firstSum
		row.FirstTokenCount = firstCount
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate activity rollup dimensions: %w", err)
	}
	return out, nil
}

// rollupDimensionRow is one grouped bucket fold, carrying the sums and counts the
// dimension rows expose.
type rollupDimensionRow struct {
	Dimension           string
	Requests            int
	InputTokens         int
	OutputTokens        int
	CachedTokens        int
	CacheCreationTokens int
	ReasoningTokens     int
	EstimatedCost       float64
	CostEstimated       bool
	DurationMsSum       int64
	DurationCount       int64
	FirstTokenMsSum     int64
	FirstTokenCount     int64
}

// rollupTrend folds the rollup into time buckets. The bucket width is derived
// from the requested range, so a day reads 24 buckets instead of every row in it.
func (s *Store) rollupTrend(ctx context.Context, query rollupQuery, bucketSeconds int64) ([]rollupTrendRow, error) {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	if bucketSeconds <= 0 {
		bucketSeconds = activityRollupBucketSeconds
	}
	// Buckets narrower than an hour cannot be answered from an hourly rollup:
	// several rollup buckets would have to be split, which is not a fold.
	if bucketSeconds < activityRollupBucketSeconds {
		return nil, fmt.Errorf("rollup cannot serve a %ds trend bucket", bucketSeconds)
	}
	where, args := rollupWhere(query)
	// The rollup's buckets are hours, so a wider trend bucket is a whole number of
	// them. The bucket start is the hour bucket floored onto the requested width:
	// dividing by the width and multiplying back by it, which is why both bound
	// parameters are the requested width rather than the hour size.
	rows, err := s.db.QueryContext(ctx, `
		SELECT (bucket_ts / ?) * ? AS bucket,
			SUM(requests), SUM(input_tokens), SUM(output_tokens), SUM(cache_tokens),
			SUM(cache_creation_tokens), SUM(reasoning_tokens), SUM(estimated_cost), MAX(cost_estimated),
			SUM(duration_ms_sum), SUM(duration_count), SUM(first_token_ms_sum), SUM(first_token_count)
		FROM activity_rollup`+where+`
		GROUP BY bucket
		ORDER BY bucket ASC
		LIMIT 200`, append([]any{bucketSeconds, bucketSeconds}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("read activity rollup trend: %w", err)
	}
	defer rows.Close()
	out := []rollupTrendRow{}
	for rows.Next() {
		var (
			bucket                   int64
			requests, input, output  int64
			cached, creation         int64
			reasoning                int64
			cost                     float64
			costEstimated            int64
			durationSum, durationCnt int64
			firstSum, firstCount     int64
		)
		if err := rows.Scan(&bucket, &requests, &input, &output, &cached,
			&creation, &reasoning, &cost, &costEstimated,
			&durationSum, &durationCnt, &firstSum, &firstCount); err != nil {
			return nil, fmt.Errorf("scan activity rollup trend: %w", err)
		}
		out = append(out, rollupTrendRow{
			Bucket:              bucket,
			Requests:            int(requests),
			InputTokens:         int(input),
			OutputTokens:        int(output),
			CachedTokens:        int(cached),
			CacheCreationTokens: int(creation),
			ReasoningTokens:     int(reasoning),
			EstimatedCost:       cost,
			CostEstimated:       costEstimated != 0,
			DurationMsSum:       durationSum,
			DurationCount:       durationCnt,
			FirstTokenMsSum:     firstSum,
			FirstTokenCount:     firstCount,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate activity rollup trend: %w", err)
	}
	return out, nil
}

type rollupTrendRow struct {
	Bucket              int64
	Requests            int
	InputTokens         int
	OutputTokens        int
	CachedTokens        int
	CacheCreationTokens int
	ReasoningTokens     int
	EstimatedCost       float64
	CostEstimated       bool
	DurationMsSum       int64
	DurationCount       int64
	FirstTokenMsSum     int64
	FirstTokenCount     int64
}

// rollupHistogram reads the two speed columns for the distribution histograms.
// These are not rollup dimensions (a histogram needs every observation, not a
// mean), so the scan stays — but it reads two columns and the time range is
// index-assisted, which keeps it cheap next to the row scan it replaces.
func (s *Store) rollupHistogram(ctx context.Context, filter ActivityFilter) ([]float64, []float64, error) {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return nil, nil, err
	}
	where, args := activityWhere(filter)
	// activityWhere emits " WHERE ..." when filters are present and "" when not,
	// so the speed predicate has to join with AND or stand alone as the WHERE.
	speedPredicate := `(prompt_per_second > 0 OR tokens_per_second > 0)`
	statement := `SELECT prompt_per_second, tokens_per_second FROM activity`
	if where == "" {
		statement += ` WHERE ` + speedPredicate
	} else {
		statement += where + ` AND ` + speedPredicate
	}
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("read speed values: %w", err)
	}
	defer rows.Close()
	var prompt, generation []float64
	for rows.Next() {
		var promptSpeed, genSpeed float64
		if err := rows.Scan(&promptSpeed, &genSpeed); err != nil {
			return nil, nil, fmt.Errorf("scan speed values: %w", err)
		}
		if promptSpeed > 0 {
			prompt = append(prompt, promptSpeed)
		}
		if genSpeed > 0 {
			generation = append(generation, genSpeed)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate speed values: %w", err)
	}
	sort.Float64s(prompt)
	sort.Float64s(generation)
	return prompt, generation, nil
}

// activityAggregatesWithSource carries which path produced the aggregates, so
// callers can apply the rules that differ between a rollup fold and a row scan.
type activityAggregatesWithSource struct {
	activityAggregates
	fromRollup bool
}
