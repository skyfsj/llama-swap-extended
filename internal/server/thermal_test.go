package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/perf"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

func thermalTestGuard() *config.ThermalGuardConfig {
	guard, _ := (&config.ThermalGuardConfig{StopC: 85, ResumeC: 80, Consecutive: 2}).Effective()
	return guard
}

func thermalTestServer(t *testing.T) (*Server, *stubRouter) {
	t.Helper()
	local := newStubRouter([]string{"model-a", "model-b", "peer-only"}, "")
	ts := newTestServer(local, nil)
	ts.cfg = config.Config{
		Power: config.PowerConfig{ThermalGuard: thermalTestGuard()},
		Models: map[string]config.ModelConfig{
			"model-a":   {Backend: config.BackendConfig{Launch: &config.ModelLaunchConfig{GPUs: []string{"index:0"}}}},
			"model-b":   {Backend: config.BackendConfig{Launch: &config.ModelLaunchConfig{GPUs: []string{"index:1"}}}},
			"peer-only": {},
		},
	}
	return ts, local
}

func TestServer_ThermalGuardTripsAndResumes(t *testing.T) {
	ts, local := thermalTestServer(t)
	monitor := newThermalMonitor(thermalTestGuard())

	// Two consecutive hot samples on GPU 0 trip the guard for model-a only.
	monitor.applySample(ts, "index-0", 90)
	monitor.applySample(ts, "index-0", 91)

	if _, parked := ts.local.InMaintenance("model-a"); !parked {
		t.Fatalf("model-a should be parked after consecutive hot samples")
	}
	if _, parked := ts.local.InMaintenance("model-b"); parked {
		t.Fatalf("model-b is on another card and must stay unparked")
	}
	reason, _ := ts.local.InMaintenance("model-a")
	if !strings.HasPrefix(reason, thermalGuardReason) {
		t.Errorf("reason = %q, want thermal prefix", reason)
	}

	// A single hot sample does not trip.
	monitor2 := newThermalMonitor(thermalTestGuard())
	monitor2.applySample(ts, "index-1", 95)
	if _, parked := ts.local.InMaintenance("model-b"); parked {
		t.Fatalf("one hot sample must not trip the guard")
	}
	monitor2.applySample(ts, "index-1", 96)
	if _, parked := ts.local.InMaintenance("model-b"); !parked {
		t.Fatalf("two hot samples should trip the guard")
	}

	// Cooling below the resume point releases the models again.
	monitor2.applySample(ts, "index-1", 75)
	if _, parked := ts.local.InMaintenance("model-b"); parked {
		t.Fatalf("model-b should be released below resumeC")
	}

	// Samples between stopC and resumeC keep the card tripped.
	monitor3 := newThermalMonitor(thermalTestGuard())
	monitor3.applySample(ts, "index-0", 90)
	monitor3.applySample(ts, "index-0", 90)
	if _, parked := ts.local.InMaintenance("model-a"); !parked {
		t.Fatalf("expected trip")
	}
	monitor3.applySample(ts, "index-0", 82)
	if _, parked := ts.local.InMaintenance("model-a"); !parked {
		t.Fatalf("82°C is above resumeC 80, model must stay parked")
	}

	_ = local
}

func TestServer_ThermalGuardRoutingGpusCards(t *testing.T) {
	ts, _ := thermalTestServer(t)
	// routing.gpus declares model-b on card "1" (an operator-chosen key that
	// matches GPU index 1).
	ts.cfg.Routing.Router.Settings.Gpus = &config.GpusConfig{
		Cards: map[string][]string{"1": {"model-b"}},
	}
	monitor := newThermalMonitor(thermalTestGuard())
	monitor.applySample(ts, "index-1", 90)
	monitor.applySample(ts, "index-1", 90)

	if _, parked := ts.local.InMaintenance("model-b"); !parked {
		t.Fatalf("model-b declared on card 1 via routing.gpus should be parked")
	}
}

func TestServer_ForwardMaintenanceModel(t *testing.T) {
	// No gateway, no peer: thermally parked requests get the explanatory 503.
	ts, local := thermalTestServer(t)
	local.EnterMaintenance("model-a", thermalGuardReason+": GPU 0 at 92°C")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model-a"}`))
	rec := httptest.NewRecorder()
	if ts.forwardMaintenanceModel(rec, req, swaputil.ReqContextData{ModelID: "model-a"}, ts.cfg) {
		t.Fatalf("parked model without failover target should be handled (503), not dispatched locally")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "thermally parked") {
		t.Errorf("body should explain thermal parking: %s", rec.Body.String())
	}

	// Non-thermal maintenance reasons fall through to the local router.
	local.EnterMaintenance("model-b", "operator parked")
	rec2 := httptest.NewRecorder()
	if !ts.forwardMaintenanceModel(rec2, req, swaputil.ReqContextData{ModelID: "model-b"}, ts.cfg) {
		t.Fatalf("non-thermal maintenance must fall through to the local 503 path")
	}

	// failover: false also falls through.
	disabled := ts.cfg
	off := false
	disabled.Power.ThermalGuard = &config.ThermalGuardConfig{StopC: 85, Failover: &off}
	local.EnterMaintenance("model-a", thermalGuardReason+": GPU 0 at 92°C")
	rec3 := httptest.NewRecorder()
	if !ts.forwardMaintenanceModel(rec3, req, swaputil.ReqContextData{ModelID: "model-a"}, disabled) {
		t.Fatalf("failover disabled should fall through")
	}
}

func TestServer_MaintenanceParkedVisibleInModelStatus(t *testing.T) {
	ts, local := thermalTestServer(t)
	local.EnterMaintenance("model-a", thermalGuardReason+": GPU 0 at 92°C")

	models := ts.modelStatus()
	byID := map[string]apiModel{}
	for _, model := range models {
		byID[model.Id] = model
	}
	if !byID["model-a"].Maintenance {
		t.Errorf("model-a should report maintenance while thermally parked")
	}
	if byID["model-b"].Maintenance {
		t.Errorf("model-b should not report maintenance")
	}
}

func TestServer_ThermalMonitorStopsCleanly(t *testing.T) {
	ts, _ := thermalTestServer(t)
	perfMon, err := perf.New(config.PerformanceConfig{}, config.PowerConfig{}, logmon.NewWriter(io.Discard))
	if err != nil {
		t.Fatalf("perf monitor: %v", err)
	}
	ts.perf = perfMon
	monitor := newThermalMonitor(thermalTestGuard())
	done := make(chan struct{})
	go func() {
		monitor.run(ts)
		close(done)
	}()
	monitor.Stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("monitor.run did not exit after Stop")
	}
}

func TestServer_ModelsOnCardUnionsSources(t *testing.T) {
	ts, _ := thermalTestServer(t)
	ts.cfg.Routing.Router.Settings.Gpus = &config.GpusConfig{
		Cards: map[string][]string{"GPU-aaaa": {"model-b"}},
	}
	// model-a is on index 0 (no UUID in the test hardware snapshot path we
	// exercise via launch.gpus index), model-b via the gpus card declaration
	// keyed by a UUID-like string that cannot match index-0.
	got := modelsOnCard(ts, "index-0")
	if len(got) != 1 || got[0] != "model-a" {
		t.Errorf("modelsOnCard(index-0) = %v, want [model-a]", got)
	}
}
