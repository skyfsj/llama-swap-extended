package store

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// usageRollupQuery is the part of a UsageQuery the rollup can express. A query
// that filters on a group or an endpoint has no rollup equivalent (the rollup
// keys on key_id, not on the key's group), so it stays on the activity scan.
type usageRollupQuery struct {
	rollupQuery
	Endpoints []string
}

// usageRollupSupports reports whether the rollup can serve a usage query.
//
// A group filter needs the key table, which the rollup does not join, so it stays
// on the activity scan. An endpoint filter is expressible (req_path is a rollup
// key) but the caller's endpoint list may name paths the rollup never saw, so it
// is served by the scan as well: the rollup answers whole selections, and a
// narrower answer than the detail table shows is worse than a slower one.
func usageRollupSupports(query UsageQuery) bool {
	if len(query.Groups) > 0 || len(query.Endpoints) > 0 {
		return false
	}
	return true
}

// usageRollupFor converts a usage query into its rollup form.
func usageRollupFor(query UsageQuery) (usageRollupQuery, bool) {
	if !usageRollupSupports(query) {
		return usageRollupQuery{}, false
	}
	filter := ActivityFilter{
		Models:    query.Models,
		KeyID:     "",
		SessionID: "",
		Start:     query.Start,
		End:       query.End,
	}
	if len(query.Keys) == 1 {
		filter.KeyID = query.Keys[0]
	}
	base, ok := rollupFilterFor(filter)
	if !ok {
		return usageRollupQuery{}, false
	}
	return usageRollupQuery{rollupQuery: base, Endpoints: query.Endpoints}, true
}

