package server

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/backend"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/protocol/anthropiccache"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/tidwall/gjson"
)

type TokenMetrics = store.TokenMetrics
type ActivityLogEntry = store.ActivityLogEntry

// ActivityLogEvent carries a single activity log entry to event subscribers.
type ActivityLogEvent struct {
	Metrics ActivityLogEntry
}

func (e ActivityLogEvent) Type() uint32 {
	return swaputil.ActivityLogEventID
}

// metricsMonitor parses upstream responses for token statistics and stores
// activity plus durable audit conversations when audit is enabled.
type metricsMonitor struct {
	store      *store.Store
	maxMetrics int
	logger     *logmon.Monitor
	audit      config.AuditConfig
	// dirtyAudit wakes the background audit maintenance task after a
	// conversation is persisted. Wired by the Server; nil in standalone use.
	dirtyAudit     func()
	priceStore     *store.Store
	priceRefs      map[string]config.PricingRef
	priceMu        sync.Mutex
	priceCache     map[string]priceLookup
	cacheObserver  func(string, backend.CacheReport)
	sessionUsage   *anthropiccache.SessionUsage
	controlMetrics *controlMetrics
}

type priceLookup struct {
	price     store.Price
	found     bool
	ambiguous bool
	expires   time.Time
}

func newMetricsMonitor(logger *logmon.Monitor, maxMetrics int, st *store.Store) *metricsMonitor {
	if maxMetrics <= 0 {
		maxMetrics = 1000
	}
	mm := &metricsMonitor{
		logger:         logger,
		store:          st,
		maxMetrics:     maxMetrics,
		sessionUsage:   anthropiccache.NewSessionUsage(0),
		controlMetrics: newControlMetrics(),
	}
	mm.priceCache = make(map[string]priceLookup)
	return mm
}

// configurePricing wires the exact model/provider mapping used for estimates.
// The catalog itself remains in SQLite so a restart with a persistent store
// keeps the last successful models.dev snapshot available immediately.
func (mp *metricsMonitor) configurePricing(st *store.Store, cfg config.Config) {
	if mp == nil {
		return
	}
	mp.priceMu.Lock()
	defer mp.priceMu.Unlock()
	mp.priceStore = st
	mp.priceRefs = make(map[string]config.PricingRef, len(cfg.Models))
	for model, modelCfg := range cfg.Models {
		mp.priceRefs[model] = modelCfg.Backend.Pricing
	}
	// A reconfiguration may change the provider/model mapping or point at a
	// different store. Cached positive and negative lookups must not survive
	// that boundary, otherwise activity rows can be priced with a stale catalog
	// for up to the five-minute lookup TTL.
	mp.priceCache = make(map[string]priceLookup)
}

func (mp *metricsMonitor) configureAudit(audit config.AuditConfig) {
	mp.audit = audit
}

// setCacheObserver connects parsed usage telemetry to the in-memory backend
// cache report. The observer is configured before the server starts serving;
// keeping it as a callback avoids coupling the metrics parser to lifecycle or
// HTTP control-plane code.
func (mp *metricsMonitor) setCacheObserver(observer func(string, backend.CacheReport)) {
	if mp == nil {
		return
	}
	mp.cacheObserver = observer
}

// queueMetrics persists a metric and returns the store-assigned row. It
// deliberately does not take the request context: record runs after the
// handler returns, and an aborted request (canceled context) must still be
// recorded — that is exactly when the error entry matters.
func (mp *metricsMonitor) queueMetrics(metric ActivityLogEntry) (ActivityLogEntry, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stored, err := mp.store.InsertActivity(ctx, metric)
	if err != nil {
		mp.warnf("failed to persist activity metric: %v", err)
		return ActivityLogEntry{}, false
	}
	if mp.store.IsInMemory() {
		if err := mp.store.PruneActivity(ctx, mp.maxMetrics); err != nil {
			mp.warnf("failed to prune activity metrics: %v", err)
		}
	}
	return stored, true
}

// emitMetric publishes an ActivityLogEvent for the given metric.
func (mp *metricsMonitor) emitMetric(metric ActivityLogEntry) {
	event.Emit(ActivityLogEvent{Metrics: metric})
}

func (mp *metricsMonitor) warnf(format string, args ...any) {
	if mp.logger != nil {
		mp.logger.Warnf(format, args...)
	}
}

func (mp *metricsMonitor) debugf(format string, args ...any) {
	if mp.logger != nil {
		mp.logger.Debugf(format, args...)
	}
}

func (mp *metricsMonitor) Close() error {
	return nil
}

