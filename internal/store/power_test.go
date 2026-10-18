package store

import (
	"context"
	"testing"
	"time"
)

// powerTestRow builds a one-minute GPU sample with an average wattage.
func powerTestRow(start time.Time, sourceID string, watts float64, price float64) PowerSampleRow {
	return PowerSampleRow{
		Start:       start,
		End:         start.Add(time.Minute),
		Source:      "gpu",
		SourceID:    sourceID,
		SourceName:  "TestGPU",
		AvgWatts:    watts,
		MinWatts:    watts,
		MaxWatts:    watts,
		Samples:     12,
		EnergyWh:    watts / 60,
		PricePerKwh: price,
	}
}

func TestStore_PowerReportAttributionAndBaseline(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	// The report buckets power by local day; an hour-long fixture started just
	// before midnight would land in "yesterday" and read as zero.
	if now, base := time.Now(), time.Now().Add(-time.Hour); base.Day() != now.Day() {
		t.Skip("fixture window crosses the local day boundary")
	}

	// Baseline: two idle minutes at 100 W before the model runs.
	base := time.Now().Add(-time.Hour).Truncate(time.Minute)
	rows := []PowerSampleRow{
		powerTestRow(base, "GPU-a", 100, 0.5),
		powerTestRow(base.Add(time.Minute), "GPU-a", 100, 0.5),
	}
	// Two occupied minutes at 300 W with the valley tariff.
	rows = append(rows,
		powerTestRow(base.Add(2*time.Minute), "GPU-a", 300, 0.3),
		powerTestRow(base.Add(3*time.Minute), "GPU-a", 300, 0.3),
	)
	// Node samples alongside.
	rows = append(rows, PowerSampleRow{
		Start: base, End: base.Add(4 * time.Minute), Source: "node", SourceID: "node",
		AvgWatts: 500, MinWatts: 400, MaxWatts: 600, Samples: 48,
		EnergyWh: 500 * 4 / 60, PricePerKwh: 0.5,
	})
	if err := s.InsertPowerSamples(ctx, rows); err != nil {
		t.Fatalf("insert power samples: %v", err)
	}

	// The model occupies GPU-a from the third minute onward, still running.
	if err := s.OpenModelGpuInterval(ctx, ModelGpuIntervalRow{
		ModelID: "model-1", GpuUUID: "GPU-a", Started: base.Add(2 * time.Minute),
	}); err != nil {
		t.Fatalf("open interval: %v", err)
	}
	// A second model shares the card for the last minute.
	if err := s.OpenModelGpuInterval(ctx, ModelGpuIntervalRow{
		ModelID: "model-2", GpuUUID: "GPU-a", Started: base.Add(3 * time.Minute),
	}); err != nil {
		t.Fatalf("open interval: %v", err)
	}

	// Requests: model-1 had one 60s request, model-2 none.
	if _, err := s.InsertActivity(ctx, ActivityLogEntry{
		Timestamp: base.Add(2 * time.Minute), Model: "model-1", DurationMs: 60000,
	}); err != nil {
		t.Fatalf("insert activity: %v", err)
	}

	report, err := s.PowerReport(ctx, PowerReportQuery{
		Start: base.Add(-time.Minute), End: base.Add(5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("power report: %v", err)
	}

	// Idle baseline on GPU-a is 100 W.
	if got := report.Baselines["GPU-a"]; got != 100 {
		t.Errorf("baseline GPU-a = %v, want 100", got)
	}

	// Window GPU energy: (100+100+300+300)/60 = 13.33 Wh.
	if diff := report.Totals.EnergyWh - 800.0/60.0; diff > 0.01 || diff < -0.01 {
		t.Errorf("window energy = %v, want ~%v", report.Totals.EnergyWh, 800.0/60.0)
	}

	// Coverage: the second sample minute belongs to model-1 alone; the third
	// is shared 50/50 (both intervals are still open).
	// Inference energy: two occupied minutes at (300-100) W = 400/60 Wh.
	wantM1 := 400.0 / 60.0 * 3 / 4
	wantM2 := 400.0 / 60.0 * 1 / 4
	byModel := map[string]PowerModelUsage{}
	for _, model := range report.Models {
		byModel[model.ModelID] = model
	}
	if diff := byModel["model-1"].EnergyWh - wantM1; diff > 0.01 || diff < -0.01 {
		t.Errorf("model-1 net energy = %v, want ~%v (%+v)", byModel["model-1"].EnergyWh, wantM1, byModel["model-1"])
	}
	if diff := byModel["model-2"].EnergyWh - wantM2; diff > 0.01 || diff < -0.01 {
		t.Errorf("model-2 net energy = %v, want ~%v (%+v)", byModel["model-2"].EnergyWh, wantM2, byModel["model-2"])
	}
	if byModel["model-1"].Requests != 1 {
		t.Errorf("model-1 requests = %d, want 1", byModel["model-1"].Requests)
	}
	// Gross: occupied samples (5 Wh each) split by coverage.
	if diff := byModel["model-1"].GrossEnergyWh - 7.5; diff > 0.01 || diff < -0.01 {
		t.Errorf("model-1 gross = %v, want ~7.5", byModel["model-1"].GrossEnergyWh)
	}
	if diff := byModel["model-2"].GrossEnergyWh - 2.5; diff > 0.01 || diff < -0.01 {
		t.Errorf("model-2 gross = %v, want ~2.5", byModel["model-2"].GrossEnergyWh)
	}

	// Cost uses the net energy and the sample price (0.3).
	if diff := byModel["model-1"].Cost - wantM1/1000*0.3; diff > 0.0005 || diff < -0.0005 {
		t.Errorf("model-1 cost = %v, want ~%v", byModel["model-1"].Cost, wantM1/1000*0.3)
	}

	// Idle = total − inference.
	if diff := report.Totals.IdleEnergyWh - (800.0/60.0 - 400.0/60.0); diff > 0.02 || diff < -0.02 {
		t.Errorf("idle energy = %v, want ~%v", report.Totals.IdleEnergyWh, 400.0/60.0)
	}

	// Window cost: idle samples at 0.5 and occupied samples at 0.3. Each row
	// spans one minute, so idle energy is 200/60 Wh and occupied 600/60 Wh.
	wantCost := ((200.0/60.0)*0.5 + (600.0/60.0)*0.3) / 1000
	if diff := report.Totals.Cost - wantCost; diff > 0.0005 || diff < -0.0005 {
		t.Errorf("window cost = %v, want ~%v", report.Totals.Cost, wantCost)
	}
	// Calendar totals use the same unit conversion.
	if diff := report.Totals.TodayCost - wantCost; diff > 0.001 || diff < -0.001 {
		t.Errorf("today cost = %v, want ~%v", report.Totals.TodayCost, wantCost)
	}

	// Node series present with its own total.
	if report.Totals.NodeEnergyWh <= 0 {
		t.Errorf("node energy = %v, want > 0", report.Totals.NodeEnergyWh)
	}

	// Series: one GPU series and one node series, with plausible buckets.
	var gpuSeries, nodeSeries *PowerSeries
	for i := range report.Series {
		if report.Series[i].IsNode {
			nodeSeries = &report.Series[i]
		} else {
			gpuSeries = &report.Series[i]
		}
	}
	if gpuSeries == nil || len(gpuSeries.Points) == 0 {
		t.Fatalf("missing GPU series: %+v", report.Series)
	}
	if nodeSeries == nil || len(nodeSeries.Points) == 0 {
		t.Fatalf("missing node series: %+v", report.Series)
	}
	if gpuSeries.Points[0].AvgWatts != 100 {
		t.Errorf("first bucket watts = %v, want 100", gpuSeries.Points[0].AvgWatts)
	}

	// Tariff breakdown: occupied minutes carry the "" (base) price here
	// because price rows were written without a tariff name.
	if len(report.Totals.ByTariff) == 0 {
		t.Errorf("expected tariff usage rows")
	}

	// Model filter restricts attribution.
	filtered, err := s.PowerReport(ctx, PowerReportQuery{
		Start: base.Add(-time.Minute), End: base.Add(5 * time.Minute), ModelID: "model-2",
	})
	if err != nil {
		t.Fatalf("filtered power report: %v", err)
	}
	if len(filtered.Models) != 1 || filtered.Models[0].ModelID != "model-2" {
		t.Errorf("filtered models = %+v, want only model-2", filtered.Models)
	}
}

func TestStore_PowerCloseAndPrune(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	base := time.Now().Add(-time.Hour).Truncate(time.Minute)
	if err := s.OpenModelGpuInterval(ctx, ModelGpuIntervalRow{ModelID: "m", GpuUUID: "GPU-a", Started: base}); err != nil {
		t.Fatal(err)
	}
	if err := s.OpenModelGpuInterval(ctx, ModelGpuIntervalRow{ModelID: "m", GpuUUID: "GPU-b", Started: base}); err != nil {
		t.Fatal(err)
	}

	closed, err := s.CloseModelGpuIntervals(ctx, "m", base.Add(10*time.Minute))
	if err != nil || closed != 2 {
		t.Fatalf("closed = %d, %v; want 2", closed, err)
	}
	closed, err = s.CloseModelGpuIntervals(ctx, "m", base.Add(10*time.Minute))
	if err != nil || closed != 0 {
		t.Fatalf("second close = %d, %v; want 0", closed, err)
	}

	// Prune removes old closed intervals and samples.
	if err := s.InsertPowerSamples(ctx, []PowerSampleRow{powerTestRow(base, "GPU-a", 100, 0.5)}); err != nil {
		t.Fatal(err)
	}
	if err := s.PrunePowerData(ctx, base.Add(2*time.Minute)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	report, err := s.PowerReport(ctx, PowerReportQuery{Start: base.Add(-time.Minute), End: base.Add(time.Minute)})
	if err != nil {
		t.Fatalf("report after prune: %v", err)
	}
	if report.Totals.EnergyWh != 0 {
		t.Errorf("energy after prune = %v, want 0", report.Totals.EnergyWh)
	}
}

func TestStore_PowerUnavailableWithoutData(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	report, err := s.PowerReport(ctx, PowerReportQuery{})
	if err != nil {
		t.Fatalf("empty report: %v", err)
	}
	if report.Totals.NodeEnergyWh != -1 {
		t.Errorf("node energy = %v, want -1 sentinel", report.Totals.NodeEnergyWh)
	}
	if len(report.Series) != 0 || len(report.Models) != 0 {
		t.Errorf("empty store should produce empty series/models: %+v", report)
	}
}

func TestStore_PowerReportModelAllowlist(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	base := time.Now().Add(-10 * time.Minute).Truncate(time.Minute)
	if err := s.InsertPowerSamples(ctx, []PowerSampleRow{
		powerTestRow(base, "GPU-a", 100, 0.5),
		powerTestRow(base.Add(time.Minute), "GPU-a", 300, 0.5),
	}); err != nil {
		t.Fatalf("insert power samples: %v", err)
	}
	if err := s.OpenModelGpuInterval(ctx, ModelGpuIntervalRow{
		ModelID: "local-model", GpuUUID: "GPU-a", Started: base.Add(time.Minute),
	}); err != nil {
		t.Fatalf("open interval: %v", err)
	}
	// One request each for a local model, a provider-routed model and a name
	// that is not configured at all: only the local one may produce a row.
	for _, model := range []string{"local-model", "glm-5.1", "does-not-exist"} {
		if _, err := s.InsertActivity(ctx, ActivityLogEntry{
			Timestamp: base.Add(time.Minute), Model: model, DurationMs: 30000,
		}); err != nil {
			t.Fatalf("insert activity %s: %v", model, err)
		}
	}

	report, err := s.PowerReport(ctx, PowerReportQuery{
		Start: base.Add(-time.Minute), End: base.Add(5 * time.Minute),
		// What the activity page passes: the locally served models.
		Models: []string{"local-model"},
	})
	if err != nil {
		t.Fatalf("power report: %v", err)
	}
	if len(report.Models) != 1 || report.Models[0].ModelID != "local-model" {
		t.Fatalf("models = %+v, want only the local model", report.Models)
	}
	if report.Models[0].Requests != 1 {
		t.Fatalf("local model requests = %d, want 1", report.Models[0].Requests)
	}

	// An empty allowlist is "no local models", never "no filter": a deployment
	// without local models must not list every requested name.
	empty, err := s.PowerReport(ctx, PowerReportQuery{
		Start: base.Add(-time.Minute), End: base.Add(5 * time.Minute),
		Models: []string{},
	})
	if err != nil {
		t.Fatalf("power report (empty allowlist): %v", err)
	}
	for _, model := range empty.Models {
		if model.ModelID == "glm-5.1" || model.ModelID == "does-not-exist" {
			t.Fatalf("empty allowlist still reported %q", model.ModelID)
		}
	}

	// nil leaves the report unfiltered for callers that want the raw view.
	unfiltered, err := s.PowerReport(ctx, PowerReportQuery{
		Start: base.Add(-time.Minute), End: base.Add(5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("power report (unfiltered): %v", err)
	}
	if len(unfiltered.Models) != 3 {
		t.Fatalf("unfiltered models = %+v, want 3 rows", unfiltered.Models)
	}
}
