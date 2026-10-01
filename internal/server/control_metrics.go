package server

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync/atomic"
)

// controlMetrics contains process-local counters that are useful to a
// Prometheus scraper but are not part of the persisted activity schema. It
// intentionally contains no request, session, key, prompt, or prefix hash
// labels. Transform names are a fixed allowlist, so malformed upstream
// telemetry cannot create an unbounded metric family.
type controlMetrics struct {
	anthropicCacheRepairs   atomic.Uint64
	anthropicCacheAnomalies atomic.Uint64
	anthropicCacheHits      atomic.Uint64
	anthropicCacheRead      atomic.Uint64
	anthropicCacheCreation  atomic.Uint64

	transformApplied [len(prometheusCacheTransforms)]atomic.Uint64
	transformSkipped [len(prometheusCacheTransforms)]atomic.Uint64
}

var prometheusCacheTransforms = [...]string{
	"fingerprint-strip",
	"sort-stabilization",
	"fresh-session-sort",
	"identity-normalization",
	"cache-control-normalize",
	"ttl-management",
	"thinking-sanitize",
	"cc-version-normalize",
	"high-risk",
}

func newControlMetrics() *controlMetrics {
	return &controlMetrics{}
}

// observeRequest records cache-repair decisions for one completed request.
// This is called once by metricsMonitor.record, including failed upstream
// requests, so the control plane can distinguish detection/repair activity
// from token usage without storing raw request content.
func (m *controlMetrics) observeRequest(telemetry anthropicCacheTelemetry) {
	if m == nil {
		return
	}
	if telemetry.Applied {
		m.anthropicCacheRepairs.Add(1)
	}
	anomalyCount := len(telemetry.Anomalies)
	transformAnomalies := 0
	for _, transform := range telemetry.Transforms {
		for index, name := range prometheusCacheTransforms {
			if transform.Name != name {
				continue
			}
			if transform.Applied {
				m.transformApplied[index].Add(1)
			}
			if transform.Skipped {
				m.transformSkipped[index].Add(1)
			}
			if transform.Anomaly != "" {
				transformAnomalies++
			}
			break
		}
	}
	if anomalyCount == 0 {
		anomalyCount = transformAnomalies
	}
	if anomalyCount > 0 {
		addCounter(&m.anthropicCacheAnomalies, anomalyCount)
	}
}

// observeUsage records non-negative cache partitions parsed from a response.
// Unknown counters use the existing -1 sentinel and are ignored.
func (m *controlMetrics) observeUsage(entry ActivityLogEntry) {
	if m == nil {
		return
	}
	if entry.Tokens.CachedTokens > 0 {
		m.anthropicCacheHits.Add(1)
		addCounter(&m.anthropicCacheRead, entry.Tokens.CachedTokens)
	}
	if entry.CacheCreationTokens > 0 {
		addCounter(&m.anthropicCacheCreation, entry.CacheCreationTokens)
	}
}

// addCounter performs a saturating atomic add. Activity parsing is bounded,
// but a long-lived process must not wrap a Prometheus counter back to zero if
// an upstream reports a very large (or repeatedly accumulated) value.
func addCounter(counter *atomic.Uint64, value int) {
	if counter == nil || value <= 0 {
		return
	}
	add := uint64(value)
	for {
		old := counter.Load()
		if ^uint64(0)-old < add {
			if counter.CompareAndSwap(old, ^uint64(0)) {
				return
			}
			continue
		}
		if counter.CompareAndSwap(old, old+add) {
			return
		}
	}
}

// writePrometheus emits the process-local cache-repair counters. Keep HELP
// and TYPE lines stable even when no requests have been observed; this makes
// dashboards and alert rules deterministic on a cold start.
func (m *controlMetrics) writePrometheus(w io.Writer) {
	if m == nil || w == nil {
		return
	}
	fmt.Fprintln(w, "# HELP llamaswap_anthropic_cache_repairs_total Anthropic cache repair requests applied")
	fmt.Fprintln(w, "# TYPE llamaswap_anthropic_cache_repairs_total counter")
	fmt.Fprintf(w, "llamaswap_anthropic_cache_repairs_total %d\n", m.anthropicCacheRepairs.Load())
	fmt.Fprintln(w, "# HELP llamaswap_anthropic_cache_anomalies_total Anthropic cache transform anomalies observed")
	fmt.Fprintln(w, "# TYPE llamaswap_anthropic_cache_anomalies_total counter")
	fmt.Fprintf(w, "llamaswap_anthropic_cache_anomalies_total %d\n", m.anthropicCacheAnomalies.Load())
	fmt.Fprintln(w, "# HELP llamaswap_anthropic_cache_hits_total Responses with cached input tokens")
	fmt.Fprintln(w, "# TYPE llamaswap_anthropic_cache_hits_total counter")
	fmt.Fprintf(w, "llamaswap_anthropic_cache_hits_total %d\n", m.anthropicCacheHits.Load())
	fmt.Fprintln(w, "# HELP llamaswap_anthropic_cache_read_tokens_total Cached input tokens observed")
	fmt.Fprintln(w, "# TYPE llamaswap_anthropic_cache_read_tokens_total counter")
	fmt.Fprintf(w, "llamaswap_anthropic_cache_read_tokens_total %d\n", m.anthropicCacheRead.Load())
	fmt.Fprintln(w, "# HELP llamaswap_anthropic_cache_creation_tokens_total Cache creation input tokens observed")
	fmt.Fprintln(w, "# TYPE llamaswap_anthropic_cache_creation_tokens_total counter")
	fmt.Fprintf(w, "llamaswap_anthropic_cache_creation_tokens_total %d\n", m.anthropicCacheCreation.Load())
	fmt.Fprintln(w, "# HELP llamaswap_anthropic_cache_transform_applied_total Anthropic cache transforms applied")
	fmt.Fprintln(w, "# TYPE llamaswap_anthropic_cache_transform_applied_total counter")
	fmt.Fprintln(w, "# HELP llamaswap_anthropic_cache_transform_skipped_total Anthropic cache transforms skipped")
	fmt.Fprintln(w, "# TYPE llamaswap_anthropic_cache_transform_skipped_total counter")
	for index, name := range prometheusCacheTransforms {
		label := escapePrometheusLabel(name)
		fmt.Fprintf(w, "llamaswap_anthropic_cache_transform_applied_total{transform=\"%s\"} %d\n", label, m.transformApplied[index].Load())
		fmt.Fprintf(w, "llamaswap_anthropic_cache_transform_skipped_total{transform=\"%s\"} %d\n", label, m.transformSkipped[index].Load())
	}
}