// record parses a completed response body and stores/emits an activity entry.
// Failed responses receive an ErrorMsg while their durable audit entry records
// the available request and response details. reqBody and reqHeaders are
// buffered before dispatch when audit is enabled.
func (mp *metricsMonitor) record(modelID string, r *http.Request, recorder *responseBodyCopier, reqBody []byte, reqHeaders map[string]string) {
	tm := ActivityLogEntry{
		Timestamp:       time.Now(),
		Model:           modelID,
		ReqPath:         r.URL.Path,
		RespContentType: recorder.Header().Get("Content-Type"),
		RespStatusCode:  recorder.Status(),
		DurationMs:      int(time.Since(recorder.StartTime()).Milliseconds()),
		FirstTokenMs:    -1,
		// The usage records page attributes rows by caller address, and an
		// operator auditing an access-key rejection needs the same address the
		// access log printed, so resolve it once here.
		ClientIP: clientIP(r),
	}
	if identity := identityFromContext(r.Context()); identity.ID != "" && identity.ID != "anonymous" {
		tm.KeyID = identity.ID
	}

	if ctxData, ok := swaputil.ReadContext(r.Context()); ok && len(ctxData.Metadata) > 0 {
		tm.Metadata = make(map[string]string, len(ctxData.Metadata))
		for k, v := range ctxData.Metadata {
			tm.Metadata[k] = v
		}
		// RequestContextMiddleware normally copies the authenticated identity
		// into metadata. Preserve that identity when an embedder or an older
		// middleware supplies unrelated metadata without a key_id; an empty map
		// lookup must never erase the key attribution used by usage/audit views.
		if keyID := strings.TrimSpace(ctxData.Metadata["key_id"]); keyID != "" {
			tm.KeyID = keyID
		}
		tm.SessionID = ctxData.Metadata["session_id"]
	}
	cacheTelemetry := cacheTelemetryFromContext(r)
	if mp.controlMetrics != nil {
		mp.controlMetrics.observeRequest(cacheTelemetry)
	}
	tm.RepairApplied = cacheTelemetry.Applied
	tm.PrefixHash = cacheTelemetry.PrefixHash
	if cacheTelemetry.Detected || cacheTelemetry.Enabled || len(cacheTelemetry.Transforms) > 0 || len(cacheTelemetry.Anomalies) > 0 {
		if tm.Metadata == nil {
			tm.Metadata = make(map[string]string)
		}
		tm.Metadata["cache_fix_detected"] = strconv.FormatBool(cacheTelemetry.Detected)
		tm.Metadata["cache_fix_enabled"] = strconv.FormatBool(cacheTelemetry.Enabled)
		if len(cacheTelemetry.Transforms) > 0 {
			if encoded, err := json.Marshal(cacheTelemetry.Transforms); err == nil {
				tm.Metadata["cache_transforms"] = string(encoded)
			}
		}
		if len(cacheTelemetry.Anomalies) > 0 {
			tm.Metadata["cache_anomalies"] = strings.Join(cacheTelemetry.Anomalies, "; ")
		}
	}
	if selectorID := selectorFromContext(r.Context()); selectorID != "" {
		if tm.Metadata == nil {
			tm.Metadata = make(map[string]string)
		}
		tm.Metadata["selector"] = selectorID
	}

	queueAndPersistAudit := func(respBody []byte, complete bool) bool {
		stored, ok := mp.queueMetrics(tm)
		if !ok {
			return false
		}
		tm = stored
		mp.persistAudit(tm, r, reqBody, reqHeaders, respBody, recorder.Header(), complete)
		return true
	}
	queueAndEmit := func(respBody []byte, complete bool) {
		if !queueAndPersistAudit(respBody, complete) {
			return
		}
		mp.emitMetric(tm)
	}

	// A client that hangs up is normal traffic, not a server fault: record the
	// entry so the abort is visible, but at debug level and without the
	// synthesized upstream error message the failure path below would attach.
	// There is no response to inspect, so the audit entry is marked incomplete.
	if recorder.Status() == swaputil.StatusClientClosedRequest {
		mp.debugf("metrics: client disconnected before response, path=%s", r.URL.Path)
		tm.ErrorMsg = "client disconnected before response"
		queueAndEmit(nil, false)
		return
	}

	// The response copier keeps only a bounded prefix. Never parse, estimate or
	// persist that prefix as if it were a complete protocol response: doing so
	// could produce fabricated usage counters and an unreplayable audit/capture
	// row. The full response has already been forwarded to the client.
	if recorder.truncated {
		tm.ErrorMsg = fmt.Sprintf("response body exceeds %d bytes", swaputil.MaxRequestBodySize)
		if !queueAndPersistAudit(nil, false) {
			return
		}
		mp.emitMetric(tm)
		return
	}

	if recorder.Status() < http.StatusOK || recorder.Status() >= http.StatusMultipleChoices {
		mp.warnf("non-200 response, recording partial metrics: status=%d, path=%s", recorder.Status(), r.URL.Path)
		decoded, decErr := mp.decodeResponseBody(recorder, r.URL.Path)
		tm.ErrorMsg = failedErrorMessage(recorder.Status(), decoded, decErr)
		if !queueAndPersistAudit(decoded, false) {
			return
		}
		mp.emitMetric(tm)
		return
	}

	body := recorder.body.Bytes()
	if len(body) == 0 {
		mp.warnf("metrics: empty body, recording minimal metrics")
		queueAndEmit(nil, true)
		return
	}

	if encoding := recorder.Header().Get("Content-Encoding"); encoding != "" {
		decoded, err := decompressBody(body, encoding)
		if err != nil {
			mp.warnf("metrics: decompression failed: %v, path=%s, recording minimal metrics", err, r.URL.Path)
			tm.ErrorMsg = fmt.Sprintf("response decompression failed: %v", err)
			// Keep the audit trail aligned with every other failed response. The
			// encoded bytes are intentionally omitted because they could not be
			// decoded safely; request metadata and the explicit failure reason are
			// still useful for diagnosing a broken upstream.
			queueAndEmit(nil, false)
			return
		}
		body = decoded
	}

	if strings.Contains(recorder.Header().Get("Content-Type"), "text/event-stream") {
		if parsed, err := processStreamingResponse(modelID, recorder.StartTime(), body); err != nil {
			mp.warnf("error processing streaming response: %v, path=%s, recording minimal metrics", err, r.URL.Path)
		} else {
			if firstContent, lastContent, ok := recorder.StreamingContentTimes(); ok {
				applyObservedStreamingRates(&parsed, recorder.StartTime(), firstContent, lastContent)
				parsed.FirstTokenMs = elapsedMillis(recorder.StartTime(), firstContent)
				// Phase telemetry for the per-request detail view: the decode
				// window plus a bounded (time, cumulative tokens) curve.
				parsed.DecodeMs = elapsedMillis(firstContent, lastContent)
				parsed.SpeedTimeline = streamingSpeedTimeline(body, recorder.writes, recorder.StartTime(), parsed.Tokens.OutputTokens)
			}
			mergeParsedMetrics(&tm, &parsed)
		}
	} else if gjson.ValidBytes(body) {
		parsed := gjson.ParseBytes(body)
		usage := parsed.Get("usage")
		timings := parsed.Get("timings")
		responseMetrics := parsed.Get("metrics")

		// /infill responses are arrays; timings live in the last element (#463).
		if strings.HasPrefix(r.URL.Path, "/infill") {
			if arr := parsed.Array(); len(arr) > 0 {
				timings = arr[len(arr)-1].Get("timings")
			}
		}

		if usage.Exists() || timings.Exists() || responseMetrics.Exists() {
			if parsedMetrics, err := parseMetrics(modelID, recorder.StartTime(), usage, timings, responseMetrics); err != nil {
				mp.warnf("error parsing metrics: %v, path=%s, recording minimal metrics", err, r.URL.Path)
			} else {
				mergeParsedMetrics(&tm, &parsedMetrics)
			}
		}
	} else {
		mp.warnf("metrics: invalid JSON in response body path=%s, recording minimal metrics", r.URL.Path)
	}
	applyUsageTelemetry(&tm, body)
	if mp.controlMetrics != nil {
		mp.controlMetrics.observeUsage(tm)
	}
	// Cache counters can be split over multiple SSE events and a single client
	// session can span many requests. Keep a process-local cumulative view for
	// the activity/audit row without adding session identifiers to Prometheus
	// labels. The persisted store still provides restart-safe aggregate queries.
	currentCacheHit := tm.Tokens.CachedTokens > 0 || tm.CacheHitRatio > 0
	hasCurrentCacheTelemetry := tm.PrefixHash != "" || currentCacheHit || tm.CacheCreationTokens > 0
	if mp.applySessionUsage(&tm, body) {
		hasCurrentCacheTelemetry = true
	}
	if mp.cacheObserver != nil && hasCurrentCacheTelemetry {
		mp.cacheObserver(modelID, backend.CacheReport{
			Hit:            currentCacheHit,
			CachedTokens:   int64(max(tm.Tokens.CachedTokens, 0)),
			CreationTokens: int64(max(tm.CacheCreationTokens, 0)),
			PrefixHash:     tm.PrefixHash,
			ObservedAt:     time.Now(),
		})
	}
	mp.applyEstimatedCost(&tm, r.URL.Path)
	if !queueAndPersistAudit(body, true) {
		return
	}
	mp.emitMetric(tm)
}

// applySessionUsage folds cache accounting from one response into the
// in-memory session accumulator. The activity row keeps its own per-request
// ratios; the cumulative session view is used only for backend cache
// observation. The accumulator keeps uncached-only requests because they are
// part of a later session hit-rate denominator. The return value is true only
// when an actual cache read/creation partition was observed and should update
// the backend cache report.
func (mp *metricsMonitor) applySessionUsage(entry *ActivityLogEntry, body []byte) bool {
	if mp == nil || mp.sessionUsage == nil || entry == nil || strings.TrimSpace(entry.SessionID) == "" {
		return false
	}
	usage := anthropiccache.ExtractUsage(body)
	if usage.CacheReadInputTokens <= 0 && usage.CacheCreationInputTokens <= 0 && usage.UncachedInputTokens <= 0 {
		return false
	}
	mp.sessionUsage.Add(entry.SessionID, usage)
	return usage.CacheReadInputTokens > 0 || usage.CacheCreationInputTokens > 0
}

