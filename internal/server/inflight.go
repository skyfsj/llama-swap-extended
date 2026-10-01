package server

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

const inflightUpdateInterval = 250 * time.Millisecond
const inflightOutboxSize = 128

const (
	inflightOperationSnapshot = "snapshot"
	inflightOperationUpsert   = "upsert"
	inflightOperationRemove   = "remove"
)

type inflightStartContextKey struct{}

func markInflightStart(r *http.Request) *http.Request {
	if _, ok := r.Context().Value(inflightStartContextKey{}).(time.Time); ok {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), inflightStartContextKey{}, time.Now()))
}

func inflightStart(r *http.Request) time.Time {
	if started, ok := r.Context().Value(inflightStartContextKey{}).(time.Time); ok {
		return started
	}
	return time.Now()
}

// inflightTracker tracks in-flight model-dispatched requests and their
// cancellable contexts.
type inflightTracker struct {
	nextID atomic.Uint64

	// mu serializes state mutations and their corresponding outbox writes so
	// request updates keep the same order in which they were applied.
	mu       sync.RWMutex
	requests map[string]*inflightRequest

	updates          chan swaputil.InFlightRequestsEvent
	needsSnapshot    atomic.Bool
	publisherRunning atomic.Bool
	publish          func(swaputil.InFlightRequestsEvent)
	progressMu       sync.RWMutex
	progress         func(swaputil.BackendProgressEvent)
}

type inflightRequest struct {
	entry        swaputil.InflightRequestEntry
	cancel       context.CancelFunc
	lastEmitted  time.Time
	timer        *time.Timer
	outputParser inflightOutputParser
	rateSamples  inflightRateSamples
}

// inflightRateSamples stores the last explicit upstream counters seen for an
// active request. The live table reports the latest counter delta, not a
// request-lifetime average. Prompt samples are usable only after both the
// total and cached prompt counters are known; otherwise the uncached portion
// cannot be determined safely.
type inflightRateSamples struct {
	promptAt     time.Time
	promptTokens int64
	hasPrompt    bool
	outputAt     time.Time
	outputTokens int64
	hasOutput    bool
}

func newInflightTracker() *inflightTracker {
	return newInflightTrackerWithPublisher(inflightOutboxSize, func(update swaputil.InFlightRequestsEvent) {
		event.Emit(update)
	})
}

func newInflightTrackerWithPublisher(size int, publish func(swaputil.InFlightRequestsEvent)) *inflightTracker {
	t := &inflightTracker{
		requests: make(map[string]*inflightRequest),
		updates:  make(chan swaputil.InFlightRequestsEvent, size),
		publish:  publish,
	}
	return t
}

// SetProgressPublisher connects request lifecycle telemetry to the common
// BackendProgress stream. The publisher is optional so embedders and focused
// tracker tests can keep the tracker independent from the event bus. It is
// normally configured once during Server construction, before requests are
// accepted.
func (t *inflightTracker) SetProgressPublisher(publish func(swaputil.BackendProgressEvent)) {
	if t == nil {
		return
	}
	t.progressMu.Lock()
	t.progress = publish
	t.progressMu.Unlock()
}

func (t *inflightTracker) publishInferenceProgress(model, phase string, progress float64, message, failure string) {
	if t == nil || strings.TrimSpace(model) == "" {
		return
	}
	t.progressMu.RLock()
	publish := t.progress
	t.progressMu.RUnlock()
	if publish == nil {
		return
	}
	publish(swaputil.BackendProgressEvent{
		Model:    model,
		Phase:    phase,
		Progress: progress,
		Message:  message,
		Error:    failure,
	})
}