// usageKeyLabels loads the key table once. It is small (one row per credential),
// so an in-memory map is both cheaper and simpler than the correlated subquery
// the activity scan ran for every row.
func (s *Store) usageKeyLabels(ctx context.Context) (map[string]usageKeyLabel, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, COALESCE(NULLIF(name,''),''), COALESCE(NULLIF(group_name,''),'') FROM api_keys`)
	if err != nil {
		return nil, fmt.Errorf("read key labels: %w", err)
	}
	defer rows.Close()
	labels := map[string]usageKeyLabel{}
	for rows.Next() {
		var (
			id, name, group string
		)
		if err := rows.Scan(&id, &name, &group); err != nil {
			return nil, fmt.Errorf("scan key label: %w", err)
		}
		labels[id] = usageKeyLabel{Name: name, Group: group}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate key labels: %w", err)
	}
	return labels, nil
}

type usageKeyLabel struct {
	Name  string
	Group string
}

// ungroupedLabel is the placeholder an empty usage group is rendered as, so a
// donut never shows an empty segment.
const ungroupedLabel = "ungrouped"

// usageDimensionFromRollup folds the rollup grouped by one dimension and maps
// the result onto the API row type. The key dimension resolves its label through
// the key table in Go rather than a subquery per row.
func (s *Store) usageDimensionFromRollup(ctx context.Context, query usageRollupQuery, dimension string) ([]UsageDimensionRow, error) {
	labels, err := s.usageKeyLabels(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.rollupDimensions(ctx, query.rollupQuery, dimension)
	if err != nil {
		return nil, err
	}
	out := make([]UsageDimensionRow, 0, len(rows))
	for _, row := range rows {
		name := row.Dimension
		switch dimension {
		case "key_id":
			label, ok := labels[row.Dimension]
			switch {
			case ok && label.Name != "":
				name = label.Name
			case row.Dimension == "":
				name = "anonymous"
			default:
				name = row.Dimension
			}
			// The API's "by key" view is labelled by name, so two keys sharing a
			// display name would otherwise collapse into one row here. The label
			// keeps the id visible when the name is ambiguous.
			if ok && label.Name != "" {
				name = label.Name
			}
		}
		out = append(out, UsageDimensionRow{
			Name:                name,
			Requests:            row.Requests,
			InputTokens:         row.InputTokens,
			OutputTokens:        row.OutputTokens,
			CachedTokens:        row.CachedTokens,
			CacheCreationTokens: row.CacheCreationTokens,
			EstimatedCost:       row.EstimatedCost,
		})
	}
	return out, nil
}

// usageGroupRowsFromRollup derives the group distribution by folding the key
// dimension in Go. The rollup already groups by key_id, so the key's group is a
// lookup rather than a subquery per activity row.
func (s *Store) usageGroupRowsFromRollup(ctx context.Context, query usageRollupQuery) ([]UsageDimensionRow, error) {
	labels, err := s.usageKeyLabels(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.rollupDimensions(ctx, query.rollupQuery, "key_id")
	if err != nil {
		return nil, err
	}
	merged := map[string]*UsageDimensionRow{}
	for _, row := range rows {
		group := ungroupedLabel
		if label, ok := labels[row.Dimension]; ok && label.Group != "" {
			group = label.Group
		}
		target := merged[group]
		if target == nil {
			target = &UsageDimensionRow{Name: group}
			merged[group] = target
		}
		target.Requests += row.Requests
		target.InputTokens += row.InputTokens
		target.OutputTokens += row.OutputTokens
		target.CachedTokens += row.CachedTokens
		target.CacheCreationTokens += row.CacheCreationTokens
		target.EstimatedCost += row.EstimatedCost
	}
	out := make([]UsageDimensionRow, 0, len(merged))
	for _, row := range merged {
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > maxUsageDimensionRows {
		out = out[:maxUsageDimensionRows]
	}
	return out, nil
}

// usageTotalsFromRollup folds the summary cards from the rollup. The averages
// divide summed values by their counts, which is why the rollup keeps the sums:
// averaging bucket averages would drift as buckets are folded together.
func usageTotalsFromRollup(aggregate activityAggregate) UsageTotals {
	summary := usageSummaryFromAggregate(aggregate)
	totals := UsageTotals{
		Requests:            summary.Requests,
		InputTokens:         summary.InputTokens,
		OutputTokens:        summary.OutputTokens,
		CachedTokens:        summary.CachedTokens,
		CacheCreationTokens: summary.CacheCreationTokens,
		ReasoningTokens:     summary.ReasoningTokens,
		EstimatedCost:       summary.EstimatedCost,
		CostEstimated:       summary.CostEstimated,
	}
	totals.AverageDurationMs = averageMs(aggregate.DurationMsSum, aggregate.DurationCount)
	totals.AverageFirstTokenMs = averageMs(aggregate.FirstTokenMsSum, aggregate.FirstTokenCount)
	totals.PromptsPerSecond = averageSpeed(aggregate.PromptSpeedSum, aggregate.PromptSpeedCount)
	totals.GenerationPerSecond = averageSpeed(aggregate.GenerationSpeedSum, aggregate.GenerationSpeedCount)
	totals.TotalTokens = totals.InputTokens + totals.OutputTokens
	totals.CacheHitRatio = aggregateCacheHitRatio(totals.InputTokens, totals.CachedTokens, totals.CacheCreationTokens)
	totals.CacheCreationRatio = aggregateCacheCreationRatio(totals.InputTokens, totals.CachedTokens, totals.CacheCreationTokens)
	return totals
}

func averageMs(sum, count int) int64 {
	if count <= 0 {
		return 0
	}
	return roundAverageMs(float64(sum) / float64(count))
}

func averageSpeed(sum float64, count int) float64 {
	if count <= 0 {
		return 0
	}
	return sum / float64(count)
}

// usageTrendFromRollup folds the trend series from the rollup. Buckets narrower
// than the rollup's hour cannot be answered (several rollup buckets would have to
// be split, which is not a fold), so the caller falls back to the activity scan
// for those.
func (s *Store) usageTrendFromRollup(ctx context.Context, query usageRollupQuery, bucketSeconds int64) ([]UsageTrendBucket, error) {
	rows, err := s.rollupTrend(ctx, query.rollupQuery, bucketSeconds)
	if err != nil {
		return nil, err
	}
	out := make([]UsageTrendBucket, 0, len(rows))
	for _, row := range rows {
		bucket := UsageTrendBucket{
			BucketStart:         time.Unix(row.Bucket, 0).UTC(),
			Requests:            row.Requests,
			InputTokens:         row.InputTokens,
			OutputTokens:        row.OutputTokens,
			CachedTokens:        row.CachedTokens,
			CacheCreationTokens: row.CacheCreationTokens,
		}
		bucket.CacheHitRatio = aggregateCacheHitRatio(bucket.InputTokens, bucket.CachedTokens, bucket.CacheCreationTokens)
		out = append(out, bucket)
	}
	return out, nil
}

// usageAnalyticsFromRollup answers the whole dashboard from the rollup. It is
// what turns the usage page from six full table scans into a handful of bucket
// folds, and the group/key dimensions no longer run a subquery per row.
func (s *Store) usageAnalyticsFromRollup(ctx context.Context, query UsageQuery, granularity string, rollupQuery usageRollupQuery, bucketSeconds int64) (UsageAnalytics, error) {
	analytics := UsageAnalytics{
		Start:       query.Start,
		End:         query.End,
		Granularity: granularity,
		ByModel:     []UsageDimensionRow{},
		ByGroup:     []UsageDimensionRow{},
		ByEndpoint:  []UsageDimensionRow{},
		ByKey:       []UsageDimensionRow{},
		Trend:       []UsageTrendBucket{},
	}
	aggregates, err := s.rollupAggregates(ctx, rollupQuery.rollupQuery)
	if err != nil {
		return analytics, err
	}
	analytics.Totals = usageTotalsFromRollup(aggregates.total)
	if analytics.ByModel, err = s.usageDimensionFromRollup(ctx, rollupQuery, "model_id"); err != nil {
		return analytics, err
	}
	if analytics.ByGroup, err = s.usageGroupRowsFromRollup(ctx, rollupQuery); err != nil {
		return analytics, err
	}
	if analytics.ByEndpoint, err = s.usageDimensionFromRollup(ctx, rollupQuery, "endpoint"); err != nil {
		return analytics, err
	}
	if analytics.ByKey, err = s.usageDimensionFromRollup(ctx, rollupQuery, "key_id"); err != nil {
		return analytics, err
	}
	if analytics.Trend, err = s.usageTrendFromRollup(ctx, rollupQuery, bucketSeconds); err != nil {
		return analytics, err
	}
	return analytics, nil
}
