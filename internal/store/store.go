package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// TokenMetrics holds token usage and performance metrics.
type TokenMetrics struct {
	CachedTokens    int     `json:"cache_tokens"`
	DraftTokens     int     `json:"draft_tokens"`
	DraftAccTokens  int     `json:"draft_acc_tokens"`
	InputTokens     int     `json:"input_tokens"`
	OutputTokens    int     `json:"output_tokens"`
	PromptPerSecond float64 `json:"prompt_per_second"`
	TokensPerSecond float64 `json:"tokens_per_second"`
}

// ActivityLogEntry represents parsed token statistics from llama-server logs.
type ActivityLogEntry struct {
	ID                  int          `json:"id"`
	Timestamp           time.Time    `json:"timestamp"`
	Model               string       `json:"model"`
	KeyID               string       `json:"key_id,omitempty"`
	SessionID           string       `json:"session_id,omitempty"`
	ReqPath             string       `json:"req_path"`
	RespContentType     string       `json:"resp_content_type"`
	RespStatusCode      int          `json:"resp_status_code"`
	Tokens              TokenMetrics `json:"tokens"`
	CacheCreationTokens int          `json:"cache_creation_tokens,omitempty"`
	ReasoningTokens     int          `json:"reasoning_tokens,omitempty"`
	EstimatedCost       float64      `json:"estimated_cost,omitempty"`
	CostEstimated       bool         `json:"cost_estimated,omitempty"`
	CacheHitRatio       float64      `json:"cache_hit_ratio,omitempty"`
	CacheCreationRatio  float64      `json:"cache_creation_ratio,omitempty"`
	RepairApplied       bool         `json:"repair_applied,omitempty"`
	PrefixHash          string       `json:"prefix_hash,omitempty"`
	// ClientIP is the originating caller address. The usage records page shows
	// and filters by it, and it is the value an access key's IP allowlist is
	// checked against.
	ClientIP   string `json:"client_ip,omitempty"`
	DurationMs int    `json:"duration_ms"`
	// FirstTokenMs is the time from request admission to the first visible
	// response token. It is -1 when the backend did not expose a trustworthy
	// first-token boundary (for example a non-streaming response without TTFT).
	FirstTokenMs int `json:"first_token_ms"`
	// DecodeMs is the interval between the first and last visible streamed
	// token. It is -1 when the request was not a text stream (or the window
	// was too small to measure), and pairs with FirstTokenMs for the phase
	// share shown in the detail view.
	DecodeMs int `json:"decode_ms"`
	// SpeedTimeline is a bounded JSON array of [msSinceStart, cumulativeTokens]
	// pairs sampled from the streamed response; empty when not measured. It
	// powers the per-request speed curve in the detail view.
	SpeedTimeline string            `json:"speed_timeline,omitempty"`
	HasAudit      bool              `json:"has_audit"`
	ErrorMsg      string            `json:"error_msg,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

// ActivityFilter narrows which activity rows a query matches. The zero value
// matches every row, and each field is independent: set fields are ANDed.
type ActivityFilter struct {
	Models    []string  // model_id IN (...); empty matches all models
	KeyID     string    // exact managed API key id; empty matches all keys
	SessionID string    // exact client session id; empty matches all sessions
	Start     time.Time // inclusive lower bound on ts_created; zero is unbounded
	End       time.Time // inclusive upper bound on ts_created; zero is unbounded
	MinID     int       // inclusive lower bound on id; 0 is unbounded
	MaxID     int       // inclusive upper bound on id; 0 is unbounded
}

type ActivityQuery struct {
	ActivityFilter
	Limit int
	Page  int
	Sort  string // sortable column key, empty defaults to "id"
	Order string // "asc" or "desc", empty defaults to "desc"
}

// activitySortColumns whitelists the sortable API keys and maps them to their
// underlying SQL columns. Keys mirror the UI column ids. The whitelist keeps
// user-supplied sort input from reaching the query as raw SQL.
var activitySortColumns = map[string]string{
	"id":                "id",
	"time":              "ts_created",
	"model":             "model_id",
	"req_path":          "req_path",
	"resp_status_code":  "resp_status_code",
	"resp_content_type": "resp_content_type",
	"cached":            "cache_tokens",
	"prompt":            "input_tokens",
	"generated":         "output_tokens",
	"drafted":           "draft_tokens",
	"prompt_speed":      "prompt_per_second",
	"gen_speed":         "tokens_per_second",
	"duration":          "duration_ms",
}

// ActivitySortColumn returns the SQL column for a sortable API key and whether
// the key is valid.
func ActivitySortColumn(key string) (string, bool) {
	col, ok := activitySortColumns[key]
	return col, ok
}

type ActivityPage struct {
	Data       []ActivityLogEntry `json:"data"`
	Page       int                `json:"page"`
	Limit      int                `json:"limit"`
	Total      int                `json:"total"`
	TotalPages int                `json:"total_pages"`
}

type ActivityStatsQuery struct {
	Model string
	// Models is an optional exact allowlist. Model takes precedence when set
	// for backwards compatibility with callers that issue one-model queries.
	Models    []string
	KeyID     string
	SessionID string
	Start     time.Time
	End       time.Time
	MinID     int
	MaxID     int
}

type ActivityStats struct {
	TotalRequests            int            `json:"total_requests"`
	TotalInputTokens         int            `json:"total_input_tokens"`
	TotalOutputTokens        int            `json:"total_output_tokens"`
	TotalCacheTokens         int            `json:"total_cache_tokens"`
	TotalCacheCreationTokens int            `json:"total_cache_creation_tokens"`
	TotalReasoningTokens     int            `json:"total_reasoning_tokens"`
	CacheHitRatio            float64        `json:"cache_hit_ratio,omitempty"`
	CacheCreationRatio       float64        `json:"cache_creation_ratio,omitempty"`
	EstimatedCost            float64        `json:"estimated_cost"`
	CostEstimated            bool           `json:"cost_estimated"`
	ByModel                  []ModelUsage   `json:"by_model,omitempty"`
	PromptHistogram          *HistogramData `json:"prompt_histogram"`
	GenerationHistogram      *HistogramData `json:"gen_histogram"`
}

type HistogramData struct {
	Bins    []int   `json:"bins"`
	Min     float64 `json:"min"`
	Max     float64 `json:"max"`
	BinSize float64 `json:"binSize"`
	P50     float64 `json:"p50"`
	P95     float64 `json:"p95"`
	P99     float64 `json:"p99"`
}

type Store struct {
	db       *sql.DB
	inMemory bool
	// blobs is the content-addressed file store for audit bodies. It is nil
	// for in-memory stores, where bodies stay inline (nothing is persisted
	// anyway) and no blob directory is created.
	blobs *BlobStore
}

// normalizeContext keeps the store boundary safe for callers that do not
// have an optional request context. database/sql rejects a nil Context and
// would otherwise panic in QueryContext/ExecContext/BeginTx.
func normalizeContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// ensureDB keeps every public Store operation safe for optional embedders. A
// nil receiver and a zero-value Store are both valid Go values to pass around
// while a control plane is being assembled; they must return a diagnostic
// error instead of panicking when a handler reaches the persistence boundary.
func (s *Store) ensureDB() error {
	if s == nil || s.db == nil {
		return errors.New("store is unavailable")
	}
	return nil
}

// IsInMemory returns true if the store is using an in-memory database.
func (s *Store) IsInMemory() bool {
	return s != nil && s.inMemory
}

// New opens a SQLite store at path. An empty path creates an in-memory store.
func New(path string) (*Store, error) {
	dsn := strings.TrimSpace(path)
	diskFile := dsn != ""
	if dsn == "" {
		dsn = ":memory:"
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite store: %w", err)
	}
	db.SetMaxOpenConns(1)

	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout = 5000`); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure sqlite store: %w", err)
	}
	if diskFile {
		var mode string
		if err := db.QueryRowContext(ctx, `PRAGMA journal_mode = WAL`).Scan(&mode); err != nil {
			db.Close()
			return nil, fmt.Errorf("enable sqlite store WAL mode: %w", err)
		}
		if !strings.EqualFold(mode, "wal") {
			db.Close()
			return nil, fmt.Errorf("enable sqlite store WAL mode: got %q", mode)
		}
	}
	if err := runMigrations(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	st := &Store{db: db, inMemory: !diskFile}
	if diskFile {
		st.blobs = newBlobStore(dsn)
	}
	return st, nil
}

