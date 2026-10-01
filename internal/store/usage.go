package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// UsageGranularity names the trend bucket size accepted by the usage records
// page. Anything else is rejected rather than silently bucketed by minute.
type UsageGranularity = string

const (
	UsageGranularityMinute UsageGranularity = "minute"
	UsageGranularityHour   UsageGranularity = "hour"
	UsageGranularityDay    UsageGranularity = "day"
)

const (
	maxUsageDimensionRows = 50
	maxUsageFilterValues  = 32
	// maxUsageRecordLimit keeps the detail table page bounded; the audit and
	// activity endpoints use the same ceiling for consistency.
	maxUsageRecordLimit = 1000
)

// UsageQuery selects the activity rows behind the usage records page. Every
// filter is an allowlist that is ANDed with the others; an empty field matches
// every row. Key filtering accepts the key ids reported by UsageFilterOptions.
type UsageQuery struct {
	Keys      []string
	Models    []string
	Groups    []string
	Endpoints []string
	Start     time.Time
	End       time.Time
	// Sort is one of the usage record sort keys ("time", "cost", "tokens",
	// "duration"); anything else falls back to "time".
	Sort   string
	Order  string
	Limit  int
	Offset int
}

// UsageTotals is the summary-card aggregate for the current filter selection.
type UsageTotals struct {
	Requests            int     `json:"requests"`
	InputTokens         int     `json:"inputTokens"`
	OutputTokens        int     `json:"outputTokens"`
	CachedTokens        int     `json:"cachedTokens"`
	CacheCreationTokens int     `json:"cacheCreationTokens"`
	ReasoningTokens     int     `json:"reasoningTokens"`
	TotalTokens         int     `json:"totalTokens"`
	EstimatedCost       float64 `json:"estimatedCost"`
	CostEstimated       bool    `json:"costEstimated"`
	AverageDurationMs   int64   `json:"averageDurationMs"`
	AverageFirstTokenMs int64   `json:"averageFirstTokenMs"`
	CacheHitRatio       float64 `json:"cacheHitRatio"`
	CacheCreationRatio  float64 `json:"cacheCreationRatio"`
	PromptsPerSecond    float64 `json:"promptsPerSecond"`
	GenerationPerSecond float64 `json:"generationPerSecond"`
}

// UsageDimensionRow is one entry of a distribution table. Rows are ordered by
// request count descending so a donut can take a stable prefix.
type UsageDimensionRow struct {
	Name                string  `json:"name"`
	Requests            int     `json:"requests"`
	InputTokens         int     `json:"inputTokens"`
	OutputTokens        int     `json:"outputTokens"`
	CachedTokens        int     `json:"cachedTokens"`
	CacheCreationTokens int     `json:"cacheCreationTokens"`
	EstimatedCost       float64 `json:"estimatedCost"`
}

// UsageTrendBucket is one time bucket of the token trend chart.
type UsageTrendBucket struct {
	BucketStart         time.Time `json:"bucketStart"`
	Requests            int       `json:"requests"`
	InputTokens         int       `json:"inputTokens"`
	OutputTokens        int       `json:"outputTokens"`
	CachedTokens        int       `json:"cachedTokens"`
	CacheCreationTokens int       `json:"cacheCreationTokens"`
	CacheHitRatio       float64   `json:"cacheHitRatio"`
}

// UsageAnalytics is the whole dashboard payload for one filter selection: the
// summary cards plus every distribution and the bucketed trend series.
type UsageAnalytics struct {
	Start       time.Time           `json:"start"`
	End         time.Time           `json:"end"`
	Granularity string              `json:"granularity"`
	Totals      UsageTotals         `json:"totals"`
	ByModel     []UsageDimensionRow `json:"byModel"`
	ByGroup     []UsageDimensionRow `json:"byGroup"`
	ByEndpoint  []UsageDimensionRow `json:"byEndpoint"`
	ByKey       []UsageDimensionRow `json:"byKey"`
	Trend       []UsageTrendBucket  `json:"trend"`
}

