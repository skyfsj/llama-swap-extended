package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
	resourcePlanner "github.com/mostlygeek/llama-swap/internal/resource"
	runtimeManager "github.com/mostlygeek/llama-swap/internal/runtime"
)

func readinessRuntimeSpec(version string) runtimeManager.Spec {
	return runtimeManager.Spec{
		Name:       "demo",
		Kind:       "vllm",
		Mode:       runtimeManager.RuntimeModeNative,
		Version:    version,
		SourceType: "pypi",
		Source:     "pypi",
	}
}

func readinessRuntimeConfig() config.RuntimeConfig {
	return config.RuntimeConfig{
		Kind:   "vllm",
		Mode:   runtimeManager.RuntimeModeNative,
		Source: config.RuntimeSource{Type: "pypi", URL: "pypi"},
		Update: config.RuntimeUpdateConfig{
			Policy:            "manual",
			ActivateOnlyIdle:  true,
			KeepVersions:      2,
			RollbackOnFailure: true,
		},
	}
}

func newReadinessServer(t *testing.T, cfg config.Config, running map[string]process.ProcessState) *Server {
	t.Helper()
	manager, err := runtimeManager.NewManager(t.TempDir(), map[string]runtimeManager.Provider{
		"vllm": catalogAPIProvider{},
	})
	if err != nil {
		t.Fatal(err)
	}
	spec := readinessRuntimeSpec("1.0.0")
	if err := manager.Configure(spec.Name, spec, runtimeManager.UpdatePolicy{Policy: "manual"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Stage(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	local := newStubRouter(nil, "")
	local.running = running
	s := newTestServer(local, newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	s.runtime = manager
	s.cfg = cfg
	return s
}

func readinessCheck(readiness RuntimeReadiness, code string) (RuntimeReadinessCheck, bool) {
	for _, check := range readiness.Checks {
		if check.Code == code {
			return check, true
		}
	}
	return RuntimeReadinessCheck{}, false
}

func TestServer_RuntimeReadinessReportsFirstActivationAndUnknownCapacity(t *testing.T) {
	cfg := config.Config{
		Runtimes: map[string]config.RuntimeConfig{"demo": readinessRuntimeConfig()},
		Models: map[string]config.ModelConfig{
			"chat": {Backend: config.BackendConfig{
				Type: "vllm", Runtime: "demo", Arguments: []string{"serve"},
			}},
		},
	}
	s := newReadinessServer(t, cfg, nil)

	readiness, ok, err := s.buildRuntimeReadiness("demo", "", auth.Identity{})
	if err != nil || !ok {
		t.Fatalf("buildRuntimeReadiness ok=%v err=%v", ok, err)
	}
	if !readiness.Selected.Installed || !readiness.Selected.Staged || readiness.Selected.Current {
		t.Fatalf("selected version=%+v, want an installed staged first activation", readiness.Selected)
	}
	if readiness.Rollback.Available {
		t.Fatal("first activation unexpectedly advertised a rollback version")
	}
	if check, found := readinessCheck(readiness, "budget-unconfigured"); !found || check.Level != "warning" {
		t.Fatalf("budget check=%+v found=%v, want warning", check, found)
	}
	if check, found := readinessCheck(readiness, "model-footprint-unknown"); !found || check.Level != "warning" {
		t.Fatalf("footprint check=%+v found=%v, want warning", check, found)
	}
	if check, found := readinessCheck(readiness, "rollback-unavailable"); !found || check.Level != "warning" {
		t.Fatalf("rollback check=%+v found=%v, want warning", check, found)
	}
	for _, check := range readiness.Checks {
		if check.Level == "block" {
			t.Fatalf("first activation has unexpected blocking check: %+v", check)
		}
	}
}

func TestServer_RuntimeReadinessBlocksWhenGlobalControlPlaneIsBusy(t *testing.T) {
	cfg := config.Config{
		Runtimes: map[string]config.RuntimeConfig{"demo": readinessRuntimeConfig()},
	}
	s := newReadinessServer(t, cfg, nil)
	s.controlPlaneActive.Store(1)

	readiness, ok, err := s.buildRuntimeReadiness("demo", "", auth.Identity{})
	if err != nil || !ok {
		t.Fatalf("buildRuntimeReadiness ok=%v err=%v", ok, err)
	}
	if readiness.Idle.Ready || len(readiness.Idle.Reasons) != 1 || readiness.Idle.Reasons[0].Code != "control-plane-busy" {
		t.Fatalf("idle report=%+v, want a control-plane busy reason", readiness.Idle)
	}
	if check, found := readinessCheck(readiness, "global-idle-required"); !found || check.Level != "block" {
		t.Fatalf("idle check=%+v found=%v, want block", check, found)
	}
}

func TestServer_RuntimeIdle_AllowsLMCacheToStageWhileEnableOwnsInstallLock(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	s.cfg = config.Config{LMCache: config.LMCacheModuleConfig{Enabled: true}}
	s.lmcacheMod = newLMCacheService(s)
	s.lmcacheMod.installBusy.Store(true)
	defer s.lmcacheMod.installBusy.Store(false)

	if reasons := s.runtimeIdleReasons(config.LMCacheRuntimeName); len(reasons) != 0 {
		t.Fatalf("runtimeIdleReasons() = %+v, want none while enable stages its own server runtime", reasons)
	}
}

func TestServer_RuntimeReadinessFiltersModelsAndEnforcesRuntimeScope(t *testing.T) {
	cfg := config.Config{
		Runtimes: map[string]config.RuntimeConfig{"demo": readinessRuntimeConfig()},
		Models: map[string]config.ModelConfig{
			"visible": {Backend: config.BackendConfig{Type: "vllm", Runtime: "demo", Arguments: []string{"serve"}}},
			"other":   {Backend: config.BackendConfig{Type: "vllm", Runtime: "other", Arguments: []string{"serve"}}},
		},
	}
	s := newReadinessServer(t, cfg, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/runtimes/demo/readiness", nil)
	request.SetPathValue("name", "demo")
	request = request.WithContext(withIdentity(request.Context(), auth.Identity{Models: []string{"visible"}}))
	recorder := httptest.NewRecorder()
	s.handleAPIRuntimeReadiness(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("scoped readiness status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var readiness RuntimeReadiness
	if err := json.Unmarshal(recorder.Body.Bytes(), &readiness); err != nil {
		t.Fatal(err)
	}
	if len(readiness.Models) != 1 || readiness.Models[0].ID != "visible" {
		t.Fatalf("scoped models=%+v, want only visible", readiness.Models)
	}

	cfg.Models["hidden"] = config.ModelConfig{Backend: config.BackendConfig{Type: "vllm", Runtime: "demo", Arguments: []string{"serve"}}}
	s.cfg = cfg
	request = httptest.NewRequest(http.MethodGet, "/api/runtimes/demo/readiness", nil)
	request.SetPathValue("name", "demo")
	request = request.WithContext(withIdentity(request.Context(), auth.Identity{Models: []string{"visible"}}))
	recorder = httptest.NewRecorder()
	s.handleAPIRuntimeReadiness(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("shared denied runtime status=%d body=%s, want 403", recorder.Code, recorder.Body.String())
	}
}

func TestServer_RuntimeReadinessBlocksIncompatibleModelBindings(t *testing.T) {
	cfg := config.Config{
		Runtimes: map[string]config.RuntimeConfig{"demo": readinessRuntimeConfig()},
		Models: map[string]config.ModelConfig{
			"wrong-kind": {Backend: config.BackendConfig{Type: "llamacpp", Runtime: "demo", Arguments: []string{"serve"}}},
			"legacy":     {Cmd: "python server.py", Backend: config.BackendConfig{Type: "vllm", Runtime: "demo", Arguments: []string{"serve"}}},
		},
	}
	s := newReadinessServer(t, cfg, nil)
	readiness, ok, err := s.buildRuntimeReadiness("demo", "", auth.Identity{})
	if err != nil || !ok {
		t.Fatalf("buildRuntimeReadiness ok=%v err=%v", ok, err)
	}
	check, found := readinessCheck(readiness, "model-incompatible")
	if !found || check.Level != "block" {
		t.Fatalf("compatibility check=%+v found=%v, want block", check, found)
	}
	models := make(map[string]RuntimeModelImpact, len(readiness.Models))
	for _, model := range readiness.Models {
		models[model.ID] = model
	}
	if models["wrong-kind"].Compatible || !models["legacy"].LegacyCommand {
		t.Fatalf("model impacts=%+v, want kind conflict and legacy command markers", models)
	}
}

func TestServer_RuntimeReadinessBlocksExplicitBudgetOverflow(t *testing.T) {
	cfg := config.Config{
		Runtimes:       map[string]config.RuntimeConfig{"demo": readinessRuntimeConfig()},
		ResourceBudget: config.ResourceBudgetConfig{VRAMMiB: 100},
		Models: map[string]config.ModelConfig{
			"loaded": {Backend: config.BackendConfig{
				Type: "vllm", Runtime: "demo", Arguments: []string{"serve"},
				Resources: config.ResourceConfig{VRAMMiB: 200},
			}},
		},
	}
	s := newReadinessServer(t, cfg, map[string]process.ProcessState{"loaded": process.StateReady})
	s.resources = resourcePlanner.NewPlanner(resourcePlanner.Budget{VRAMMiB: 100})

	readiness, ok, err := s.buildRuntimeReadiness("demo", "", auth.Identity{})
	if err != nil || !ok {
		t.Fatalf("buildRuntimeReadiness ok=%v err=%v", ok, err)
	}
	if readiness.Resources.UsageVRAM != 200 || !readiness.Resources.Configured {
		t.Fatalf("resources=%+v, want configured 100 MiB budget and 200 MiB usage", readiness.Resources)
	}
	if check, found := readinessCheck(readiness, "budget-vram-exceeded"); !found || check.Level != "block" {
		t.Fatalf("budget check=%+v found=%v, want block", check, found)
	}
}

func TestServer_RuntimeReadinessReportsRetainedRollback(t *testing.T) {
	cfg := config.Config{
		Runtimes: map[string]config.RuntimeConfig{"demo": readinessRuntimeConfig()},
	}
	s := newReadinessServer(t, cfg, nil)
	spec := readinessRuntimeSpec("2.0.0")
	if _, err := s.runtime.Stage(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	s.runtime.SetIdleProbe(func() bool { return true })
	if err := s.runtime.Activate(context.Background(), "demo", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := s.runtime.Activate(context.Background(), "demo", "2.0.0"); err != nil {
		t.Fatal(err)
	}

	readiness, ok, err := s.buildRuntimeReadiness("demo", "", auth.Identity{})
	if err != nil || !ok {
		t.Fatalf("buildRuntimeReadiness ok=%v err=%v", ok, err)
	}
	if !readiness.Rollback.Available || readiness.Rollback.Version != "1.0.0" || !readiness.Rollback.Automatic {
		t.Fatalf("rollback=%+v, want automatic retained 1.0.0", readiness.Rollback)
	}
	if check, found := readinessCheck(readiness, "rollback-available"); !found || check.Level != "pass" {
		t.Fatalf("rollback check=%+v found=%v, want pass", check, found)
	}
}

func TestServer_RuntimeReadinessAllowsInstalledUnstagedVersion(t *testing.T) {
	cfg := config.Config{
		Runtimes: map[string]config.RuntimeConfig{"demo": readinessRuntimeConfig()},
	}
	s := newReadinessServer(t, cfg, nil)
	manager := s.runtime

	// Install three versions: current=2.0.0, staged=3.0.0, and 1.0.0
	// retained but neither current nor staged.
	if err := manager.Activate(context.Background(), "demo", "1.0.0"); err != nil {
		t.Fatalf("activate 1.0.0: %v", err)
	}
	if _, err := manager.Stage(context.Background(), readinessRuntimeSpec("2.0.0")); err != nil {
		t.Fatalf("stage 2.0.0: %v", err)
	}
	if err := manager.Activate(context.Background(), "demo", "2.0.0"); err != nil {
		t.Fatalf("activate 2.0.0: %v", err)
	}
	if _, err := manager.Stage(context.Background(), readinessRuntimeSpec("3.0.0")); err != nil {
		t.Fatalf("stage 3.0.0: %v", err)
	}

	readiness, ok, err := s.buildRuntimeReadiness("demo", "1.0.0", auth.Identity{})
	if err != nil || !ok {
		t.Fatalf("buildRuntimeReadiness ok=%v err=%v", ok, err)
	}
	if !readiness.Selected.Installed || readiness.Selected.Current || readiness.Selected.Staged {
		t.Fatalf("selected=%+v, want installed non-current non-staged version", readiness.Selected)
	}
	if check, found := readinessCheck(readiness, "candidate-not-staged"); found {
		t.Fatalf("installed version unexpectedly blocked: %+v", check)
	}
	for _, check := range readiness.Checks {
		if check.Level == "block" {
			t.Fatalf("unexpected blocking check: %+v", check)
		}
	}

	// A version that is neither installed nor staged must still be blocked.
	unstaged, ok, err := s.buildRuntimeReadiness("demo", "9.9.9", auth.Identity{})
	if err != nil || !ok {
		t.Fatalf("buildRuntimeReadiness ok=%v err=%v", ok, err)
	}
	if check, found := readinessCheck(unstaged, "candidate-not-staged"); !found || check.Level != "block" {
		t.Fatalf("candidate-not-staged check=%+v found=%v, want block", check, found)
	}
}