func runMigrations(ctx context.Context, db *sql.DB) error {
	migrations, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		return fmt.Errorf("sqlite store migrations: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations)
	if err != nil {
		return fmt.Errorf("sqlite store migrations: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("sqlite store migrations up: %w", err)
	}
	return nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) InsertActivity(ctx context.Context, entry ActivityLogEntry) (ActivityLogEntry, error) {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return ActivityLogEntry{}, err
	}
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now()
	}
	entry.CacheCreationRatio = normalizeCacheRatio(entry.CacheCreationRatio)
	if entry.FirstTokenMs <= 0 {
		entry.FirstTokenMs = -1
	}
	if entry.CacheCreationRatio == 0 && entry.CacheCreationTokens > 0 {
		entry.CacheCreationRatio = cacheCreationRatio(entry.Tokens.InputTokens, entry.Tokens.CachedTokens, entry.CacheCreationTokens)
	}
	metadataJSON, err := marshalMetadata(entry.Metadata)
	if err != nil {
		return ActivityLogEntry{}, err
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO activity (
			ts_created, model_id, key_id, session_id, req_path, resp_content_type, resp_status_code,
			cache_tokens, draft_tokens, draft_acc_tokens, input_tokens, output_tokens,
			prompt_per_second, tokens_per_second, duration_ms, first_token_ms, error_msg, metadata_json,
			cache_creation_tokens, reasoning_tokens, estimated_cost, cost_estimated,
			cache_hit_ratio, cache_creation_ratio, repair_applied, prefix_hash, client_ip,
			decode_ms, speed_timeline
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.Timestamp.Unix(),
		entry.Model,
		entry.KeyID,
		entry.SessionID,
		entry.ReqPath,
		entry.RespContentType,
		entry.RespStatusCode,
		entry.Tokens.CachedTokens,
		entry.Tokens.DraftTokens,
		entry.Tokens.DraftAccTokens,
		entry.Tokens.InputTokens,
		entry.Tokens.OutputTokens,
		entry.Tokens.PromptPerSecond,
		entry.Tokens.TokensPerSecond,
		entry.DurationMs,
		entry.FirstTokenMs,
		entry.ErrorMsg,
		metadataJSON,
		entry.CacheCreationTokens,
		entry.ReasoningTokens,
		entry.EstimatedCost,
		boolInt(entry.CostEstimated),
		entry.CacheHitRatio,
		entry.CacheCreationRatio,
		boolInt(entry.RepairApplied),
		entry.PrefixHash,
		entry.ClientIP,
		normalizeDecodeMs(entry.DecodeMs),
		entry.SpeedTimeline,
	)
	if err != nil {
		return ActivityLogEntry{}, fmt.Errorf("insert activity: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return ActivityLogEntry{}, fmt.Errorf("insert activity id: %w", err)
	}
	entry.ID = int(id)
	return entry, nil
}

// normalizeDecodeMs keeps unknown/absent decode windows in the -1 sentinel
// convention the rest of the activity telemetry uses.
func normalizeDecodeMs(value int) int {
	if value <= 0 {
		return -1
	}
	return value
}

func (s *Store) ListActivity(ctx context.Context, query ActivityQuery) (ActivityPage, error) {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return ActivityPage{}, err
	}
	query = normalizeActivityQuery(query)
	offset := (query.Page - 1) * query.Limit

	where, args := activityWhere(query.ActivityFilter)
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM activity`+where, args...).Scan(&total); err != nil {
		return ActivityPage{}, fmt.Errorf("count activity: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT
			id, ts_created, model_id, key_id, session_id, req_path, resp_content_type, resp_status_code,
			cache_tokens, draft_tokens, draft_acc_tokens, input_tokens, output_tokens,
			prompt_per_second, tokens_per_second, duration_ms, first_token_ms, error_msg, metadata_json,
			cache_creation_tokens, reasoning_tokens, estimated_cost, cost_estimated,
			cache_hit_ratio, cache_creation_ratio, repair_applied, prefix_hash, client_ip,
			decode_ms, speed_timeline,
			EXISTS (SELECT 1 FROM audit_conversations AS audit WHERE audit.activity_id = activity.id)
		FROM activity`+where+activityOrderBy(query)+`
		LIMIT ? OFFSET ?`,
		append(args, query.Limit, offset)...,
	)
	if err != nil {
		return ActivityPage{}, fmt.Errorf("list activity: %w", err)
	}
	defer rows.Close()

	entries := []ActivityLogEntry{}
	for rows.Next() {
		entry, err := scanActivity(rows)
		if err != nil {
			return ActivityPage{}, err
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return ActivityPage{}, fmt.Errorf("list activity rows: %w", err)
	}

	return ActivityPage{
		Data:       entries,
		Page:       query.Page,
		Limit:      query.Limit,
		Total:      total,
		TotalPages: calculateTotalPages(total, query.Limit),
	}, nil
}

// GetActivity returns one persisted activity row by its stable database id.
// Keeping this lookup at the store boundary lets control-plane capture access
// enforce the same model scope as activity pages without exposing the raw key
// or duplicating SQL in the HTTP layer.
func (s *Store) GetActivity(ctx context.Context, id int) (ActivityLogEntry, bool, error) {
	if id < 1 {
		return ActivityLogEntry{}, false, nil
	}
	page, err := s.ListActivity(ctx, ActivityQuery{
		ActivityFilter: ActivityFilter{MinID: id, MaxID: id},
		Limit:          1,
		Page:           1,
	})
	if err != nil {
		return ActivityLogEntry{}, false, err
	}
	if len(page.Data) == 0 {
		return ActivityLogEntry{}, false, nil
	}
	return page.Data[0], true, nil
}

func (s *Store) ActivityStats(ctx context.Context, query ActivityStatsQuery) (ActivityStats, error) {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return ActivityStats{}, err
	}
	filter := ActivityFilter{}
	if model := strings.TrimSpace(query.Model); model != "" {
		filter.Models = []string{model}
	} else if len(query.Models) > 0 {
		filter.Models = append([]string(nil), query.Models...)
	}
	filter.KeyID = strings.TrimSpace(query.KeyID)
	filter.SessionID = strings.TrimSpace(query.SessionID)
	filter.Start = query.Start
	filter.End = query.End
	filter.MinID = query.MinID
	filter.MaxID = query.MaxID
	where, args := activityWhere(filter)
	aggregates, err := s.aggregateActivity(ctx, where, args)
	if err != nil {
		return ActivityStats{}, fmt.Errorf("activity stats: %w", err)
	}
	aggregate := aggregates.total
	stats := ActivityStats{
		TotalRequests:            aggregate.Requests,
		TotalInputTokens:         aggregate.InputTokens,
		TotalOutputTokens:        aggregate.OutputTokens,
		TotalCacheTokens:         aggregate.CachedTokens,
		TotalCacheCreationTokens: aggregate.CacheCreationTokens,
		TotalReasoningTokens:     aggregate.ReasoningTokens,
		EstimatedCost:            aggregate.EstimatedCost,
		CostEstimated:            aggregate.CostEstimated,
	}
	stats.CacheHitRatio = aggregateCacheHitRatio(stats.TotalInputTokens, stats.TotalCacheTokens, stats.TotalCacheCreationTokens)
	stats.CacheCreationRatio = aggregateCacheCreationRatio(stats.TotalInputTokens, stats.TotalCacheTokens, stats.TotalCacheCreationTokens)
	stats.ByModel = modelUsageRows(aggregates.byModel)

	promptValues, genValues, err := s.speedValues(ctx, where, args)
	if err != nil {
		return ActivityStats{}, err
	}
	stats.PromptHistogram = calculateHistogramData(promptValues)
	stats.GenerationHistogram = calculateHistogramData(genValues)
	return stats, nil
}