func (mp *metricsMonitor) applyEstimatedCost(entry *ActivityLogEntry, path string) {
	if entry == nil {
		return
	}
	price, ok := mp.lookupPrice(entry.Model)
	if !ok {
		return
	}
	input := int64(entry.Tokens.InputTokens)
	cached := int64(entry.Tokens.CachedTokens)
	created := int64(entry.CacheCreationTokens)
	// A missing cache counter is represented as -1 by the low-level parser so
	// callers can distinguish "unknown" from a real zero. Cost estimates must
	// never turn that sentinel into a negative cache charge or an extra input
	// token, however.
	if cached < 0 {
		cached = 0
	}
	if created < 0 {
		created = 0
	}
	// Anthropic reports input_tokens as the uncached portion, while OpenAI
	// compatible APIs report the total input and expose cached tokens inside
	// that total. Keep the partition explicit to avoid double charging cache
	// reads/writes in the estimate.
	uncached := input
	if !strings.Contains(path, "/messages") {
		uncached = input - cached - created
		if uncached < 0 {
			uncached = 0
		}
	}
	cost := float64(uncached)*price.Input/1e6 +
		float64(cached)*price.CacheRead/1e6 +
		float64(created)*price.CacheWrite/1e6 +
		float64(entry.Tokens.OutputTokens)*price.Output/1e6 +
		float64(entry.ReasoningTokens)*price.Reasoning/1e6
	entry.EstimatedCost = cost
	entry.CostEstimated = true
}

func (mp *metricsMonitor) lookupPrice(model string) (store.Price, bool) {
	if mp == nil || strings.TrimSpace(model) == "" {
		return store.Price{}, false
	}
	model = strings.TrimSpace(model)
	now := time.Now()
	mp.priceMu.Lock()
	if mp.priceStore == nil {
		mp.priceMu.Unlock()
		return store.Price{}, false
	}
	if cached, ok := mp.priceCache[model]; ok && cached.expires.After(now) {
		mp.priceMu.Unlock()
		return cached.price, cached.found && !cached.ambiguous
	}
	ref := mp.priceRefs[model]
	st := mp.priceStore
	mp.priceMu.Unlock()
	var prices []store.Price
	var err error
	if strings.TrimSpace(ref.Provider) != "" || strings.TrimSpace(ref.Model) != "" {
		providerModel := strings.TrimSpace(ref.Model)
		if providerModel == "" {
			providerModel = model
		}
		prices, err = st.FindPrices(context.Background(), strings.TrimSpace(ref.Provider), providerModel)
	} else {
		prices, err = st.FindPrices(context.Background(), "", model)
	}
	lookup := priceLookup{expires: now.Add(5 * time.Minute)}
	if err == nil {
		lookup.found = len(prices) > 0
		lookup.ambiguous = len(prices) != 1
		if len(prices) == 1 {
			lookup.price = prices[0]
		}
	}
	mp.priceMu.Lock()
	if mp.priceCache == nil {
		mp.priceCache = make(map[string]priceLookup)
	}
	mp.priceCache[model] = lookup
	mp.priceMu.Unlock()
	return lookup.price, lookup.found && !lookup.ambiguous
}

func (mp *metricsMonitor) persistAudit(metric ActivityLogEntry, r *http.Request, reqBody []byte, reqHeaders map[string]string, respBody []byte, respHeaders http.Header, complete bool) {
	if !mp.audit.Enabled || mp.store == nil {
		return
	}
	requestHeaders, _ := json.Marshal(reqHeaders)
	responseHeaders, _ := json.Marshal(headerMap(respHeaders))
	// Authentication material is never persisted, even when an operator
	// disables the optional broad header-redaction setting. This protects
	// Authorization, API-key, cookie and token headers from both the audit
	// database and the UI's raw-conversation view.
	var values map[string]string
	if json.Unmarshal(requestHeaders, &values) == nil {
		redactHeaders(values)
		requestHeaders, _ = json.Marshal(values)
	}
	if json.Unmarshal(responseHeaders, &values) == nil {
		redactHeaders(values)
		responseHeaders, _ = json.Marshal(values)
	}
	// Media persistence is explicit because multipart/audio bodies can be very
	// large. The default is true, matching the product's raw-audit contract.
	// Non-media endpoints are retained regardless of whether a client used
	// JSON or SSE. StoreMedia only controls large binary/media routes.
	storeBody := mp.audit.StoreMedia || !isMediaRequestPath(r.URL.Path)
	if !storeBody {
		reqBody = nil
		respBody = nil
	}
	// The activity row is the single public request identity. The audit table's
	// string key remains an internal storage key, but using the same decimal
	// value keeps new records deterministic and makes accidental divergence
	// impossible.
	id := strconv.Itoa(metric.ID)
	requestID := r.Header.Get("X-Request-ID")
	audit := store.AuditConversation{
		ID:                  id,
		ActivityID:          metric.ID,
		RequestID:           requestID,
		KeyID:               metric.KeyID,
		Model:               metric.Model,
		SessionID:           metric.SessionID,
		ReqPath:             r.URL.Path,
		Timestamp:           metric.Timestamp,
		RequestHeaders:      requestHeaders,
		RequestBody:         append([]byte(nil), reqBody...),
		ResponseHeaders:     responseHeaders,
		ResponseBody:        append([]byte(nil), respBody...),
		ResponseStatus:      metric.RespStatusCode,
		InputTokens:         metric.Tokens.InputTokens,
		OutputTokens:        metric.Tokens.OutputTokens,
		CachedTokens:        metric.Tokens.CachedTokens,
		CacheCreationTokens: metric.CacheCreationTokens,
		ReasoningTokens:     metric.ReasoningTokens,
		CacheHitRatio:       metric.CacheHitRatio,
		CacheCreationRatio:  metric.CacheCreationRatio,
		RepairApplied:       metric.RepairApplied,
		PrefixHash:          metric.PrefixHash,
		EstimatedCost:       metric.EstimatedCost,
		Complete:            complete,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := mp.store.InsertAuditConversation(ctx, audit); err != nil {
		mp.warnf("failed to persist audit conversation: %v", err)
		return
	}
	// Retention and the byte budget are enforced by the background audit
	// maintenance task, never on the request path. Purging after every
	// insert held the store's single connection behind a full-table sweep
	// while the response had already gone to the client, and a large table
	// turned those sweeps into multi-second holds.
	if mp.dirtyAudit != nil {
		mp.dirtyAudit()
	}
}

func isMediaRequestPath(path string) bool {
	return strings.Contains(path, "/audio/") || strings.Contains(path, "/images/") || strings.HasPrefix(path, "/sdapi/")
}

// decodeResponseBody returns the buffered response body, decompressing it when
// the upstream set a Content-Encoding we recognize. On decompression failure it
// logs a warning and returns an error so the caller can record a description
// (via ErrorMsg) instead of storing unreadable raw bytes.
func (mp *metricsMonitor) decodeResponseBody(recorder *responseBodyCopier, path string) ([]byte, error) {
	body := recorder.body.Bytes()
	if len(body) == 0 {
		return nil, nil
	}
	encoding := recorder.Header().Get("Content-Encoding")
	if encoding == "" {
		return body, nil
	}
	decoded, err := decompressBody(body, encoding)
	if err != nil {
		mp.warnf("metrics: response decompression failed: %v, path=%s", err, path)
		return nil, err
	}
	return decoded, nil
}

// errorMessagePaths lists JSON paths where a human-readable error message can
// live across OpenAI- and llama.cpp-style error responses.
var errorMessagePaths = []string{"error.message", "error", "message", "detail"}

// extractErrorMessage pulls a human-readable error string from a JSON error
// response. Returns "" if no message is found or the body is not valid JSON.
func extractErrorMessage(body []byte) string {
	if !gjson.ValidBytes(body) {
		return ""
	}
	parsed := gjson.ParseBytes(body)
	for _, path := range errorMessagePaths {
		v := parsed.Get(path)
		if v.Exists() && v.Type == gjson.String {
			if s := strings.TrimSpace(v.String()); s != "" {
				return s
			}
		}
	}
	return ""
}

// failedErrorMessage builds a human-readable description for a non-200 response.
// It prefers an error message parsed from the (decompressed) body and falls back
// to the HTTP status text. A non-nil decErr indicates the body could not be
// decoded, in which case the decode error is described instead.
func failedErrorMessage(status int, body []byte, decErr error) string {
	const maxLen = 500
	if decErr != nil {
		return fmt.Sprintf("response decode failed: %v", decErr)
	}
	if msg := extractErrorMessage(body); msg != "" {
		if len(msg) > maxLen {
			// maxLen is a byte budget, so cut on a rune boundary: slicing
			// mid-rune leaves an invalid byte that JSON later renders as \ufffd
			// and the dashboard shows as a corrupted character.
			msg = strings.ToValidUTF8(msg[:maxLen], "") + "..."
		}
		return msg
	}
	if text := http.StatusText(status); text != "" {
		return fmt.Sprintf("%d %s", status, text)
	}
	return fmt.Sprintf("HTTP %d", status)
}

// usagePaths lists the JSON paths where a per-event usage object can live.
var usagePaths = []string{"usage", "response.usage", "message.usage"}

// extractUsageTokens reads input/output/cached token counts from a usage
// gjson.Result, handling the field-name differences across endpoints.
func extractUsageTokens(usage gjson.Result) (input, output, cached int64, ok bool) {
	cached = -1
	if !usage.Exists() {
		return
	}

	if v := firstUsageResult(usage, "prompt_tokens", "promptTokens"); v.Exists() {
		if parsed, valid := usageInt(v); valid {
			input = parsed
			ok = true
		} else {
			input = 0
		}
	} else if v := firstUsageResult(usage, "input_tokens", "inputTokens"); v.Exists() {
		if parsed, valid := usageInt(v); valid {
			input = parsed
			ok = true
		} else {
			input = 0
		}
	}

	if v := firstUsageResult(usage, "completion_tokens", "completionTokens"); v.Exists() {
		if parsed, valid := usageInt(v); valid {
			output = parsed
			ok = true
		} else {
			output = 0
		}
	} else if v := firstUsageResult(usage, "output_tokens", "outputTokens"); v.Exists() {
		if parsed, valid := usageInt(v); valid {
			output = parsed
			ok = true
		} else {
			output = 0
		}
	}

	if v := firstUsageResult(usage, "cache_read_input_tokens", "cacheReadInputTokens", "cache_read_tokens", "cacheReadTokens"); v.Exists() {
		if parsed, valid := usageInt(v); valid {
			cached = parsed
			ok = true
		} else {
			cached = -1
		}
	} else if v := firstUsageResult(usage, "input_tokens_details.cached_tokens", "inputTokensDetails.cachedTokens"); v.Exists() {
		if parsed, valid := usageInt(v); valid {
			cached = parsed
			ok = true
		} else {
			cached = -1
		}
	} else if v := firstUsageResult(usage, "prompt_tokens_details.cached_tokens", "promptTokensDetails.cachedTokens"); v.Exists() {
		if parsed, valid := usageInt(v); valid {
			cached = parsed
			ok = true
		} else {
			cached = -1
		}
	}
	return
}

// usageInt parses the integer-like values emitted by OpenAI-compatible
// servers. gjson.Int silently turns malformed strings into zero, which would
// make a broken usage payload look like a valid zero-token response. Keep the
// absent/invalid distinction in the boolean and clamp explicit negatives so
// malformed upstream counters cannot create negative usage or hit rates.
func usageInt(value gjson.Result) (int64, bool) {
	if !value.Exists() {
		return 0, false
	}
	var raw string
	switch value.Type {
	case gjson.Number:
		raw = strings.TrimSpace(value.Raw)
	case gjson.String:
		raw = strings.TrimSpace(value.String())
	default:
		return 0, false
	}
	if raw == "" {
		return 0, false
	}
	var parsed int64
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		parsed = n
	} else {
		// Some providers serialize integral counters as 20.0. Accept that
		// representation, but reject fractional/non-finite values rather than
		// rounding them into an unrelated token count.
		f, floatErr := strconv.ParseFloat(raw, 64)
		// float64(math.MaxInt64) rounds to 2^63, so use an exclusive
		// 2^63 bound before converting to int64; otherwise an overflowing
		// value could wrap to MinInt64 on some architectures.
		if floatErr != nil || math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) || f >= math.Ldexp(1, 63) || f < float64(math.MinInt64) {
			return 0, false
		}
		parsed = int64(f)
	}
	if parsed < 0 {
		parsed = 0
	}
	return parsed, true
}

