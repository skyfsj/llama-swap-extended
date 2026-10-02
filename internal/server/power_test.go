package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/hw"
	"github.com/mostlygeek/llama-swap/internal/perf"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/store"
)

func powerTestHardware() *hw.HardwareSnapshot {
	uuid0 := "GPU-aaaa"
	uuid1 := "GPU-bbbb"
	vendor := "NVIDIA"
	return &hw.HardwareSnapshot{
		Accelerators: []hw.Accelerator{
			{Index: 0, Vendor: &vendor, UUID: &uuid0, Model: &[]string{"Test GPU 0"}[0]},
			{Index: 1, Vendor: &vendor, UUID: &uuid1},
		},
	}
}

func TestServer_ResolveModelGPUs(t *testing.T) {
	hardware := powerTestHardware()
	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			"native-uuid":  {Backend: config.BackendConfig{Launch: &config.ModelLaunchConfig{GPUs: []string{"GPU-aaaa"}}}},
			"native-index": {Backend: config.BackendConfig{Launch: &config.ModelLaunchConfig{GPUs: []string{"index:1"}}}},
			"container-device": {Backend: config.BackendConfig{
				Container: config.RuntimeContainerConfig{GPUs: "device=0,1"},
			}},
			"container-all": {Backend: config.BackendConfig{
				Container: config.RuntimeContainerConfig{GPUs: "all"},
			}},
			"no-gpus":    {},
			"stale-uuid": {Backend: config.BackendConfig{Launch: &config.ModelLaunchConfig{GPUs: []string{"GPU-dead"}}}},
		},
	}

	cases := []struct {
		modelID string
		want    []string
	}{
		{"native-uuid", []string{"GPU-aaaa"}},
		{"native-index", []string{"GPU-bbbb"}},
		{"container-device", []string{"GPU-aaaa", "GPU-bbbb"}},
		{"container-all", []string{"GPU-aaaa", "GPU-bbbb"}},
		{"no-gpus", nil},
		{"stale-uuid", nil},
		{"unknown-model", nil},
	}
	for _, tc := range cases {
		got := resolveModelGPUs(cfg, hardware, tc.modelID)
		if len(got) != len(tc.want) {
			t.Errorf("%s: got %v, want %v", tc.modelID, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: got %v, want %v", tc.modelID, got, tc.want)
				break
			}
		}
	}

	// Without a hardware snapshot the configured selections pass through.
	got := resolveModelGPUs(cfg, nil, "native-index")
	if len(got) != 1 || got[0] != "index:1" {
		t.Errorf("nil hardware: got %v, want [index:1]", got)
	}
}

func TestServer_PowerOccupancyTrackerReconcile(t *testing.T) {
	ts := newTestServer(newStubRouter([]string{"model-a"}, ""), nil)
	local := ts.local.(*stubRouter)
	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			"model-a": {Backend: config.BackendConfig{Launch: &config.ModelLaunchConfig{GPUs: []string{"GPU-aaaa"}}}},
		},
	}
	ts.cfg = cfg

	tracker := newPowerOccupancyTracker()

	// Model running: interval opens.
	local.running = map[string]process.ProcessState{"model-a": process.StateReady}
	tracker.reconcile(ts)

	open, err := ts.store.CloseModelGpuIntervals(context.Background(), "model-a", time.Now())
	if err != nil || open != 1 {
		t.Fatalf("open intervals after reconcile = %d, %v; want 1", open, err)
	}

	// Reconcile is idempotent while the model keeps running: the tracker's
	// in-memory state, not the database, decides whether to open an interval.
	tracker.reconcile(ts)
	open, err = ts.store.CloseModelGpuIntervals(context.Background(), "model-a", time.Now())
	if err != nil || open != 0 {
		t.Fatalf("second reconcile opened a duplicate interval: %d, %v", open, err)
	}

	// Model stops: interval closes.
	local.running = nil
	tracker.reconcile(ts)
	closed, err := ts.store.CloseModelGpuIntervals(context.Background(), "model-a", time.Now())
	if err != nil {
		t.Fatalf("close check: %v", err)
	}
	if closed != 0 {
		t.Errorf("interval should already be closed, got %d", closed)
	}

	// A model without resolvable GPUs is warned once and never tracked.
	ts.cfg = config.Config{Models: cfg.Models}
	local.running = map[string]process.ProcessState{"model-b": process.StateReady}
	tracker2 := newPowerOccupancyTracker()
	tracker2.reconcile(ts)
	if _, warned := tracker2.warned["model-b"]; !warned {
		t.Errorf("expected model-b to be warned about missing GPU config")
	}
}