// speedValues reads both histogram source columns in a single scan. Zero
// values mean the speed was not reported and are excluded per column. No
// ORDER BY: calculateHistogramData sorts the values itself.
func (s *Store) speedValues(ctx context.Context, where string, args []any) (prompt, gen []float64, err error) {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return nil, nil, err
	}
	filter := where
	if filter == "" {
		filter = ` WHERE prompt_per_second > 0 OR tokens_per_second > 0`
	} else {
		filter += ` AND (prompt_per_second > 0 OR tokens_per_second > 0)`
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT prompt_per_second, tokens_per_second FROM activity`+filter, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("activity histogram: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var promptValue, genValue float64
		if err := rows.Scan(&promptValue, &genValue); err != nil {
			return nil, nil, fmt.Errorf("activity histogram row: %w", err)
		}
		if promptValue > 0 {
			prompt = append(prompt, promptValue)
		}
		if genValue > 0 {
			gen = append(gen, genValue)
		}
	}
	return prompt, gen, rows.Err()
}

func (s *Store) PruneActivity(ctx context.Context, maxRows int) error {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return err
	}
	if maxRows <= 0 {
		return nil
	}
	// AUTOINCREMENT ids are monotonic and never reused, so the rows beyond
	// the newest maxRows are exactly those with id <= MAX(id) - maxRows.
	// One statement keeps the per-insert prune cheap; if ids ever become
	// sparse this retains fewer than maxRows rows, which is fine for a
	// bounded recent-activity cap.
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM activity WHERE id <= (SELECT MAX(id) FROM activity) - ?`, maxRows,
	); err != nil {
		return fmt.Errorf("prune activity: %w", err)
	}
	return nil
}