// UsageRecord is one row of the usage detail table. Key metadata is joined
// from the api_keys table so the table can show the human name and usage group
// of the credential that made the request.
type UsageRecord struct {
	ID                  int       `json:"id"`
	Timestamp           time.Time `json:"timestamp"`
	Model               string    `json:"model"`
	KeyID               string    `json:"keyId"`
	KeyName             string    `json:"keyName"`
	KeyKind             string    `json:"keyKind"`
	KeyGroup            string    `json:"keyGroup"`
	Endpoint            string    `json:"endpoint"`
	ClientIP            string    `json:"clientIp"`
	RespStatusCode      int       `json:"respStatusCode"`
	InputTokens         int       `json:"inputTokens"`
	OutputTokens        int       `json:"outputTokens"`
	CachedTokens        int       `json:"cachedTokens"`
	CacheCreationTokens int       `json:"cacheCreationTokens"`
	ReasoningTokens     int       `json:"reasoningTokens"`
	EstimatedCost       float64   `json:"estimatedCost"`
	CostEstimated       bool      `json:"costEstimated"`
	DurationMs          int       `json:"durationMs"`
	FirstTokenMs        int       `json:"firstTokenMs"`
}

type UsageRecordPage struct {
	Data       []UsageRecord `json:"data"`
	Total      int           `json:"total"`
	Limit      int           `json:"limit"`
	Offset     int           `json:"offset"`
	TotalPages int           `json:"totalPages"`
}

// UsageFilterOptions lists the distinct values available in the activity
// table so the dashboard's filter selects never offer a value that matches no
// row.
type UsageFilterOptions struct {
	Keys      []UsageKeyOption `json:"keys"`
	Models    []string         `json:"models"`
	Groups    []string         `json:"groups"`
	Endpoints []string         `json:"endpoints"`
}

type UsageKeyOption struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Group string `json:"group"`
}

func normalizeUsageGranularity(granularity string) (string, int64, error) {
	switch strings.ToLower(strings.TrimSpace(granularity)) {
	case "", UsageGranularityDay:
		return UsageGranularityDay, 86400, nil
	case UsageGranularityHour:
		return UsageGranularityHour, 3600, nil
	case UsageGranularityMinute:
		return UsageGranularityMinute, 60, nil
	default:
		return "", 0, fmt.Errorf("unsupported usage granularity: %s", granularity)
	}
}

// UsageAnalytics aggregates the filtered activity rows in one pass over the
// table. SQLite does the grouping so the payload stays bounded even for a
// deployment that kept months of rows.
func (s *Store) UsageAnalytics(ctx context.Context, query UsageQuery, granularity string) (UsageAnalytics, error) {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return UsageAnalytics{}, err
	}
	canonical, bucketSeconds, err := normalizeUsageGranularity(granularity)
	if err != nil {
		return UsageAnalytics{}, err
	}
	where, args := usageWhere(query)
	analytics := UsageAnalytics{
		Start:       query.Start,
		End:         query.End,
		Granularity: canonical,
		ByModel:     []UsageDimensionRow{},
		ByGroup:     []UsageDimensionRow{},
		ByEndpoint:  []UsageDimensionRow{},
		ByKey:       []UsageDimensionRow{},
		Trend:       []UsageTrendBucket{},
	}
	totals, err := s.usageTotals(ctx, where, args)
	if err != nil {
		return UsageAnalytics{}, err
	}
	analytics.Totals = totals
	if analytics.ByModel, err = s.usageDimension(ctx, where, args, "model_id"); err != nil {
		return UsageAnalytics{}, err
	}
	if analytics.ByGroup, err = s.usageDimension(ctx, where, args, usageGroupExpression); err != nil {
		return UsageAnalytics{}, err
	}
	if analytics.ByEndpoint, err = s.usageDimension(ctx, where, args, "req_path"); err != nil {
		return UsageAnalytics{}, err
	}
	if analytics.ByKey, err = s.usageDimension(ctx, where, args, usageKeyExpression); err != nil {
		return UsageAnalytics{}, err
	}
	if analytics.Trend, err = s.usageTrend(ctx, where, args, bucketSeconds); err != nil {
		return UsageAnalytics{}, err
	}
	return analytics, nil
}

// usageGroupExpression labels a row by its key's usage group. An empty group
// is rendered as a stable placeholder instead of an empty donut segment.
const usageGroupExpression = `COALESCE(NULLIF((SELECT group_name FROM api_keys WHERE api_keys.id = activity.key_id), ''), 'ungrouped')`

// usageKeyExpression prefers the managed key's display name and falls back to
// the raw key id (legacy credentials have no row in api_keys).
const usageKeyExpression = `COALESCE(NULLIF((SELECT name FROM api_keys WHERE api_keys.id = activity.key_id), ''), CASE WHEN activity.key_id = '' THEN 'anonymous' ELSE activity.key_id END)`

