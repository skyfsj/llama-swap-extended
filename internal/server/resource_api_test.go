package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/backend"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
	resourcePlanner "github.com/mostlygeek/llama-swap/internal/resource"
	"github.com/stretchr/testify/require"
)

type resourceSleepAdapter struct {
	sleeping  bool
	cacheErr  error
	sleepCall atomic.Int32
}

func (a *resourceSleepAdapter) Name() string { return "resource-test" }
func (a *resourceSleepAdapter) Capabilities(context.Context) (backend.CapabilitySet, error) {
	return backend.CapabilitySet{"sleep": true}, nil
}
func (a *resourceSleepAdapter) TransformRequest(context.Context, string, backend.RequestTransform) (backend.RequestTransform, error) {
	return backend.RequestTransform{}, nil
}
func (a *resourceSleepAdapter) TransformResponse(context.Context, string, []byte, http.Header) ([]byte, error) {
	return nil, nil
}
func (a *resourceSleepAdapter) CacheState(context.Context) (backend.CacheState, error) {
	return backend.CacheState{Supported: true, Sleeping: a.sleeping}, a.cacheErr
}
func (a *resourceSleepAdapter) ResetCache(context.Context) error { return backend.ErrUnsupported }
func (a *resourceSleepAdapter) Sleep(context.Context, int) error {
	a.sleepCall.Add(1)
	return nil
}
func (a *resourceSleepAdapter) Wake(context.Context) error { return backend.ErrUnsupported }
func (a *resourceSleepAdapter) Progress(context.Context) (backend.Progress, error) {
	return backend.Progress{Phase: "idle", Completed: 1, Total: 1}, nil
}