func processStreamingResponse(modelID string, start time.Time, body []byte) (ActivityLogEntry, error) {
	var (
		inputTokens, outputTokens int64
		cachedTokens              int64 = -1
		hasAny                    bool
		timings                   gjson.Result
		responseMetrics           gjson.Result
	)

	var eventData bytes.Buffer
	consume := func(data []byte) {
		data = bytes.TrimSpace(data)
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) || !gjson.ValidBytes(data) {
			return
		}
		parsed := gjson.ParseBytes(data)

		for _, path := range usagePaths {
			u := parsed.Get(path)
			if !u.Exists() {
				continue
			}
			i, o, c, ok := extractUsageTokens(u)
			if !ok {
				continue
			}
			hasAny = true
			if i >= 0 {
				inputTokens = i
			}
			if o >= 0 {
				outputTokens = o
			}
			if c >= 0 {
				cachedTokens = c
			}
		}
		if t := parsed.Get("timings"); t.Exists() {
			timings = t
			hasAny = true
		}
		if m := parsed.Get("metrics"); m.Exists() {
			responseMetrics = m
			hasAny = true
		}
	}
	flush := func() {
		if eventData.Len() == 0 {
			return
		}
		consume(eventData.Bytes())
		eventData.Reset()
	}
	for offset := 0; offset < len(body); {
		nl := bytes.IndexByte(body[offset:], '\n')
		var line []byte
		if nl == -1 {
			line = body[offset:]
			offset = len(body)
		} else {
			line = body[offset : offset+nl]
			offset += nl + 1
		}
		line = bytes.TrimSuffix(line, []byte{'\r'})
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			flush()
			continue
		}
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
		// Accept providers that omit the blank line between complete JSON events,
		// while still joining genuine multiline SSE data fields.
		if eventData.Len() > 0 && gjson.ValidBytes(bytes.TrimSpace(eventData.Bytes())) {
			flush()
		}
		if bytes.Equal(payload, []byte("[DONE]")) {
			flush()
			continue
		}
		if eventData.Len() > 0 {
			eventData.WriteByte('\n')
		}
		eventData.Write(payload)
	}
	flush()

	if !hasAny {
		return ActivityLogEntry{}, fmt.Errorf("no valid JSON data found in stream")
	}

	return buildMetrics(modelID, start, inputTokens, outputTokens, cachedTokens, timings, responseMetrics), nil
}

func parseMetrics(modelID string, start time.Time, usage, timings, responseMetrics gjson.Result) (ActivityLogEntry, error) {
	input, output, cached, _ := extractUsageTokens(usage)
	return buildMetrics(modelID, start, input, output, cached, timings, responseMetrics), nil
}

