package server

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/perf"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/router"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// This file runs the thermal guard end to end against a real model process:
// a prebuilt simple-responder is supervised through the normal router, GPU
// temperature samples cross the actual thermalMonitor.applySamples path, the
// guard stops the process and parks the model, dispatch refuses inference
// with the thermal 503 (and would forward to a gateway/peer), and cooling
// below the resume point lets an operator start serve again.
//
// The temperature itself is injected through applySamples because no test
// host can heat a GPU on demand; everything downstream of the sensor reading
// — trip counting, maintenance parking, process unload, dispatch failover,
// resume — runs the production code.

// skipIfNoSimpleResponderThermal mirrors the guard used by the other
// real-process tests.
func skipIfNoSimpleResponderThermal(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "build", fmt.Sprintf("simple-responder_%s_%s", runtime.GOOS, runtime.GOARCH))
	if _, err := os.Stat(path); err != nil {
		t.Skipf("simple-responder not found at %s, run `make simple-responder`", path)
	}
	return path
}

// freePortForTest reserves an ephemeral port and releases it.
func freePortForTest(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// thermalE2EServer builds a full Server with one simple-responder-backed
// model and a running thermal monitor on an aggressive trip profile.
func thermalE2EServer(t *testing.T, responderPath string) (*Server, *thermalMonitor) {
	t.Helper()

	port := freePortForTest(t)
	cfg := config.Config{
		HealthCheckTimeout: 10,
		Performance:        config.PerformanceConfig{Every: 5 * time.Second},
		Routing: config.RoutingConfig{
			Router: config.RouterConfig{
				Use: "group",
				Settings: config.RouterSettings{
					Groups: map[string]config.GroupConfig{
						"(default)": {Swap: true, Exclusive: true, Members: []string{"hot-model"}},
					},
				},
			},
		},
		Power: config.PowerConfig{ThermalGuard: func() *config.ThermalGuardConfig {
			guard, err := (&config.ThermalGuardConfig{StopC: 85, ResumeC: 80, Consecutive: 2}).Effective()
			if err != nil {
				t.Fatalf("thermal guard: %v", err)
			}
			return guard
		}()},
		Models: map[string]config.ModelConfig{
			"hot-model": {
				Cmd:           strings.Join([]string{filepath.ToSlash(responderPath), fmt.Sprintf("-port %d", port)}, " "),
				Proxy:         fmt.Sprintf("http://127.0.0.1:%d", port),
				CheckEndpoint: "/health",
				Backend: config.BackendConfig{
					Launch: &config.ModelLaunchConfig{GPUs: []string{"index:0"}},
				},
				HealthCheckTimeout: 10,
			},
		},
	}

	// A real group router supervises the real simple-responder process; the
	// maintenance semantics under test live on this router.
	logger := logmon.NewWriter(io.Discard)
	localRouter, err := router.NewGroup(cfg, logger, logger)
	if err != nil {
		t.Fatalf("group router: %v", err)
	}
	t.Cleanup(func() { _ = localRouter.Shutdown(5 * time.Second) })

	ts := newTestServer(localRouter, nil)
	ts.cfg = cfg

	perfMon, err := perf.New(cfg.Performance, cfg.Power, logger)
	if err != nil {
		t.Fatalf("perf monitor: %v", err)
	}
	ts.perf = perfMon

	guard := cfg.Power.ThermalGuard
	monitor := newThermalMonitor(guard)
	done := make(chan struct{})
	go func() {
		defer close(done)
		monitor.run(ts)
	}()
	t.Cleanup(func() {
		monitor.Stop()
		<-done
	})
	return ts, monitor
}

// TestServer_ThermalGuardEndToEnd parks a live model on overheat and releases
// it after cooling, verifying the full loop through real processes and the
// real dispatch path.
func TestServer_ThermalGuardEndToEnd(t *testing.T) {
	responderPath := skipIfNoSimpleResponderThermal(t)
	ts, monitor := thermalE2EServer(t, responderPath)
	local := ts.local

	// Models start on their first inference request: the operator start path
	// dispatches normally while maintenance only ever refuses *later*
	// requests. The 503 body confirms a start was refused, which is what we
	// must detect after a trip.
	operatorStart := func() error {
		req := newThermalInferenceRequest()
		req = req.WithContext(swaputil.WithOperatorStart(req.Context()))
		rec := httptest.NewRecorder()
		ts.localPeerHandlerWithConfig(rec, req, ts.cfg)
		if rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
			return fmt.Errorf("operator start dispatch status %d: %s", rec.Code, rec.Body.String())
		}
		if rec.Code == http.StatusServiceUnavailable && strings.Contains(rec.Body.String(), "thermally parked") {
			return fmt.Errorf("start refused: thermally parked")
		}
		return waitLocalReady(t, local, "hot-model")
	}

	// 1. Bring the model up through the operator path.
	if err := operatorStart(); err != nil {
		t.Fatalf("operator start: %v", err)
	}
	if state := local.RunningModels()["hot-model"]; state != process.StateReady {
		t.Fatalf("model state = %s, want ready", state)
	}

	// 2. Inference works before the trip.
	req := newThermalInferenceRequest()
	rec := httptest.NewRecorder()
	ts.localPeerHandlerWithConfig(rec, req, ts.cfg)
	if rec.Code != http.StatusOK {
		t.Fatalf("pre-trip inference status = %d, want 200", rec.Code)
	}

	// 3. One hot sample must not trip (consecutive=2).
	monitor.applySamples(ts, []perf.GpuStat{{ID: 0, TempC: 92}})
	if _, parked := local.InMaintenance("hot-model"); parked {
		t.Fatalf("single hot sample must not park the model")
	}

	// 4. The second hot sample parks the model and unloads the process.
	monitor.applySamples(ts, []perf.GpuStat{{ID: 0, TempC: 93}})
	if _, parked := local.InMaintenance("hot-model"); !parked {
		t.Fatalf("model should be parked in maintenance after consecutive hot samples")
	}
	waitLocalStopped(t, local, "hot-model")

	// 5. Inference is now refused with the thermal 503 (no failover targets).
	req = newThermalInferenceRequest()
	rec = httptest.NewRecorder()
	ts.localPeerHandlerWithConfig(rec, req, ts.cfg)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("parked inference status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "thermally parked") {
		t.Errorf("503 body should explain thermal parking: %s", rec.Body.String())
	}

	// 6. A warm sample between stopC and resumeC keeps the card tripped.
	monitor.applySamples(ts, []perf.GpuStat{{ID: 0, TempC: 82}})
	if _, parked := local.InMaintenance("hot-model"); !parked {
		t.Fatalf("82°C is above resumeC: the model must stay parked")
	}

	// 7. Cooling below the resume point exits maintenance.
	monitor.applySamples(ts, []perf.GpuStat{{ID: 0, TempC: 76}})
	if _, parked := local.InMaintenance("hot-model"); parked {
		t.Fatalf("model should leave maintenance after cooling below resumeC")
	}

	// 8. The operator restarts the model and inference serves again.
	if err := operatorStart(); err != nil {
		t.Fatalf("operator restart after cooling: %v", err)
	}
	req = newThermalInferenceRequest()
	rec = httptest.NewRecorder()
	ts.localPeerHandlerWithConfig(rec, req, ts.cfg)
	if rec.Code != http.StatusOK {
		t.Fatalf("post-resume inference status = %d, want 200", rec.Code)
	}
}