const (
	// maxActivityPage and maxActivityLimit bound the OFFSET arithmetic in
	// ListActivity. The page arrives straight from the query string and is only
	// lower-bounded by the parser, so (Page-1)*Limit can overflow int and wrap
	// to a negative offset — which SQLite reads as 0, silently answering with
	// page 1 instead of an empty page. Neither bound is reachable in practice:
	// at the API's own 999-row ceiling this page is past any row count the
	// store can hold.
	maxActivityPage  = 1 << 32
	maxActivityLimit = 1000
)

func normalizeActivityQuery(query ActivityQuery) ActivityQuery {
	if query.Page < 1 {
		query.Page = 1
	}
	if query.Page > maxActivityPage {
		query.Page = maxActivityPage
	}
	if query.Limit < 1 {
		query.Limit = 25
	}
	if query.Limit > maxActivityLimit {
		query.Limit = maxActivityLimit
	}
	return query
}

// activityOrderBy builds a safe ORDER BY clause from the query's sort key and
// direction. Unknown sort keys fall back to "id". A secondary "id" sort keeps
// pagination stable when the primary column has duplicate values.
func activityOrderBy(query ActivityQuery) string {
	column, ok := activitySortColumns[query.Sort]
	if !ok {
		column = "id"
	}
	direction := "DESC"
	if strings.EqualFold(query.Order, "asc") {
		direction = "ASC"
	}
	if column == "id" {
		return " ORDER BY id " + direction
	}
	return " ORDER BY " + column + " " + direction + ", id " + direction
}