// applyUsageTelemetry fills Anthropic's cache-specific counters and reasoning
// tokens. OpenAI/vLLM usage remains handled by buildMetrics; these fields are
// additive and therefore safe for mixed backends.
func applyUsageTelemetry(entry *ActivityLogEntry, body []byte) {
	if entry == nil || len(body) == 0 {
		return
	}
	apply := func(usage gjson.Result) {
		if !usage.Exists() {
			return
		}
		if created := firstUsageInt(usage, "cache_creation_input_tokens", "cache_creation_tokens", "cacheCreationInputTokens", "cacheCreationTokens", "prompt_tokens_details.created_cache_tokens", "prompt_tokens_details.createdCacheTokens", "promptTokensDetails.createdCacheTokens"); created >= 0 {
			entry.CacheCreationTokens = boundedInt(created)
		}
		if reasoning := firstUsageInt(usage, "reasoning_tokens", "reasoningTokens", "output_tokens_details.reasoning_tokens", "output_tokens_details.reasoningTokens", "outputTokensDetails.reasoningTokens", "completion_tokens_details.reasoning_tokens", "completion_tokens_details.reasoningTokens", "completionTokensDetails.reasoningTokens"); reasoning > 0 {
			entry.ReasoningTokens = boundedInt(reasoning)
		}
		input := firstUsageInt(usage, "input_tokens", "inputTokens", "prompt_tokens", "promptTokens")
		output := firstUsageInt(usage, "output_tokens", "outputTokens", "completion_tokens", "completionTokens")
		uncached := firstUsageInt(usage, "uncached_input_tokens", "uncached_tokens", "input_tokens_uncached", "uncachedInputTokens", "uncachedTokens", "inputTokensUncached")
		cached := firstUsageInt(usage, "cache_read_input_tokens", "cache_read_tokens", "cacheReadInputTokens", "cacheReadTokens", "input_tokens_details.cached_tokens", "input_tokens_details.cachedTokens", "inputTokensDetails.cachedTokens", "prompt_tokens_details.cached_tokens", "prompt_tokens_details.cachedTokens", "promptTokensDetails.cachedTokens")
		created := firstUsageInt(usage, "cache_creation_input_tokens", "cache_creation_tokens", "cacheCreationInputTokens", "cacheCreationTokens", "prompt_tokens_details.created_cache_tokens", "prompt_tokens_details.createdCacheTokens", "promptTokensDetails.createdCacheTokens")
		if output >= 0 {
			entry.Tokens.OutputTokens = boundedInt(output)
		}
		if cached >= 0 {
			entry.Tokens.CachedTokens = boundedInt(cached)
		}
		if uncached >= 0 {
			// Gateways that expose the complete Anthropic partition may also
			// retain input_tokens for compatibility. Keep activity's input count
			// aligned with the explicit uncached portion so cost and hit-rate
			// consumers do not double-count the cache read/write partitions.
			entry.Tokens.InputTokens = boundedInt(uncached)
		} else if input >= 0 {
			// OpenAI-compatible usage reports total input_tokens and carries the
			// cached partition in token details. Preserve that total for the
			// existing cost/throughput paths; they subtract cached tokens when the
			// endpoint is not Anthropic Messages.
			entry.Tokens.InputTokens = boundedInt(input)
		}
		if (input >= 0 || uncached >= 0) && (cached >= 0 || created >= 0) {
			cachedKnown := cached >= 0
			createdKnown := created >= 0
			hasTopLevelCacheRead := usageHasAny(usage, "cache_read_input_tokens", "cache_read_tokens", "cacheReadInputTokens", "cacheReadTokens")
			hasNestedCacheRead := usageHasAny(usage,
				"input_tokens_details.cached_tokens", "input_tokens_details.cachedTokens", "inputTokensDetails.cachedTokens",
				"prompt_tokens_details.cached_tokens", "prompt_tokens_details.cachedTokens", "promptTokensDetails.cachedTokens")
			cachedValue := cached
			if cachedValue < 0 {
				cachedValue = 0
			}
			createdValue := created
			if createdValue < 0 {
				createdValue = 0
			}
			// Anthropic exposes cache_read_input_tokens as a separate prefix
			// partition while OpenAI-compatible APIs expose cached_tokens inside
			// total input_tokens. Use the corresponding denominator for each
			// shape so cache hit percentages are not understated.
			denominator := input
			if uncached >= 0 {
				denominator = uncached
			}
			if uncached < 0 && hasTopLevelCacheRead {
				denominator = saturatingUsageAdd(cachedValue, createdValue, input)
			}
			if uncached >= 0 {
				denominator = saturatingUsageAdd(denominator, cachedValue, createdValue)
			} else if !hasTopLevelCacheRead && !hasNestedCacheRead && createdKnown {
				// A creation-only Anthropic partition still contributes to the
				// denominator when the provider omits a cache-read field.
				denominator = saturatingUsageAdd(input, createdValue)
			}
			if denominator > 0 {
				if cachedKnown {
					ratio := float64(cachedValue) / float64(denominator)
					if ratio < 0 {
						ratio = 0
					} else if ratio > 1 {
						ratio = 1
					}
					entry.CacheHitRatio = ratio
				}
				if createdKnown {
					ratio := float64(createdValue) / float64(denominator)
					if ratio < 0 {
						ratio = 0
					} else if ratio > 1 {
						ratio = 1
					}
					entry.CacheCreationRatio = ratio
				}
			}
		}
	}
	if gjson.ValidBytes(body) {
		parsed := gjson.ParseBytes(body)
		apply(parsed.Get("usage"))
		apply(parsed.Get("message.usage"))
		apply(parsed.Get("response.usage"))
		return
	}
	var eventData bytes.Buffer
	flush := func() {
		data := bytes.TrimSpace(eventData.Bytes())
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			eventData.Reset()
			return
		}
		if gjson.ValidBytes(data) {
			parsed := gjson.ParseBytes(data)
			apply(parsed.Get("usage"))
			apply(parsed.Get("message.usage"))
			apply(parsed.Get("response.usage"))
		}
		eventData.Reset()
	}
	for offset := 0; offset < len(body); {
		nl := bytes.IndexByte(body[offset:], '\n')
		var line []byte
		if nl < 0 {
			line = body[offset:]
			offset = len(body)
		} else {
			line = body[offset : offset+nl]
			offset += nl + 1
		}
		line = bytes.TrimSuffix(line, []byte{'\r'})
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			flush()
			continue
		}
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
		if eventData.Len() > 0 && gjson.ValidBytes(bytes.TrimSpace(eventData.Bytes())) {
			flush()
		}
		if bytes.Equal(payload, []byte("[DONE]")) {
			flush()
			continue
		}
		if eventData.Len() > 0 {
			eventData.WriteByte('\n')
		}
		eventData.Write(payload)
	}
	flush()
}

func saturatingUsageAdd(values ...int64) int64 {
	var total int64
	for _, value := range values {
		if value <= 0 {
			continue
		}
		if total > math.MaxInt64-value {
			return math.MaxInt64
		}
		total += value
	}
	return total
}

func firstUsageInt(value gjson.Result, paths ...string) int64 {
	for _, path := range paths {
		if found := value.Get(path); found.Exists() {
			if parsed, ok := usageInt(found); ok {
				return parsed
			}
			// Present but unusable (null, or non-numeric): keep walking the
			// alias list. Returning here made `{"input_tokens": null,
			// "prompt_tokens": 42}` report "unknown" instead of 42 for every
			// caller that passes ordered aliases.
			continue
		}
	}
	return -1
}

func firstUsageResult(value gjson.Result, paths ...string) gjson.Result {
	for _, path := range paths {
		if found := value.Get(path); found.Exists() {
			return found
		}
	}
	return gjson.Result{}
}

func usageHasAny(value gjson.Result, paths ...string) bool {
	return firstUsageResult(value, paths...).Exists()
}

