package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/backend"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	resourcePlanner "github.com/mostlygeek/llama-swap/internal/resource"
	"github.com/mostlygeek/llama-swap/internal/route"
	"github.com/mostlygeek/llama-swap/internal/router"
	runtimeManager "github.com/mostlygeek/llama-swap/internal/runtime"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// stubRouter is a minimal router.LocalRouter for Server dispatch tests.
type stubRouter struct {
	models        map[string]bool
	response      string
	serveHTTP     func(http.ResponseWriter, *http.Request)
	shutdownCalls atomic.Int32
	running       map[string]process.ProcessState
	runningFunc   func() map[string]process.ProcessState
	unloadCalls   atomic.Int32
	unloadModels  []string
	unloadTimeout time.Duration
	loggers       map[string]*logmon.Monitor
	loggersMu     sync.Mutex
}

func newStubRouter(models []string, response string) *stubRouter {
	m := make(map[string]bool, len(models))
	for _, id := range models {
		m[id] = true
	}
	return &stubRouter{models: m, response: response}
}

func (s *stubRouter) Handles(model string) bool      { return s.models[model] }
func (s *stubRouter) Shutdown(_ time.Duration) error { s.shutdownCalls.Add(1); return nil }
func (s *stubRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.serveHTTP != nil {
		s.serveHTTP(w, r)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(s.response))
}

func (s *stubRouter) RunningModels() map[string]process.ProcessState {
	if s.runningFunc != nil {
		return s.runningFunc()
	}
	return s.running
}
func (s *stubRouter) Unload(timeout time.Duration, models ...string) {
	s.unloadCalls.Add(1)
	s.unloadTimeout = timeout
	s.unloadModels = append([]string(nil), models...)
}
func (s *stubRouter) ProcessLogger(modelID string) (*logmon.Monitor, bool) {
	s.loggersMu.Lock()
	defer s.loggersMu.Unlock()
	if s.loggers != nil {
		if lg, ok := s.loggers[modelID]; ok {
			return lg, true
		}
	}
	return nil, false
}

// setModelLogger installs the monitor that ProcessLogger resolves for a
// model. Safe to call while a log stream re-resolves its monitor in the
// background.
func (s *stubRouter) setModelLogger(modelID string, monitor *logmon.Monitor) {
	s.loggersMu.Lock()
	defer s.loggersMu.Unlock()
	if s.loggers == nil {
		s.loggers = make(map[string]*logmon.Monitor)
	}
	s.loggers[modelID] = monitor
}

// newTestServer wires a Server with stub routers and a built mux.
func newTestServer(local router.LocalRouter, peer router.Router) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	proxylog := logmon.NewWriter(io.Discard)
	st, err := store.New("")
	if err != nil {
		panic(err)
	}
	s := &Server{
		cfg:         config.Config{},
		muxlog:      logmon.NewWriter(io.Discard),
		proxylog:    proxylog,
		upstreamlog: logmon.NewWriter(io.Discard),
		inflight:    newInflightTracker(),
		metrics:     newMetricsMonitor(proxylog, 0, st),
		store:       st,
		local:       local,
		peer:        peer,
		shutdownCtx: ctx,
		shutdownFn:  cancel,
	}
	s.routes()
	return s
}

func TestServer_ProcessStatePublishesBackendProgress(t *testing.T) {
	events := make(chan swaputil.BackendProgressEvent, 8)
	cancel := event.On(func(progress swaputil.BackendProgressEvent) {
		select {
		case events <- progress:
		default:
		}
	})
	defer cancel()

	s := &Server{}
	cases := []struct {
		state    process.ProcessState
		phase    string
		progress float64
		err      string
	}{
		{state: process.StateStarting, phase: "loading"},
		{state: process.StateReady, phase: "active", progress: 1},
		{state: process.StateSleeping, phase: "sleeping", progress: 1},
		{state: process.StateStopping, phase: "unloading"},
		{state: process.StateStopped, phase: "idle", progress: 1},
		{state: process.StateShutdown, phase: "error", err: "model process entered shutdown state"},
	}
	for i, tc := range cases {
		model := fmt.Sprintf("process-progress-%d", i)
		s.recordProcessProgress(swaputil.ProcessStateChangeEvent{ProcessName: model, NewState: string(tc.state)})

		var got swaputil.BackendProgressEvent
		deadline := time.NewTimer(time.Second)
		matched := false
		for !matched {
			select {
			case got = <-events:
				matched = got.Model == model
			case <-deadline.C:
				t.Fatalf("timed out waiting for progress event for %s", model)
			}
		}
		if !deadline.Stop() {
			select {
			case <-deadline.C:
			default:
			}
		}
		if got.Phase != tc.phase || got.Progress != tc.progress {
			t.Fatalf("state %s progress = %+v, want phase=%q progress=%v", tc.state, got, tc.phase, tc.progress)
		}
		if got.Error != tc.err {
			t.Fatalf("state %s error = %q, want %q", tc.state, got.Error, tc.err)
		}
	}
}