func (t *inflightTracker) Add(r *http.Request, cancel context.CancelFunc) string {
	id := strconv.FormatUint(t.nextID.Add(1), 10)
	entry := swaputil.InflightRequestEntry{
		ID:           id,
		Timestamp:    inflightStart(r),
		ReqPath:      r.URL.Path,
		Method:       r.Method,
		Phase:        "waiting",
		PhaseMessage: "waiting for process readiness",
		ReqHeaders:   headerMap(r.Header),
		RemoteIP:     clientIP(r),
		RespHeaders:  map[string]string{},
	}
	redactHeaders(entry.ReqHeaders)
	if data, ok := swaputil.ReadContext(r.Context()); ok {
		entry.Model = data.ModelID
		entry.Metadata = copyMetadata(data.Metadata)
	}

	t.mu.Lock()
	req := &inflightRequest{entry: entry, cancel: cancel, lastEmitted: time.Now()}
	t.requests[id] = req
	t.enqueueLocked(upsertInflightEvent(req.entry))
	t.mu.Unlock()
	return id
}

func (t *inflightTracker) Remove(id string) {
	t.mu.Lock()
	req, ok := t.requests[id]
	if ok {
		if req.timer != nil {
			req.timer.Stop()
			req.timer = nil
		}
		// The last SSE chunk commonly carries the only usage/timing object. It
		// arrives immediately before the handler returns, so it is often still
		// behind the 250ms byte-update throttle. Publish that final state before
		// removing the request; otherwise the UI can only ever see empty live
		// rates even though the completed activity row is populated.
		if inflightEntryHasTelemetry(req.entry) {
			t.enqueueLocked(upsertInflightEvent(req.entry))
		}
		delete(t.requests, id)
		t.enqueueLocked(swaputil.InFlightRequestsEvent{Operation: inflightOperationRemove, ID: id})
	}
	t.mu.Unlock()
}

func (t *inflightTracker) SetResponseHeaders(id string, headers http.Header) {
	values := headerMap(headers)
	redactHeaders(values)

	t.mu.Lock()
	req, ok := t.requests[id]
	if ok {
		req.entry.RespHeaders = values
		req.lastEmitted = time.Now()
		t.enqueueLocked(upsertInflightEvent(req.entry))
	}
	t.mu.Unlock()
}

// SetPhase updates the lifecycle phase of one active request and emits it
// immediately. Phase changes are sparse compared with response byte updates,
// so they should not wait for the byte-update throttle.
func (t *inflightTracker) SetPhase(id, phase, message string) {
	t.setPhase(id, phase, message, true)
}

func (t *inflightTracker) setPhase(id, phase, message string, emit bool) {
	if t == nil || strings.TrimSpace(id) == "" {
		return
	}
	phase = strings.TrimSpace(phase)
	if phase == "" {
		return
	}
	t.mu.Lock()
	req, ok := t.requests[id]
	if ok {
		req.entry.Phase = phase
		req.entry.PhaseMessage = strings.TrimSpace(message)
		if emit {
			req.lastEmitted = time.Now()
			t.enqueueLocked(upsertInflightEvent(req.entry))
		}
	}
	t.mu.Unlock()
}

func (t *inflightTracker) AddResponseBytes(id string, total int) {
	t.observeResponseBytes(id, total, nil)
}

func (t *inflightTracker) observeResponseBytes(id string, total int, payload []byte) {
	if total <= 0 && len(payload) == 0 {
		return
	}

	t.mu.Lock()
	req, ok := t.requests[id]
	if !ok {
		t.mu.Unlock()
		return
	}
	now := time.Now()
	req.entry.RespBytes += int64(total)
	if len(payload) > 0 {
		req.entry.OutputPreview = req.outputParser.Append(payload)
		telemetry := req.outputParser.Telemetry()
		applyInflightTelemetry(&req.entry, telemetry)
		applyInflightCurrentRates(&req.entry, telemetry, now, &req.rateSamples)
	}

	remaining := inflightUpdateInterval - now.Sub(req.lastEmitted)
	if remaining <= 0 && req.timer == nil {
		req.lastEmitted = now
		t.enqueueLocked(upsertInflightEvent(req.entry))
		t.mu.Unlock()
		return
	}
	if req.timer == nil {
		if remaining < 0 {
			remaining = 0
		}
		req.timer = time.AfterFunc(remaining, func() { t.emitPending(id) })
	}
	t.mu.Unlock()
}

