package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// speedStoreReady reports whether the store can answer a speed query. The guard
// is local instead of the package-level helper because a nil receiver and a
// zero-value Store are both valid Go values to hand around while a control
// plane is assembled, and they must return a diagnostic error rather than panic.
func speedStoreReady(s *Store) bool {
	return s != nil && s.db != nil
}

// Speed point, series and context-bucket types used by the activity page's
// inference-speed views. Rates are token rates in tokens per second, latencies
// are milliseconds.
//
// A metric that no request in the bucket reported (a non-streaming response
// without first-token telemetry, a backend that does not publish prefill
// timings, or an empty bucket) is reported as a negative value instead of zero.
// Zero is a legitimate measurement for a latency, so the two states must stay
// distinguishable for charts: a gap, not a dive to the axis.

// SpeedPoint is one time bucket of one model's inference telemetry.
type SpeedPoint struct {
	Timestamp  time.Time `json:"timestamp"`
	Requests   int       `json:"requests"`
	PrefillTPS float64   `json:"prefill_tps"`
	DecodeTPS  float64   `json:"decode_tps"`
	TTFTMs     float64   `json:"ttft_ms"`
}

// SpeedSeries holds one model's time-ordered speed points.
type SpeedSeries struct {
	Model  string       `json:"model"`
	Points []SpeedPoint `json:"points"`
}

// ContextBucket aggregates the requests whose prompt fell into one
// context-length range. The range is (MinTokens, MaxTokens]; a MaxTokens of 0
// means unbounded, so a prompt of exactly 4096 tokens counts as a 4k context.
type ContextBucket struct {
	Label           string  `json:"label"`
	MinTokens       int     `json:"min_tokens"`
	MaxTokens       int     `json:"max_tokens"`
	Requests        int     `json:"requests"`
	AvgInputTokens  float64 `json:"avg_input_tokens"`
	AvgOutputTokens float64 `json:"avg_output_tokens"`
	PrefillTPS      float64 `json:"prefill_tps"`
	DecodeTPS       float64 `json:"decode_tps"`
	TTFTMs          float64 `json:"ttft_ms"`
}

// ContextSeries holds one model's context-length buckets, ordered by range.
// Only buckets with at least one request are returned.
type ContextSeries struct {
	Model   string          `json:"model"`
	Buckets []ContextBucket `json:"buckets"`
}

// SpeedReport is the payload behind the activity page's speed curves and
// context-length throughput table.
type SpeedReport struct {
	BucketSeconds int             `json:"bucket_seconds"`
	Series        []SpeedSeries   `json:"series"`
	Context       []ContextSeries `json:"context"`
}

// ActivitySpeedQuery narrows a SpeedReport to matching activity rows.
type ActivitySpeedQuery struct {
	ActivityFilter
	// BucketSeconds forces the time series bucket width. Values <= 0 select the
	// width from the requested span (see autoBucketSeconds).
	BucketSeconds int
}

const (
	// minSpeedBucketSeconds and maxSpeedBucketSeconds bound a caller-requested
	// bucket width so a one-second bucket over a month of history cannot build
	// millions of points.
	minSpeedBucketSeconds = 60
	maxSpeedBucketSeconds = 24 * 60 * 60
	// maxSpeedPoints caps the points per model series. A range longer than the
	// cap keeps only the most recent buckets, which is what a speed curve is
	// read for.
	maxSpeedPoints = 240
)

// speedBucketWidths are the candidate time bucket widths, smallest first.
var speedBucketWidths = []int{5 * 60, 15 * 60, 60 * 60, 6 * 60 * 60, 24 * 60 * 60}

// contextBucketDefs are the context-length ranges reported per model, in
// ascending order. A range covers (MinTokens, MaxTokens]; a MaxTokens of 0
// means unbounded. Labels double as the JSON contract, so they stay ASCII.
var contextBucketDefs = []struct {
	label     string
	minTokens int
	maxTokens int
}{
	{label: "<=4k", minTokens: 0, maxTokens: 4 * 1024},
	{label: "4k-8k", minTokens: 4 * 1024, maxTokens: 8 * 1024},
	{label: "8k-16k", minTokens: 8 * 1024, maxTokens: 16 * 1024},
	{label: "16k-32k", minTokens: 16 * 1024, maxTokens: 32 * 1024},
	{label: "32k-64k", minTokens: 32 * 1024, maxTokens: 64 * 1024},
	{label: "64k-128k", minTokens: 64 * 1024, maxTokens: 128 * 1024},
	{label: "128k-256k", minTokens: 128 * 1024, maxTokens: 256 * 1024},
	{label: "256k-512k", minTokens: 256 * 1024, maxTokens: 512 * 1024},
	{label: "512k-1M", minTokens: 512 * 1024, maxTokens: 1024 * 1024},
	{label: ">1M", minTokens: 1024 * 1024, maxTokens: 0},
}