// usageTotals computes the summary cards. Average durations only consider rows
// that reported a boundary: a backend that never exposed a first token must
// not drag the TTFT average toward zero.
func (s *Store) usageTotals(ctx context.Context, where string, args []any) (UsageTotals, error) {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return UsageTotals{}, err
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
			COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
			COALESCE(SUM(cache_tokens),0), COALESCE(SUM(cache_creation_tokens),0), COALESCE(SUM(reasoning_tokens),0),
			COALESCE(SUM(estimated_cost),0), COALESCE(MAX(cost_estimated),0),
			COALESCE(AVG(duration_ms),0), COALESCE(AVG(NULLIF(first_token_ms,-1)),0),
			COALESCE(AVG(NULLIF(prompt_per_second,0)),0), COALESCE(AVG(NULLIF(tokens_per_second,0)),0)
		FROM activity`+where, args...)
	var totals UsageTotals
	var costEstimated sql.NullInt64
	// SQLite always returns REAL for AVG, including a whole-number average, so
	// the duration columns are scanned as floats and rounded here. Scanning them
	// into the int fields directly fails on the first non-integral average.
	var averageDuration, averageFirstToken float64
	if err := row.Scan(&totals.Requests, &totals.InputTokens, &totals.OutputTokens, &totals.CachedTokens,
		&totals.CacheCreationTokens, &totals.ReasoningTokens, &totals.EstimatedCost, &costEstimated,
		&averageDuration, &averageFirstToken, &totals.PromptsPerSecond, &totals.GenerationPerSecond); err != nil {
		return UsageTotals{}, fmt.Errorf("usage totals: %w", err)
	}
	totals.AverageDurationMs = roundAverageMs(averageDuration)
	totals.AverageFirstTokenMs = roundAverageMs(averageFirstToken)
	totals.TotalTokens = totals.InputTokens + totals.OutputTokens
	totals.CostEstimated = costEstimated.Valid && costEstimated.Int64 != 0
	totals.CacheHitRatio = aggregateCacheHitRatio(totals.InputTokens, totals.CachedTokens, totals.CacheCreationTokens)
	totals.CacheCreationRatio = aggregateCacheCreationRatio(totals.InputTokens, totals.CachedTokens, totals.CacheCreationTokens)
	return totals, nil
}

// roundAverageMs converts an SQLite REAL average into the whole milliseconds
// the API reports. It is nearest-even rounding rather than truncation so a mean
// of 2774.5 does not report 2774 on one query and 2775 on the next.
func roundAverageMs(value float64) int64 {
	if !(value > 0) || value >= float64(math.MaxInt64) {
		return 0
	}
	return int64(math.Round(value))
}

func (s *Store) usageDimension(ctx context.Context, where string, args []any, expression string) ([]UsageDimensionRow, error) {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	// The group and key expressions are package constants, never caller input;
	// filter values arrive as bound parameters below.
	statement := fmt.Sprintf(`
		SELECT %s AS dimension, COUNT(*),
			COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
			COALESCE(SUM(cache_tokens),0), COALESCE(SUM(cache_creation_tokens),0),
			COALESCE(SUM(estimated_cost),0)
		FROM activity%s
		GROUP BY dimension
		ORDER BY COUNT(*) DESC, dimension ASC
		LIMIT %d`, expression, where, maxUsageDimensionRows)
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("usage dimension: %w", err)
	}
	defer rows.Close()
	dimension := []UsageDimensionRow{}
	for rows.Next() {
		var row UsageDimensionRow
		if err := rows.Scan(&row.Name, &row.Requests, &row.InputTokens, &row.OutputTokens,
			&row.CachedTokens, &row.CacheCreationTokens, &row.EstimatedCost); err != nil {
			return nil, fmt.Errorf("usage dimension row: %w", err)
		}
		dimension = append(dimension, row)
	}
	return dimension, rows.Err()
}

func (s *Store) usageTrend(ctx context.Context, where string, args []any, bucketSeconds int64) ([]UsageTrendBucket, error) {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	// ts_created is stored in whole seconds, so integer division buckets every
	// row without depending on SQLite date functions.
	statement := fmt.Sprintf(`
		SELECT (ts_created / ?) * ? AS bucket, COUNT(*),
			COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
			COALESCE(SUM(cache_tokens),0), COALESCE(SUM(cache_creation_tokens),0)
		FROM activity%s
		GROUP BY bucket
		ORDER BY bucket ASC
		LIMIT %d`, where, maxUsageDimensionRows*4)
	rows, err := s.db.QueryContext(ctx, statement, append([]any{bucketSeconds, bucketSeconds}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("usage trend: %w", err)
	}
	defer rows.Close()
	trend := []UsageTrendBucket{}
	for rows.Next() {
		var bucket UsageTrendBucket
		var bucketStart int64
		if err := rows.Scan(&bucketStart, &bucket.Requests, &bucket.InputTokens, &bucket.OutputTokens,
			&bucket.CachedTokens, &bucket.CacheCreationTokens); err != nil {
			return nil, fmt.Errorf("usage trend row: %w", err)
		}
		bucket.BucketStart = time.Unix(bucketStart, 0).UTC()
		bucket.CacheHitRatio = aggregateCacheHitRatio(bucket.InputTokens, bucket.CachedTokens, bucket.CacheCreationTokens)
		trend = append(trend, bucket)
	}
	return trend, rows.Err()
}

// UsageRecords returns one page of the detail table. Sorting accepts a small
// whitelist so a query-string column name can never reach the SQL verbatim.
func (s *Store) UsageRecords(ctx context.Context, query UsageQuery) (UsageRecordPage, error) {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return UsageRecordPage{}, err
	}
	limit := query.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > maxUsageRecordLimit {
		limit = maxUsageRecordLimit
	}
	offset := query.Offset
	if offset < 0 {
		offset = 0
	}
	where, args := usageWhere(query)
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM activity`+where, args...).Scan(&total); err != nil {
		return UsageRecordPage{}, fmt.Errorf("count usage records: %w", err)
	}
	statement := `
		SELECT activity.id, activity.ts_created, activity.model_id,
			activity.key_id, COALESCE(NULLIF(api_keys.name,''),''), COALESCE(NULLIF(api_keys.kind,''),''), COALESCE(NULLIF(api_keys.group_name,''),''),
			activity.req_path, activity.client_ip, activity.resp_status_code,
			activity.input_tokens, activity.output_tokens, activity.cache_tokens, activity.cache_creation_tokens, activity.reasoning_tokens,
			activity.estimated_cost, activity.cost_estimated, activity.duration_ms, activity.first_token_ms
		FROM activity
		LEFT JOIN api_keys ON api_keys.id = activity.key_id` + where + usageRecordOrder(query) + `
		LIMIT ? OFFSET ?`
	rows, err := s.db.QueryContext(ctx, statement, append(args, limit, offset)...)
	if err != nil {
		return UsageRecordPage{}, fmt.Errorf("list usage records: %w", err)
	}
	defer rows.Close()
	records := []UsageRecord{}
	for rows.Next() {
		var record UsageRecord
		var timestamp int64
		var costEstimated sql.NullInt64
		var firstToken sql.NullInt64
		if err := rows.Scan(&record.ID, &timestamp, &record.Model,
			&record.KeyID, &record.KeyName, &record.KeyKind, &record.KeyGroup,
			&record.Endpoint, &record.ClientIP, &record.RespStatusCode,
			&record.InputTokens, &record.OutputTokens, &record.CachedTokens, &record.CacheCreationTokens, &record.ReasoningTokens,
			&record.EstimatedCost, &costEstimated, &record.DurationMs, &firstToken); err != nil {
			return UsageRecordPage{}, fmt.Errorf("usage record row: %w", err)
		}
		record.Timestamp = time.Unix(timestamp, 0).UTC()
		if costEstimated.Valid {
			record.CostEstimated = costEstimated.Int64 != 0
		}
		record.FirstTokenMs = int(firstToken.Int64)
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return UsageRecordPage{}, fmt.Errorf("usage record rows: %w", err)
	}
	return UsageRecordPage{Data: records, Total: total, Limit: limit, Offset: offset, TotalPages: calculateTotalPages(total, limit)}, nil
}