func (t *inflightTracker) emitPending(id string) {
	t.mu.Lock()
	req, ok := t.requests[id]
	if !ok {
		t.mu.Unlock()
		return
	}
	req.timer = nil
	req.lastEmitted = time.Now()
	t.enqueueLocked(upsertInflightEvent(req.entry))
	t.mu.Unlock()
}

// enqueueLocked adds an update without allowing event-bus backpressure to
// block the request path. On overflow, a later snapshot replaces any dropped
// incremental updates with the tracker's authoritative state.
func (t *inflightTracker) enqueueLocked(update swaputil.InFlightRequestsEvent) {
	select {
	case t.updates <- update:
	default:
		t.needsSnapshot.Store(true)
	}
	t.startPublisher()
}

func (t *inflightTracker) startPublisher() {
	if t.publisherRunning.CompareAndSwap(false, true) {
		go t.publishUpdates()
	}
}

func (t *inflightTracker) publishUpdates() {
	for {
		select {
		case update := <-t.updates:
			t.publish(refreshInflightElapsed(update))
			t.publishRecoverySnapshots()
		default:
			t.publisherRunning.Store(false)
			// An enqueue racing with the transition to idle either starts a new
			// publisher or leaves work here for this publisher to reclaim.
			if len(t.updates) > 0 && t.publisherRunning.CompareAndSwap(false, true) {
				continue
			}
			return
		}
	}
}

func (t *inflightTracker) publishRecoverySnapshots() {
	for t.needsSnapshot.Swap(false) {
		t.discardQueuedUpdates()
		t.publish(t.Current())
	}
}

func (t *inflightTracker) discardQueuedUpdates() {
	for {
		select {
		case <-t.updates:
		default:
			return
		}
	}
}

func (t *inflightTracker) Cancel(id string) bool {
	t.mu.RLock()
	req, ok := t.requests[id]
	t.mu.RUnlock()
	if !ok {
		return false
	}
	req.cancel()
	return true
}

// CancelModel cancels every currently tracked request for modelID. The
// cancellation functions are copied under the read lock and invoked after it
// is released so request cleanup can safely remove entries while cancellation
// propagates through the HTTP stack.
func (t *inflightTracker) CancelModel(modelID string) int {
	if t == nil || strings.TrimSpace(modelID) == "" {
		return 0
	}
	t.mu.RLock()
	cancels := make([]context.CancelFunc, 0)
	for _, req := range t.requests {
		if req.entry.Model == modelID && req.cancel != nil {
			cancels = append(cancels, req.cancel)
		}
	}
	t.mu.RUnlock()
	for _, cancel := range cancels {
		cancel()
	}
	return len(cancels)
}

// CancelModelAndRemove cancels every tracked request for modelID and removes
// its rows from the authoritative tracker immediately. Force restart uses this
// stronger variant because the old generation is intentionally discarded; the
// later handler cleanup is allowed to become a no-op.
func (t *inflightTracker) CancelModelAndRemove(modelID string) int {
	if t == nil || strings.TrimSpace(modelID) == "" {
		return 0
	}
	t.mu.Lock()
	cancels := make([]context.CancelFunc, 0)
	removed := 0
	for id, req := range t.requests {
		if req.entry.Model != modelID {
			continue
		}
		if req.timer != nil {
			req.timer.Stop()
			req.timer = nil
		}
		if req.cancel != nil {
			cancels = append(cancels, req.cancel)
		}
		delete(t.requests, id)
		t.enqueueLocked(swaputil.InFlightRequestsEvent{Operation: inflightOperationRemove, ID: id})
		removed++
	}
	t.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	return removed
}

func (t *inflightTracker) Current() swaputil.InFlightRequestsEvent {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return swaputil.InFlightRequestsEvent{
		Operation: inflightOperationSnapshot,
		Requests:  t.snapshotLocked(),
	}
}