// ActivitySpeedReport builds both speed views in a single pass over the
// matching activity rows: a per-model time series of prefill rate, decode rate
// and first-token latency, plus the same metrics aggregated by prompt length.
func (s *Store) ActivitySpeedReport(ctx context.Context, query ActivitySpeedQuery) (SpeedReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !speedStoreReady(s) {
		return SpeedReport{}, errors.New("store is unavailable")
	}

	where, args := activityWhere(query.ActivityFilter)
	// The speed views aggregate per-request rates into averages; a handful of
	// physically impossible samples (collapsed timing windows, buggy usage
	// frames) once skewed them into the tens of thousands of tokens/sec.
	// Server-side caps mirror the ingest-time clamps in internal/server and
	// also exclude already-persisted rows recorded before that fix. Negative
	// rates are the "not measured" sentinel and pass through.
	speedCaps := `(prompt_per_second < 0 OR prompt_per_second <= 150000)
		 AND (tokens_per_second < 0 OR tokens_per_second <= 2000)`
	if where == "" {
		where = " WHERE " + speedCaps
	} else {
		where += " AND " + speedCaps
	}
	bucketSeconds := query.BucketSeconds
	if bucketSeconds <= 0 {
		spanStart, spanEnd := query.Start, query.End
		if spanStart.IsZero() || spanEnd.IsZero() || !spanEnd.After(spanStart) {
			// A half-open or unbounded filter needs the data's own range, which
			// is an indexed MIN/MAX rather than a table scan.
			queriedStart, queriedEnd, err := s.activitySpan(ctx, where, args)
			if err != nil {
				return SpeedReport{}, fmt.Errorf("activity speed span: %w", err)
			}
			spanStart, spanEnd = queriedStart, queriedEnd
		}
		bucketSeconds = autoBucketSeconds(spanStart, spanEnd)
	}
	bucketSeconds = clampSpeedBucketSeconds(bucketSeconds)

	rows, err := s.db.QueryContext(ctx, `
		SELECT model_id, ts_created, input_tokens, output_tokens,
			prompt_per_second, tokens_per_second, first_token_ms
		FROM activity`+where+` ORDER BY ts_created`, args...)
	if err != nil {
		return SpeedReport{}, fmt.Errorf("activity speed rows: %w", err)
	}
	defer rows.Close()

	speed := newSpeedAccumulators(bucketSeconds)
	for rows.Next() {
		var (
			model           string
			ts              int64
			input, output   int
			prefill, decode float64
			firstTokenMs    int
		)
		if err := rows.Scan(&model, &ts, &input, &output, &prefill, &decode, &firstTokenMs); err != nil {
			return SpeedReport{}, fmt.Errorf("activity speed row: %w", err)
		}
		speed.add(model, ts, input, output, prefill, decode, firstTokenMs)
	}
	if err := rows.Err(); err != nil {
		return SpeedReport{}, fmt.Errorf("activity speed rows: %w", err)
	}

	return speed.report(bucketSeconds), nil
}

// activitySpan returns the oldest and newest matching timestamps. Callers that
// already bound the filter pass those bounds instead, so this only runs for
// half-open or unbounded queries.
func (s *Store) activitySpan(ctx context.Context, where string, args []any) (time.Time, time.Time, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !speedStoreReady(s) {
		return time.Time{}, time.Time{}, errors.New("store is unavailable")
	}
	var start, end sql.NullInt64
	row := s.db.QueryRowContext(ctx,
		`SELECT MIN(ts_created), MAX(ts_created) FROM activity`+where, args...)
	if err := row.Scan(&start, &end); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if !start.Valid || !end.Valid || end.Int64 < start.Int64 {
		return time.Time{}, time.Time{}, nil
	}
	return time.Unix(start.Int64, 0), time.Unix(end.Int64, 0), nil
}