// TestServer_ThermalGuardEndToEndFailoverToPeer proves the failover leg: with
// a peer claiming the parked model, dispatch forwards the request instead of
// answering 503.
func TestServer_ThermalGuardEndToEndFailoverToPeer(t *testing.T) {
	responderPath := skipIfNoSimpleResponderThermal(t)
	ts, _ := thermalE2EServer(t, responderPath)
	local := ts.local

	peer := newStubRouter([]string{"hot-model"}, "peer response")
	ts.peer = peer

	// Park the model directly (the trip path is covered by the other test).
	local.EnterMaintenance("hot-model", thermalGuardReason+": GPU 0 at 93°C")
	waitLocalStopped(t, local, "hot-model")

	req := newThermalInferenceRequest()
	rec := httptest.NewRecorder()
	ts.localPeerHandlerWithConfig(rec, req, ts.cfg)
	if rec.Code != http.StatusOK {
		t.Fatalf("failover status = %d, body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "peer response") {
		t.Errorf("request should have been served by the peer: %s", rec.Body.String())
	}
}

// TestServer_ThermalGuardEndToEndOperatorStartAllowed proves that an operator
// start passes the dispatch failover check and reaches the router even while
// the model is thermally parked — that is the path that revives the model.
func TestServer_ThermalGuardEndToEndOperatorStartAllowed(t *testing.T) {
	responderPath := skipIfNoSimpleResponderThermal(t)
	ts, _ := thermalE2EServer(t, responderPath)
	local := ts.local

	local.EnterMaintenance("hot-model", thermalGuardReason+": GPU 0 at 93°C")

	req := httptest.NewRequest(http.MethodPost, "/upstream/hot-model/", nil)
	req = req.WithContext(swaputil.WithOperatorStart(req.Context()))
	rec := httptest.NewRecorder()
	ts.localPeerHandlerWithConfig(rec, req, ts.cfg)
	// The operator start must reach the local router (which then starts the
	// process); the dispatch layer must not answer it with the thermal 503.
	if rec.Code == http.StatusServiceUnavailable && strings.Contains(rec.Body.String(), "thermally parked") {
		t.Fatalf("operator start was blocked by the failover layer")
	}
}

// newThermalInferenceRequest builds a chat-completions dispatch request whose
// model FetchContext can resolve (JSON content type is required).
func newThermalInferenceRequest() *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"hot-model"}`))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// waitLocalReady polls the stub router's running map until the model reports
// ready, failing the test on timeout.
func waitLocalReady(t *testing.T, local router.LocalRouter, modelID string) error {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if state, ok := local.RunningModels()[modelID]; ok && state == process.StateReady {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("model %s did not become ready within 10s", modelID)
}

// waitLocalStopped polls until the model left the running map.
func waitLocalStopped(t *testing.T, local router.LocalRouter, modelID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, running := local.RunningModels()[modelID]; !running {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("model %s still running 10s after the thermal stop", modelID)
}