func calculateTotalPages(total, limit int) int {
	if total == 0 || limit <= 0 {
		return 0
	}
	return int(math.Ceil(float64(total) / float64(limit)))
}

func calculateHistogramData(values []float64) *HistogramData {
	// Speed values originate in backend logs and may be supplied by an
	// integration that decodes non-finite JSON numbers. Sorting or converting
	// Inf/NaN into a bin index is not safe (NaN can become a negative index and
	// Inf can overflow int), so discard those samples at the store boundary.
	// Non-positive values are not useful speed samples either; they represent a
	// missing or not-yet-observed rate in the activity schema.
	finite := make([]float64, 0, len(values))
	for _, value := range values {
		if value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0) {
			finite = append(finite, value)
		}
	}
	if len(finite) == 0 {
		return nil
	}
	finite = clipHistogramOutliers(finite)

	sorted := append([]float64(nil), finite...)
	sort.Float64s(sorted)
	minVal := sorted[0]
	maxVal := sorted[len(sorted)-1]

	p50 := percentile(sorted, 50)
	p95 := percentile(sorted, 95)
	p99 := percentile(sorted, 99)

	if minVal == maxVal {
		return &HistogramData{
			Bins:    []int{len(finite)},
			Min:     minVal,
			Max:     maxVal,
			BinSize: 0,
			P50:     p50,
			P95:     p95,
			P99:     p99,
		}
	}

	const minBins = 5
	const maxBins = 20
	sturges := int(math.Ceil(math.Log2(float64(len(finite))))) + 1
	binCount := min(maxBins, max(minBins, sturges))
	binSize := (maxVal - minVal) / float64(binCount)

	bins := make([]int, binCount)
	for _, value := range finite {
		idx := min(int(math.Floor((value-minVal)/binSize)), binCount-1)
		bins[idx]++
	}

	return &HistogramData{
		Bins:    bins,
		Min:     minVal,
		Max:     maxVal,
		BinSize: binSize,
		P50:     p50,
		P95:     p95,
		P99:     p99,
	}
}

