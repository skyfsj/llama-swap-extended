package server

import (
	"context"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/router/scheduler"
	runtimeManager "github.com/mostlygeek/llama-swap/internal/runtime"
)

type runtimeSwitchRouter struct {
	running  map[string]process.ProcessState
	config   config.Config
	restarts []string
}

func (*runtimeSwitchRouter) Shutdown(time.Duration) error                 { return nil }
func (*runtimeSwitchRouter) ServeHTTP(http.ResponseWriter, *http.Request) {}
func (*runtimeSwitchRouter) Handles(string) bool                          { return true }
func (r *runtimeSwitchRouter) RunningModels() map[string]process.ProcessState {
	return r.running
}
func (*runtimeSwitchRouter) Unload(time.Duration, ...string)              {}
func (*runtimeSwitchRouter) ProcessLogger(string) (*logmon.Monitor, bool) { return nil, false }
func (r *runtimeSwitchRouter) Reconfigure(cfg config.Config) error {
	r.config = cfg
	return nil
}
func (r *runtimeSwitchRouter) RestartModel(model string) error {
	r.restarts = append(r.restarts, model)
	return nil
}
func (*runtimeSwitchRouter) ModelLifecycleStatuses() map[string]scheduler.ModelLifecycleStatus {
	return nil
}

func TestServer_RuntimeReadyModelsSelectsOnlyReadyRuntimeConsumers(t *testing.T) {
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"ready-b":     {Backend: config.BackendConfig{Runtime: "vllm"}},
		"ready-a":     {Backend: config.BackendConfig{Runtime: "vllm"}},
		"starting":    {Backend: config.BackendConfig{Runtime: "vllm"}},
		"other":       {Backend: config.BackendConfig{Runtime: "llama"}},
		"not-running": {Backend: config.BackendConfig{Runtime: "vllm"}},
	}}
	running := map[string]process.ProcessState{
		"ready-a":  process.StateReady,
		"ready-b":  process.StateReady,
		"starting": process.StateStarting,
		"other":    process.StateReady,
	}
	got := runtimeReadyModels(cfg, running, "vllm")
	want := []string{"ready-a", "ready-b"}
	if !slices.Equal(got, want) {
		t.Fatalf("ready runtime models=%v want %v", got, want)
	}
}