func TestServer_AuditMaintenancePurgesExpiredAndOverBudget(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	now := time.Now().Truncate(time.Second)
	for _, conversation := range []store.AuditConversation{
		{ID: "expired", Model: "m", Timestamp: now.Add(-2 * time.Hour), RequestBody: []byte("old")},
		{ID: "middle", Model: "m", Timestamp: now.Add(-30 * time.Minute), RequestBody: []byte("middle")},
		{ID: "newest", Model: "m", Timestamp: now.Add(-5 * time.Minute), RequestBody: []byte("newest")},
	} {
		if err := st.InsertAuditConversation(context.Background(), conversation); err != nil {
			t.Fatalf("InsertAuditConversation(%s): %v", conversation.ID, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{
		cfg:   config.Config{Audit: config.AuditConfig{Enabled: true, Retention: time.Hour, MaxBytes: 4}},
		store: st, shutdownCtx: ctx,
	}
	s.maintainAudit()
	rows, err := st.ListAuditConversations(context.Background(), store.AuditQuery{Limit: 10})
	if err != nil {
		t.Fatalf("ListAuditConversations: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "newest" {
		t.Fatalf("audit rows after cleanup = %+v, want only newest row", rows)
	}
}

func countFiles(t *testing.T, root string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return n
}

// Blob GC is rate-limited: the first maintenance pass reclaims files whose
// rows are gone, a second pass inside the interval is a no-op, and a pass
// after the interval picks up the new orphans.
func TestServer_AuditMaintenanceReclaimsOrphanBlobs(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	now := time.Now().Truncate(time.Second)
	for _, id := range []string{"first", "second"} {
		if err := st.InsertAuditConversation(context.Background(), store.AuditConversation{
			ID: id, Model: "m", Timestamp: now, RequestBody: []byte("media-" + id),
		}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{
		cfg:   config.Config{Audit: config.AuditConfig{Enabled: true}},
		store: st, shutdownCtx: ctx,
	}
	blobs := filepath.Join(dir, "blobs")

	// First pass reclaims nothing (both rows are live) and arms the gate.
	s.gcAuditBlobs(ctx)
	if got := countFiles(t, blobs); got != 2 {
		t.Fatalf("files after first pass=%d, want 2 (rows still live)", got)
	}
	// Deleting a row and re-running inside the interval must be a no-op:
	// the rate limit keeps a burst of deletes from turning into walks.
	if err := st.DeleteAuditConversation(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	s.gcAuditBlobs(ctx)
	if got := countFiles(t, blobs); got != 2 {
		t.Fatalf("files inside GC interval=%d, want 2 (rate-limited)", got)
	}
	// After the interval the orphan is reclaimed, referenced files survive.
	// Backdate the orphan past the blob grace period: it was written moments
	// ago, so GC would otherwise treat it as possibly still-uncommitted.
	orphan := sha256.Sum256([]byte("media-first"))
	orphanPath := filepath.Join(blobs, hex.EncodeToString(orphan[:])[:2], hex.EncodeToString(orphan[:]))
	stale := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(orphanPath, stale, stale); err != nil {
		t.Fatal(err)
	}
	s.auditLastGC = time.Now().Add(-time.Hour)
	s.gcAuditBlobs(ctx)
	if got := countFiles(t, blobs); got != 1 {
		t.Fatalf("files after GC=%d, want 1 (only second row's blob)", got)
	}
	full, found, err := st.GetAuditConversation(ctx, "second")
	if err != nil || !found {
		t.Fatalf("surviving row after GC found=%v err=%v", found, err)
	}
	file, _, err := st.OpenBlob(full.RequestBodyRef)
	if err != nil {
		t.Fatalf("surviving row body is unreadable after GC: %v", err)
	}
	defer file.Close()
	body, err := io.ReadAll(file)
	if err != nil {
		t.Fatalf("read surviving row body: %v", err)
	}
	if string(body) != "media-second" {
		t.Fatalf("surviving row body=%q, want %q", body, "media-second")
	}
}

func newTestMetricsMonitor(t *testing.T, logger *logmon.Monitor, maxMetrics int) *metricsMonitor {
	t.Helper()
	st, err := store.New("")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	return newMetricsMonitor(logger, maxMetrics, st)
}

func metricsEntries(t *testing.T, mm *metricsMonitor) []ActivityLogEntry {
	t.Helper()
	page, err := mm.store.ListActivity(context.Background(), store.ActivityQuery{Limit: 1000, Page: 1})
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	return page.Data
}

func chatRequest(model string) *http.Request {
	body := strings.NewReader(`{"model":"` + model + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestServer_ModelRouteListsIncludeRegistryDescriptors(t *testing.T) {
	registry, err := route.NewRegistry(route.DefaultDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	contains := func(paths []string, target string) bool {
		for _, path := range paths {
			if path == target {
				return true
			}
		}
		return false
	}
	for _, descriptor := range registry.List() {
		if descriptor.ModelField == "" || strings.Contains(descriptor.Pattern, "{") {
			continue
		}
		var paths []string
		switch {
		case descriptor.Method == http.MethodPost && descriptor.Body == route.BodyJSON && descriptor.ModelLocation == "body":
			paths = modelPostJSONRoutes
		case descriptor.Method == http.MethodPost && descriptor.Body == route.BodyForm && descriptor.ModelLocation == "form":
			paths = modelPostFormRoutes
		case descriptor.Method == http.MethodGet && descriptor.Body == route.BodyQuery && descriptor.ModelLocation == "query":
			paths = modelGetRoutes
		default:
			continue
		}
		if !contains(paths, descriptor.Pattern) {
			t.Errorf("registry descriptor %s %s is missing from model route list", descriptor.Method, descriptor.Pattern)
		}
	}
}

func TestServer_NewModelBackendAdapterRegistersExplicitGenericBackend(t *testing.T) {
	adapter, err := newModelBackendAdapter("generic", config.ModelConfig{Backend: config.BackendConfig{
		Type: "generic",
		APIs: []string{"chat", "*"},
	}})
	if err != nil {
		t.Fatalf("newModelBackendAdapter: %v", err)
	}
	if backend.IsNilAdapter(adapter) {
		t.Fatal("explicit generic backend returned no adapter")
	}
	caps, err := adapter.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("generic capabilities: %v", err)
	}
	for _, capability := range []string{"chat", "responses", "realtime", "anthropic"} {
		if !caps.Has(capability) {
			t.Fatalf("generic capabilities missing %q: %#v", capability, caps)
		}
	}
}

func TestServer_NewModelBackendAdapterKeepsLegacyModelsUnregistered(t *testing.T) {
	adapter, err := newModelBackendAdapter("legacy", config.ModelConfig{Proxy: "http://127.0.0.1:8000"})
	if err != nil {
		t.Fatalf("newModelBackendAdapter: %v", err)
	}
	if !backend.IsNilAdapter(adapter) {
		t.Fatalf("legacy model unexpectedly received adapter %#v", adapter)
	}
}

// audioTaskRequest builds a JSON POST to the /audioapi/v1/tasks/run endpoint
// carrying the given model field.
func audioTaskRequest(model string) *http.Request {
	body := strings.NewReader(`{"model":"` + model + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/audioapi/v1/tasks/run", body)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestServer_New_GroupConfig(t *testing.T) {
	discard := logmon.NewWriter(io.Discard)
	cfg := config.Config{HealthCheckTimeout: 15}
	cfg.RuntimeManager.Root = t.TempDir()
	cfg.Routing.Router.Use = "group"
	st, err := store.New("")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()
	s, err := New(cfg, discard, discard, discard, nil, st, BuildInfo{}, nil)
	if err != nil {
		t.Fatalf("New (group): %v", err)
	}
	if _, ok := s.local.(*router.Group); !ok {
		t.Fatalf("localRouter=%T want *router.Group", s.local)
	}
	if s.runtime == nil {
		t.Fatal("runtime manager was not initialized without an enable flag")
	}
	if err := s.Shutdown(time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestServer_NewAlwaysRegistersBundledLlamaCPP(t *testing.T) {
	discard := logmon.NewWriter(io.Discard)
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "llama-server"), []byte("#!/bin/sh\nexit 0\n"), 0o750); err != nil {
		t.Fatal(err)
	}
	t.Setenv(bundledLlamaCPPPathEnv, source)

	cfg := config.Config{HealthCheckTimeout: 15}
	cfg.RuntimeManager.Root = t.TempDir()
	cfg.Routing.Router.Use = "group"
	st, err := store.New("")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()
	s, err := New(cfg, discard, discard, discard, nil, st, BuildInfo{}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = s.Shutdown(time.Second) }()

	status, found := s.runtime.Get("llamacpp")
	if !found || status.Current != "bundled" || status.State != runtimeManager.StateActive {
		t.Fatalf("bundled llama.cpp was not registered: found=%v status=%+v", found, status)
	}
}

func TestServer_New_MatrixConfig(t *testing.T) {
	discard := logmon.NewWriter(io.Discard)
	cfg := config.Config{HealthCheckTimeout: 15}
	cfg.RuntimeManager.Root = t.TempDir()
	cfg.Models = map[string]config.ModelConfig{
		"model": {
			Cmd:   "echo ready",
			Proxy: "http://localhost:8080",
		},
	}
	cfg.Routing.Router.Use = "matrix"
	cfg.Routing.Router.Settings.Matrix = &config.MatrixConfig{
		Sets: config.OrderedSets{{Name: "single", DSL: "model"}},
	}
	st, err := store.New("")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()
	s, err := New(cfg, discard, discard, discard, nil, st, BuildInfo{}, nil)
	if err != nil {
		t.Fatalf("New (matrix): %v", err)
	}
	if _, ok := s.local.(*router.Matrix); !ok {
		t.Fatalf("localRouter=%T want *router.Matrix", s.local)
	}
	if err := s.Shutdown(time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestServer_New_GpusConfig(t *testing.T) {
	discard := logmon.NewWriter(io.Discard)
	cfg := config.Config{HealthCheckTimeout: 15}
	cfg.RuntimeManager.Root = t.TempDir()
	cfg.Models = map[string]config.ModelConfig{
		"model": {
			Cmd:   "echo ready",
			Proxy: "http://localhost:8080",
		},
	}
	cfg.Routing.Router.Use = "gpus"
	cfg.Routing.Router.Settings.Gpus = &config.GpusConfig{
		Cards: map[string][]string{"0": {"model"}},
	}
	st, err := store.New("")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()
	s, err := New(cfg, discard, discard, discard, nil, st, BuildInfo{}, nil)
	if err != nil {
		t.Fatalf("New (gpus): %v", err)
	}
	if _, ok := s.local.(*router.Gpus); !ok {
		t.Fatalf("localRouter=%T want *router.Gpus", s.local)
	}
	if err := s.Shutdown(time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestServer_RouteToLocalModel(t *testing.T) {
	s := newTestServer(
		newStubRouter([]string{"local-model"}, "local response"),
		newStubRouter(nil, ""),
	)

	w := httptest.NewRecorder()
	s.ServeHTTP(w, chatRequest("local-model"))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if w.Body.String() != "local response" {
		t.Errorf("body=%q want %q", w.Body.String(), "local response")
	}
}

func TestServer_BackendProgressSnapshotKeepsLatestEvent(t *testing.T) {
	s := &Server{}
	s.recordBackendProgress(swaputil.BackendProgressEvent{Model: "m", Phase: "loading", Progress: 0.4})
	s.recordBackendProgress(swaputil.BackendProgressEvent{Model: "m", Phase: "active", Progress: 1})
	progress, ok := s.backendProgressSnapshot("m")
	if !ok {
		t.Fatal("backend progress snapshot was not recorded")
	}
	if progress.Phase != "active" || progress.Progress != 1 {
		t.Fatalf("latest progress = %+v", progress)
	}
}

func TestServer_RouteToPeerModel(t *testing.T) {
	s := newTestServer(
		newStubRouter(nil, ""),
		newStubRouter([]string{"peer-model"}, "peer response"),
	)

	w := httptest.NewRecorder()
	s.ServeHTTP(w, chatRequest("peer-model"))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if w.Body.String() != "peer response" {
		t.Errorf("body=%q want %q", w.Body.String(), "peer response")
	}
}

func TestServer_RouteToLocalModel_PrefersLocalCollision(t *testing.T) {
	s := newTestServer(
		newStubRouter([]string{"shared"}, "local response"),
		newStubRouter([]string{"shared"}, "peer response"),
	)

	w := httptest.NewRecorder()
	s.ServeHTTP(w, chatRequest("shared"))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if w.Body.String() != "local response" {
		t.Errorf("body=%q want local response", w.Body.String())
	}
}

func TestServer_UnknownModelReturns404(t *testing.T) {
	s := newTestServer(
		newStubRouter([]string{"local-model"}, ""),
		newStubRouter(nil, ""),
	)

	w := httptest.NewRecorder()
	s.ServeHTTP(w, chatRequest("unknown-model"))

	if w.Code != http.StatusNotFound {
		t.Errorf("status=%d want 404 body=%q", w.Code, w.Body.String())
	}
}

func TestServer_AudioAPIRoutesTaskRequest(t *testing.T) {
	s := newTestServer(
		newStubRouter([]string{"local-audio"}, "local audio response"),
		newStubRouter(nil, ""),
	)

	w := httptest.NewRecorder()
	s.ServeHTTP(w, audioTaskRequest("local-audio"))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if w.Body.String() != "local audio response" {
		t.Errorf("body=%q want %q", w.Body.String(), "local audio response")
	}
}

func TestServer_AudioAPIRewritesUpstreamPath(t *testing.T) {
	var gotPath string
	local := newStubRouter([]string{"m1"}, "")
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}
	s := newTestServer(local, newStubRouter(nil, ""))

	w := httptest.NewRecorder()
	s.ServeHTTP(w, audioTaskRequest("m1"))

	if gotPath != "/v1/tasks/run" {
		t.Errorf("upstream path = %q, want /v1/tasks/run", gotPath)
	}
}

func TestServer_UnknownPathReturns404(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/does-not-exist", nil))

	if w.Code != http.StatusNotFound {
		t.Errorf("status=%d want 404", w.Code)
	}
}

func TestServer_Health(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))

	for _, path := range []string{"/health", "/wol-health"} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK || w.Body.String() != "OK" {
			t.Errorf("%s: status=%d body=%q", path, w.Code, w.Body.String())
		}
	}
}

func TestServer_CORSPreflight(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))

	req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status=%d want 204", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin=%q want *", got)
	}
}

func TestServer_Unload(t *testing.T) {
	local := newStubRouter([]string{"m1"}, "")
	s := newTestServer(local, newStubRouter(nil, ""))

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/unload", nil))

	if w.Code != http.StatusOK || w.Body.String() != "OK" {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if got := local.unloadCalls.Load(); got != 1 {
		t.Errorf("unloadCalls=%d want 1", got)
	}
	if len(local.unloadModels) != 0 {
		t.Errorf("unloadModels=%v want empty for unload all", local.unloadModels)
	}
	if local.unloadTimeout != 0 {
		t.Errorf("unloadTimeout=%v want 0 (use configured timeouts)", local.unloadTimeout)
	}
}

func TestServer_Running(t *testing.T) {
	local := newStubRouter([]string{"m1"}, "")
	local.running = map[string]process.ProcessState{"m1": process.StateReady}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{
		"m1": {
			Cmd:         "llama-server",
			Proxy:       "http://localhost:9999",
			UnloadAfter: 300,
			Name:        "Model One",
			Description: "the first model",
		},
	}}

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/running", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}

	var resp struct {
		Running []runningModel `json:"running"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%q", err, w.Body.String())
	}
	if len(resp.Running) != 1 {
		t.Fatalf("running=%v want 1 entry", resp.Running)
	}
	want := runningModel{
		Model:       "m1",
		State:       "ready",
		Cmd:         "llama-server",
		Proxy:       "http://localhost:9999",
		TTL:         300,
		Name:        "Model One",
		Description: "the first model",
	}
	if resp.Running[0] != want {
		t.Errorf("got %+v want %+v", resp.Running[0], want)
	}
}

func TestServer_Preload(t *testing.T) {
	local := newStubRouter([]string{"m1"}, "ok")
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Hooks: config.HooksConfig{
		OnStartup: config.HookOnStartup{Preload: []string{"m1"}},
	}}

	got := make(chan swaputil.ModelPreloadedEvent, 1)
	cancel := event.On(func(e swaputil.ModelPreloadedEvent) { got <- e })
	defer cancel()

	s.startPreload()

	select {
	case e := <-got:
		if e.ModelName != "m1" || !e.Success {
			t.Errorf("event=%+v want {ModelName:m1 Success:true}", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("preload event not received")
	}
}

func TestServer_PreloadRespectsResourceBudget(t *testing.T) {
	local := newStubRouter([]string{"too-large"}, "ok")
	local.serveHTTP = func(w http.ResponseWriter, _ *http.Request) {
		t.Error("preload reached local router despite an unsatisfied resource budget")
		w.WriteHeader(http.StatusOK)
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{
		Models: map[string]config.ModelConfig{
			"too-large": {Backend: config.BackendConfig{Resources: config.ResourceConfig{VRAMMiB: 200}}},
		},
		ResourceBudget: config.ResourceBudgetConfig{VRAMMiB: 100},
		Hooks:          config.HooksConfig{OnStartup: config.HookOnStartup{Preload: []string{"too-large"}}},
	}
	s.resources = resourcePlanner.NewPlanner(resourcePlanner.Budget{VRAMMiB: 100})

	got := make(chan swaputil.ModelPreloadedEvent, 1)
	cancel := event.On(func(e swaputil.ModelPreloadedEvent) { got <- e })
	defer cancel()
	s.startPreload()

	select {
	case event := <-got:
		if event.ModelName != "too-large" || event.Success {
			t.Fatalf("preload event = %+v, want failed admission", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("preload admission event not received")
	}
	if got := local.unloadCalls.Load(); got != 0 {
		t.Fatalf("unexpected eviction while admitting oversized preload: %d", got)
	}
}

func TestServer_Shutdown_StopsRoutersAndIsIdempotent(t *testing.T) {
	local := newStubRouter([]string{"local-model"}, "")
	peer := newStubRouter(nil, "")
	s := newTestServer(local, peer)

	if err := s.Shutdown(time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := s.Shutdown(time.Second); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
	if got := local.shutdownCalls.Load(); got != 1 {
		t.Errorf("local shutdownCalls=%d want 1", got)
	}
	if got := peer.shutdownCalls.Load(); got != 1 {
		t.Errorf("peer shutdownCalls=%d want 1", got)
	}
}

func TestServer_LogStream_ModelID(t *testing.T) {
	buf := logmon.NewWriter(io.Discard)
	buf.Write([]byte("hello from model"))

	local := newStubRouter([]string{"mymodel"}, "")
	local.loggers = map[string]*logmon.Monitor{"mymodel": buf}

	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"mymodel": {}}}

	// Pre-cancel the context so the streaming loop exits immediately after
	// flushing history.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req := httptest.NewRequest(http.MethodGet, "/logs/stream/mymodel", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control=%q want no-store", got)
	}
	if got := w.Body.String(); got != "hello from model" {
		t.Errorf("body=%q want %q", got, "hello from model")
	}
}

func TestServer_LogStream_EmptyHistoryCommitsResponse(t *testing.T) {
	buf := logmon.NewWriter(io.Discard)
	local := newStubRouter([]string{"mymodel"}, "")
	local.loggers = map[string]*logmon.Monitor{"mymodel": buf}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"mymodel": {}}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/logs/stream/mymodel", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if !w.Flushed {
		t.Fatal("empty log stream did not flush response headers")
	}
	if got := w.Body.String(); got != "" {
		t.Fatalf("body=%q want empty history", got)
	}
}

func TestServer_LogStream_ModelScopedIdentityAllowsOnlyPermittedModel(t *testing.T) {
	buf := logmon.NewWriter(io.Discard)
	buf.Write([]byte("allowed model log"))
	local := newStubRouter([]string{"allowed", "denied"}, "")
	local.loggers = map[string]*logmon.Monitor{"allowed": buf}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"allowed": {}, "denied": {}}}
	identity := auth.Identity{ID: "scoped", Scopes: map[string]struct{}{auth.ScopeLogs: {}}, Models: []string{"allowed"}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/logs/stream/allowed", nil).WithContext(withIdentity(ctx, identity))
	req.SetPathValue("logMonitorID", "allowed")
	w := httptest.NewRecorder()
	s.handleLogStream(w, req)
	if w.Code != http.StatusOK || w.Body.String() != "allowed model log" {
		t.Fatalf("allowed stream = %d %q", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/logs/stream/denied", nil).WithContext(withIdentity(context.Background(), identity))
	req.SetPathValue("logMonitorID", "denied")
	w = httptest.NewRecorder()
	s.handleLogStream(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("denied stream status=%d body=%q, want 403", w.Code, w.Body.String())
	}
}

func TestServer_LogStream_UnknownID_Returns400(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/logs/stream/no-such-model", nil))

	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", w.Code)
	}
}
