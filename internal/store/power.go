package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// This file persists and aggregates the power statistics shown on the
// activity page's energy panel:
//
//   - power_samples holds one aggregated power reading per (source, flush
//     interval): GPU cards and, when IPMI is available, the whole node. Each
//     row records the electricity price in effect when it was written, so
//     historical costs stay stable when the tariff changes later.
//   - model_gpu_intervals holds model occupancy per GPU card, written by the
//     occupancy tracker when a model starts and closed when it stops. Energy
//     is attributed to models by joining these intervals with samples.
//
// Aggregation happens in Go over sequential query passes (samples, intervals,
// request activity), mirroring activity_speed.go: the row counts involved
// (minutes × cards) are small and the attribution math does not map cleanly
// onto SQL. Each query is fully drained before the next one starts because
// the store's single connection cannot interleave them.

// PowerSampleRow is one aggregated power reading interval ready to persist.
type PowerSampleRow struct {
	Start       time.Time
	End         time.Time
	Source      string // "gpu" or "node"
	SourceID    string // GPU UUID, or "node"
	SourceName  string // GPU marketing name, empty for the node
	AvgWatts    float64
	MinWatts    float64 // -1 when unavailable
	MaxWatts    float64 // -1 when unavailable
	Samples     int
	EnergyWh    float64
	PricePerKwh float64
	TariffName  string // matched time-of-use window name, empty for the base price
}

// ModelGpuIntervalRow is one model occupancy interval on one GPU. Ended zero
// means the model is still running.
type ModelGpuIntervalRow struct {
	ModelID string
	GpuUUID string
	Started time.Time
	Ended   time.Time
}

// InsertPowerSamples persists a batch of aggregated power rows.
func (s *Store) InsertPowerSamples(ctx context.Context, rows []PowerSampleRow) error {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("power samples begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO power_samples (
			ts_start, ts_end, source, source_id, source_name,
			avg_watts, min_watts, max_watts, samples, energy_wh, price_per_kwh, tariff_name
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (source, source_id, ts_start) DO UPDATE SET
			ts_end = excluded.ts_end,
			source_name = excluded.source_name,
			avg_watts = excluded.avg_watts,
			min_watts = excluded.min_watts,
			max_watts = excluded.max_watts,
			samples = excluded.samples,
			energy_wh = excluded.energy_wh,
			price_per_kwh = excluded.price_per_kwh,
			tariff_name = excluded.tariff_name`)
	if err != nil {
		return fmt.Errorf("power samples prepare: %w", err)
	}
	defer stmt.Close()

	for _, row := range rows {
		if _, err := stmt.ExecContext(ctx,
			row.Start.Unix(),
			row.End.Unix(),
			row.Source,
			row.SourceID,
			row.SourceName,
			row.AvgWatts,
			row.MinWatts,
			row.MaxWatts,
			row.Samples,
			row.EnergyWh,
			row.PricePerKwh,
			row.TariffName,
		); err != nil {
			return fmt.Errorf("power samples insert: %w", err)
		}
	}
	return tx.Commit()
}

// OpenModelGpuInterval records that a model started occupying a GPU. A model
// that is already open on the same card (a crashed predecessor's row, or a
// race) is closed first so the open-interval unique index stays satisfied and
// coverage is never counted twice.
func (s *Store) OpenModelGpuInterval(ctx context.Context, row ModelGpuIntervalRow) error {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return err
	}
	if row.Started.IsZero() {
		row.Started = time.Now()
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE model_gpu_intervals SET ended_ts = ? WHERE model_id = ? AND gpu_uuid = ? AND ended_ts = 0`,
		row.Started.Unix(), row.ModelID, row.GpuUUID); err != nil {
		return fmt.Errorf("model gpu interval supersede: %w", err)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO model_gpu_intervals (model_id, gpu_uuid, started_ts, ended_ts)
		VALUES (?, ?, ?, 0)`,
		row.ModelID, row.GpuUUID, row.Started.Unix())
	if err != nil {
		return fmt.Errorf("model gpu interval insert: %w", err)
	}
	return nil
}

// CloseStaleModelGpuIntervals closes every interval still open at the given
// time, regardless of model. The flusher runs it once at startup so a crashed
// process's open rows cannot stretch their attribution coverage forever.
func (s *Store) CloseStaleModelGpuIntervals(ctx context.Context, ended time.Time) (int64, error) {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return 0, err
	}
	if ended.IsZero() {
		ended = time.Now()
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE model_gpu_intervals SET ended_ts = ? WHERE ended_ts = 0`, ended.Unix())
	if err != nil {
		return 0, fmt.Errorf("model gpu interval stale sweep: %w", err)
	}
	count, _ := res.RowsAffected()
	return count, nil
}