func (t *inflightTracker) snapshotLocked() []swaputil.InflightRequestEntry {
	requests := make([]swaputil.InflightRequestEntry, 0, len(t.requests))
	for _, req := range t.requests {
		requests = append(requests, copyInflightEntry(req.entry))
	}
	sort.Slice(requests, func(i, j int) bool {
		if requests[i].Timestamp.Equal(requests[j].Timestamp) {
			return requests[i].ID < requests[j].ID
		}
		return requests[i].Timestamp.Before(requests[j].Timestamp)
	})
	return requests
}

func upsertInflightEvent(entry swaputil.InflightRequestEntry) swaputil.InFlightRequestsEvent {
	entry = copyInflightEntry(entry)
	return swaputil.InFlightRequestsEvent{Operation: inflightOperationUpsert, Request: &entry}
}

func copyInflightEntry(entry swaputil.InflightRequestEntry) swaputil.InflightRequestEntry {
	entry.Metadata = copyMetadata(entry.Metadata)
	entry.ReqHeaders = copyStringMap(entry.ReqHeaders)
	entry.RespHeaders = copyStringMap(entry.RespHeaders)
	setInflightElapsed(&entry)
	return entry
}

func applyInflightTelemetry(entry *swaputil.InflightRequestEntry, telemetry inflightTokenTelemetry) {
	if entry == nil {
		return
	}
	if telemetry.hasInput {
		entry.InputTokens = telemetry.inputTokens
	}
	if telemetry.hasOutput {
		entry.OutputTokens = telemetry.outputTokens
	}
	if telemetry.hasCached {
		entry.CachedTokens = telemetry.cachedTokens
	}
	if telemetry.hasPromptDuration {
		promptTokens := entry.InputTokens
		if telemetry.promptUsesUncached && !telemetry.hasCached {
			promptTokens = 0
		} else if telemetry.promptUsesUncached {
			promptTokens = uncachedPromptTokens(entry.InputTokens, entry.CachedTokens)
		}
		if promptTokens > 0 {
			entry.PromptPerSecond = metricRate(float64(promptTokens) / (telemetry.promptMs / 1000))
		}
	} else if telemetry.hasPromptRate {
		entry.PromptPerSecond = telemetry.promptPerSecond
	}
	if telemetry.hasDecodeDuration && entry.OutputTokens > 0 {
		entry.TokensPerSecond = metricRate(float64(entry.OutputTokens) / (telemetry.decodeMs / 1000))
	} else if telemetry.hasTokenRate {
		entry.TokensPerSecond = telemetry.tokensPerSecond
	}
	if telemetry.hasFirstToken && telemetry.firstTokenMs > 0 {
		entry.FirstTokenMs = telemetry.firstTokenMs
	}
}

func inflightEntryHasTelemetry(entry swaputil.InflightRequestEntry) bool {
	return entry.OutputPreview != "" ||
		entry.InputTokens > 0 ||
		entry.OutputTokens > 0 ||
		entry.CachedTokens > 0 ||
		entry.PromptPerSecond > 0 ||
		entry.TokensPerSecond > 0 ||
		entry.FirstTokenMs > 0
}

// applyInflightCurrentRates derives the current rate from the latest explicit
// counter delta when the upstream did not provide its own rate or duration.
// It never uses response bytes or text as a token count. A first counter is
// only a baseline, so it cannot produce a rate until a second counter arrives.
func applyInflightCurrentRates(entry *swaputil.InflightRequestEntry, telemetry inflightTokenTelemetry, at time.Time, samples *inflightRateSamples) {
	if entry == nil || samples == nil || at.IsZero() {
		return
	}

	if !telemetry.hasPromptRate && !telemetry.hasPromptDuration && telemetry.hasInput && telemetry.hasCached {
		promptTokens := uncachedPromptTokens(telemetry.inputTokens, telemetry.cachedTokens)
		if samples.hasPrompt {
			if promptTokens >= samples.promptTokens && at.Sub(samples.promptAt) >= time.Millisecond {
				if delta := promptTokens - samples.promptTokens; delta > 0 {
					if rate := metricRate(float64(delta) / at.Sub(samples.promptAt).Seconds()); rate >= 0 {
						entry.PromptPerSecond = rate
					}
				}
			}
		}
		samples.promptAt = at
		samples.promptTokens = promptTokens
		samples.hasPrompt = true
	}

	if !telemetry.hasTokenRate && !telemetry.hasDecodeDuration && telemetry.hasOutput && telemetry.outputTokens > 0 {
		if samples.hasOutput {
			if telemetry.outputTokens >= samples.outputTokens && at.Sub(samples.outputAt) >= time.Millisecond {
				if delta := telemetry.outputTokens - samples.outputTokens; delta > 0 {
					if rate := metricRate(float64(delta) / at.Sub(samples.outputAt).Seconds()); rate >= 0 {
						entry.TokensPerSecond = rate
					}
				}
			}
		}
		samples.outputAt = at
		samples.outputTokens = telemetry.outputTokens
		samples.hasOutput = true
	}
}