// buildMetrics composes an ActivityLogEntry from accumulated token counts and
// optional llama-server timings (which override input/output and provide rates)
// or vLLM response metrics (cache, rates and speculative decoding counters).
func buildMetrics(modelID string, start time.Time, inputTokens, outputTokens, cachedTokens int64, timings, responseMetrics gjson.Result) ActivityLogEntry {
	wallDurationMs := int(time.Since(start).Milliseconds())
	durationMs := wallDurationMs
	tokensPerSecond := -1.0
	promptPerSecond := -1.0
	draftTokens := -1
	draftAccTokens := -1
	cacheCreationTokens := 0
	cacheHitRatio := 0.0
	firstTokenMs := -1

	if timings.Exists() {
		if value, ok := usageInt(timings.Get("prompt_n")); ok {
			inputTokens = value
		}
		if value, ok := usageInt(timings.Get("predicted_n")); ok {
			outputTokens = value
		}
		if value := metricPromptRate(nonNegativeFinite(timings.Get("prompt_per_second"))); value >= 0 {
			promptPerSecond = value
		}
		if value := metricDecodeRate(nonNegativeFinite(timings.Get("predicted_per_second"))); value >= 0 {
			tokensPerSecond = value
		}
		promptMs := nonNegativeFinite(timings.Get("prompt_ms"))
		predictedMs := nonNegativeFinite(timings.Get("predicted_ms"))
		if promptMs <= math.MaxInt64-float64(durationMs) && predictedMs <= math.MaxInt64-promptMs {
			timingsDurationMs := int(promptMs + predictedMs)
			if timingsDurationMs > durationMs {
				durationMs = timingsDurationMs
			}
		}
		if cachedValue := timings.Get("cache_n"); cachedValue.Exists() {
			if value, ok := usageInt(cachedValue); ok {
				cachedTokens = value
			}
		}
		if timings.Get("draft_n").Exists() && timings.Get("draft_n_accepted").Exists() {
			if drafted, draftedOK := usageInt(timings.Get("draft_n")); draftedOK {
				if accepted, acceptedOK := usageInt(timings.Get("draft_n_accepted")); acceptedOK {
					draftTokens = boundedInt(drafted)
					draftAccTokens = boundedInt(accepted)
				}
			}
		}
	}
	// The 1Cat-vLLM OpenAI adapter emits request telemetry as flat fields on
	// the response metrics object. Keep the aliases here because some vLLM
	// releases use camelCase when the response is passed through a gateway.
	if responseMetrics.Exists() {
		if value, ok := firstMetricInt(responseMetrics, "cached_tokens", "cachedTokens", "num_cached_tokens", "numCachedTokens", "cache_tokens", "cacheTokens"); ok {
			cachedTokens = value
		}
		if value, ok := firstMetricInt(responseMetrics, "cache_creation_tokens", "cacheCreationTokens", "cache_write_tokens", "cacheWriteTokens"); ok {
			cacheCreationTokens = boundedInt(value)
		}
		if value := firstMetricFloat(responseMetrics, "cache_hit_ratio", "cacheHitRatio"); value >= 0 {
			cacheHitRatio = clampRatio(value)
		} else if cachedTokens >= 0 && inputTokens > 0 {
			cacheHitRatio = cacheHitRatioFor(inputTokens, cachedTokens)
		}

		// prefill_time_ms starts when the request is scheduled, so it is the
		// correct denominator for prompt speed. TTFT includes queueing and is
		// retained as the fallback for older vLLM responses.
		prefillMs := firstMetricFloat(responseMetrics, "prefill_time_ms", "prefillTimeMs")
		timeToFirstTokenMs := firstMetricFloat(responseMetrics, "time_to_first_token_ms", "timeToFirstTokenMs", "first_token_ms", "firstTokenMs", "ttft_ms", "ttftMs")
		if timeToFirstTokenMs > 0 {
			firstTokenMs = metricMillis(timeToFirstTokenMs)
		}
		promptRate := metricPromptRate(firstMetricFloat(responseMetrics, "prompt_per_second", "prompt_tokens_per_second", "promptPerSecond", "promptTokensPerSecond"))
		promptTokens := uncachedPromptTokens(inputTokens, cachedTokens)
		if prefillMs > 0 && promptTokens > 0 {
			promptPerSecond = metricPromptRate(float64(promptTokens) / (prefillMs / 1000))
		}
		if promptPerSecond < 0 && timeToFirstTokenMs > 0 && promptTokens > 0 {
			promptPerSecond = metricPromptRate(float64(promptTokens) / (timeToFirstTokenMs / 1000))
		}
		if promptPerSecond < 0 && promptRate >= 0 {
			promptPerSecond = promptRate
		}

		generationMs := firstMetricFloat(responseMetrics, "generation_time_ms", "generationTimeMs")
		meanInterTokenLatency := firstMetricFloat(responseMetrics, "mean_itl_ms", "meanItlMs")
		outputRate := metricDecodeRate(firstMetricFloat(responseMetrics, "tokens_per_second", "output_tokens_per_second", "tokensPerSecond", "outputTokensPerSecond"))
		// Official vLLM defines tokens_per_second as overall output throughput
		// from scheduling to the last output token. Prefer it over mean_itl_ms,
		// which is only the decode interval and has different semantics.
		if outputRate >= 0 {
			tokensPerSecond = outputRate
		} else if meanInterTokenLatency > 0 {
			tokensPerSecond = metricDecodeRate(1000 / meanInterTokenLatency)
		} else if generationMs > 0 && outputTokens > 0 {
			tokensPerSecond = metricDecodeRate(float64(outputTokens) / (generationMs / 1000))
		}

		// Use backend timings as a lower bound for duration when response
		// buffering or serialization makes wall time appear shorter.
		queueMs := firstMetricFloat(responseMetrics, "queue_time_ms", "queueTimeMs")
		metricsDurationMs := 0.0
		for _, value := range []float64{queueMs, prefillMs, generationMs} {
			if value > 0 && metricsDurationMs <= math.MaxInt64-value {
				metricsDurationMs += value
			}
		}
		if metricsDurationMs == 0 && timeToFirstTokenMs > 0 && generationMs > 0 && timeToFirstTokenMs <= math.MaxInt64-generationMs {
			metricsDurationMs = timeToFirstTokenMs + generationMs
		}
		if metricsDurationMs > float64(durationMs) && metricsDurationMs < math.Ldexp(1, 63) {
			durationMs = int(metricsDurationMs)
		}
	}
	// vLLM reports speculative decoding counts under metrics.speculative_decoding
	// when started with --per-request-spec-decode-metrics (#1032). Both counters
	// are required so the acceptance rate is never derived from a half-filled
	// object.
	if spec := responseMetrics.Get("speculative_decoding"); spec.Exists() {
		drafted := spec.Get("num_draft_tokens")
		accepted := spec.Get("num_accepted_draft_tokens")
		if drafted.Exists() && accepted.Exists() {
			if draftedValue, draftedOK := usageInt(drafted); draftedOK {
				if acceptedValue, acceptedOK := usageInt(accepted); acceptedOK {
					draftTokens = boundedInt(draftedValue)
					draftAccTokens = boundedInt(acceptedValue)
				}
			}
		}
	}
	return ActivityLogEntry{
		Timestamp: time.Now(),
		Model:     modelID,
		Tokens: TokenMetrics{
			CachedTokens:    boundedCacheInt(cachedTokens),
			DraftTokens:     draftTokens,
			DraftAccTokens:  draftAccTokens,
			InputTokens:     boundedInt(inputTokens),
			OutputTokens:    boundedInt(outputTokens),
			PromptPerSecond: promptPerSecond,
			TokensPerSecond: tokensPerSecond,
		},
		CacheCreationTokens: cacheCreationTokens,
		CacheHitRatio:       cacheHitRatio,
		DurationMs:          durationMs,
		FirstTokenMs:        firstTokenMs,
	}
}

func mergeParsedMetrics(dst, parsed *ActivityLogEntry) {
	if dst == nil || parsed == nil {
		return
	}
	dst.Tokens = parsed.Tokens
	dst.CacheCreationTokens = parsed.CacheCreationTokens
	dst.CacheHitRatio = parsed.CacheHitRatio
	dst.CacheCreationRatio = parsed.CacheCreationRatio
	dst.DurationMs = parsed.DurationMs
	dst.FirstTokenMs = parsed.FirstTokenMs
	dst.DecodeMs = parsed.DecodeMs
	dst.SpeedTimeline = parsed.SpeedTimeline
}