// CloseModelGpuIntervals stamps an end time onto every still-open occupancy
// interval of one model, or of every model when modelID is empty. It returns
// the number of intervals closed.
func (s *Store) CloseModelGpuIntervals(ctx context.Context, modelID string, ended time.Time) (int64, error) {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return 0, err
	}
	if ended.IsZero() {
		ended = time.Now()
	}
	query := `UPDATE model_gpu_intervals SET ended_ts = ? WHERE ended_ts = 0`
	args := []any{ended.Unix()}
	if modelID != "" {
		query += ` AND model_id = ?`
		args = append(args, modelID)
	}
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("model gpu interval close: %w", err)
	}
	count, _ := res.RowsAffected()
	return count, nil
}

// PrunePowerData deletes power samples and closed occupancy intervals older
// than the retention horizon.
func (s *Store) PrunePowerData(ctx context.Context, before time.Time) error {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return err
	}
	for _, query := range []string{
		`DELETE FROM power_samples WHERE ts_end < ?`,
		`DELETE FROM model_gpu_intervals WHERE ended_ts != 0 AND ended_ts < ?`,
	} {
		if _, err := s.db.ExecContext(ctx, query, before.Unix()); err != nil {
			return fmt.Errorf("power data prune: %w", err)
		}
	}
	return nil
}

// PowerSeriesPoint is one time bucket of one power source's readings.
type PowerSeriesPoint struct {
	Timestamp time.Time `json:"timestamp"`
	AvgWatts  float64   `json:"avg_watts"`
	MinWatts  float64   `json:"min_watts"`
	MaxWatts  float64   `json:"max_watts"`
}

// PowerSeries holds one power source's time-ordered points. MinWatts and
// MaxWatts stay -1 when no sample in the bucket reported a range.
type PowerSeries struct {
	SourceID   string             `json:"source_id"`
	SourceName string             `json:"source_name"`
	IsNode     bool               `json:"is_node"`
	Points     []PowerSeriesPoint `json:"points"`
}

// PowerModelUsage is one model's attributed energy consumption over the
// report window.
type PowerModelUsage struct {
	ModelID       string  `json:"model_id"`
	EnergyWh      float64 `json:"energy_wh"`       // net of the idle baseline
	GrossEnergyWh float64 `json:"gross_energy_wh"` // full energy while occupying the card
	GpuSeconds    float64 `json:"gpu_seconds"`
	Requests      int     `json:"requests"`
	Cost          float64 `json:"cost"`
}

// PowerTariffUsage aggregates window energy by the tariff in effect when the
// samples were written. The base price (no tariff window) uses an empty name.
type PowerTariffUsage struct {
	Name     string  `json:"name"`
	EnergyWh float64 `json:"energy_wh"`
	Cost     float64 `json:"cost"`
}

// PowerTotals summarizes the window and the calendar periods the panel shows.
type PowerTotals struct {
	// Window totals.
	EnergyWh          float64 `json:"energy_wh"`           // all GPU energy in the window
	InferenceEnergyWh float64 `json:"inference_energy_wh"` // attributed to models, net of baseline
	IdleEnergyWh      float64 `json:"idle_energy_wh"`      // window total minus attributed
	Cost              float64 `json:"cost"`
	NodeEnergyWh      float64 `json:"node_energy_wh"` // -1 when IPMI has no samples
	NodeCost          float64 `json:"node_cost"`

	// Calendar aggregates over GPU sources, independent of the window.
	TodayEnergyWh float64            `json:"today_energy_wh"`
	TodayCost     float64            `json:"today_cost"`
	MonthEnergyWh float64            `json:"month_energy_wh"`
	MonthCost     float64            `json:"month_cost"`
	ByTariff      []PowerTariffUsage `json:"by_tariff"`
}

