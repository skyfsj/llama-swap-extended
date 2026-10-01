package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
)

type lmcacheDashboardFixture struct {
	service *lmcacheService
	config  config.Config
	server  *httptest.Server
}

func newLMCacheDashboardFixture(t *testing.T, handler http.Handler) lmcacheDashboardFixture {
	t.Helper()
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep unavailable")
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	host, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		LMCache: config.LMCacheModuleConfig{
			Enabled: true,
			Server: config.LMCacheServerConfig{
				Enabled:  true,
				HTTPHost: host,
				HTTPPort: port,
			},
		},
	}
	s := &Server{}
	s.setConfig(cfg)
	service := newLMCacheService(s)
	s.lmcacheMod = service
	service.proc.readyTimeout = 2 * time.Second
	service.proc.healthInterval = time.Hour
	startFakeServer(t, service, "sleep", server.URL+"/healthcheck")
	if state := waitForState(t, &service.proc, LMCacheStateRunning, time.Second); state.State != LMCacheStateRunning {
		t.Fatalf("fixture server = %+v, want RUNNING", state)
	}
	t.Cleanup(func() { _ = service.proc.Stop(lmcacheStopTimeout) })
	return lmcacheDashboardFixture{service: service, config: cfg, server: server}
}

func TestLMCacheDashboard_HealthyResponseAggregatesAllowlistedEndpoints(t *testing.T) {
	fixture := newLMCacheDashboardFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/healthcheck":
			_, _ = w.Write([]byte(`{"status":"healthy"}`))
		case "/status":
			_, _ = w.Write([]byte(`{"is_healthy":true,"engine_type":"cpu","chunk_size":256,"active_sessions":2,"active_prefetch_jobs":3,"storage_manager":{"l1_manager":{"memory_used_bytes":4096}}}`))
		case "/config/adapters":
			_, _ = w.Write([]byte(`{"adapters":[{"type_name":"LMCacheMPConnector"}]}`))
		case "/version":
			_, _ = w.Write([]byte(`"frontend-1"`))
		case "/lmc_version":
			_, _ = w.Write([]byte(`"0.5.4"`))
		case "/commit_id":
			_, _ = w.Write([]byte(`"abc123"`))
		case "/metrics":
			w.Header().Set("Content-Type", "text/plain; version=0.0.4")
			_, _ = w.Write([]byte("lmcache_lookup_requested_total 10\n"))
		case "/periodic-threads-health":
			_, _ = w.Write([]byte(`{"healthy":true,"unhealthy_threads":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))

	response := fixture.service.dashboard(context.Background(), fixture.config)
	if !response.Available || response.Reason != "" {
		t.Fatalf("dashboard availability = %v/%q, want true/empty; response=%+v", response.Available, response.Reason, response)
	}
	if !response.Health.Healthy || response.Health.Status != "healthy" || response.Health.HTTPStatus != http.StatusOK {
		t.Fatalf("health = %+v, want an explicit healthy result", response.Health)
	}
	if response.Status == nil || response.Status.EngineType != "cpu" || response.Status.ActiveSessions != 2 || response.Status.L1MemoryUsedBytes != 4096 || !response.Status.L1MemoryUsedPresent {
		t.Fatalf("status = %+v, want normalized live status", response.Status)
	}
	if string(response.Adapters) != `[{"type_name":"LMCacheMPConnector"}]` {
		t.Fatalf("adapters = %s, want upstream adapter data", response.Adapters)
	}
	if response.Versions.Version != "frontend-1" || response.Versions.LMCache != "0.5.4" || response.Versions.CommitID != "abc123" {
		t.Fatalf("versions = %+v, want all allowlisted version endpoints", response.Versions)
	}
	if response.Metrics.Body == "" || response.PeriodicHealth.Healthy == nil || !*response.PeriodicHealth.Healthy {
		t.Fatalf("metrics/periodic health = %+v/%+v, want live data", response.Metrics, response.PeriodicHealth)
	}
}

func TestLMCacheDashboard_StoppedDoesNotRequestUpstreamOrExposeSnapshot(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	fixture := lmcacheDashboardFixture{service: newLMCacheService(&Server{})}
	fixture.config.LMCache.Server = config.LMCacheServerConfig{HTTPHost: "127.0.0.1", HTTPPort: 1}
	response := fixture.service.dashboard(context.Background(), fixture.config)
	if response.Available || response.Reason != "stopped" {
		t.Fatalf("stopped dashboard = %+v, want unavailable/stopped", response)
	}
	if response.Status != nil || response.Adapters != nil || response.Metrics.Body != "" || response.Versions != (lmcacheDashboardVersions{}) {
		t.Fatalf("stopped dashboard exposed stale data: %+v", response)
	}
	if requests.Load() != 0 {
		t.Fatalf("stopped dashboard made %d upstream requests, want 0", requests.Load())
	}
}

func TestLMCacheDashboard_RejectsUnhealthyStatusString(t *testing.T) {
	var requests atomic.Int32
	var healthChecks atomic.Int32
	fixture := newLMCacheDashboardFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/healthcheck" {
			if healthChecks.Add(1) == 1 {
				_, _ = w.Write([]byte(`{"status":"healthy"}`))
			} else {
				_, _ = w.Write([]byte(`{"status":"unhealthy"}`))
			}
			return
		}
		http.NotFound(w, r)
	}))

	response := fixture.service.dashboard(context.Background(), fixture.config)
	if response.Available || response.Reason != "unhealthy" || response.Health.Healthy {
		t.Fatalf("unhealthy dashboard = %+v, want unavailable with false health", response)
	}
	if !strings.Contains(response.Errors["health"], "unhealthy") {
		t.Fatalf("health error = %q, want explicit unhealthy diagnostic", response.Errors["health"])
	}
	if requests.Load() != 2 {
		t.Fatalf("unhealthy dashboard made %d requests, want one supervisor and one dashboard health probe", requests.Load())
	}
}

func TestLMCacheDashboard_AllowsPartialEndpointFailure(t *testing.T) {
	fixture := newLMCacheDashboardFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/healthcheck":
			_, _ = w.Write([]byte(`{"healthy":true}`))
		case "/status":
			_, _ = w.Write([]byte(`{"is_healthy":true,"active_sessions":1}`))
		case "/metrics":
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))

	response := fixture.service.dashboard(context.Background(), fixture.config)
	if !response.Available || response.Status == nil || response.Status.ActiveSessions != 1 {
		t.Fatalf("partial dashboard = %+v, want available status", response)
	}
	if !strings.Contains(response.Errors["metrics"], "HTTP 503") {
		t.Fatalf("metrics error = %q, want isolated endpoint error", response.Errors["metrics"])
	}
	if response.Metrics.Body != "" {
		t.Fatalf("metrics body = %q, want no stale/failed body", response.Metrics.Body)
	}
}

func TestLMCacheDashboardEndpoint_ContextCancellationAndBodyLimit(t *testing.T) {
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(blocked.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := fetchLMCacheDashboardEndpoint(ctx, blocked.Client(), blocked.URL)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled fetch error = %v, want context deadline", err)
	}

	large := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, strings.Repeat("x", lmcacheDashboardMaxBody+1))
	}))
	t.Cleanup(large.Close)
	_, err = fetchLMCacheDashboardEndpoint(context.Background(), large.Client(), large.URL)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized fetch error = %v, want response size limit", err)
	}
}

func TestLMCacheDashboardHandlerReturnsNoStoreAndStableShape(t *testing.T) {
	var calls atomic.Int32
	fixture := newLMCacheDashboardFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/healthcheck" {
			_, _ = w.Write([]byte(`{"healthy":true}`))
			return
		}
		if r.URL.Path == "/status" {
			_, _ = w.Write([]byte(`{"is_healthy":true}`))
			return
		}
		http.NotFound(w, r)
	}))
	s := fixture.service.srv
	recorder := httptest.NewRecorder()
	s.handleAPILMCacheDashboard(recorder, httptest.NewRequest(http.MethodGet, "/api/lmcache/dashboard", nil))
	if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("handler = status %d headers=%v, want 200/no-store", recorder.Code, recorder.Header())
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"available", "checkedAt", "health", "versions", "metrics", "periodicHealth", "errors"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("handler payload missing %q: %s", key, recorder.Body.String())
		}
	}
	if calls.Load() == 0 {
		t.Fatal("handler did not query the healthy fixture")
	}
}

func TestServer_HandleAPILMCacheServerLogs(t *testing.T) {
	root := t.TempDir()
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	s.setConfig(config.Config{RuntimeManager: config.RuntimeManagerConfig{Root: root}})

	// No log file yet: the panel's empty state, not an error.
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/lmcache/server/logs", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("missing log status = %d body = %q", w.Code, w.Body.String())
	}
	var payload struct {
		LogPath   string `json:"logPath"`
		Size      int64  `json:"size"`
		Truncated bool   `json:"truncated"`
		Output    string `json:"output"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Output != "" || payload.Truncated {
		t.Fatalf("missing log payload = %+v", payload)
	}

	logPath := filepath.Join(root, "lmcache", "server.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o750); err != nil {
		t.Fatal(err)
	}
	// Layout: the far head sits beyond the tail-limit window, the filler sits
	// inside the tail-limit window but beyond the 24KB serve window, and the
	// near-tail lines plus the final marker must appear in the response.
	head := strings.Repeat("HEAD-BLOCK\n", 40000)
	filler := strings.Repeat("FILLER-LINE\n", 20000)
	nearTail := strings.Repeat("NEAR-TAIL-LINE\n", 100)
	tail := "lmcache server ready\n"
	if err := os.WriteFile(logPath, []byte(head+filler+nearTail+tail), 0o640); err != nil {
		t.Fatal(err)
	}

	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/lmcache/server/logs", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("log status = %d", w.Code)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !payload.Truncated {
		t.Fatal("truncated = false, want true for an oversized log")
	}
	if !strings.HasSuffix(payload.Output, tail) {
		t.Fatalf("output does not end with the newest line: %q", payload.Output[len(payload.Output)-40:])
	}
	if strings.Contains(payload.Output, "HEAD-BLOCK") {
		t.Fatal("output contains bytes from before the tail window")
	}
	if !strings.Contains(payload.Output, "NEAR-TAIL-LINE") {
		t.Fatal("output is missing lines that fall inside the tail window")
	}
	if len(payload.Output) > lmcacheServerLogTailLimit {
		t.Fatalf("output = %d bytes, want at most %d", len(payload.Output), lmcacheServerLogTailLimit)
	}
}