func elapsedMillis(start, end time.Time) int {
	if start.IsZero() || end.Before(start) {
		return -1
	}
	return metricMillis(float64(end.Sub(start).Milliseconds()))
}

func metricMillis(value float64) int {
	if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) || value >= math.Ldexp(1, 63) {
		return -1
	}
	maxInt := int64(^uint(0) >> 1)
	if value >= float64(maxInt) {
		return int(maxInt)
	}
	return int(value)
}

func firstMetricFloat(value gjson.Result, paths ...string) float64 {
	for _, path := range paths {
		if found := value.Get(path); found.Exists() {
			if parsed := nonNegativeFinite(found); parsed >= 0 {
				return parsed
			}
		}
	}
	return -1
}

func firstMetricInt(value gjson.Result, paths ...string) (int64, bool) {
	for _, path := range paths {
		if found := value.Get(path); found.Exists() {
			if parsed, ok := usageInt(found); ok {
				return parsed, true
			}
		}
	}
	return 0, false
}

func uncachedPromptTokens(inputTokens, cachedTokens int64) int64 {
	if inputTokens <= 0 {
		return 0
	}
	if cachedTokens < 0 {
		cachedTokens = 0
	}
	if cachedTokens > inputTokens {
		cachedTokens = inputTokens
	}
	return inputTokens - cachedTokens
}

func cacheHitRatioFor(inputTokens, cachedTokens int64) float64 {
	if inputTokens <= 0 || cachedTokens < 0 {
		return 0
	}
	if cachedTokens > inputTokens {
		cachedTokens = inputTokens
	}
	return clampRatio(float64(cachedTokens) / float64(inputTokens))
}

func clampRatio(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

// applyObservedStreamingRates fills in end-to-end rates when an
// OpenAI-compatible streaming backend supplies token counts but not per-request
// timings. It uses the first and last writes containing a non-blank text delta,
// rather than response headers or role-only SSE frames. These values are
// deliberately only fallbacks: backend-provided timings remain authoritative.
func applyObservedStreamingRates(entry *ActivityLogEntry, start, firstContent, lastContent time.Time) {
	if entry == nil || firstContent.IsZero() || !firstContent.After(start) || lastContent.Before(firstContent) {
		return
	}

	if entry.Tokens.PromptPerSecond < 0 {
		promptTokens := entry.Tokens.InputTokens
		if entry.Tokens.CachedTokens > 0 {
			promptTokens -= entry.Tokens.CachedTokens
		}
		if promptTokens > 0 {
			if ttft := firstContent.Sub(start); ttft >= time.Millisecond {
				entry.Tokens.PromptPerSecond = metricPromptRate(float64(promptTokens) / ttft.Seconds())
			}
		}
	}

	if entry.Tokens.TokensPerSecond < 0 && entry.Tokens.OutputTokens > 0 {
		// A sub-100ms decode window carries no signal: a single coalesced
		// flush at the end of a request used to divide the full output token
		// count by ~2ms and report hundreds of thousands of tokens/sec.
		if decode := lastContent.Sub(firstContent); decode >= 100*time.Millisecond {
			entry.Tokens.TokensPerSecond = metricDecodeRate(float64(entry.Tokens.OutputTokens) / decode.Seconds())
		}
	}
}

// Metric rates are advisory telemetry, not counters. A near-zero timing
// denominator can produce values in the millions of tokens/sec even when the
// actual request was healthy. Keep those samples out of both persisted
// activity rows and the dashboard histogram while retaining the request
// itself and its token counts.
//
// The caps are per-phase because they describe different hardware limits:
// decode is bounded by per-token latency (a single stream cannot physically
// exceed a few hundred tokens/sec, MTP/speculative included), while prompt
// processing through a prefix/KV-cache hit can legitimately reach tens of
// thousands of tokens/sec.
const (
	maxReasonableMetricRate = 1_000_000.0
	maxDecodeRate           = 2_000.0
	maxPromptRate           = 150_000.0
)

func metricRate(value float64) float64 {
	if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) || value > maxReasonableMetricRate {
		return -1
	}
	return value
}

func metricDecodeRate(value float64) float64 {
	if value < 0 || value > maxDecodeRate {
		return -1
	}
	return metricRate(value)
}

func metricPromptRate(value float64) float64 {
	if value < 0 || value > maxPromptRate {
		return -1
	}
	return metricRate(value)
}

func nonNegativeFinite(value gjson.Result) float64 {
	if !value.Exists() {
		return -1
	}
	f := value.Float()
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return -1
	}
	return f
}

func boundedInt(value int64) int {
	if value <= 0 {
		return 0
	}
	maxInt := int64(^uint(0) >> 1)
	if value > maxInt {
		return int(maxInt)
	}
	return int(value)
}

func boundedCacheInt(value int64) int {
	if value < 0 {
		return -1
	}
	return boundedInt(value)
}

// decompressBody decompresses the body based on the Content-Encoding header.
func decompressBody(body []byte, encoding string) ([]byte, error) {
	const maxDecompressedBody = swaputil.MaxRequestBodySize
	readDecoded := func(reader io.Reader) ([]byte, error) {
		decoded, err := io.ReadAll(io.LimitReader(reader, maxDecompressedBody+1))
		if err != nil {
			return nil, err
		}
		if len(decoded) > maxDecompressedBody {
			return nil, fmt.Errorf("decompressed response body exceeds %d bytes", maxDecompressedBody)
		}
		return decoded, nil
	}
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		return readDecoded(reader)
	case "deflate":
		reader := flate.NewReader(bytes.NewReader(body))
		defer reader.Close()
		return readDecoded(reader)
	default:
		if len(body) > maxDecompressedBody {
			return nil, fmt.Errorf("response body exceeds %d bytes", maxDecompressedBody)
		}
		return body, nil
	}
}

// filterAcceptEncoding filters Accept-Encoding to only gzip/deflate so response
// bodies remain decompressible for metrics parsing.
func filterAcceptEncoding(acceptEncoding string) string {
	if acceptEncoding == "" {
		return ""
	}

	supported := map[string]bool{"gzip": true, "deflate": true}
	var filtered []string
	for part := range strings.SplitSeq(acceptEncoding, ",") {
		encoding, _, _ := strings.Cut(strings.TrimSpace(part), ";")
		if supported[strings.ToLower(encoding)] {
			filtered = append(filtered, strings.TrimSpace(part))
		}
	}
	return strings.Join(filtered, ", ")
}

// responseBodyCopier tees the upstream response to the client while buffering
// it for metrics parsing. Status defaults to 200 until WriteHeader is called.
type responseBodyCopier struct {
	http.ResponseWriter
	body        *bytes.Buffer
	status      int
	wroteHeader bool
	truncated   bool
	start       time.Time
	writes      []responseBodyWrite
}

type responseBodyWrite struct {
	end int
	at  time.Time
	// hole marks the first record of the tail segment after compaction: the
	// body offsets between the previous record's end and this one were
	// compacted away, so a frame mapping into that gap has no exact timestamp.
	hole bool
}

// Write-record retention: a multi-hour SSE stream emits one timestamp record
// per Write, so an unbounded log would let a single request accumulate
// megabytes of bookkeeping. StreamingContentTimes only maps timestamps for
// the first and final content-bearing frames, so the head and tail segments
// are retained and the middle is compacted away. The compaction trigger is
// offset from the retained sizes so it runs amortized (once per tail segment
// of appends, not per Write).
const (
	responseWriteHeadRecords = 512
	responseWriteTailRecords = 512
	responseWriteCompactAt   = responseWriteHeadRecords + 2*responseWriteTailRecords
)