// autoBucketSeconds picks the smallest width that keeps a span within
// maxSpeedPoints buckets, so a five-minute window and a year of history both
// render as a readable curve.
func autoBucketSeconds(start, end time.Time) int {
	if start.IsZero() || end.IsZero() || !end.After(start) {
		return 60 * 60
	}
	span := end.Sub(start).Seconds()
	for _, width := range speedBucketWidths {
		if span/float64(width) <= maxSpeedPoints {
			return width
		}
	}
	return maxSpeedBucketSeconds
}

func clampSpeedBucketSeconds(seconds int) int {
	if seconds < minSpeedBucketSeconds {
		return minSpeedBucketSeconds
	}
	if seconds > maxSpeedBucketSeconds {
		return maxSpeedBucketSeconds
	}
	return seconds
}

// speedAccumulator accumulates one bucket's measurements. Running sums keep a
// single pass; counts stay beside them so an average never divides by a bucket
// that only saw requests without that telemetry.
type speedAccumulator struct {
	requests       int
	prefillSum     float64
	prefillCount   int
	decodeSum      float64
	decodeCount    int
	ttftSum        float64
	ttftCount      int
	inputTokenSum  float64
	outputTokenSum float64
}

func (a *speedAccumulator) add(input, output int, prefill, decode float64, firstTokenMs int) {
	a.requests++
	if prefill > 0 {
		a.prefillSum += prefill
		a.prefillCount++
	}
	if decode > 0 {
		a.decodeSum += decode
		a.decodeCount++
	}
	if firstTokenMs > 0 {
		a.ttftSum += float64(firstTokenMs)
		a.ttftCount++
	}
	if input > 0 {
		a.inputTokenSum += float64(input)
	}
	if output > 0 {
		a.outputTokenSum += float64(output)
	}
}

// speedAverages is a resolved bucket: request count plus the mean of each
// metric, or -1 when no request reported it.
type speedAverages struct {
	requests        int
	prefillTPS      float64
	decodeTPS       float64
	ttftMs          float64
	avgInputTokens  float64
	avgOutputTokens float64
}

func (a *speedAccumulator) averages() speedAverages {
	result := speedAverages{requests: a.requests, prefillTPS: -1, decodeTPS: -1, ttftMs: -1}
	if a.prefillCount > 0 {
		result.prefillTPS = a.prefillSum / float64(a.prefillCount)
	}
	if a.decodeCount > 0 {
		result.decodeTPS = a.decodeSum / float64(a.decodeCount)
	}
	if a.ttftCount > 0 {
		result.ttftMs = a.ttftSum / float64(a.ttftCount)
	}
	if a.requests > 0 {
		result.avgInputTokens = a.inputTokenSum / float64(a.requests)
		result.avgOutputTokens = a.outputTokenSum / float64(a.requests)
	}
	return result
}

// speedAccumulators collects every model's time and context buckets in one
// pass over the activity rows. Time buckets are aligned to bucketSeconds from
// the Unix epoch so the same request always lands in the same bucket.
type speedAccumulators struct {
	byModel       map[string]*speedModelBuckets
	modelOrder    []string
	bucketSeconds int
}

type speedModelBuckets struct {
	timeBuckets    map[int64]*speedAccumulator
	contextBuckets []*speedAccumulator
	earliest       int64
	latest         int64
	requests       int
}

func newSpeedAccumulators(bucketSeconds int) *speedAccumulators {
	return &speedAccumulators{
		byModel:       make(map[string]*speedModelBuckets),
		bucketSeconds: bucketSeconds,
	}
}

func (s *speedAccumulators) buckets(model string) *speedModelBuckets {
	existing := s.byModel[model]
	if existing != nil {
		return existing
	}
	created := &speedModelBuckets{
		timeBuckets:    make(map[int64]*speedAccumulator),
		contextBuckets: make([]*speedAccumulator, len(contextBucketDefs)),
	}
	for i := range created.contextBuckets {
		created.contextBuckets[i] = &speedAccumulator{}
	}
	s.byModel[model] = created
	s.modelOrder = append(s.modelOrder, model)
	return created
}

