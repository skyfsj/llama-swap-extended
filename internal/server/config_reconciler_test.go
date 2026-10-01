package server

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/modeldownload"
	"github.com/mostlygeek/llama-swap/internal/modelmanager"
	"github.com/mostlygeek/llama-swap/internal/router"
	runtimeManager "github.com/mostlygeek/llama-swap/internal/runtime"
)

type reconfigurableStubRouter struct {
	*stubRouter
	reconfigured   []config.Config
	reconfigureErr error
}

func (r *reconfigurableStubRouter) Reconfigure(candidate config.Config) error {
	r.reconfigured = append(r.reconfigured, candidate)
	return r.reconfigureErr
}

func waitForDesiredRevision(t *testing.T, reconciler *ConfigReconciler, want uint64) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if reconciler.Status().DesiredRevision >= want {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("desired revision did not reach %d; status=%+v", want, reconciler.Status())
		case <-ticker.C:
		}
	}
}

func TestConfigReconciler_CoalescesLatestCandidate(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})

	reconciler := NewConfigReconciler(config.Config{LogLevel: "initial"}, func(_, desired config.Config, _ config.ConfigChangeSet) error {
		mu.Lock()
		calls = append(calls, desired.LogLevel)
		mu.Unlock()
		if desired.LogLevel == "first" {
			close(firstStarted)
			<-releaseFirst
		}
		return nil
	}, nil)

	firstErr := make(chan error, 1)
	go func() { firstErr <- reconciler.Reconcile(config.Config{LogLevel: "first"}) }()
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first candidate was not applied")
	}

	secondErr := make(chan error, 1)
	go func() { secondErr <- reconciler.Reconcile(config.Config{LogLevel: "second"}) }()
	waitForDesiredRevision(t, reconciler, 2)
	close(releaseFirst)

	if err := <-firstErr; err != nil {
		t.Fatalf("first reconcile error=%v", err)
	}
	if err := <-secondErr; err != nil {
		t.Fatalf("second reconcile error=%v", err)
	}

	mu.Lock()
	gotCalls := append([]string(nil), calls...)
	mu.Unlock()
	if len(gotCalls) != 2 || gotCalls[0] != "first" || gotCalls[1] != "second" {
		t.Fatalf("apply calls=%v want [first second]", gotCalls)
	}
	status := reconciler.Status()
	if status.ActiveRevision != 2 || status.DesiredRevision != 2 {
		t.Fatalf("status=%+v want both revisions 2", status)
	}
}

func TestConfigReconciler_StaleErrorDoesNotDiscardNewerCandidate(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	staleErr := errors.New("stale candidate rejected")

	reconciler := NewConfigReconciler(config.Config{LogLevel: "initial"}, func(_, desired config.Config, _ config.ConfigChangeSet) error {
		mu.Lock()
		calls = append(calls, desired.LogLevel)
		mu.Unlock()
		if desired.LogLevel == "stale" {
			close(firstStarted)
			<-releaseFirst
			return staleErr
		}
		return nil
	}, nil)

	firstErr := make(chan error, 1)
	go func() { firstErr <- reconciler.Reconcile(config.Config{LogLevel: "stale"}) }()
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("stale candidate was not applied")
	}

	secondErr := make(chan error, 1)
	go func() { secondErr <- reconciler.Reconcile(config.Config{LogLevel: "latest"}) }()
	waitForDesiredRevision(t, reconciler, 2)
	close(releaseFirst)

	if err := <-firstErr; err != nil {
		t.Fatalf("stale reconcile error=%v; latest candidate should win", err)
	}
	if err := <-secondErr; err != nil {
		t.Fatalf("latest reconcile error=%v", err)
	}
	mu.Lock()
	gotCalls := append([]string(nil), calls...)
	mu.Unlock()
	if len(gotCalls) != 2 || gotCalls[0] != "stale" || gotCalls[1] != "latest" {
		t.Fatalf("apply calls=%v want [stale latest]", gotCalls)
	}
	status := reconciler.Status()
	if status.ActiveRevision != 1 || status.DesiredRevision != 2 {
		t.Fatalf("status=%+v want latest successful active revision", status)
	}
}