func newBodyCopier(w http.ResponseWriter) *responseBodyCopier {
	buf := &bytes.Buffer{}
	return &responseBodyCopier{
		ResponseWriter: w,
		body:           buf,
		status:         http.StatusOK,
		start:          time.Now(),
	}
}

func (w *responseBodyCopier) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	// On a protocol upgrade (e.g. websocket) the body is raw framed data, not a
	// metrics-parseable response, so write straight to the client without
	// buffering a copy we can't use.
	if w.status == http.StatusSwitchingProtocols {
		return w.ResponseWriter.Write(b)
	}
	// Keep the client-visible response lossless while bounding the copy used by
	// metrics, audit and capture processing. A model can legally stream a very
	// large response; retaining it all here would turn an otherwise successful
	// request into an unbounded memory allocation. Once the limit is crossed the
	// recorder marks the body incomplete and record() deliberately skips parsing
	// or persisting the partial response.
	if len(b) > 0 {
		writeAt := time.Now()
		previousLen := w.body.Len()
		remaining := swaputil.MaxRequestBodySize - w.body.Len()
		if remaining > 0 {
			if len(b) <= remaining {
				_, _ = w.body.Write(b)
			} else {
				_, _ = w.body.Write(b[:remaining])
				w.truncated = true
			}
		} else {
			w.truncated = true
		}
		if w.body.Len() > previousLen {
			w.writes = append(w.writes, responseBodyWrite{end: w.body.Len(), at: writeAt})
			if len(w.writes) >= responseWriteCompactAt {
				w.compactWriteRecords()
			}
		}
	}
	return w.ResponseWriter.Write(b)
}

func (w *responseBodyCopier) WriteHeader(statusCode int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

// MarkStatus records code for metrics without writing to the client, and
// forwards to the wrapped writer so the access-log recorder agrees.
func (w *responseBodyCopier) MarkStatus(code int) {
	w.status = code
	if marker, ok := w.ResponseWriter.(swaputil.StatusMarker); ok {
		marker.MarkStatus(code)
	}
}

// WroteHeader reports whether a response status reached the client.
func (w *responseBodyCopier) WroteHeader() bool { return w.wroteHeader }

// Flush forwards to the underlying writer so streaming responses still flush.
func (w *responseBodyCopier) Flush() {
	f, ok := w.ResponseWriter.(http.Flusher)
	if !ok {
		return
	}
	// Flushing commits the implicit 200, so the response has started even if
	// nothing wrote a header. Recorded here for the same reason as in
	// statusRecorder: the client-closed sentinel must not overwrite it.
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	f.Flush()
}

// Hijack forwards to the underlying writer so httputil.ReverseProxy can take
// over the connection for websocket upgrades.
func (w *responseBodyCopier) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, fmt.Errorf("underlying ResponseWriter does not support hijacking")
}

func (w *responseBodyCopier) Status() int          { return w.status }
func (w *responseBodyCopier) StartTime() time.Time { return w.start }

// compactWriteRecords keeps the head and tail segments of the write log,
// dropping the middle. Called once per responseWriteTailRecords appends, so
// the per-Write cost stays amortized O(1).
func (w *responseBodyCopier) compactWriteRecords() {
	kept := make([]responseBodyWrite, 0, responseWriteHeadRecords+responseWriteTailRecords)
	kept = append(kept, w.writes[:responseWriteHeadRecords]...)
	// Clear stale hole markers: the tail segment moves on every compaction,
	// so exactly one record — the new tail's first — carries the boundary.
	for i := range kept {
		kept[i].hole = false
	}
	tail := append([]responseBodyWrite(nil), w.writes[len(w.writes)-responseWriteTailRecords:]...)
	if len(tail) > 0 {
		tail[0].hole = true
	}
	kept = append(kept, tail...)
	w.writes = kept
}

// StreamingContentTimes returns the write timestamps that contain the first
// and final non-blank text deltas in a buffered SSE response. vLLM can emit
// role-only or whitespace-only frames long before generation actually begins,
// especially while a cold engine initializes; those frames must not distort
// the user-facing prompt/decode rates.
func (w *responseBodyCopier) StreamingContentTimes() (time.Time, time.Time, bool) {
	if w == nil || w.truncated || len(w.writes) == 0 {
		return time.Time{}, time.Time{}, false
	}
	return streamingContentTimes(w.body.Bytes(), w.writes)
}

func streamingContentTimes(body []byte, writes []responseBodyWrite) (time.Time, time.Time, bool) {
	var first, last time.Time
	// writeAt maps a body offset to its write timestamp. A frame whose offset
	// fell inside the compacted middle has no exact timestamp; its true time
	// lies between the previous record's time and the hole boundary. For the
	// first content frame the lower bound is used (earliest plausible), for
	// the last the upper bound, so the measured window can only come out
	// wider — never narrower — than reality. An inflated window slightly
	// understates a rate; a collapsed one produced absurd tokens/sec values.
	writeAt := func(offset int, upper bool) time.Time {
		for index, write := range writes {
			if offset > write.end {
				continue
			}
			if write.hole && index > 0 {
				if upper {
					return write.at
				}
				return writes[index-1].at
			}
			return write.at
		}
		return time.Time{}
	}

	for offset := 0; offset < len(body); {
		lineStart := offset
		newline := bytes.IndexByte(body[offset:], '\n')
		if newline < 0 {
			offset = len(body)
		} else {
			offset += newline + 1
		}
		line := bytes.TrimSpace(body[lineStart:offset])
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if !gjson.ValidBytes(payload) {
			continue
		}
		parsed := gjson.ParseBytes(payload)
		if !streamingEventHasText(parsed) {
			continue
		}
		// The first content frame takes the earliest plausible bound inside a
		// compaction hole; later frames (including the last) take the latest.
		at := writeAt(offset, !first.IsZero())
		if at.IsZero() {
			continue
		}
		if first.IsZero() {
			first = at
		}
		last = at
	}
	if first.IsZero() || last.Before(first) {
		return time.Time{}, time.Time{}, false
	}
	return first, last, true
}

// streamingTextPaths covers the text-bearing fields used by OpenAI chat
// chunks, OpenAI completion chunks, llama.cpp's native /completion SSE and the
// Responses API. Keeping this list separate from the usage parser is important:
// identifying a first visible token is a transport concern, while token counts
// still come only from explicit usage/timing telemetry.
//
// "delta" is the Responses API's per-token field (response.output_text.delta /
// response.reasoning_text.delta). Without it only the terminal *.done frames
// match — and those are end-of-phase markers, so the measured interval
// collapsed to the gap between the end of reasoning and the end of the answer
// while the numerator still counted every output token. That inflated the
// decode rate by roughly an order of magnitude for /v1/responses requests.
var streamingTextPaths = []string{
	"choices.#.delta.content",
	"choices.#.delta.reasoning_content",
	"choices.#.delta.reasoning",
	"choices.#.text",
	"delta",
	"content",
	"text",
	"output_text",
}

func streamingEventHasText(parsed gjson.Result) bool {
	for _, path := range streamingTextPaths {
		if gjsonResultHasText(parsed.Get(path)) {
			return true
		}
	}
	return false
}

func gjsonResultHasText(value gjson.Result) bool {
	if !value.Exists() {
		return false
	}
	if value.Type == gjson.String {
		return strings.TrimSpace(value.String()) != ""
	}
	found := false
	value.ForEach(func(_, child gjson.Result) bool {
		if child.Type == gjson.String && strings.TrimSpace(child.String()) != "" {
			found = true
			return false
		}
		return true
	})
	return found
}