// writeControlMetrics appends control-plane metrics to the existing system
// and GPU metrics response. Runtime names and configured model IDs are finite
// deployment dimensions; request/session/key/prompt identifiers are never
// used as labels.
func (s *Server) writeControlMetrics(w io.Writer) {
	if s == nil || w == nil {
		return
	}
	if s.metrics != nil {
		s.metrics.controlMetrics.writePrometheus(w)
	}

	if s.cacheController != nil {
		reports := s.cacheController.Snapshot()
		models := make([]string, 0, len(reports))
		for model := range reports {
			models = append(models, model)
		}
		sort.Strings(models)
		fmt.Fprintln(w, "# HELP llamaswap_backend_cache_hit Latest observed backend cache hit (1 or 0)")
		fmt.Fprintln(w, "# TYPE llamaswap_backend_cache_hit gauge")
		fmt.Fprintln(w, "# HELP llamaswap_backend_cache_cached_tokens Latest observed cached input tokens")
		fmt.Fprintln(w, "# TYPE llamaswap_backend_cache_cached_tokens gauge")
		fmt.Fprintln(w, "# HELP llamaswap_backend_cache_creation_tokens Latest observed cache creation input tokens")
		fmt.Fprintln(w, "# TYPE llamaswap_backend_cache_creation_tokens gauge")
		fmt.Fprintln(w, "# HELP llamaswap_backend_cache_observed_timestamp_seconds Unix timestamp of latest cache observation")
		fmt.Fprintln(w, "# TYPE llamaswap_backend_cache_observed_timestamp_seconds gauge")
		for _, model := range models {
			report := reports[model]
			label := escapePrometheusLabel(model)
			hit := 0
			if report.Hit {
				hit = 1
			}
			fmt.Fprintf(w, "llamaswap_backend_cache_hit{model=\"%s\"} %d\n", label, hit)
			fmt.Fprintf(w, "llamaswap_backend_cache_cached_tokens{model=\"%s\"} %d\n", label, maxInt64ForMetric(report.CachedTokens))
			fmt.Fprintf(w, "llamaswap_backend_cache_creation_tokens{model=\"%s\"} %d\n", label, maxInt64ForMetric(report.CreationTokens))
			fmt.Fprintf(w, "llamaswap_backend_cache_observed_timestamp_seconds{model=\"%s\"} %d\n", label, report.ObservedAt.Unix())
		}
	}

	if s.runtime != nil {
		statuses := s.runtime.List()
		fmt.Fprintln(w, "# HELP llamaswap_runtime_state Current managed runtime state (1 for the active state)")
		fmt.Fprintln(w, "# TYPE llamaswap_runtime_state gauge")
		fmt.Fprintln(w, "# HELP llamaswap_runtime_update_available Whether a managed runtime has a candidate update")
		fmt.Fprintln(w, "# TYPE llamaswap_runtime_update_available gauge")
		fmt.Fprintln(w, "# HELP llamaswap_runtime_pinned Whether a managed runtime is pinned")
		fmt.Fprintln(w, "# TYPE llamaswap_runtime_pinned gauge")
		fmt.Fprintln(w, "# HELP llamaswap_runtime_last_check_timestamp_seconds Unix timestamp of the latest runtime check")
		fmt.Fprintln(w, "# TYPE llamaswap_runtime_last_check_timestamp_seconds gauge")
		for _, status := range statuses {
			name := escapePrometheusLabel(status.Name)
			state := escapePrometheusLabel(string(status.State))
			fmt.Fprintf(w, "llamaswap_runtime_state{runtime=\"%s\",state=\"%s\"} 1\n", name, state)
			available := 0
			if strings.TrimSpace(status.Available) != "" {
				available = 1
			}
			pinned := 0
			if strings.TrimSpace(status.Pinned) != "" {
				pinned = 1
			}
			fmt.Fprintf(w, "llamaswap_runtime_update_available{runtime=\"%s\"} %d\n", name, available)
			fmt.Fprintf(w, "llamaswap_runtime_pinned{runtime=\"%s\"} %d\n", name, pinned)
			lastCheck := int64(0)
			if !status.LastCheck.IsZero() {
				lastCheck = status.LastCheck.Unix()
			}
			fmt.Fprintf(w, "llamaswap_runtime_last_check_timestamp_seconds{runtime=\"%s\"} %d\n", name, lastCheck)
		}
	}
}

func maxInt64ForMetric(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func escapePrometheusLabel(value string) string {
	return strings.NewReplacer(`\\`, `\\\\`, `"`, `\\"`, "\n", `\n`).Replace(value)
}