// clipHistogramOutliers keeps one broken or undersampled timing measurement
// from stretching the activity chart across the entire x-axis. The original
// samples remain intact in the activity table; only the values used to draw
// the histogram are winsorized. A Tukey-style upper fence handles both the
// common "many normal samples plus one huge value" case and distributions
// with a real high-speed tail. Flat distributions use a small relative
// cushion so a single outlier does not become the chart's scale.
func clipHistogramOutliers(values []float64) []float64 {
	const (
		minSamples       = 8
		upperFenceFactor = 3.0
		flatCushion      = 4.0
	)
	if len(values) < minSamples {
		return append([]float64(nil), values...)
	}

	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	q1 := percentile(sorted, 25)
	q3 := percentile(sorted, 75)
	upper := q3 + upperFenceFactor*(q3-q1)
	if q3 == q1 {
		upper = q3 * flatCushion
	}
	if upper <= 0 || math.IsNaN(upper) || math.IsInf(upper, 0) || upper >= sorted[len(sorted)-1] {
		return append([]float64(nil), values...)
	}

	clipped := append([]float64(nil), values...)
	for i, value := range clipped {
		if value > upper {
			clipped[i] = upper
		}
	}
	return clipped
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	rank := (p / 100) * float64(len(sorted)-1)
	lower := int(math.Floor(rank))
	upper := int(math.Ceil(rank))
	fraction := rank - float64(lower)
	return sorted[lower] + fraction*(sorted[upper]-sorted[lower])
}