func TestServer_ResourceAPIReportsBudgetAndSafeCandidates(t *testing.T) {
	local := newStubRouter([]string{"busy", "idle"}, "")
	local.running = map[string]process.ProcessState{"busy": process.StateReady, "idle": process.StateReady}
	s := newTestServer(local, newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	s.cfg.Models = map[string]config.ModelConfig{
		"busy": {Backend: config.BackendConfig{Resources: config.ResourceConfig{VRAMMiB: 70, RAMMiB: 20, EvictionPriority: 10}}},
		"idle": {Backend: config.BackendConfig{Resources: config.ResourceConfig{VRAMMiB: 20, RAMMiB: 10, EvictionPriority: 1}}},
	}
	s.resources = resourcePlanner.NewPlanner(resourcePlanner.Budget{VRAMMiB: 100, RAMMiB: 100})

	recorder := httptest.NewRecorder()
	s.handleAPIResources(recorder, httptest.NewRequest(http.MethodGet, "/api/resources", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	var payload resourceStatus
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
	require.True(t, payload.Enabled)
	require.Equal(t, 90, payload.UsageVRAM)
	require.Equal(t, []string{"idle", "busy"}, payload.Candidates)
}

func TestServer_ReconcileConfigUpdatesResourceBudgetInPlace(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	old := config.Config{ResourceBudget: config.ResourceBudgetConfig{VRAMMiB: 100}}
	newConfig := config.Config{ResourceBudget: config.ResourceBudgetConfig{VRAMMiB: 70}}
	s.cfg = old
	planner := resourcePlanner.NewPlanner(resourcePlanner.Budget{VRAMMiB: 100})
	planner.Upsert(resourcePlanner.Model{ID: "warm", Loaded: true, Footprint: resourcePlanner.Footprint{VRAMMiB: 60}})
	s.resources = planner

	if err := s.ReconcileConfig(newConfig); err != nil {
		t.Fatalf("ReconcileConfig: %v", err)
	}
	if got := s.resourcePlanner(); got != planner {
		t.Fatal("resource planner was replaced during an online budget update")
	}
	if got := planner.Budget().VRAMMiB; got != 70 {
		t.Fatalf("budget=%d want 70", got)
	}
	if got, _ := planner.Usage(); got != 60 {
		t.Fatalf("usage=%d want preserved 60", got)
	}
}

func TestServer_ResourceProjectionNormalizesMalformedFootprint(t *testing.T) {
	footprint := resourceFootprint(config.ResourceConfig{VRAMMiB: -10, RAMMiB: -20})
	if footprint.VRAMMiB != 0 || footprint.RAMMiB != 0 {
		t.Fatalf("normalized footprint=%+v", footprint)
	}
	maxInt := int(^uint(0) >> 1)
	if got := saturatingResourceAdd(maxInt-1, 10); got != maxInt {
		t.Fatalf("saturating resource usage=%d", got)
	}
}

func TestServer_ReleaseResourceCandidateToleratesDetachedRouter(t *testing.T) {
	// Resource admission can be exercised by embedders before the local router
	// is attached. A rejected candidate must remain an explicit budget result,
	// not become a nil-router panic while attempting the conservative unload
	// fallback.
	s := &Server{cfg: config.Config{Models: map[string]config.ModelConfig{
		"model": {},
	}}}
	s.releaseResourceCandidate("model")
	s.releaseResourceCandidate("missing")
}

func TestServer_ReleaseResourceCandidateReportsUnloadFailure(t *testing.T) {
	local := newStubRouter([]string{"model"}, "")
	local.running = map[string]process.ProcessState{"model": process.StateReady}
	s := newTestServer(local, newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	s.cfg.Models = map[string]config.ModelConfig{
		"model": {Backend: config.BackendConfig{Resources: config.ResourceConfig{VRAMMiB: 80}}},
	}
	if err := s.releaseResourceCandidate("model"); err == nil || !strings.Contains(err.Error(), "unload left process") {
		t.Fatalf("unload failure = %v, want explicit postcondition error", err)
	}
}

func TestServer_ReleaseResourceCandidateVerifiesSleepState(t *testing.T) {
	local := newStubRouter([]string{"model"}, "")
	local.running = map[string]process.ProcessState{"model": process.StateReady}
	adapter := &resourceSleepAdapter{sleeping: true}
	s := newTestServer(local, newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	s.cfg.Models = map[string]config.ModelConfig{
		"model": {Backend: config.BackendConfig{Lifecycle: config.LifecycleConfig{Mode: "sleep"}}},
	}
	s.backendAdapters = map[string]backend.BackendAdapter{"model": adapter}

	if err := s.releaseResourceCandidate("model"); err != nil {
		t.Fatalf("verified sleep eviction failed: %v", err)
	}
	if got := adapter.sleepCall.Load(); got != 1 {
		t.Fatalf("sleep calls=%d, want 1", got)
	}
	if got := local.unloadCalls.Load(); got != 0 {
		t.Fatalf("verified sleep unexpectedly unloaded process: %d", got)
	}
}

func TestServer_ReleaseResourceCandidateFallsBackWhenSleepStateIsUnconfirmed(t *testing.T) {
	local := newStubRouter([]string{"model"}, "")
	local.runningFunc = func() map[string]process.ProcessState {
		if local.unloadCalls.Load() > 0 {
			return map[string]process.ProcessState{}
		}
		return map[string]process.ProcessState{"model": process.StateReady}
	}
	adapter := &resourceSleepAdapter{}
	s := newTestServer(local, newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	s.cfg.Models = map[string]config.ModelConfig{
		"model": {Backend: config.BackendConfig{Lifecycle: config.LifecycleConfig{Mode: "sleep"}}},
	}
	s.backendAdapters = map[string]backend.BackendAdapter{"model": adapter}

	if err := s.releaseResourceCandidate("model"); err != nil {
		t.Fatalf("fallback unload failed: %v", err)
	}
	if got := adapter.sleepCall.Load(); got != 1 {
		t.Fatalf("sleep calls=%d, want 1", got)
	}
	if got := local.unloadCalls.Load(); got != 1 {
		t.Fatalf("fallback unload calls=%d, want 1", got)
	}
}

func TestServer_ResourceAdmissionReportsEvictionFailure(t *testing.T) {
	local := newStubRouter([]string{"busy", "requested"}, "")
	local.running = map[string]process.ProcessState{"busy": process.StateReady}
	s := newTestServer(local, newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	s.cfg.ResourceBudget = config.ResourceBudgetConfig{VRAMMiB: 100, AutoEvict: true}
	s.cfg.Models = map[string]config.ModelConfig{
		"busy":      {Backend: config.BackendConfig{Resources: config.ResourceConfig{VRAMMiB: 80}}},
		"requested": {Backend: config.BackendConfig{Resources: config.ResourceConfig{VRAMMiB: 80}}},
	}
	s.resources = resourcePlanner.NewPlanner(resourcePlanner.Budget{VRAMMiB: 100})
	err := s.prepareResourceLoadContext(context.Background(), "requested")
	if err == nil || !strings.Contains(err.Error(), "after eviction attempts") || !strings.Contains(err.Error(), "unload left process") {
		t.Fatalf("resource admission error = %v, want explicit eviction failure", err)
	}
}

func TestServer_ResourceAPIIsDisabledWithoutBudget(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	recorder := httptest.NewRecorder()
	s.handleAPIResources(recorder, httptest.NewRequest(http.MethodGet, "/api/resources", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.JSONEq(t, `{"enabled":false,"models":[]}`, recorder.Body.String())
}

func TestServer_ResourceQueueLoadsWaitsForExternalRelease(t *testing.T) {
	local := newStubRouter([]string{"busy", "requested"}, "")
	var released atomic.Bool
	local.runningFunc = func() map[string]process.ProcessState {
		if released.Load() {
			return map[string]process.ProcessState{"requested": process.StateStarting}
		}
		return map[string]process.ProcessState{"busy": process.StateReady}
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	s.cfg.HealthCheckTimeout = 1
	s.cfg.ResourceBudget = config.ResourceBudgetConfig{VRAMMiB: 100, QueueLoads: true}
	s.cfg.Models = map[string]config.ModelConfig{
		"busy":      {Backend: config.BackendConfig{Resources: config.ResourceConfig{VRAMMiB: 80}}},
		"requested": {Backend: config.BackendConfig{Resources: config.ResourceConfig{VRAMMiB: 80}}},
	}
	s.resources = resourcePlanner.NewPlanner(resourcePlanner.Budget{VRAMMiB: 100})

	go func() {
		time.Sleep(150 * time.Millisecond)
		released.Store(true)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.prepareResourceLoadContext(ctx, "requested"); err != nil {
		t.Fatalf("queued resource admission failed: %v", err)
	}
}

func TestServer_ResourceQueueLoadsHonorsCancellation(t *testing.T) {
	local := newStubRouter([]string{"busy", "requested"}, "")
	local.running = map[string]process.ProcessState{"busy": process.StateReady}
	s := newTestServer(local, newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	s.cfg.HealthCheckTimeout = 5
	s.cfg.ResourceBudget = config.ResourceBudgetConfig{VRAMMiB: 100, QueueLoads: true}
	s.cfg.Models = map[string]config.ModelConfig{
		"busy":      {Backend: config.BackendConfig{Resources: config.ResourceConfig{VRAMMiB: 80}}},
		"requested": {Backend: config.BackendConfig{Resources: config.ResourceConfig{VRAMMiB: 80}}},
	}
	s.resources = resourcePlanner.NewPlanner(resourcePlanner.Budget{VRAMMiB: 100})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.prepareResourceLoadContext(ctx, "requested"); err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("canceled resource admission error = %v", err)
	}
}

func TestServer_ResourceAdmissionProtectsStartingAndStoppingProcesses(t *testing.T) {
	local := newStubRouter([]string{"transitioning", "requested"}, "")
	local.running = map[string]process.ProcessState{
		"transitioning": process.StateStarting,
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	s.cfg.ResourceBudget = config.ResourceBudgetConfig{VRAMMiB: 100, AutoEvict: true}
	s.cfg.Models = map[string]config.ModelConfig{
		"transitioning": {Backend: config.BackendConfig{Resources: config.ResourceConfig{VRAMMiB: 80}}},
		"requested":     {Backend: config.BackendConfig{Resources: config.ResourceConfig{VRAMMiB: 80}}},
	}
	s.resources = resourcePlanner.NewPlanner(resourcePlanner.Budget{VRAMMiB: 100})

	if err := s.prepareResourceLoadContext(context.Background(), "requested"); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("starting process should remain protected, err=%v", err)
	}
	if got := local.unloadCalls.Load(); got != 0 {
		t.Fatalf("starting process was unloaded during admission: %d calls", got)
	}
}

func TestServer_ResourceReservationSerializesConcurrentColdLoads(t *testing.T) {
	local := newStubRouter([]string{"first", "second"}, "")
	local.running = map[string]process.ProcessState{}
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	local.serveHTTP = func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		w.WriteHeader(http.StatusOK)
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	s.cfg.Models = map[string]config.ModelConfig{
		"first":  {Backend: config.BackendConfig{Resources: config.ResourceConfig{VRAMMiB: 80}}},
		"second": {Backend: config.BackendConfig{Resources: config.ResourceConfig{VRAMMiB: 80}}},
	}
	s.resources = resourcePlanner.NewPlanner(resourcePlanner.Budget{VRAMMiB: 100})

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		s.localPeerHandler(recorder, chatRequest("first"))
		firstDone <- recorder
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first request did not reach the local router")
	}

	secondDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		s.localPeerHandler(recorder, chatRequest("second"))
		secondDone <- recorder
	}()
	var second *httptest.ResponseRecorder
	select {
	case second = <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("second request waited instead of returning a resource error")
	}
	if second.Code != http.StatusInsufficientStorage {
		t.Fatalf("second cold load status=%d body=%s, want %d", second.Code, second.Body.String(), http.StatusInsufficientStorage)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("second request reached local router despite reservation: calls=%d", got)
	}

	close(release)
	select {
	case first := <-firstDone:
		if first.Code != http.StatusOK {
			t.Fatalf("first request status=%d body=%s", first.Code, first.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("first request did not finish after release")
	}
	if got, _ := s.resources.Usage(); got != 0 {
		t.Fatalf("resource reservation leaked after dispatch: usage=%d", got)
	}
}