func TestConfigReconciler_InvalidCandidateKeepsActiveSnapshot(t *testing.T) {
	invalidErr := errors.New("invalid runtime configuration")
	var activeSeen config.Config
	reconciler := NewConfigReconciler(config.Config{LogLevel: "initial"}, func(active, desired config.Config, _ config.ConfigChangeSet) error {
		if desired.LogLevel == "invalid" {
			return invalidErr
		}
		activeSeen = active
		return nil
	}, nil)

	if err := reconciler.Reconcile(config.Config{LogLevel: "invalid"}); !errors.Is(err, invalidErr) {
		t.Fatalf("invalid reconcile error=%v want %v", err, invalidErr)
	}
	status := reconciler.Status()
	if status.ActiveRevision != 0 || status.DesiredRevision != 1 {
		t.Fatalf("status after invalid candidate=%+v", status)
	}

	if err := reconciler.Reconcile(config.Config{LogLevel: "valid"}); err != nil {
		t.Fatalf("valid reconcile error=%v", err)
	}
	if activeSeen.LogLevel != "initial" {
		t.Fatalf("valid candidate saw active=%q want initial", activeSeen.LogLevel)
	}
	if status := reconciler.Status(); status.ActiveRevision != 1 || status.DesiredRevision != 2 {
		t.Fatalf("final status=%+v", status)
	}
}