// PowerReport is the payload behind the activity page's energy panel.
type PowerReport struct {
	Start         time.Time         `json:"start"`
	End           time.Time         `json:"end"`
	BucketSeconds int               `json:"bucket_seconds"`
	Series        []PowerSeries     `json:"series"`
	Models        []PowerModelUsage `json:"models"`
	Totals        PowerTotals       `json:"totals"`
	// Baselines maps GPU source_id to the idle-baseline watts used for net
	// attribution. A GPU missing from the map had no idle samples in the
	// window, so only gross energy is reported for its models.
	Baselines map[string]float64 `json:"baselines"`
}

// PowerReportQuery narrows a PowerReport.
type PowerReportQuery struct {
	Start time.Time
	End   time.Time
	// ModelID restricts model attribution and filters the models list; the
	// power series always covers all sources.
	ModelID string
	// BucketSeconds forces the time series bucket width. Values <= 0 select
	// the width from the requested span.
	BucketSeconds int
}

const (
	// minPowerBucketSeconds and maxPowerBucketSeconds bound the auto bucket
	// width so a one-second bucket over a month of history cannot build
	// millions of points.
	minPowerBucketSeconds = 60
	maxPowerBucketSeconds = 24 * 60 * 60
	maxPowerPoints        = 240
)

func autoPowerBucketSeconds(span time.Duration) int {
	seconds := int(span.Seconds()) / maxPowerPoints
	if seconds < minPowerBucketSeconds {
		seconds = minPowerBucketSeconds
	}
	if seconds > maxPowerBucketSeconds {
		seconds = maxPowerBucketSeconds
	}
	return seconds
}

// gpuInterval is the in-memory form of one occupancy interval overlapping the
// report window. An interval with ended.IsZero() is still open and extends to
// the window end.
type gpuInterval struct {
	modelID string
	gpuUUID string
	start   time.Time
	end     time.Time
}