func TestServer_AggregatePowerRows(t *testing.T) {
	base := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	from := base

	power := config.PowerConfig{ElectricityPricePerKwh: 0.6, Tariffs: []config.PowerTariff{
		{Name: "peak", PricePerKwh: 1.0, Start: "08:00", End: "22:00"},
	}}.Effective()

	gpuStats := []perf.GpuStat{
		{Timestamp: base.Add(10 * time.Second), ID: 0, Name: "GPU0", UUID: "GPU-aaaa", PowerDrawW: 100},
		{Timestamp: base.Add(20 * time.Second), ID: 0, Name: "GPU0", UUID: "GPU-aaaa", PowerDrawW: 200},
		{Timestamp: base.Add(15 * time.Second), ID: 1, Name: "GPU1", PowerDrawW: 50},
	}
	nodeStats := []perf.NodeStat{
		{Timestamp: base.Add(10 * time.Second), PowerW: 400},
	}

	rows, newest := aggregatePowerRows(gpuStats, nodeStats, from, power)
	if !newest.Equal(base.Add(20 * time.Second)) {
		t.Errorf("newest = %v, want %v", newest, base.Add(20*time.Second))
	}

	bySource := map[string]store.PowerSampleRow{}
	for _, row := range rows {
		bySource[row.SourceID] = row
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (two GPUs + node)", len(rows))
	}

	// 100 W covering [from, from+10s) plus 200 W over [from+10s, from+20s):
	// 100 W × 10 s + 200 W × 10 s = 5/6 Wh.
	gpuA := bySource["GPU-aaaa"]
	wantEnergy := (100.0*10 + 200.0*10) / 3600
	if diff := gpuA.EnergyWh - wantEnergy; diff > 0.0001 || diff < -0.0001 {
		t.Errorf("GPU-aaaa energy = %v, want ~%v", gpuA.EnergyWh, wantEnergy)
	}
	if gpuA.MinWatts != 100 || gpuA.MaxWatts != 200 || gpuA.Samples != 2 {
		t.Errorf("GPU-aaaa stats = %+v", gpuA)
	}

	// The second GPU has no UUID, so its key falls back to the index. Its
	// single sample covers [from, from+15s) at 50 W.
	gpuB := bySource["index-1"]
	wantB := 50.0 * 15 / 3600
	if diff := gpuB.EnergyWh - wantB; diff > 0.0001 || diff < -0.0001 {
		t.Errorf("index-1 energy = %v, want ~%v", gpuB.EnergyWh, wantB)
	}

	// 12:00 falls in the peak window: the row price is the tariff price.
	if gpuA.PricePerKwh != 1.0 || gpuA.TariffName != "peak" {
		t.Errorf("price resolution = %v/%q, want 1.0/peak", gpuA.PricePerKwh, gpuA.TariffName)
	}

	node := bySource["node"]
	if node.Source != "node" || node.Samples != 1 {
		t.Errorf("node row = %+v", node)
	}
}

func TestServer_HandleAPIPower(t *testing.T) {
	ts := newTestServer(newStubRouter([]string{"model-a"}, ""), nil)
	defer ts.store.Close()

	base := time.Now().Add(-time.Hour).Truncate(time.Minute)
	rows := []store.PowerSampleRow{
		{Start: base, End: base.Add(time.Minute), Source: "gpu", SourceID: "GPU-aaaa", SourceName: "Test GPU",
			AvgWatts: 200, MinWatts: 150, MaxWatts: 250, Samples: 12, EnergyWh: 200.0 / 60, PricePerKwh: 0.6},
		{Start: base, End: base.Add(time.Minute), Source: "node", SourceID: "node",
			AvgWatts: 500, MinWatts: 450, MaxWatts: 550, Samples: 2, EnergyWh: 500.0 / 60, PricePerKwh: 0.6},
	}
	if err := ts.store.InsertPowerSamples(context.Background(), rows); err != nil {
		t.Fatalf("insert samples: %v", err)
	}

	ts.cfg = config.Config{Power: config.PowerConfig{ElectricityPricePerKwh: 0.6, Currency: "CNY"}}

	req := httptest.NewRequest(http.MethodGet, "/api/metrics/power?window=6h", nil)
	rec := httptest.NewRecorder()
	ts.handleAPIPower(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Currency    string                  `json:"currency"`
		PricePerKwh float64                 `json:"price_per_kwh"`
		Sources     map[string]bool         `json:"sources"`
		Series      []store.PowerSeries     `json:"series"`
		Models      []store.PowerModelUsage `json:"models"`
		Totals      store.PowerTotals       `json:"totals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Currency != "CNY" || payload.PricePerKwh != 0.6 {
		t.Errorf("config echo = %s/%v", payload.Currency, payload.PricePerKwh)
	}
	if len(payload.Series) != 2 {
		t.Errorf("series = %d, want 2 (gpu + node)", len(payload.Series))
	}
	if payload.Totals.EnergyWh <= 0 {
		t.Errorf("window energy = %v", payload.Totals.EnergyWh)
	}
}