// activityWhere builds a parameterized WHERE clause from a filter. It returns
// an empty string when nothing is filtered. Callers append further conditions
// with " AND ...", so the clause is always a single unparenthesized conjunction.
func activityWhere(filter ActivityFilter) (string, []any) {
	var conditions []string
	var args []any

	models := make([]string, 0, len(filter.Models))
	for _, model := range filter.Models {
		if model = strings.TrimSpace(model); model != "" {
			models = append(models, model)
		}
	}
	if len(models) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(models)), ", ")
		conditions = append(conditions, "model_id IN ("+placeholders+")")
		for _, model := range models {
			args = append(args, model)
		}
	}
	if keyID := strings.TrimSpace(filter.KeyID); keyID != "" {
		conditions = append(conditions, "key_id = ?")
		args = append(args, keyID)
	}
	if sessionID := strings.TrimSpace(filter.SessionID); sessionID != "" {
		conditions = append(conditions, "session_id = ?")
		args = append(args, sessionID)
	}

	if !filter.Start.IsZero() {
		conditions = append(conditions, "ts_created >= ?")
		args = append(args, filter.Start.Unix())
	}
	if !filter.End.IsZero() {
		conditions = append(conditions, "ts_created <= ?")
		args = append(args, filter.End.Unix())
	}
	if filter.MinID > 0 {
		conditions = append(conditions, "id >= ?")
		args = append(args, filter.MinID)
	}
	if filter.MaxID > 0 {
		conditions = append(conditions, "id <= ?")
		args = append(args, filter.MaxID)
	}

	if len(conditions) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

func marshalMetadata(metadata map[string]string) (string, error) {
	if len(metadata) == 0 {
		return "", nil
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		return "", fmt.Errorf("marshal activity metadata: %w", err)
	}
	return string(data), nil
}

type activityScanner interface {
	Scan(dest ...any) error
}

func scanActivity(scanner activityScanner) (ActivityLogEntry, error) {
	var entry ActivityLogEntry
	var ts int64
	var metadataJSON string
	var costEstimated, repairApplied, hasAudit int
	if err := scanner.Scan(
		&entry.ID,
		&ts,
		&entry.Model,
		&entry.KeyID,
		&entry.SessionID,
		&entry.ReqPath,
		&entry.RespContentType,
		&entry.RespStatusCode,
		&entry.Tokens.CachedTokens,
		&entry.Tokens.DraftTokens,
		&entry.Tokens.DraftAccTokens,
		&entry.Tokens.InputTokens,
		&entry.Tokens.OutputTokens,
		&entry.Tokens.PromptPerSecond,
		&entry.Tokens.TokensPerSecond,
		&entry.DurationMs,
		&entry.FirstTokenMs,
		&entry.ErrorMsg,
		&metadataJSON,
		&entry.CacheCreationTokens,
		&entry.ReasoningTokens,
		&entry.EstimatedCost,
		&costEstimated,
		&entry.CacheHitRatio,
		&entry.CacheCreationRatio,
		&repairApplied,
		&entry.PrefixHash,
		&entry.ClientIP,
		&entry.DecodeMs,
		&entry.SpeedTimeline,
		&hasAudit,
	); err != nil {
		return ActivityLogEntry{}, fmt.Errorf("scan activity: %w", err)
	}
	entry.Timestamp = time.Unix(ts, 0)
	entry.DecodeMs = normalizeDecodeMs(entry.DecodeMs)
	entry.CostEstimated = costEstimated != 0
	entry.CacheHitRatio = normalizeCacheRatio(entry.CacheHitRatio)
	entry.CacheCreationRatio = normalizeCacheRatio(entry.CacheCreationRatio)
	if entry.FirstTokenMs <= 0 {
		entry.FirstTokenMs = -1
	}
	entry.RepairApplied = repairApplied != 0
	entry.HasAudit = hasAudit != 0
	if metadataJSON != "" {
		if err := json.Unmarshal([]byte(metadataJSON), &entry.Metadata); err != nil {
			return ActivityLogEntry{}, fmt.Errorf("unmarshal activity metadata: %w", err)
		}
	}
	return entry, nil
}