// PowerReport builds the energy panel payload: per-source power series, the
// model attribution table and window/calendar totals.
func (s *Store) PowerReport(ctx context.Context, query PowerReportQuery) (PowerReport, error) {
	ctx = normalizeContext(ctx)
	if !speedStoreReady(s) {
		return PowerReport{}, errors.New("store is unavailable")
	}
	if query.End.IsZero() {
		query.End = time.Now()
	}
	if query.Start.IsZero() || !query.Start.Before(query.End) {
		query.Start = query.End.Add(-24 * time.Hour)
	}
	bucketSeconds := query.BucketSeconds
	if bucketSeconds <= 0 {
		bucketSeconds = autoPowerBucketSeconds(query.End.Sub(query.Start))
	}

	report := PowerReport{
		Start:         query.Start,
		End:           query.End,
		BucketSeconds: bucketSeconds,
		Series:        []PowerSeries{},
		Models:        []PowerModelUsage{},
		Baselines:     map[string]float64{},
	}
	report.Totals.ByTariff = []PowerTariffUsage{}

	// --- Load occupancy intervals overlapping the window. ---
	intervalQuery := `
		SELECT model_id, gpu_uuid, started_ts, ended_ts
		FROM model_gpu_intervals
		WHERE started_ts < ? AND (ended_ts = 0 OR ended_ts > ?)`
	intervalArgs := []any{query.End.Unix(), query.Start.Unix()}
	if query.ModelID != "" {
		intervalQuery += ` AND model_id = ?`
		intervalArgs = append(intervalArgs, query.ModelID)
	}
	rows, err := s.db.QueryContext(ctx, intervalQuery, intervalArgs...)
	if err != nil {
		return PowerReport{}, fmt.Errorf("model gpu intervals query: %w", err)
	}
	intervals := []gpuInterval{}
	for rows.Next() {
		var interval gpuInterval
		var startedUnix, endedUnix int64
		if err := rows.Scan(&interval.modelID, &interval.gpuUUID, &startedUnix, &endedUnix); err != nil {
			rows.Close()
			return PowerReport{}, fmt.Errorf("model gpu intervals scan: %w", err)
		}
		interval.start = time.Unix(startedUnix, 0)
		if endedUnix > 0 {
			interval.end = time.Unix(endedUnix, 0)
		} else {
			interval.end = query.End
		}
		intervals = append(intervals, interval)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return PowerReport{}, fmt.Errorf("model gpu intervals rows: %w", err)
	}

	// --- Scan samples: series buckets, tariff totals, idle watts, and the
	// --- per-sample coverage needed by the attribution pass. ---
	type bucketAccumulator struct {
		energyWh float64
		seconds  float64
		minWatts float64
		maxWatts float64
		hasRange bool
	}
	type seriesAccumulator struct {
		sourceName string
		isNode     bool
		buckets    map[int]*bucketAccumulator
	}
	series := map[string]*seriesAccumulator{}

	type modelShare struct {
		modelID string
		seconds float64
	}
	type gpuSample struct {
		sourceID    string
		energyWh    float64
		seconds     float64
		avgWatts    float64
		pricePerKwh float64
		shares      []modelShare
		coverage    float64
	}
	gpuSamples := []gpuSample{}
	idleWatts := map[string][]float64{}

	tariffEnergy := map[string]float64{}
	tariffCost := map[string]float64{}

	var windowEnergyWh, windowCost, nodeEnergyWh, nodeCost float64
	nodeSeen := false

	rows, err = s.db.QueryContext(ctx, `
		SELECT ts_start, ts_end, source, source_id, source_name,
		       avg_watts, min_watts, max_watts, energy_wh, price_per_kwh, tariff_name
		FROM power_samples
		WHERE ts_end > ? AND ts_start <= ?
		ORDER BY source, source_id, ts_start`,
		query.Start.Unix(), query.End.Unix())
	if err != nil {
		return PowerReport{}, fmt.Errorf("power samples query: %w", err)
	}
	for rows.Next() {
		var source, sourceID, sourceName, tariffName string
		var startUnix, endUnix int64
		var avgWatts, minWatts, maxWatts, energyWh, pricePerKwh float64
		if err := rows.Scan(&startUnix, &endUnix, &source, &sourceID, &sourceName,
			&avgWatts, &minWatts, &maxWatts, &energyWh, &pricePerKwh, &tariffName); err != nil {
			rows.Close()
			return PowerReport{}, fmt.Errorf("power samples scan: %w", err)
		}
		start := time.Unix(startUnix, 0)
		end := time.Unix(endUnix, 0)

		rowSeconds := end.Sub(start).Seconds()
		if rowSeconds <= 0 {
			continue
		}
		// Clip the row to the window by measuring the actual overlap span.
		// (Multiplying the two edge fractions would be wrong when a short
		// window cuts both sides of one row.)
		overlapStart, overlapEnd := start, end
		if overlapStart.Before(query.Start) {
			overlapStart = query.Start
		}
		if overlapEnd.After(query.End) {
			overlapEnd = query.End
		}
		clipped := clampFraction(overlapEnd.Sub(overlapStart).Seconds(), rowSeconds)
		if clipped <= 0 {
			continue
		}

		energy := energyWh * clipped

		acc := series[sourceID]
		if acc == nil {
			acc = &seriesAccumulator{sourceName: sourceName, isNode: source == "node", buckets: map[int]*bucketAccumulator{}}
			series[sourceID] = acc
		}
		// Rows may start before the window (partial overlap); clamp them into
		// the first bucket instead of letting integer truncation produce a
		// negative index, and skip rows that start at or after the window end.
		if !start.Before(query.End) {
			continue
		}
		index := int(start.Sub(query.Start).Seconds()) / bucketSeconds
		if index < 0 {
			index = 0
		}
		bucket := acc.buckets[index]
		if bucket == nil {
			bucket = &bucketAccumulator{}
			acc.buckets[index] = bucket
		}
		bucket.energyWh += energy
		bucket.seconds += rowSeconds * clipped
		if minWatts >= 0 && (!bucket.hasRange || minWatts < bucket.minWatts) {
			bucket.minWatts = minWatts
		}
		if maxWatts >= 0 && (!bucket.hasRange || maxWatts > bucket.maxWatts) {
			bucket.maxWatts = maxWatts
		}
		bucket.hasRange = bucket.hasRange || minWatts >= 0 || maxWatts >= 0

		tariffEnergy[tariffName] += energy
		tariffCost[tariffName] += energy / 1000 * pricePerKwh

		if source == "node" {
			nodeSeen = true
			nodeEnergyWh += energy
			nodeCost += energy / 1000 * pricePerKwh
			continue
		}

		windowEnergyWh += energy
		windowCost += energy / 1000 * pricePerKwh

		shares := []modelShare{}
		totalCoverage := 0.0
		for _, interval := range intervals {
			if interval.gpuUUID != sourceID {
				continue
			}
			overlapStart := maxTime(maxTime(interval.start, start), query.Start)
			overlapEnd := minTime(minTime(interval.end, end), query.End)
			overlap := overlapEnd.Sub(overlapStart).Seconds()
			if overlap <= 0 {
				continue
			}
			shares = append(shares, modelShare{interval.modelID, overlap})
			totalCoverage += overlap
		}

		if totalCoverage > 0 {
			gpuSamples = append(gpuSamples, gpuSample{
				sourceID:    sourceID,
				energyWh:    energy,
				seconds:     rowSeconds * clipped,
				avgWatts:    avgWatts,
				pricePerKwh: pricePerKwh,
				shares:      shares,
				coverage:    totalCoverage,
			})
		} else {
			idleWatts[sourceID] = append(idleWatts[sourceID], avgWatts)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return PowerReport{}, fmt.Errorf("power samples rows: %w", err)
	}

	// --- Baselines: median idle watts per GPU. ---
	for sourceID, watts := range idleWatts {
		if baseline := medianFloat(watts); baseline >= 0 {
			report.Baselines[sourceID] = baseline
		}
	}

	// --- Attribution: excess energy above the baseline goes to the models
	// --- occupying the card, shared by coverage seconds.
	modelNet := map[string]float64{}
	modelGross := map[string]float64{}
	modelCost := map[string]float64{}
	modelSeconds := map[string]float64{}

	for _, sample := range gpuSamples {
		baseline, hasBaseline := report.Baselines[sample.sourceID]
		for _, share := range sample.shares {
			fraction := share.seconds / sample.coverage
			modelGross[share.modelID] += sample.energyWh * fraction
			modelSeconds[share.modelID] += share.seconds
			if !hasBaseline {
				continue
			}
			excessWatts := sample.avgWatts - baseline
			if excessWatts <= 0 {
				continue
			}
			net := excessWatts * sample.seconds / 3600 * fraction
			modelNet[share.modelID] += net
			modelCost[share.modelID] += net / 1000 * sample.pricePerKwh
		}
	}

	// --- Request counts per model inside the window. ---
	modelRequests := map[string]int{}
	activityQuery := `
		SELECT model_id, COUNT(*)
		FROM activity
		WHERE ts_created >= ? AND ts_created <= ?`
	activityArgs := []any{query.Start.Unix(), query.End.Unix()}
	if query.ModelID != "" {
		activityQuery += ` AND model_id = ?`
		activityArgs = append(activityArgs, query.ModelID)
	}
	activityQuery += ` GROUP BY model_id`
	rows, err = s.db.QueryContext(ctx, activityQuery, activityArgs...)
	if err != nil {
		return PowerReport{}, fmt.Errorf("activity query: %w", err)
	}
	for rows.Next() {
		var modelID string
		var count int
		if err := rows.Scan(&modelID, &count); err != nil {
			rows.Close()
			return PowerReport{}, fmt.Errorf("activity scan: %w", err)
		}
		modelRequests[modelID] = count
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return PowerReport{}, fmt.Errorf("activity rows: %w", err)
	}

	// --- Totals. ---
	report.Totals.EnergyWh = windowEnergyWh
	report.Totals.Cost = windowCost
	if nodeSeen {
		report.Totals.NodeEnergyWh = nodeEnergyWh
		report.Totals.NodeCost = nodeCost
	} else {
		report.Totals.NodeEnergyWh = -1
	}

	modelIDs := make([]string, 0, len(modelGross))
	for modelID := range modelGross {
		modelIDs = append(modelIDs, modelID)
	}
	for modelID := range modelRequests {
		if _, ok := modelGross[modelID]; !ok {
			modelIDs = append(modelIDs, modelID)
		}
	}
	sort.Strings(modelIDs)
	for _, modelID := range modelIDs {
		report.Models = append(report.Models, PowerModelUsage{
			ModelID:       modelID,
			EnergyWh:      modelNet[modelID],
			GrossEnergyWh: modelGross[modelID],
			GpuSeconds:    modelSeconds[modelID],
			Requests:      modelRequests[modelID],
			Cost:          modelCost[modelID],
		})
		report.Totals.InferenceEnergyWh += modelNet[modelID]
	}
	idle := windowEnergyWh - report.Totals.InferenceEnergyWh
	if idle < 0 {
		idle = 0
	}
	report.Totals.IdleEnergyWh = idle

	// --- Series output, ordered by source then time. ---
	sourceIDs := make([]string, 0, len(series))
	for sourceID := range series {
		sourceIDs = append(sourceIDs, sourceID)
	}
	sort.Strings(sourceIDs)
	for _, sourceID := range sourceIDs {
		acc := series[sourceID]
		indexes := make([]int, 0, len(acc.buckets))
		for index := range acc.buckets {
			indexes = append(indexes, index)
		}
		sort.Ints(indexes)
		points := make([]PowerSeriesPoint, 0, len(indexes))
		for _, index := range indexes {
			bucket := acc.buckets[index]
			point := PowerSeriesPoint{
				Timestamp: query.Start.Add(time.Duration(index) * time.Duration(bucketSeconds) * time.Second),
				MinWatts:  -1,
				MaxWatts:  -1,
			}
			if bucket.seconds > 0 {
				point.AvgWatts = bucket.energyWh / bucket.seconds * 3600
			}
			if bucket.hasRange {
				point.MinWatts = bucket.minWatts
				point.MaxWatts = bucket.maxWatts
			}
			points = append(points, point)
		}
		report.Series = append(report.Series, PowerSeries{
			SourceID:   sourceID,
			SourceName: acc.sourceName,
			IsNode:     acc.isNode,
			Points:     points,
		})
	}

	// --- Tariff breakdown over the window. ---
	tariffNames := make([]string, 0, len(tariffEnergy))
	for name := range tariffEnergy {
		tariffNames = append(tariffNames, name)
	}
	sort.Strings(tariffNames)
	for _, name := range tariffNames {
		report.Totals.ByTariff = append(report.Totals.ByTariff, PowerTariffUsage{
			Name:     name,
			EnergyWh: tariffEnergy[name],
			Cost:     tariffCost[name],
		})
	}

	// --- Calendar totals (today and this month, local time). ---
	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	todayEnergy, todayCost, err := s.powerEnergySince(ctx, todayStart)
	if err != nil {
		return PowerReport{}, err
	}
	monthEnergy, monthCost, err := s.powerEnergySince(ctx, monthStart)
	if err != nil {
		return PowerReport{}, err
	}
	report.Totals.TodayEnergyWh = todayEnergy
	report.Totals.TodayCost = todayCost
	report.Totals.MonthEnergyWh = monthEnergy
	report.Totals.MonthCost = monthCost

	return report, nil
}

// powerEnergySince sums GPU-source energy and cost from the given local time.
func (s *Store) powerEnergySince(ctx context.Context, since time.Time) (float64, float64, error) {
	var energy, cost float64
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(energy_wh), 0), COALESCE(SUM(energy_wh * price_per_kwh) / 1000, 0)
		FROM power_samples
		WHERE source = 'gpu' AND ts_end > ?`,
		since.Unix()).Scan(&energy, &cost)
	if err != nil {
		return 0, 0, fmt.Errorf("power calendar totals: %w", err)
	}
	return energy, cost, nil
}

// clampFraction scales one row's duration proportionally when the row sticks
// out of the report window.
func clampFraction(overlapSeconds, rowSeconds float64) float64 {
	if rowSeconds <= 0 {
		return 0
	}
	fraction := overlapSeconds / rowSeconds
	if fraction < 0 {
		return 0
	}
	if fraction > 1 {
		return 1
	}
	return fraction
}

// medianFloat returns the median of values, or -1 when empty.
func medianFloat(values []float64) float64 {
	if len(values) == 0 {
		return -1
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
