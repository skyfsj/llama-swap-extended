package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/backend"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/perf"
	"github.com/mostlygeek/llama-swap/internal/protocol/anthropiccache"
	"github.com/mostlygeek/llama-swap/internal/runtime"
)

func TestControlMetrics_ObserveAndRenderHasBoundedFamilies(t *testing.T) {
	metrics := newControlMetrics()
	metrics.observeRequest(anthropicCacheTelemetry{
		Applied:    true,
		Anomalies:  []string{"one", "two"},
		Transforms: []anthropiccache.TransformResult{},
	})
	// Use the protocol type directly for transform telemetry. The conversion
	// keeps this test focused on the fixed metric-family allowlist.
	metrics.observeRequest(anthropicCacheTelemetry{
		Transforms: []anthropiccache.TransformResult{
			{Name: "fingerprint-strip", Applied: true},
			{Name: "thinking-sanitize", Skipped: true},
			{Name: "untrusted-user-label", Applied: true},
		},
	})
	metrics.observeUsage(ActivityLogEntry{Tokens: TokenMetrics{CachedTokens: 4}, CacheCreationTokens: 3})

	var out bytes.Buffer
	metrics.writePrometheus(&out)
	text := out.String()
	for _, want := range []string{
		"llamaswap_anthropic_cache_repairs_total 1",
		"llamaswap_anthropic_cache_anomalies_total 2",
		"llamaswap_anthropic_cache_hits_total 1",
		"llamaswap_anthropic_cache_read_tokens_total 4",
		"llamaswap_anthropic_cache_creation_tokens_total 3",
		`llamaswap_anthropic_cache_transform_applied_total{transform="fingerprint-strip"} 1`,
		`llamaswap_anthropic_cache_transform_skipped_total{transform="thinking-sanitize"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, `transform="untrusted-user-label"`) || strings.Contains(text, `session="`) || strings.Contains(text, `request="`) || strings.Contains(text, `prompt="`) {
		t.Fatalf("metrics exposed an unbounded or request-scoped label:\n%s", text)
	}
}

func TestServer_WriteControlMetrics_UsesCacheAndRuntimeState(t *testing.T) {
	cache := backend.NewCacheController()
	cache.Observe("m\\\"odel", backend.CacheReport{Hit: true, CachedTokens: 9, CreationTokens: 2})
	runtimeRoot := t.TempDir()
	manager, err := runtime.NewManager(runtimeRoot, nil)
	if err != nil {
		t.Fatalf("runtime.NewManager: %v", err)
	}
	if err := manager.Register("vllm", "vllm"); err != nil {
		t.Fatalf("runtime.Register: %v", err)
	}
	s := &Server{metrics: &metricsMonitor{controlMetrics: newControlMetrics()}, cacheController: cache, runtime: manager}
	var out bytes.Buffer
	s.writeControlMetrics(&out)
	text := out.String()
	for _, want := range []string{
		`llamaswap_backend_cache_hit{model="m\\\"odel"} 1`,
		`llamaswap_backend_cache_cached_tokens{model="m\\\"odel"} 9`,
		`llamaswap_runtime_state{runtime="vllm",state="IDLE"} 1`,
		`llamaswap_runtime_update_available{runtime="vllm"} 0`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("control metrics missing %q:\n%s", want, text)
		}
	}
}

func TestServer_HandleMetrics_AppendsControlFamilies(t *testing.T) {
	performance, err := perf.New(config.PerformanceConfig{Every: 5 * time.Second}, logmon.NewWriter(io.Discard))
	if err != nil {
		t.Fatalf("perf.New: %v", err)
	}
	s := &Server{perf: performance, metrics: &metricsMonitor{controlMetrics: newControlMetrics()}}
	s.metrics.controlMetrics.observeUsage(ActivityLogEntry{Tokens: TokenMetrics{CachedTokens: 2}})

	recorder := httptest.NewRecorder()
	s.handleMetrics(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "llamaswap_anthropic_cache_read_tokens_total 2") {
		t.Fatalf("control metrics were not appended:\n%s", recorder.Body.String())
	}
}