func TestServer_SwitchRuntimeProcessesBindsCandidateAndRestartsReadyModels(t *testing.T) {
	root := t.TempDir()
	for _, version := range []string{"1", "2"} {
		writeRuntimeVersion(t, root, "vllm-git", runtimeManager.Manifest{
			Name: "vllm-git", Version: version, Kind: "vllm", Source: "pypi",
			Metadata: map[string]string{"mode": runtimeManager.RuntimeModeNative, "sourceType": "pypi"},
		})
	}
	cfg := config.Config{
		RuntimeManager: config.RuntimeManagerConfig{Root: root},
		Runtimes:       map[string]config.RuntimeConfig{"vllm-git": {Kind: "vllm"}},
		Models: map[string]config.ModelConfig{
			"ready":    {Backend: config.BackendConfig{Type: "vllm", Runtime: "vllm-git", Arguments: []string{"vllm", "serve", "model"}}},
			"starting": {Backend: config.BackendConfig{Type: "vllm", Runtime: "vllm-git", Arguments: []string{"vllm", "serve", "other"}}},
		},
	}
	local := &runtimeSwitchRouter{running: map[string]process.ProcessState{
		"ready": process.StateReady, "starting": process.StateStarting,
	}}
	server := &Server{cfg: cfg, local: local}
	if err := server.switchRuntimeProcesses(context.Background(), "vllm-git", "1", "2"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(local.restarts, []string{"ready"}) {
		t.Fatalf("restarted models=%v", local.restarts)
	}
	if got, want := local.config.Models["ready"].Backend.Arguments[0], filepath.Join(root, "vllm-git", "versions", "2", ".venv", "bin", "vllm"); got != want {
		t.Fatalf("candidate executable=%q want %q", got, want)
	}
}

// runtimeSwitchLifecycleProvider keeps this test on the Manager/Server
// boundary: manifests and current pointers are real, while the local router
// fixture controls when a process drain or candidate start completes.
type runtimeSwitchLifecycleProvider struct{}

func (runtimeSwitchLifecycleProvider) Stage(_ context.Context, spec runtimeManager.Spec, _ string) (runtimeManager.Manifest, error) {
	return runtimeManager.Manifest{
		Name: spec.Name, Version: spec.Version, Kind: spec.Kind, Source: "pypi",
		Metadata: map[string]string{"mode": runtimeManager.RuntimeModeNative, "sourceType": "pypi"},
	}, nil
}

func (runtimeSwitchLifecycleProvider) Verify(context.Context, runtimeManager.Manifest, string) error {
	return nil
}
func (runtimeSwitchLifecycleProvider) Health(context.Context, runtimeManager.Manifest, string) error {
	return nil
}

type runtimeSwitchLifecycleRouter struct {
	mu sync.Mutex

	desired      config.Config
	running      map[string]process.ProcessState
	statuses     map[string]scheduler.ModelLifecycleStatus
	restarts     []string
	restartBegan chan struct{}
	release      chan struct{}
	failVersion  string
}

func newRuntimeSwitchLifecycleRouter() *runtimeSwitchLifecycleRouter {
	return &runtimeSwitchLifecycleRouter{
		running:      make(map[string]process.ProcessState),
		statuses:     make(map[string]scheduler.ModelLifecycleStatus),
		restartBegan: make(chan struct{}),
	}
}

func (*runtimeSwitchLifecycleRouter) Shutdown(time.Duration) error                 { return nil }
func (*runtimeSwitchLifecycleRouter) ServeHTTP(http.ResponseWriter, *http.Request) {}
func (*runtimeSwitchLifecycleRouter) Handles(string) bool                          { return true }
func (*runtimeSwitchLifecycleRouter) Unload(time.Duration, ...string)              {}
func (*runtimeSwitchLifecycleRouter) ProcessLogger(string) (*logmon.Monitor, bool) { return nil, false }

func (r *runtimeSwitchLifecycleRouter) RunningModels() map[string]process.ProcessState {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make(map[string]process.ProcessState, len(r.running))
	for model, state := range r.running {
		if state != process.StateStopped && state != process.StateShutdown {
			result[model] = state
		}
	}
	return result
}

func (r *runtimeSwitchLifecycleRouter) Reconfigure(cfg config.Config) error {
	r.mu.Lock()
	r.desired = cfg
	r.mu.Unlock()
	return nil
}

func (r *runtimeSwitchLifecycleRouter) RestartModel(modelID string) error {
	r.mu.Lock()
	model, found := r.desired.Models[modelID]
	if !found || len(model.Backend.Arguments) == 0 {
		r.mu.Unlock()
		return scheduler.ErrRestartNotPending
	}
	executable := model.Backend.Arguments[0]
	r.restarts = append(r.restarts, executable)
	r.statuses[modelID] = scheduler.ModelLifecycleStatus{ConfigStatus: scheduler.ConfigStatusDraining}
	select {
	case <-r.restartBegan:
	default:
		close(r.restartBegan)
	}
	release := r.release
	fail := r.failVersion != "" && strings.Contains(filepath.ToSlash(executable), "/versions/"+r.failVersion+"/")
	r.mu.Unlock()

	go func() {
		if release != nil {
			<-release
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		// The production router recreates the last-known-good process before
		// reporting apply_failed. Keep the fixture serviceable so the Manager's
		// compensation hook can publish the old durable runtime generation.
		r.running[modelID] = process.StateReady
		if fail {
			r.statuses[modelID] = scheduler.ModelLifecycleStatus{ConfigStatus: scheduler.ConfigStatusApplyFailed, Error: "candidate process failed"}
			return
		}
		delete(r.statuses, modelID)
	}()
	return nil
}

func (r *runtimeSwitchLifecycleRouter) ModelLifecycleStatuses() map[string]scheduler.ModelLifecycleStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make(map[string]scheduler.ModelLifecycleStatus, len(r.statuses))
	for model, status := range r.statuses {
		result[model] = status
	}
	return result
}

func (r *runtimeSwitchLifecycleRouter) setRunning(model string, state process.ProcessState) {
	r.mu.Lock()
	r.running[model] = state
	r.mu.Unlock()
}

func (r *runtimeSwitchLifecycleRouter) restartExecutables() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.restarts...)
}