func usageRecordOrder(query UsageQuery) string {
	descending := !strings.EqualFold(strings.TrimSpace(query.Order), "asc")
	column := "activity.ts_created"
	switch strings.ToLower(strings.TrimSpace(query.Sort)) {
	case "cost":
		column = "activity.estimated_cost"
	case "tokens":
		column = "activity.input_tokens + activity.output_tokens"
	case "duration":
		column = "activity.duration_ms"
	}
	direction := "DESC"
	if !descending {
		direction = "ASC"
	}
	return fmt.Sprintf(` ORDER BY %s %s, activity.id %s`, column, direction, direction)
}

// UsageFilterOptions reports the distinct filter values currently present.
// Keys come from the durable key table (so revoked keys stay selectable for
// historical rows) while models, groups and endpoints come from the activity
// rows themselves.
func (s *Store) UsageFilterOptions(ctx context.Context, query UsageQuery) (UsageFilterOptions, error) {
	ctx = normalizeContext(ctx)
	options := UsageFilterOptions{Keys: []UsageKeyOption{}, Models: []string{}, Groups: []string{}, Endpoints: []string{}}
	if s == nil || s.db == nil {
		return options, errors.New("store is unavailable")
	}
	where, args := usageWhere(query)
	// usageWhere already emits " WHERE ..." when filters are present, so the
	// non-null condition joins with AND instead of embedding a second WHERE
	// (a filtered request used to fail with "near WHERE: syntax error").
	// usageWhere already emits " WHERE ..." when filters are present, so the
	// non-null condition joins with AND instead of embedding a second WHERE
	// (a filtered request used to fail with "near WHERE: syntax error").
	distinct := func(column string) ([]string, error) {
		prefix := where
		if prefix == "" {
			prefix = " WHERE 1=1"
		}
		nonEmpty := fmt.Sprintf(`%s IS NOT NULL AND %s <> ''`, column, column)
		statement := fmt.Sprintf(`SELECT DISTINCT %s FROM activity%s AND %s ORDER BY %s ASC LIMIT %d`,
			column, prefix, nonEmpty, column, maxUsageFilterValues)
		rows, err := s.db.QueryContext(ctx, statement, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		values := []string{}
		for rows.Next() {
			var value string
			if err := rows.Scan(&value); err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		return values, rows.Err()
	}
	keyRows, err := s.db.QueryContext(ctx, `SELECT id, COALESCE(NULLIF(name,''),id), COALESCE(NULLIF(kind,''), 'management'), COALESCE(NULLIF(group_name,''),'') FROM api_keys ORDER BY created_at DESC, id`)
	if err != nil {
		return options, fmt.Errorf("usage filter keys: %w", err)
	}
	defer keyRows.Close()
	for keyRows.Next() {
		var option UsageKeyOption
		if err := keyRows.Scan(&option.ID, &option.Name, &option.Kind, &option.Group); err != nil {
			return options, err
		}
		options.Keys = append(options.Keys, option)
	}
	if err := keyRows.Err(); err != nil {
		return options, err
	}
	if options.Models, err = distinct("model_id"); err != nil {
		return options, fmt.Errorf("usage filter models: %w", err)
	}
	if options.Endpoints, err = distinct("req_path"); err != nil {
		return options, fmt.Errorf("usage filter endpoints: %w", err)
	}
	if options.Groups, err = distinct("COALESCE(NULLIF((SELECT group_name FROM api_keys WHERE api_keys.id = activity.key_id), ''), 'ungrouped')"); err != nil {
		return options, fmt.Errorf("usage filter groups: %w", err)
	}
	return options, nil
}

// usageWhere builds the shared filter clause for every usage query. Empty
// allowlists contribute no clause so a caller that omits everything still gets
// an unfiltered table.
func usageWhere(query UsageQuery) (string, []any) {
	conditions := make([]string, 0, 8)
	args := make([]any, 0, 8)
	if len(query.Keys) > 0 {
		clause, clauseArgs := inClause("key_id", query.Keys)
		conditions = append(conditions, clause)
		args = append(args, clauseArgs...)
	}
	if len(query.Models) > 0 {
		clause, clauseArgs := inClause("model_id", query.Models)
		conditions = append(conditions, clause)
		args = append(args, clauseArgs...)
	}
	if len(query.Endpoints) > 0 {
		clause, clauseArgs := inClause("req_path", query.Endpoints)
		conditions = append(conditions, clause)
		args = append(args, clauseArgs...)
	}
	if len(query.Groups) > 0 {
		// The group lives on the key, so an access row can only be matched
		// through the subquery. Rows whose key was deleted or that arrived
		// anonymously are addressable through the "ungrouped" label.
		clause, clauseArgs := inClause("COALESCE(NULLIF((SELECT group_name FROM api_keys WHERE api_keys.id = activity.key_id), ''), 'ungrouped')", query.Groups)
		conditions = append(conditions, clause)
		args = append(args, clauseArgs...)
	}
	if !query.Start.IsZero() {
		conditions = append(conditions, "ts_created >= ?")
		args = append(args, query.Start.Unix())
	}
	if !query.End.IsZero() {
		conditions = append(conditions, "ts_created <= ?")
		args = append(args, query.End.Unix())
	}
	if len(conditions) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

func inClause(column string, values []string) (string, []any) {
	placeholders := make([]string, 0, len(values))
	args := make([]any, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		placeholders = append(placeholders, "?")
		args = append(args, trimmed)
	}
	if len(placeholders) == 0 {
		return "1=1", nil
	}
	return column + " IN (" + strings.Join(placeholders, ",") + ")", args
}
