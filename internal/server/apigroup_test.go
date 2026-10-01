package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/hw"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/router/scheduler"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/tidwall/gjson"
)

type lifecycleStatusStubRouter struct {
	*stubRouter
	statuses map[string]scheduler.ModelLifecycleStatus
}

type loadConflictStubRouter struct {
	*stubRouter
	conflicts []string
}

func (r *loadConflictStubRouter) LoadConflicts(string) []string {
	return append([]string(nil), r.conflicts...)
}

func (r *lifecycleStatusStubRouter) ModelLifecycleStatuses() map[string]scheduler.ModelLifecycleStatus {
	return r.statuses
}

func TestServer_ModelStatusRedactsLifecycleError(t *testing.T) {
	secret := "--api-key=do-not-expose"
	local := &lifecycleStatusStubRouter{
		stubRouter: newStubRouter([]string{"m"}, ""),
		statuses: map[string]scheduler.ModelLifecycleStatus{
			"m": {ConfigStatus: scheduler.ConfigStatusApplyFailed, Error: "starting command " + secret + " failed"},
		},
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	s.setConfig(config.Config{Models: map[string]config.ModelConfig{"m": {}}})

	status := s.modelStatus()
	if len(status) != 1 {
		t.Fatalf("model status=%+v want one model", status)
	}
	if got := status[0].ConfigError; got != configLifecycleErrorCode {
		t.Fatalf("public config error=%q want %q", got, configLifecycleErrorCode)
	}
	if strings.Contains(status[0].ConfigError, secret) {
		t.Fatalf("public config error leaked secret: %q", status[0].ConfigError)
	}
}

func TestServer_ModelStatusExposesOnlySafeExitStatus(t *testing.T) {
	got := publicConfigLifecycleError("upstream command --api-key=do-not-expose failed: exit status 2")
	if got != configLifecycleExitStatusPrefix+"2" {
		t.Fatalf("public config error=%q, want safe exit-status code", got)
	}
	if strings.Contains(got, "do-not-expose") {
		t.Fatalf("public config error leaked command data: %q", got)
	}
}

func TestServer_HandleAPIModelLoadConflicts(t *testing.T) {
	local := &loadConflictStubRouter{
		stubRouter: newStubRouter([]string{"target/model", "old/model"}, ""),
		conflicts:  []string{"old/model", "not-running"},
	}
	local.running = map[string]process.ProcessState{
		"old/model": process.StateReady,
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	s.setConfig(config.Config{Models: map[string]config.ModelConfig{
		"target/model": {Name: "Target"},
		"old/model":    {Name: "Old model"},
	}})

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/models/conflicts/target%2Fmodel", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q, want 200", w.Code, w.Body.String())
	}
	var payload struct {
		Model     string                 `json:"model"`
		Conflicts []apiModelLoadConflict `json:"conflicts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode conflicts response: %v", err)
	}
	if payload.Model != "target/model" {
		t.Fatalf("model=%q, want target/model", payload.Model)
	}
	if len(payload.Conflicts) != 1 || payload.Conflicts[0].ID != "old/model" || payload.Conflicts[0].Name != "Old model" || payload.Conflicts[0].State != string(process.StateReady) {
		t.Fatalf("conflicts=%+v, want only running old/model", payload.Conflicts)
	}
}

func TestServer_ActiveProfileRejectsOversizedJSONBody(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	padding := bytes.Repeat([]byte{'x'}, maxAPIKeyJSONBody+1)
	request := httptest.NewRequest(http.MethodPut, "/api/profiles/active", bytes.NewReader(append([]byte(`{"name":"`), append(padding, []byte(`"}`)...)...)))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	s.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%q, want 400", response.Code, response.Body.String())
	}
	if got := s.ActiveProfile(); got != "" {
		t.Fatalf("active profile changed after oversized request: %q", got)
	}
}

func TestServer_InflightMiddleware_AddsAndRemovesEntriesAroundRequestHandling(t *testing.T) {
	tracker := newInflightTracker()
	mw := CreateInflightMiddleware(tracker, config.Config{})

	var duringRequest swaputil.InFlightRequestsEvent
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		duringRequest = tracker.Current()
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	started := time.Now().Add(-time.Second)
	req = req.WithContext(context.WithValue(req.Context(), inflightStartContextKey{}, started))
	req.Header.Set("User-Agent", "test-agent")
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	req = req.WithContext(swaputil.SetContext(req.Context(), swaputil.ReqContextData{
		Model:    "requested-model",
		ModelID:  "resolved-model",
		Metadata: map[string]string{"source": "test"},
	}))

	handler.ServeHTTP(httptest.NewRecorder(), req)

	if len(duringRequest.Requests) != 1 {
		t.Fatalf("inflight requests during request = %d, want 1", len(duringRequest.Requests))
	}
	entry := duringRequest.Requests[0]
	if entry.ID == "" || entry.Model != "resolved-model" || entry.Method != http.MethodPost || entry.ReqPath != "/v1/chat/completions" {
		t.Errorf("inflight entry = %+v", entry)
	}
	if entry.Phase != "waiting" || entry.PhaseMessage == "" {
		t.Errorf("initial phase = %q (%q), want waiting with a message", entry.Phase, entry.PhaseMessage)
	}
	if !entry.Timestamp.Equal(started) {
		t.Errorf("timestamp = %v, want request start %v", entry.Timestamp, started)
	}
	if entry.ElapsedMs < 1000 {
		t.Errorf("elapsed ms = %d, want at least 1000", entry.ElapsedMs)
	}
	if entry.Metadata["source"] != "test" {
		t.Errorf("metadata = %v, want source=test", entry.Metadata)
	}
	if entry.RemoteIP != "203.0.113.9" {
		t.Errorf("remote ip = %q, want 203.0.113.9", entry.RemoteIP)
	}
	if entry.ReqHeaders["User-Agent"] != "test-agent" || entry.ReqHeaders["Authorization"] != "[REDACTED]" {
		t.Errorf("request headers = %v", entry.ReqHeaders)
	}
	if got := tracker.Current(); len(got.Requests) != 0 {
		t.Errorf("inflight after request = %+v, want empty", got)
	}
}

func TestServer_InflightMiddleware_IgnoresConfiguredWebsocket(t *testing.T) {
	tracker := newInflightTracker()
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"m1": {Compat: config.CompatConfig{IgnoreWebsockets: true}},
	}}
	var duringRequest swaputil.InFlightRequestsEvent
	handler := CreateInflightMiddleware(tracker, cfg)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		duringRequest = tracker.Current()
		w.WriteHeader(http.StatusSwitchingProtocols)
	}))

	req := httptest.NewRequest(http.MethodGet, "/props?model=m1", nil)
	req.Header.Set("Connection", "keep-alive, Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req = req.WithContext(swaputil.SetContext(req.Context(), swaputil.ReqContextData{Model: "m1", ModelID: "m1"}))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if len(duringRequest.Requests) != 0 {
		t.Fatalf("inflight during ignored websocket = %+v, want empty", duringRequest)
	}
}

func TestServer_InflightMiddleware_StreamsResponseUpdates(t *testing.T) {
	events := make(chan swaputil.InFlightRequestsEvent, 8)
	tracker := newInflightTrackerWithPublisher(8, func(update swaputil.InFlightRequestsEvent) {
		events <- update
	})

	release := make(chan struct{})
	done := make(chan struct{})
	handler := CreateInflightMiddleware(tracker, config.Config{})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Set-Cookie", "secret=value")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello"))
		w.(http.Flusher).Flush()
		<-release
	}))

	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req = req.WithContext(swaputil.SetContext(req.Context(), swaputil.ReqContextData{ModelID: "m1"}))
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}()

	_ = waitInflightEvent(t, events, inflightOperationUpsert)
	headers := waitInflightEvent(t, events, inflightOperationUpsert)
	if headers.Request == nil || headers.Request.RespHeaders["Content-Type"] != "text/event-stream" {
		t.Fatalf("response headers event = %+v", headers)
	}
	if headers.Request.RespHeaders["Set-Cookie"] != "[REDACTED]" {
		t.Errorf("response headers = %v", headers.Request.RespHeaders)
	}

	// Consume the phase-transition upserts that publish before the
	// response-bytes throttle fires and wait for the bytes update itself.
	bytesUpdate := waitInflightEventPredicate(t, events, func(e swaputil.InFlightRequestsEvent) bool {
		return e.Operation == inflightOperationUpsert && e.Request != nil && e.Request.RespBytes == 5
	})
	if bytesUpdate.Request == nil || bytesUpdate.Request.RespBytes != 5 {
		t.Errorf("response bytes event = %+v, want 5", bytesUpdate.Request)
	}

	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return")
	}
	removed := waitInflightEvent(t, events, inflightOperationRemove)
	if removed.ID == "" {
		t.Error("remove event missing id")
	}
}

func TestServer_InflightMiddleware_ReportsInferencePhases(t *testing.T) {
	events := make(chan swaputil.InFlightRequestsEvent, 8)
	tracker := newInflightTrackerWithPublisher(8, func(update swaputil.InFlightRequestsEvent) {
		events <- update
	})
	release := make(chan struct{})
	handler := CreateInflightMiddleware(tracker, config.Config{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		swaputil.ReportInferencePhase(r.Context(), "prefill", "prefill started")
		_, _ = w.Write([]byte("hello"))
		<-release
	}))

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req = req.WithContext(swaputil.SetContext(req.Context(), swaputil.ReqContextData{ModelID: "m1"}))
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}()

	initial := waitInflightEvent(t, events, inflightOperationUpsert)
	if initial.Request == nil || initial.Request.Phase != "waiting" {
		t.Fatalf("initial inflight phase = %+v, want waiting", initial.Request)
	}
	prefill := waitInflightEvent(t, events, inflightOperationUpsert)
	if prefill.Request == nil || prefill.Request.Phase != "prefill" {
		t.Fatalf("prefill inflight phase = %+v, want prefill", prefill.Request)
	}
	decode := waitInflightEventPredicate(t, events, func(update swaputil.InFlightRequestsEvent) bool {
		return update.Operation == inflightOperationUpsert && update.Request != nil && update.Request.Phase == "decode"
	})
	if decode.Request.RespBytes != 5 {
		t.Errorf("decode phase bytes = %d, want 5", decode.Request.RespBytes)
	}

	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("phase-tracked handler did not return")
	}
}

func TestServer_InflightMiddleware_PublishesInferenceProgress(t *testing.T) {
	progressEvents := make(chan swaputil.BackendProgressEvent, 4)
	tracker := newInflightTracker()
	tracker.SetProgressPublisher(func(progress swaputil.BackendProgressEvent) {
		progressEvents <- progress
	})
	handler := CreateInflightMiddleware(tracker, config.Config{})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = req.WithContext(swaputil.SetContext(req.Context(), swaputil.ReqContextData{ModelID: "m1"}))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	got := make([]swaputil.BackendProgressEvent, 0, 3)
	for range 3 {
		select {
		case progress := <-progressEvents:
			got = append(got, progress)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for inference progress, got %+v", got)
		}
	}
	if got[0].Model != "m1" || got[0].Phase != "prefill" || got[0].Progress != 0 {
		t.Errorf("start progress = %+v, want m1/prefill/0", got[0])
	}
	if got[1].Model != "m1" || got[1].Phase != "generation" || got[1].Progress != 0 {
		t.Errorf("generation progress = %+v, want m1/generation/0", got[1])
	}
	if got[2].Model != "m1" || got[2].Phase != "generation" || got[2].Progress != 1 || got[2].Error != "" {
		t.Errorf("completion progress = %+v, want m1/generation/1 without error", got[2])
	}
}

func TestServer_InflightMiddleware_PublishesInferenceErrorProgress(t *testing.T) {
	progressEvents := make(chan swaputil.BackendProgressEvent, 3)
	tracker := newInflightTracker()
	tracker.SetProgressPublisher(func(progress swaputil.BackendProgressEvent) {
		progressEvents <- progress
	})
	handler := CreateInflightMiddleware(tracker, config.Config{})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = req.WithContext(swaputil.SetContext(req.Context(), swaputil.ReqContextData{ModelID: "m1"}))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	select {
	case progress := <-progressEvents:
		if progress.Phase != "prefill" {
			t.Fatalf("first progress = %+v, want prefill", progress)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for prefill progress")
	}
	select {
	case progress := <-progressEvents:
		if progress.Phase != "error" || progress.Error != "upstream returned HTTP 502" {
			t.Fatalf("error progress = %+v, want HTTP 502", progress)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for error progress")
	}
}

type inferenceProgressWriteError struct {
	header http.Header
}

func (w *inferenceProgressWriteError) Header() http.Header { return w.header }

func (w *inferenceProgressWriteError) WriteHeader(int) {}

func (w *inferenceProgressWriteError) Write([]byte) (int, error) {
	return 0, errors.New("client write failed")
}

func TestServer_InflightMiddleware_PublishesInferenceWriteErrorProgress(t *testing.T) {
	progressEvents := make(chan swaputil.BackendProgressEvent, 3)
	tracker := newInflightTracker()
	tracker.SetProgressPublisher(func(progress swaputil.BackendProgressEvent) {
		progressEvents <- progress
	})
	handler := CreateInflightMiddleware(tracker, config.Config{})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("partial"))
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = req.WithContext(swaputil.SetContext(req.Context(), swaputil.ReqContextData{ModelID: "m1"}))
	handler.ServeHTTP(&inferenceProgressWriteError{header: make(http.Header)}, req)

	select {
	case progress := <-progressEvents:
		if progress.Phase != "prefill" {
			t.Fatalf("first progress = %+v, want prefill", progress)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for prefill progress")
	}
	select {
	case progress := <-progressEvents:
		if progress.Phase != "error" || progress.Error != "client write failed" {
			t.Fatalf("write error progress = %+v, want client write failure", progress)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for write error progress")
	}
}

func TestServer_InflightEventPayloadIncludesRequestEntries(t *testing.T) {
	events := make(chan swaputil.InFlightRequestsEvent, 4)
	tracker := newInflightTrackerWithPublisher(4, func(update swaputil.InFlightRequestsEvent) {
		events <- update
	})

	release := make(chan struct{})
	done := make(chan struct{})
	handler := CreateInflightMiddleware(tracker, config.Config{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))

	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodGet, "/props?model=m1", nil)
		req = req.WithContext(swaputil.SetContext(req.Context(), swaputil.ReqContextData{
			Model:   "m1",
			ModelID: "m1",
		}))
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}()

	added := waitInflightEvent(t, events, inflightOperationUpsert)
	if added.Request == nil {
		t.Fatal("added request is nil")
	}
	if added.Request.Model != "m1" || added.Request.ReqPath != "/props" {
		t.Errorf("added request = %+v", added.Request)
	}

	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return")
	}
	removed := waitInflightEvent(t, events, inflightOperationRemove)
	if removed.ID != added.Request.ID {
		t.Errorf("removed id = %q, want %q", removed.ID, added.Request.ID)
	}
}

func TestServer_InflightTracker_OutboxDoesNotBlockRequests(t *testing.T) {
	publishStarted := make(chan struct{})
	releasePublisher := make(chan struct{})
	published := make(chan swaputil.InFlightRequestsEvent, 4)
	var blockFirst sync.Once

	tracker := newInflightTrackerWithPublisher(1, func(update swaputil.InFlightRequestsEvent) {
		blockFirst.Do(func() {
			close(publishStarted)
			<-releasePublisher
		})
		published <- update
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	id := tracker.Add(req, func() {})
	select {
	case <-publishStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("publisher did not start")
	}

	// Fill the one-item outbox while the publisher is blocked, then overflow
	// it with the removal. The request path must still return immediately.
	tracker.SetResponseHeaders(id, http.Header{"Content-Type": {"text/event-stream"}})
	removed := make(chan struct{})
	go func() {
		tracker.Remove(id)
		close(removed)
	}()
	select {
	case <-removed:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Remove blocked on a busy publisher")
	}

	close(releasePublisher)
	_ = waitInflightEvent(t, published, inflightOperationUpsert)
	recovered := waitInflightEvent(t, published, inflightOperationSnapshot)
	if len(recovered.Requests) != 0 {
		t.Errorf("recovery snapshot = %+v, want no requests", recovered.Requests)
	}
}

func TestServer_InflightCancelByIDCancelsRequestContext(t *testing.T) {
	tracker := newInflightTracker()
	idCh := make(chan string, 1)
	done := make(chan struct{})
	handler := CreateInflightMiddleware(tracker, config.Config{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := tracker.Current()
		if len(current.Requests) != 1 {
			t.Errorf("inflight requests = %d, want 1", len(current.Requests))
			return
		}
		idCh <- current.Requests[0].ID
		<-r.Context().Done()
		close(done)
	}))

	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req = req.WithContext(swaputil.SetContext(req.Context(), swaputil.ReqContextData{ModelID: "m1"}))
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}()

	var id string
	select {
	case id = <-idCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for inflight id")
	}
	if !tracker.Cancel(id) {
		t.Fatalf("Cancel(%q) = false, want true", id)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("request context was not canceled")
	}
	waitInflightTrackerCount(t, tracker, 0)
}

func waitInflightTrackerCount(t *testing.T, tracker *inflightTracker, total int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if got := tracker.Current(); len(got.Requests) == total {
			return
		}
		select {
		case <-deadline:
			got := tracker.Current()
			t.Fatalf("inflight total = %d, want %d", len(got.Requests), total)
		case <-tick.C:
		}
	}
}

func waitInflightEvent(t *testing.T, events <-chan swaputil.InFlightRequestsEvent, operation string) swaputil.InFlightRequestsEvent {
	t.Helper()
	timer := time.After(2 * time.Second)
	for {
		select {
		case got := <-events:
			if got.Operation == operation {
				return got
			}
		case <-timer:
			t.Fatalf("timed out waiting for inflight operation %q", operation)
		}
	}
}

// waitInflightEventPredicate is waitInflightEvent with an arbitrary match.
// Phase transitions publish their own upserts before the expected update
// arrives, so a match on operation alone is not selective enough there.
func waitInflightEventPredicate(t *testing.T, events <-chan swaputil.InFlightRequestsEvent, match func(swaputil.InFlightRequestsEvent) bool) swaputil.InFlightRequestsEvent {
	t.Helper()
	timer := time.After(2 * time.Second)
	for {
		select {
		case got := <-events:
			if match(got) {
				return got
			}
		case <-timer:
			t.Fatal("timed out waiting for inflight event")
		}
	}
}

func TestServer_APIVersion(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	s.build = BuildInfo{Version: "1.2.3", Commit: "deadbeef", Date: "2026-05-19"}

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/version", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["version"] != "1.2.3" || got["commit"] != "deadbeef" || got["build_date"] != "2026-05-19" {
		t.Errorf("body = %v", got)
	}
}

func TestServer_APIHardware(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	s.hardware = &hw.HardwareSnapshot{
		SchemaVersion: hw.SchemaVersion,
		CapturedAt:    time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC),
		Capture: hw.HardwareCapture{
			Scope:    hw.CaptureScopeInferenceHost,
			Method:   hw.CaptureMethodDetected,
			Detector: &hw.DetectorInfo{Name: "llama-swap", Version: "246"},
		},
		Architecture:    hw.Architecture{Name: "x86_64"},
		OperatingSystem: hw.OperatingSystem{Family: "linux"},
		Environment:     hw.ExecutionEnvironment{Kind: "unknown"},
		Memory:          hw.SystemMemory{CapacityBytes: 1024},
		Accelerators:    []hw.Accelerator{},
	}

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/hardware", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var got hw.HardwareSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.SchemaVersion != 1 || got.Memory.CapacityBytes != 1024 || got.Accelerators == nil {
		t.Errorf("body = %+v", got)
	}
}

func TestServer_APIHardwareUnavailable(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/hardware", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestServer_APIMetricsActivity_Empty(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/metrics/activity", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var page store.ActivityPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if page.Total != 0 || len(page.Data) != 0 {
		t.Errorf("page = %+v, want empty", page)
	}
}

func TestServer_APIMetricsActivity(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	storedM1, ok := s.metrics.queueMetrics(ActivityLogEntry{
		Timestamp: time.Unix(1, 0),
		Model:     "m1",
		ReqPath:   "/v1/chat/completions",
		Tokens:    TokenMetrics{InputTokens: 1, OutputTokens: 2},
	})
	if !ok {
		t.Fatal("queueMetrics m1 failed")
	}
	if _, ok := s.metrics.queueMetrics(ActivityLogEntry{
		Timestamp: time.Unix(2, 0),
		Model:     "m2",
		ReqPath:   "/v1/chat/completions",
		Tokens:    TokenMetrics{InputTokens: 3, OutputTokens: 4},
	}); !ok {
		t.Fatal("queueMetrics m2 failed")
	}

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/metrics/activity?model=m1&limit=10&page=1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", w.Code, w.Body.String())
	}
	var page store.ActivityPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if page.Total != 1 || len(page.Data) != 1 {
		t.Fatalf("page = %+v", page)
	}
	if page.Data[0].ID != storedM1.ID {
		t.Fatalf("entry = %+v", page.Data[0])
	}
}

func TestServer_APIMetricsActivityFilters(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))

	// ids 1..4 at ts_created 1000..1003, models m1/m2/m1/m3.
	for i, model := range []string{"m1", "m2", "m1", "m3"} {
		if _, ok := s.metrics.queueMetrics(ActivityLogEntry{
			Timestamp: time.Unix(int64(1000+i), 0),
			Model:     model,
			ReqPath:   "/v1/chat/completions",
		}); !ok {
			t.Fatal("queueMetrics failed")
		}
	}

	// ts_created 1000..1003 as RFC3339. The UI never sends these; they are
	// part of the API for direct consumers.
	at := func(offset int) string {
		return url.QueryEscape(time.Unix(int64(1000+offset), 0).UTC().Format(time.RFC3339))
	}

	tests := []struct {
		name  string
		query string
		want  []int
	}{
		{"repeated model", "?model=m1&model=m3", []int{4, 3, 1}},
		{"single model still works", "?model=m2", []int{2}},
		{"id range", "?min_id=2&max_id=3", []int{3, 2}},
		{"min id only", "?min_id=4", []int{4}},
		{"max id only", "?max_id=2", []int{2, 1}},
		{"id range and model combined", "?model=m1&min_id=2", []int{3}},
		{"start only", "?start=" + at(2), []int{4, 3}},
		{"end only", "?end=" + at(1), []int{2, 1}},
		{"time range inclusive", "?start=" + at(1) + "&end=" + at(2), []int{3, 2}},
		{"time and model combined", "?start=" + at(0) + "&end=" + at(3) + "&model=m1", []int{3, 1}},
		{"time and id range combined", "?start=" + at(0) + "&min_id=3", []int{4, 3}},
		{"no filters", "", []int{4, 3, 2, 1}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/metrics/activity"+tt.query, nil))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d body=%q", w.Code, w.Body.String())
			}
			var page store.ActivityPage
			if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if page.Total != len(tt.want) {
				t.Fatalf("Total = %d, want %d", page.Total, len(tt.want))
			}
			if len(page.Data) != len(tt.want) {
				t.Fatalf("len(Data) = %d, want %d", len(page.Data), len(tt.want))
			}
			for i, wantID := range tt.want {
				if page.Data[i].ID != wantID {
					t.Fatalf("Data[%d].ID = %d, want %d", i, page.Data[i].ID, wantID)
				}
			}
		})
	}
}

func TestServer_APIMetricsActivityInvalidFilters(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))

	for _, query := range []string{
		"?min_id=abc",
		"?max_id=0",
		"?min_id=-1",
		"?min_id=9&max_id=2",
		"?start=nonsense",
		"?end=2026-01-01",
		"?start=2026-02-01T00:00:00Z&end=2026-01-01T00:00:00Z",
	} {
		t.Run(query, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/metrics/activity"+query, nil))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d (body=%q)", w.Code, http.StatusBadRequest, w.Body.String())
			}
		})
	}
}

func TestServer_APIMetricsStats(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	for _, entry := range []ActivityLogEntry{
		{Timestamp: time.Unix(1, 0), Model: "m1", Tokens: TokenMetrics{InputTokens: 1, OutputTokens: 2, CachedTokens: 1, PromptPerSecond: 10, TokensPerSecond: 20}},
		{Timestamp: time.Unix(2, 0), Model: "m1", Tokens: TokenMetrics{InputTokens: 3, OutputTokens: 4, PromptPerSecond: 30, TokensPerSecond: 40}},
		{Timestamp: time.Unix(3, 0), Model: "m2", Tokens: TokenMetrics{InputTokens: 5, OutputTokens: 6, PromptPerSecond: 50}},
	} {
		if _, ok := s.metrics.queueMetrics(entry); !ok {
			t.Fatal("queueMetrics failed")
		}
	}

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/metrics/stats?model=m1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", w.Code, w.Body.String())
	}
	var stats store.ActivityStats
	if err := json.Unmarshal(w.Body.Bytes(), &stats); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if stats.TotalRequests != 2 || stats.TotalInputTokens != 4 || stats.TotalOutputTokens != 6 || stats.TotalCacheTokens != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if len(stats.ByModel) != 1 || stats.ByModel[0].Model != "m1" || stats.ByModel[0].Requests != 2 || stats.ByModel[0].InputTokens != 4 || stats.ByModel[0].OutputTokens != 6 || stats.ByModel[0].CachedTokens != 1 {
		t.Fatalf("by-model stats = %+v", stats.ByModel)
	}
	if stats.PromptHistogram == nil || stats.GenerationHistogram == nil {
		t.Fatalf("expected histograms: %+v", stats)
	}
}

func TestServer_APIMetricsConfiguredOnlyHidesDeletedModels(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	s.setConfig(config.Config{Models: map[string]config.ModelConfig{
		"current": {},
	}})
	for _, entry := range []ActivityLogEntry{
		{Model: "current", Tokens: TokenMetrics{InputTokens: 2, OutputTokens: 3}},
		{Model: "deleted", Tokens: TokenMetrics{InputTokens: 100, OutputTokens: 100}},
	} {
		if _, ok := s.metrics.queueMetrics(entry); !ok {
			t.Fatal("queueMetrics failed")
		}
	}

	request := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}
	page := request("/api/metrics/activity?configured_only=true")
	if page.Code != http.StatusOK {
		t.Fatalf("activity status=%d body=%s", page.Code, page.Body.String())
	}
	var activity store.ActivityPage
	if err := json.Unmarshal(page.Body.Bytes(), &activity); err != nil {
		t.Fatal(err)
	}
	if activity.Total != 1 || len(activity.Data) != 1 || activity.Data[0].Model != "current" {
		t.Fatalf("configured-only activity = %+v", activity)
	}

	stats := request("/api/metrics/stats?configured_only=true")
	if stats.Code != http.StatusOK {
		t.Fatalf("stats status=%d body=%s", stats.Code, stats.Body.String())
	}
	var activityStats store.ActivityStats
	if err := json.Unmarshal(stats.Body.Bytes(), &activityStats); err != nil {
		t.Fatal(err)
	}
	if activityStats.TotalRequests != 1 || activityStats.TotalInputTokens != 2 || activityStats.TotalOutputTokens != 3 {
		t.Fatalf("configured-only stats = %+v", activityStats)
	}

	stale := request("/api/metrics/activity?model=deleted&configured_only=true")
	if stale.Code != http.StatusOK {
		t.Fatalf("stale model status=%d body=%s", stale.Code, stale.Body.String())
	}
	var stalePage store.ActivityPage
	if err := json.Unmarshal(stale.Body.Bytes(), &stalePage); err != nil {
		t.Fatal(err)
	}
	if stalePage.Total != 0 || len(stalePage.Data) != 0 {
		t.Fatalf("stale model was not hidden: %+v", stalePage)
	}
}

func TestServer_APIMetricsSpeed(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	base := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC).Unix()
	for _, entry := range []ActivityLogEntry{
		{Timestamp: time.Unix(base, 0), Model: "m1", Tokens: TokenMetrics{InputTokens: 4096, OutputTokens: 100, PromptPerSecond: 800, TokensPerSecond: 30}, FirstTokenMs: 700},
		{Timestamp: time.Unix(base+600, 0), Model: "m1", Tokens: TokenMetrics{InputTokens: 4097, OutputTokens: 300, PromptPerSecond: 700, TokensPerSecond: 20}, FirstTokenMs: 900},
		{Timestamp: time.Unix(base+600, 0), Model: "m2", Tokens: TokenMetrics{InputTokens: 20000, OutputTokens: 50, PromptPerSecond: 400, TokensPerSecond: 25}},
	} {
		if _, ok := s.metrics.queueMetrics(entry); !ok {
			t.Fatal("queueMetrics failed")
		}
	}

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/metrics/speed?model=m1&bucket=3600", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", w.Code, w.Body.String())
	}
	var report store.SpeedReport
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if report.BucketSeconds != 3600 {
		t.Fatalf("bucket seconds = %d, want 3600", report.BucketSeconds)
	}
	if len(report.Series) != 1 || report.Series[0].Model != "m1" {
		t.Fatalf("series = %+v, want only m1", report.Series)
	}
	// Both m1 requests fall in the same hour, so the bucket reports the mean of
	// every rate and latency they published.
	point := report.Series[0].Points[0]
	if point.Requests != 2 || point.PrefillTPS != 750 || point.DecodeTPS != 25 || point.TTFTMs != 800 {
		t.Fatalf("first point = %+v", point)
	}
	if len(report.Context) != 1 || len(report.Context[0].Buckets) != 2 {
		t.Fatalf("context = %+v, want the <=4k and 4k-8k buckets", report.Context)
	}
	if report.Context[0].Buckets[0].Label != "<=4k" || report.Context[0].Buckets[1].Label != "4k-8k" {
		t.Fatalf("context labels = %+v", report.Context[0].Buckets)
	}

	// Assert the wire contract itself, not just the Go round trip: the UI
	// reads these field names directly, so a renamed json tag must fail here.
	wire := gjson.ParseBytes(w.Body.Bytes())
	for _, path := range []string{
		"bucket_seconds",
		"series.0.model",
		"series.0.points.0.timestamp",
		"series.0.points.0.requests",
		"series.0.points.0.prefill_tps",
		"series.0.points.0.decode_tps",
		"series.0.points.0.ttft_ms",
		"context.0.model",
		"context.0.buckets.0.label",
		"context.0.buckets.0.min_tokens",
		"context.0.buckets.0.max_tokens",
		"context.0.buckets.0.avg_input_tokens",
		"context.0.buckets.0.avg_output_tokens",
		"context.0.buckets.0.prefill_tps",
		"context.0.buckets.0.decode_tps",
		"context.0.buckets.0.ttft_ms",
	} {
		if !wire.Get(path).Exists() {
			t.Fatalf("speed report is missing %s: %s", path, w.Body.String())
		}
	}

	// An explicit bucket is clamped into the store's supported range.
	clamped := httptest.NewRecorder()
	s.ServeHTTP(clamped, httptest.NewRequest(http.MethodGet, "/api/metrics/speed?bucket=1", nil))
	if clamped.Code != http.StatusOK {
		t.Fatalf("clamped status = %d body=%q", clamped.Code, clamped.Body.String())
	}
	var clampedReport store.SpeedReport
	if err := json.Unmarshal(clamped.Body.Bytes(), &clampedReport); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if clampedReport.BucketSeconds != 60 {
		t.Fatalf("bucket seconds = %d, want the 60s floor", clampedReport.BucketSeconds)
	}
	if len(clampedReport.Series) != 2 {
		t.Fatalf("unfiltered series = %+v, want both models", clampedReport.Series)
	}
}

func TestServer_APIMetricsSpeedInvalidFilters(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	for _, query := range []string{
		"?bucket=abc",
		"?bucket=-1",
		"?start=nonsense",
		"?min_id=9&max_id=2",
		"?start=2026-02-01T00:00:00Z&end=2026-01-01T00:00:00Z",
		"?configured_only=maybe",
	} {
		t.Run(query, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/metrics/speed"+query, nil))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d (body=%q)", w.Code, http.StatusBadRequest, w.Body.String())
			}
		})
	}
}

func TestServer_APIMetricsSpeedConfiguredOnly(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	s.setConfig(config.Config{Models: map[string]config.ModelConfig{"current": {}}})
	for _, entry := range []ActivityLogEntry{
		{Timestamp: time.Unix(1, 0), Model: "current", Tokens: TokenMetrics{InputTokens: 4096, OutputTokens: 10, PromptPerSecond: 100, TokensPerSecond: 20}},
		{Timestamp: time.Unix(2, 0), Model: "deleted", Tokens: TokenMetrics{InputTokens: 4096, OutputTokens: 10, PromptPerSecond: 100, TokensPerSecond: 20}},
	} {
		if _, ok := s.metrics.queueMetrics(entry); !ok {
			t.Fatal("queueMetrics failed")
		}
	}

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/metrics/speed?configured_only=true", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", w.Code, w.Body.String())
	}
	var report store.SpeedReport
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(report.Series) != 1 || report.Series[0].Model != "current" {
		t.Fatalf("configured-only series = %+v, want only current", report.Series)
	}
}

func TestServer_ParseActivityBool(t *testing.T) {
	for _, value := range []string{"1", "true", "yes", "TRUE"} {
		r := httptest.NewRequest(http.MethodGet, "/?configured_only="+value, nil)
		got, err := parseActivityBool(r, "configured_only")
		if err != nil || !got {
			t.Fatalf("parseActivityBool(%q) = %v, %v", value, got, err)
		}
	}
	for _, value := range []string{"0", "false", "no", "FALSE"} {
		r := httptest.NewRequest(http.MethodGet, "/?configured_only="+value, nil)
		got, err := parseActivityBool(r, "configured_only")
		if err != nil || got {
			t.Fatalf("parseActivityBool(%q) = %v, %v", value, got, err)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/?configured_only=maybe", nil)
	if _, err := parseActivityBool(r, "configured_only"); err == nil {
		t.Fatal("invalid configured_only value was accepted")
	}
}

func TestServer_APICancelInflight(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	ctx, cancel := context.WithCancel(context.Background())
	id := s.inflight.Add(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx), cancel)
	defer s.inflight.Remove(id)

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/inflight/"+id+"/cancel", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%q", w.Code, w.Body.String())
	}
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("cancel endpoint did not cancel request context")
	}

	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/inflight/missing/cancel", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d, want 404", w.Code)
	}
}

func TestInflightTracker_CancelModel(t *testing.T) {
	tracker := newInflightTracker()
	modelContext, cancelModel := context.WithCancel(context.Background())
	modelRequest := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(
		swaputil.SetContext(modelContext, swaputil.ReqContextData{ModelID: "m"}),
	)
	modelID := tracker.Add(modelRequest, cancelModel)
	defer tracker.Remove(modelID)

	otherContext, cancelOther := context.WithCancel(context.Background())
	otherRequest := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(
		swaputil.SetContext(otherContext, swaputil.ReqContextData{ModelID: "other"}),
	)
	otherID := tracker.Add(otherRequest, cancelOther)
	defer tracker.Remove(otherID)

	if got := tracker.CancelModel("m"); got != 1 {
		t.Fatalf("cancelled requests = %d, want 1", got)
	}
	select {
	case <-modelContext.Done():
	case <-time.After(time.Second):
		t.Fatal("model request context was not cancelled")
	}
	select {
	case <-otherContext.Done():
		t.Fatal("request for another model was cancelled")
	default:
	}
}

func TestServer_InflightMetricsRecordsCompletedOnce(t *testing.T) {
	local := newStubRouter([]string{"m1"}, `{"usage":{"prompt_tokens":1,"completion_tokens":2}}`)
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = configWithModels("m1")

	w := httptest.NewRecorder()
	s.ServeHTTP(w, chatRequest("m1"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", w.Code, w.Body.String())
	}
	if got := s.inflight.Current(); len(got.Requests) != 0 {
		t.Fatalf("inflight total after request = %d, want 0", len(got.Requests))
	}
	gotMetrics := metricsEntries(t, s.metrics)
	if len(gotMetrics) != 1 {
		t.Fatalf("metrics len = %d, want 1", len(gotMetrics))
	}
	if gotMetrics[0].Model != "m1" || gotMetrics[0].Tokens.InputTokens != 1 || gotMetrics[0].Tokens.OutputTokens != 2 {
		t.Errorf("metric = %+v", gotMetrics[0])
	}
}

func configWithModels(models ...string) config.Config {
	cfg := config.Config{Models: make(map[string]config.ModelConfig, len(models))}
	for _, model := range models {
		cfg.Models[model] = config.ModelConfig{}
	}
	return cfg
}

func TestServer_APIPerformance_Unavailable(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/performance", nil))

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestServer_APIEvents_InitialPayload(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	s.cfg.UI.Activity.SessionID = []string{"X-Trace-ID"}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		s.ServeHTTP(w, req)
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after context cancel")
	}

	body := w.Body.String()
	for _, want := range []string{`"type":"modelStatus"`, `"type":"inflight"`, `"type":"uiConfig"`, `"type":"profileChanged"`, `"type":"logData"`, `X-Trace-ID`} {
		if !strings.Contains(body, want) {
			t.Errorf("initial SSE payload missing %s; body=%q", want, body)
		}
	}
}