func TestServer_ReconcileConfigReloadsModelFileSources(t *testing.T) {
	oldRoot := t.TempDir()
	newRoot := t.TempDir()
	active := config.Config{ModelFiles: config.ModelFilesConfig{Sources: map[string]config.ModelFileSourceConfig{
		"old-models": {Type: config.ModelFileSourceDirectory, Path: oldRoot},
	}}}
	desired := config.Config{ModelFiles: config.ModelFilesConfig{Sources: map[string]config.ModelFileSourceConfig{
		"new-models": {Type: config.ModelFileSourceDirectory, Path: newRoot},
	}}}
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() {
		_ = s.Shutdown(time.Second)
		_ = s.store.Close()
	})
	s.setConfig(active)

	modelFiles, err := modelmanager.New(active.ModelFiles.Effective())
	if err != nil {
		t.Fatal(err)
	}
	downloads, err := modeldownload.New(modeldownload.Config{
		Store: s.store, Sources: modelFiles, Settings: active.ModelFiles.Effective().Downloads,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := downloads.Start(s.shutdownCtx); err != nil {
		t.Fatal(err)
	}
	s.modelFiles = modelFiles
	s.downloads = downloads

	if err := s.ReconcileConfig(desired); err != nil {
		t.Fatalf("ReconcileConfig() error = %v", err)
	}
	response := httptest.NewRecorder()
	s.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/model-files?source=new-models", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("model file response status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"id":"new-models"`) || !strings.Contains(response.Body.String(), newRoot) {
		t.Fatalf("model file source was not reloaded: %s", response.Body.String())
	}
	if strings.Contains(response.Body.String(), `"id":"old-models"`) {
		t.Fatalf("stale model file source remained after reload: %s", response.Body.String())
	}
}

func TestServer_ReconcileConfigRollsBackLocalWhenPeerRejects(t *testing.T) {
	oldConfig := config.Config{LogLevel: "old"}
	candidate := config.Config{LogLevel: "new"}
	local := &reconfigurableStubRouter{stubRouter: newStubRouter(nil, "")}
	peer := &reconfigurableStubRouter{
		stubRouter:     newStubRouter(nil, ""),
		reconfigureErr: errors.New("peer topology rejected candidate"),
	}
	s := newTestServer(local, peer)
	t.Cleanup(func() { _ = s.store.Close() })
	s.setConfig(oldConfig)

	err := s.ReconcileConfig(candidate)
	if !errors.Is(err, peer.reconfigureErr) {
		t.Fatalf("ReconcileConfig error=%v want peer error", err)
	}
	if got := s.currentConfig(); got.LogLevel != oldConfig.LogLevel {
		t.Fatalf("server config=%q want active %q after rejection", got.LogLevel, oldConfig.LogLevel)
	}
	if len(local.reconfigured) != 2 {
		t.Fatalf("local Reconfigure calls=%d want candidate then rollback", len(local.reconfigured))
	}
	if got := local.reconfigured[0].LogLevel; got != candidate.LogLevel {
		t.Fatalf("first local Reconfigure=%q want candidate %q", got, candidate.LogLevel)
	}
	if got := local.reconfigured[1].LogLevel; got != oldConfig.LogLevel {
		t.Fatalf("rollback local Reconfigure=%q want active %q", got, oldConfig.LogLevel)
	}
	if len(peer.reconfigured) != 1 || peer.reconfigured[0].LogLevel != candidate.LogLevel {
		t.Fatalf("peer Reconfigure calls=%v want candidate once", peer.reconfigured)
	}
}

func TestServer_ReconcileConfigRollsBackRuntimeDefinitionsWhenPeerRejects(t *testing.T) {
	root := t.TempDir()
	oldConfig := config.Config{
		RuntimeManager: config.RuntimeManagerConfig{Root: root},
		Runtimes: map[string]config.RuntimeConfig{
			"vllm": {Kind: "vllm", Source: config.RuntimeSource{Type: "pypi"}, Update: config.RuntimeUpdateConfig{Version: "0.1.0"}},
		},
	}
	candidate := oldConfig
	candidate.Runtimes = map[string]config.RuntimeConfig{
		"vllm": {Kind: "vllm", Source: config.RuntimeSource{Type: "pypi"}, Update: config.RuntimeUpdateConfig{Version: "0.2.0"}},
	}
	local := &reconfigurableStubRouter{stubRouter: newStubRouter(nil, "")}
	peer := &reconfigurableStubRouter{
		stubRouter:     newStubRouter(nil, ""),
		reconfigureErr: errors.New("peer topology rejected candidate"),
	}
	s := newTestServer(local, peer)
	t.Cleanup(func() { _ = s.store.Close() })
	manager, err := runtimeManager.NewManager(root, runtimeProvidersForConfig(oldConfig))
	if err != nil {
		t.Fatal(err)
	}
	s.runtime = manager
	s.setConfig(oldConfig)
	if err := s.syncManagedRuntimeDefinitions(config.Config{}, oldConfig); err != nil {
		t.Fatal(err)
	}

	err = s.ReconcileConfig(candidate)
	if !errors.Is(err, peer.reconfigureErr) {
		t.Fatalf("ReconcileConfig error=%v want peer error", err)
	}
	if flushErr := manager.FlushControlUpdate(); flushErr != nil {
		t.Fatal(flushErr)
	}
	definition, ok := manager.Definition("vllm")
	if !ok || definition.Spec.Version != "0.1.0" {
		t.Fatalf("runtime definition after rollback = %+v, found=%v", definition, ok)
	}
}

func TestConfigReconciler_RestartWaitsForInFlightApply(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	restartCalled := make(chan struct{})
	reconciler := NewConfigReconciler(config.Config{LogLevel: "initial"}, func(_, desired config.Config, _ config.ConfigChangeSet) error {
		if desired.LogLevel == "next" {
			close(started)
			<-release
		}
		return nil
	}, func(string) error {
		close(restartCalled)
		return nil
	})

	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- reconciler.Reconcile(config.Config{LogLevel: "next"}) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("config apply did not start")
	}

	restartDone := make(chan error, 1)
	go func() { restartDone <- reconciler.RestartModel("model") }()
	select {
	case <-restartCalled:
		t.Fatal("restart confirmation raced an in-flight config apply")
	default:
	}
	close(release)
	if err := <-reconcileDone; err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if err := <-restartDone; err != nil {
		t.Fatalf("RestartModel: %v", err)
	}
}

func TestConfigReconciler_ForceRestartCancelsAfterRestartAccepted(t *testing.T) {
	var events []string
	reconciler := NewConfigReconciler(config.Config{}, nil, func(modelID string) error {
		events = append(events, "restart:"+modelID)
		return nil
	})

	if err := reconciler.ForceRestartModel("model", func(modelID string) {
		events = append(events, "cancel:"+modelID)
	}); err != nil {
		t.Fatalf("ForceRestartModel: %v", err)
	}
	want := []string{"restart:model", "cancel:model"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v want %v", events, want)
	}
}

// TestServer_ReconcileConfigGatesHotAddedLMCacheModel pins the decoration
// ordering in applyConfigCandidate: the router creates the process for a
// hot-added model during Reconfigure, while the previous configuration is
// still the current one, so a config-reading decorator (the LMCache
// pre-start gate) must be re-run after the candidate is committed or the
// model starts without its dependency gate.
func TestServer_ReconcileConfigGatesHotAddedLMCacheModel(t *testing.T) {
	log := logmon.NewWriter(io.Discard)
	root := t.TempDir()
	runtimes := map[string]config.RuntimeConfig{
		"vllm-e2e": {
			Kind:   "vllm",
			Source: config.RuntimeSource{Type: "pypi"},
			Update: config.RuntimeUpdateConfig{Version: "0.1.0"},
		},
	}
	// The daemon is up with an empty catalog.
	active := config.Config{
		HealthCheckTimeout: 5,
		RuntimeManager:     config.RuntimeManagerConfig{Root: root},
		Runtimes:           runtimes,
	}
	local, err := router.NewGroup(active, log, log)
	if err != nil {
		t.Fatalf("NewGroup() error = %v", err)
	}
	t.Cleanup(func() { _ = local.Shutdown(time.Second) })

	s := newTestServer(local, newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	manager, err := runtimeManager.NewManager(root, runtimeProvidersForConfig(active))
	if err != nil {
		t.Fatal(err)
	}
	s.runtime = manager
	s.lmcacheMod = newLMCacheService(s)
	s.setConfig(active)
	s.installModelTracking()

	// The LMCache-enabled model joins the catalog while the daemon runs. The
	// LMCache server stays disabled, so a model whose pre-start gate is
	// installed must fail closed with the gate error before its engine
	// command executes.
	desired := config.Config{
		HealthCheckTimeout: 5,
		RuntimeManager:     active.RuntimeManager,
		Runtimes:           runtimes,
		Models: map[string]config.ModelConfig{
			"lm-e2e": {
				Backend: config.BackendConfig{
					Type:      "vllm",
					Runtime:   "vllm-e2e",
					Launch:    &config.ModelLaunchConfig{Model: "e2e-model"},
					Arguments: []string{"--enforce-eager"},
					LMCache:   &config.LMCacheModelConfig{Enabled: true},
				},
			},
		},
	}
	desired.Routing.Router.Settings.Groups = map[string]config.GroupConfig{
		config.DEFAULT_GROUP_ID: {Swap: true, Exclusive: true, Members: []string{"lm-e2e"}},
	}
	if err := s.ReconcileConfig(desired); err != nil {
		t.Fatalf("ReconcileConfig() error = %v", err)
	}

	body := `{"model":"lm-e2e"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	local.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500 (the gate must reject the start), body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "the LMCache server is disabled") {
		t.Fatalf("hot-added model started without the LMCache pre-start gate: body=%s", w.Body.String())
	}
}

// A pending daemon restart (store path, runtime manager root, logToStdout)
// keeps the effective snapshot at the active values. Reconciling unrelated
// edits must not replay the apply callback just because the raw desired
// snapshot differs from active by those pending keys.
func TestConfigReconciler_DaemonRestartPendingDoesNotReplayApply(t *testing.T) {
	base := config.Config{
		Store:  &config.Store{Path: "/old/store"},
		Models: map[string]config.ModelConfig{"a": {Cmd: "old"}},
	}

	var mu sync.Mutex
	applyCalls := 0
	reconciler := NewConfigReconciler(base, func(_, _ config.Config, _ config.ConfigChangeSet) error {
		mu.Lock()
		applyCalls++
		mu.Unlock()
		return nil
	}, nil)

	// 1. A candidate that only changes the daemon-restart key: nothing is
	// hot-applicable, so the apply callback is skipped and the pending
	// restart is reported through the status.
	storeOnly := base
	storeOnly.Store = &config.Store{Path: "/new/store"}
	if err := reconciler.Reconcile(storeOnly); err != nil {
		t.Fatalf("store-only reconcile: %v", err)
	}
	mu.Lock()
	calls := applyCalls
	mu.Unlock()
	if calls != 0 {
		t.Fatalf("apply calls after store-only change = %d, want 0", calls)
	}
	status := reconciler.Status()
	if !status.RestartRequired || len(status.RestartPaths) != 1 || status.RestartPaths[0] != "/store/path" {
		t.Fatalf("status=%+v, want a pending /store/path restart", status)
	}

	// 2. An unrelated model edit on top: the hot-applicable change must be
	// applied exactly once.
	modelEdit := storeOnly
	modelEdit.Models = map[string]config.ModelConfig{"a": {Cmd: "new"}}
	if err := reconciler.Reconcile(modelEdit); err != nil {
		t.Fatalf("model-edit reconcile: %v", err)
	}
	mu.Lock()
	calls = applyCalls
	mu.Unlock()
	if calls != 1 {
		t.Fatalf("apply calls after model edit = %d, want 1", calls)
	}

	// 3. Reconciling the identical candidate again (the file-watcher echo):
	// the pending daemon restart must not replay the callback.
	if err := reconciler.Reconcile(modelEdit); err != nil {
		t.Fatalf("echo reconcile: %v", err)
	}
	mu.Lock()
	calls = applyCalls
	mu.Unlock()
	if calls != 1 {
		t.Fatalf("apply calls after echo reconcile = %d, want 1", calls)
	}
}