func refreshInflightElapsed(update swaputil.InFlightRequestsEvent) swaputil.InFlightRequestsEvent {
	if update.Request != nil {
		entry := *update.Request
		setInflightElapsed(&entry)
		update.Request = &entry
	}
	for i := range update.Requests {
		setInflightElapsed(&update.Requests[i])
	}
	return update
}

func setInflightElapsed(entry *swaputil.InflightRequestEntry) {
	elapsed := time.Since(entry.Timestamp)
	if elapsed < 0 {
		elapsed = 0
	}
	entry.ElapsedMs = elapsed.Milliseconds()
}

func copyStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func copyMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}
	out := make(map[string]string, len(metadata))
	for k, v := range metadata {
		out[k] = v
	}
	return out
}

type inflightResponseWriter struct {
	http.ResponseWriter
	tracker     *inflightTracker
	id          string
	wroteHeader bool
	status      int
	model       string
	progress    bool
	writeErr    error
}

func (w *inflightResponseWriter) WriteHeader(statusCode int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = statusCode
	w.tracker.SetResponseHeaders(w.id, w.Header())
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *inflightResponseWriter) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(data)
	if err != nil && w.writeErr == nil {
		w.writeErr = err
	}
	// Only report generation once the wrapped writer confirms that bytes made
	// it to the client. A writer can reject the first non-empty payload with
	// n=0; classifying that as generation would hide the write-error terminal
	// state and falsely claim that the backend produced visible output.
	if n > 0 && !w.progress {
		w.progress = true
		// Fold the first output phase into the next byte update. This keeps the
		// existing response-byte update cadence while making the phase visible
		// atomically with the first bytes received.
		w.tracker.setPhase(w.id, "decode", "decode output started", false)
		w.tracker.publishInferenceProgress(w.model, "generation", 0, "first response bytes received", "")
	}
	if n < 0 {
		n = 0
	}
	if n > len(data) {
		n = len(data)
	}
	w.tracker.observeResponseBytes(w.id, n, data[:n])
	return n, err
}

// MarkStatus forwards a recorded-only status to the wrapped writer. The
// tracker records bytes and headers rather than a status code, so there is
// nothing to update here.
func (w *inflightResponseWriter) MarkStatus(code int) {
	w.status = code
	if marker, ok := w.ResponseWriter.(swaputil.StatusMarker); ok {
		marker.MarkStatus(code)
	}
}

// WroteHeader reports whether a response status reached the client.
func (w *inflightResponseWriter) WroteHeader() bool { return w.wroteHeader }

// finishProgress emits the terminal request phase after downstream handling
// returns. A response body may be empty (for example a 204 or an upstream
// error), so prefill completion remains observable even when generation never
// produced bytes. The event deliberately carries no request/session ID.
func (w *inflightResponseWriter) finishProgress() {
	if w == nil || w.tracker == nil || strings.TrimSpace(w.model) == "" {
		return
	}
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	if w.writeErr != nil {
		w.tracker.publishInferenceProgress(w.model, "error", 0, "inference response write failed", w.writeErr.Error())
		return
	}
	if status >= http.StatusBadRequest {
		w.tracker.publishInferenceProgress(w.model, "error", 0, "inference request failed", fmt.Sprintf("upstream returned HTTP %d", status))
		return
	}
	if w.progress {
		w.tracker.publishInferenceProgress(w.model, "generation", 1, "inference request completed", "")
		return
	}
	w.tracker.publishInferenceProgress(w.model, "prefill", 1, "inference request completed without response body", "")
}