func runtimeSwitchLifecycleConfig(root string) config.Config {
	return config.Config{
		RuntimeManager: config.RuntimeManagerConfig{Root: root},
		Runtimes:       map[string]config.RuntimeConfig{"vllm": {Kind: "vllm", Source: config.RuntimeSource{Type: "pypi"}}},
		Models: map[string]config.ModelConfig{
			"model": {Backend: config.BackendConfig{Type: "vllm", Runtime: "vllm", Arguments: []string{"vllm", "serve", "fixture"}}},
		},
	}
}

func newRuntimeSwitchLifecycleManager(t *testing.T, cfg config.Config, local *runtimeSwitchLifecycleRouter) *runtimeManager.Manager {
	t.Helper()
	manager, err := runtimeManager.NewManager(cfg.RuntimeManager.Root, map[string]runtimeManager.Provider{"vllm": runtimeSwitchLifecycleProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	manager.SetIdleProbe(func() bool { return true })
	server := &Server{cfg: cfg, local: local}
	server.configReconciler = NewConfigReconciler(cfg, nil, nil)
	manager.SetActivationHooks(nil, server.switchRuntimeProcesses)
	return manager
}

func stageAndActivateRuntimeSwitchVersion(t *testing.T, manager *runtimeManager.Manager, version string) {
	t.Helper()
	if _, err := manager.Stage(context.Background(), runtimeManager.Spec{
		Name: "vllm", Kind: "vllm", Mode: runtimeManager.RuntimeModeNative,
		Version: version, SourceType: "pypi", Source: "pypi",
	}); err != nil {
		t.Fatalf("stage %s: %v", version, err)
	}
	if err := manager.Activate(context.Background(), "vllm", version); err != nil {
		t.Fatalf("activate %s: %v", version, err)
	}
}

func TestServer_RuntimeActivationWaitsForReadyModelDrain(t *testing.T) {
	cfg := runtimeSwitchLifecycleConfig(t.TempDir())
	local := newRuntimeSwitchLifecycleRouter()
	manager := newRuntimeSwitchLifecycleManager(t, cfg, local)
	stageAndActivateRuntimeSwitchVersion(t, manager, "v1")
	local.setRunning("model", process.StateReady)
	local.release = make(chan struct{})
	if _, err := manager.Stage(context.Background(), runtimeManager.Spec{Name: "vllm", Kind: "vllm", Mode: runtimeManager.RuntimeModeNative, Version: "v2", SourceType: "pypi", Source: "pypi"}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	activated := make(chan error, 1)
	go func() { activated <- manager.Activate(ctx, "vllm", "v2") }()
	select {
	case <-local.restartBegan:
	case <-time.After(time.Second):
		t.Fatal("candidate model restart did not begin")
	}
	select {
	case err := <-activated:
		t.Fatalf("activation returned before the old model drained: %v", err)
	default:
	}
	if state := local.RunningModels()["model"]; state != process.StateReady {
		t.Fatalf("old generation was not retained during drain: %q", state)
	}
	close(local.release)
	if err := <-activated; err != nil {
		t.Fatalf("activate v2: %v", err)
	}
	status, found := manager.Get("vllm")
	if !found || status.Current != "v2" {
		t.Fatalf("runtime status after replacement=%+v found=%v", status, found)
	}
	restarts := local.restartExecutables()
	if len(restarts) != 1 {
		t.Fatalf("restart executables=%q want one candidate restart", restarts)
	}
	if want := filepath.Join(cfg.RuntimeManager.Root, "vllm", "versions", "v2", ".venv", "bin", "vllm"); restarts[0] != want {
		t.Fatalf("candidate restart executable=%q want %q", restarts[0], want)
	}
}

func TestServer_RuntimeActivationFailureRestoresOldProcessBinding(t *testing.T) {
	cfg := runtimeSwitchLifecycleConfig(t.TempDir())
	local := newRuntimeSwitchLifecycleRouter()
	manager := newRuntimeSwitchLifecycleManager(t, cfg, local)
	stageAndActivateRuntimeSwitchVersion(t, manager, "v1")
	local.setRunning("model", process.StateReady)
	local.failVersion = "v2"
	if _, err := manager.Stage(context.Background(), runtimeManager.Spec{Name: "vllm", Kind: "vllm", Mode: runtimeManager.RuntimeModeNative, Version: "v2", SourceType: "pypi", Source: "pypi"}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Activate(context.Background(), "vllm", "v2"); err == nil || !strings.Contains(err.Error(), "candidate process failed") {
		t.Fatalf("candidate activation error=%v", err)
	}
	status, found := manager.Get("vllm")
	if !found || status.Current != "v1" {
		t.Fatalf("runtime pointer/status was not restored: %+v found=%v", status, found)
	}
	restarts := local.restartExecutables()
	if len(restarts) != 2 {
		t.Fatalf("restart executables=%q want candidate and compensation", restarts)
	}
	for index, version := range []string{"v2", "v1"} {
		want := filepath.Join(cfg.RuntimeManager.Root, "vllm", "versions", version, ".venv", "bin", "vllm")
		if restarts[index] != want {
			t.Fatalf("restart %d executable=%q want %q", index, restarts[index], want)
		}
	}
}

// TestServer_RuntimeActivationEscapesApplyDeadlock reproduces the H9 deadlock
// pair: an activation holds the manager lock while its switch hook waits for
// a config apply that is itself blocked on the manager lock from inside its
// apply callback (the LMCache module sync staging a server runtime). The
// bounded apply wait must convert the former permanent freeze into a failed,
// compensated activation.
func TestServer_RuntimeActivationEscapesApplyDeadlock(t *testing.T) {
	cfg := runtimeSwitchLifecycleConfig(t.TempDir())
	local := newRuntimeSwitchLifecycleRouter()
	manager := newRuntimeSwitchLifecycleManager(t, cfg, local)
	stageAndActivateRuntimeSwitchVersion(t, manager, "v1")

	oldBound := restartLockApplyWaitTimeout
	restartLockApplyWaitTimeout = 250 * time.Millisecond
	t.Cleanup(func() { restartLockApplyWaitTimeout = oldBound })

	// A config apply that never finishes: the callback parks until the test
	// ends, keeping the reconciler's applying flag true the whole time.
	applyBegan := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	desired := cfg
	desired.Models = map[string]config.ModelConfig{
		"model": {Backend: config.BackendConfig{Type: "vllm", Runtime: "vllm", Arguments: []string{"vllm", "serve", "changed"}}},
	}
	reconciler := NewConfigReconciler(cfg, func(active, desired config.Config, _ config.ConfigChangeSet) error {
		close(applyBegan)
		<-release
		return nil
	}, nil)
	server := &Server{cfg: cfg, local: local, configReconciler: reconciler}
	manager.SetActivationHooks(nil, server.switchRuntimeProcesses)

	go func() { _ = reconciler.Reconcile(desired) }()
	select {
	case <-applyBegan:
	case <-time.After(time.Second):
		t.Fatal("apply callback never began")
	}

	if _, err := manager.Stage(context.Background(), runtimeManager.Spec{Name: "vllm", Kind: "vllm", Mode: runtimeManager.RuntimeModeNative, Version: "v2", SourceType: "pypi", Source: "pypi"}); err != nil {
		t.Fatal(err)
	}

	activated := make(chan error, 1)
	go func() { activated <- manager.Activate(context.Background(), "vllm", "v2") }()
	select {
	case err := <-activated:
		if err == nil {
			t.Fatal("activation succeeded while the apply wait should have timed out")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("activation deadlocked: it did not escape the apply wait")
	}
	status, found := manager.Get("vllm")
	if !found || status.Current != "v1" {
		t.Fatalf("runtime pointer was not restored after the escape: %+v found=%v", status, found)
	}
}