func (s *speedAccumulators) add(model string, ts int64, input, output int, prefill, decode float64, firstTokenMs int) {
	buckets := s.buckets(model)
	buckets.requests++
	if buckets.earliest == 0 || ts < buckets.earliest {
		buckets.earliest = ts
	}
	if ts > buckets.latest {
		buckets.latest = ts
	}

	bucketKey := (ts / int64(s.bucketSeconds)) * int64(s.bucketSeconds)
	timeBucket, ok := buckets.timeBuckets[bucketKey]
	if !ok {
		timeBucket = &speedAccumulator{}
		buckets.timeBuckets[bucketKey] = timeBucket
	}
	timeBucket.add(input, output, prefill, decode, firstTokenMs)

	if index := contextBucketIndex(input); index >= 0 {
		buckets.contextBuckets[index].add(input, output, prefill, decode, firstTokenMs)
	}
}

// contextBucketIndex maps a prompt length to its context bucket, or -1 when the
// count is not usable (unknown or negative). Ranges are inclusive at the upper
// bound so 4096 tokens is a 4k context.
func contextBucketIndex(inputTokens int) int {
	if inputTokens <= 0 {
		return -1
	}
	for i, def := range contextBucketDefs {
		if def.maxTokens == 0 || inputTokens <= def.maxTokens {
			return i
		}
	}
	return -1
}

// report resolves the accumulators into ordered series. Models are ordered by
// request volume so the busiest model is what a reader sees first.
func (s *speedAccumulators) report(bucketSeconds int) SpeedReport {
	report := SpeedReport{
		BucketSeconds: bucketSeconds,
		Series:        make([]SpeedSeries, 0, len(s.modelOrder)),
		Context:       make([]ContextSeries, 0, len(s.modelOrder)),
	}
	models := append([]string(nil), s.modelOrder...)
	sort.SliceStable(models, func(i, j int) bool {
		left, right := s.byModel[models[i]], s.byModel[models[j]]
		if left.requests != right.requests {
			return left.requests > right.requests
		}
		return models[i] < models[j]
	})

	for _, model := range models {
		buckets := s.byModel[model]
		points := speedPoints(buckets, bucketSeconds)
		if len(points) > 0 {
			report.Series = append(report.Series, SpeedSeries{Model: model, Points: points})
		}
		if contextBuckets := contextSeriesFor(model, buckets); len(contextBuckets) > 0 {
			report.Context = append(report.Context, ContextSeries{Model: model, Buckets: contextBuckets})
		}
	}
	return report
}

// speedPoints walks each model's own bucket range so a model that started later
// does not chart a long flat prologue. Ranges longer than maxSpeedPoints keep
// the most recent buckets.
func speedPoints(buckets *speedModelBuckets, bucketSeconds int) []SpeedPoint {
	if buckets == nil || len(buckets.timeBuckets) == 0 || bucketSeconds <= 0 {
		return nil
	}
	first := (buckets.earliest / int64(bucketSeconds)) * int64(bucketSeconds)
	last := (buckets.latest / int64(bucketSeconds)) * int64(bucketSeconds)
	steps := int((last-first)/int64(bucketSeconds)) + 1
	if steps > maxSpeedPoints {
		first = last - int64(maxSpeedPoints-1)*int64(bucketSeconds)
		steps = maxSpeedPoints
	}

	points := make([]SpeedPoint, 0, steps)
	for bucket := first; bucket <= last; bucket += int64(bucketSeconds) {
		point := SpeedPoint{Timestamp: time.Unix(bucket, 0), PrefillTPS: -1, DecodeTPS: -1, TTFTMs: -1}
		if accumulated, ok := buckets.timeBuckets[bucket]; ok {
			averages := accumulated.averages()
			point.Requests = averages.requests
			point.PrefillTPS = averages.prefillTPS
			point.DecodeTPS = averages.decodeTPS
			point.TTFTMs = averages.ttftMs
		}
		points = append(points, point)
	}
	return points
}

func contextSeriesFor(model string, buckets *speedModelBuckets) []ContextBucket {
	if buckets == nil {
		return nil
	}
	series := make([]ContextBucket, 0, len(buckets.contextBuckets))
	for i, accumulated := range buckets.contextBuckets {
		if accumulated == nil || accumulated.requests == 0 {
			continue
		}
		def := contextBucketDefs[i]
		averages := accumulated.averages()
		series = append(series, ContextBucket{
			Label:           def.label,
			MinTokens:       def.minTokens,
			MaxTokens:       def.maxTokens,
			Requests:        averages.requests,
			AvgInputTokens:  averages.avgInputTokens,
			AvgOutputTokens: averages.avgOutputTokens,
			PrefillTPS:      averages.prefillTPS,
			DecodeTPS:       averages.decodeTPS,
			TTFTMs:          averages.ttftMs,
		})
	}
	return series
}