func (w *inflightResponseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *inflightResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hijacker, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hijacker.Hijack()
	}
	return nil, nil, fmt.Errorf("underlying ResponseWriter does not support hijacking")
}

// CreateInflightMiddleware returns middleware that tracks model-dispatched
// requests until downstream handling completes.
func CreateInflightMiddleware(t *inflightTracker, cfg config.Config) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if swaputil.ShouldIgnoreWebsocket(r, cfg) {
				next.ServeHTTP(w, r)
				return
			}

			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()

			r = r.WithContext(ctx)
			id := t.Add(r, cancel)
			r = r.WithContext(swaputil.WithInferencePhaseReporter(r.Context(), func(phase, message string) {
				t.SetPhase(id, phase, message)
			}))
			data, _ := swaputil.ReadContext(r.Context())
			model := data.ModelID
			if model != "" {
				t.publishInferenceProgress(model, "prefill", 0, "inference request started", "")
			}
			writer := &inflightResponseWriter{ResponseWriter: w, tracker: t, id: id, status: http.StatusOK, model: model}
			defer func() {
				writer.finishProgress()
				t.Remove(id)
			}()

			next.ServeHTTP(writer, r)
		})
	}
}

// CreateUpstreamInflightMiddleware tracks /upstream/<model>/<path> requests
// only when the stripped upstream path is one of the model-dispatched
// inference endpoints.
func CreateUpstreamInflightMiddleware(t *inflightTracker, cfg config.Config) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.URL.Path, "/upstream/") {
				next.ServeHTTP(w, r)
				return
			}
			r = markInflightStart(r)

			_, _, remainingPath, found := swaputil.FindModelInPath(cfg, strings.TrimPrefix(r.URL.Path, "/upstream"))
			// A request that is not a model-dispatched inference call is an
			// explicit start: the UI's load button reaches a model through this
			// route. Marking it before the early return lets maintenance mode
			// refuse inference while still allowing the operator to perform the
			// start that clears the state.
			if !found || !isModelDispatchedRequest(r.Method, remainingPath) {
				if found {
					r = r.WithContext(swaputil.WithOperatorStart(r.Context()))
				}
				next.ServeHTTP(w, r)
				return
			}

			if _, err := swaputil.FetchContext(r, cfg); err != nil {
				next.ServeHTTP(w, r)
				return
			}
			if swaputil.ShouldIgnoreWebsocket(r, cfg) {
				next.ServeHTTP(w, r)
				return
			}

			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()

			r = r.WithContext(ctx)
			tracked := r.Clone(ctx)
			tracked.URL.Path = remainingPath
			id := t.Add(tracked, cancel)
			r = r.WithContext(swaputil.WithInferencePhaseReporter(r.Context(), func(phase, message string) {
				t.SetPhase(id, phase, message)
			}))
			data, _ := swaputil.ReadContext(tracked.Context())
			model := data.ModelID
			if model != "" {
				t.SetPhase(id, "prefill", "prefill started")
				t.publishInferenceProgress(model, "prefill", 0, "inference request started", "")
			}
			writer := &inflightResponseWriter{ResponseWriter: w, tracker: t, id: id, status: http.StatusOK, model: model}
			defer func() {
				writer.finishProgress()
				t.Remove(id)
			}()

			next.ServeHTTP(writer, r)
		})
	}
}

func isModelDispatchedRequest(method, path string) bool {
	switch method {
	case http.MethodPost:
		for _, p := range modelPostJSONRoutes {
			if p == path {
				return true
			}
		}
		for _, p := range modelPostFormRoutes {
			if p == path {
				return true
			}
		}
	case http.MethodGet:
		for _, p := range modelGetRoutes {
			if p == path {
				return true
			}
		}
	}
	return false
}
