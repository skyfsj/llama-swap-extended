package router

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/router/scheduler"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// These tests cover baseRouter's own machinery — the run loop, process
// lifecycle (doSwap), grant/ServeHTTP plumbing, Unload, and Shutdown. The
// scheduling decision logic (queueing, collation, eviction collisions) lives in
// the scheduler package and is tested directly there; see fifo_test.go.

// stubPlanner evicts configured targets. baseRouter tests drive the run loop
// through the default FIFO scheduler without exercising router planner details.
type stubPlanner struct {
	evict map[string][]string
}

func (s *stubPlanner) EvictionFor(target string, _ []string) []string {
	if s.evict == nil {
		return nil
	}
	return s.evict[target]
}
func (s *stubPlanner) OnSwapStart(string, []string) {}

func newTestBase(t *testing.T, processes map[string]process.Process, planner scheduler.Swapper) *baseRouter {
	t.Helper()
	conf := config.Config{HealthCheckTimeout: 5}
	return newTestBaseWithConfig(t, conf, processes, planner)
}

func newTestBaseWithConfig(t *testing.T, conf config.Config, processes map[string]process.Process, planner scheduler.Swapper) *baseRouter {
	t.Helper()
	b, err := newBaseRouter("test", conf, processes, logmon.NewWriter(io.Discard), planner)
	if err != nil {
		t.Fatalf("newBaseRouter: %v", err)
	}
	b.testProcessed = make(chan struct{}, 64)
	go b.run()
	t.Cleanup(func() {
		if !b.shuttingDown.Load() {
			_ = b.Shutdown(time.Second)
		}
	})
	return b
}

func TestBaseRouter_RunningModels(t *testing.T) {
	ready := newFakeProcess("ready")
	ready.markReady()
	starting := newFakeProcess("starting")
	starting.setState(process.StateStarting)
	stopped := newFakeProcess("stopped")

	b := newTestBase(t, map[string]process.Process{
		"ready": ready, "starting": starting, "stopped": stopped,
	}, &stubPlanner{})

	running := b.RunningModels()
	if len(running) != 2 {
		t.Fatalf("running=%v want 2 entries", running)
	}
	if running["ready"] != process.StateReady {
		t.Errorf("ready state=%q want ready", running["ready"])
	}
	if running["starting"] != process.StateStarting {
		t.Errorf("starting state=%q want starting", running["starting"])
	}
	if _, ok := running["stopped"]; ok {
		t.Errorf("stopped process should be excluded from RunningModels")
	}
}

func TestBaseRouter_UnloadAll(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	c := newFakeProcess("c")
	c.markReady()

	b := newTestBase(t, map[string]process.Process{"a": a, "c": c}, &stubPlanner{})
	b.Unload(time.Second)

	if a.State() != process.StateStopped || c.State() != process.StateStopped {
		t.Fatalf("Unload() should stop every process: a=%q c=%q", a.State(), c.State())
	}
}

func TestBaseRouter_UnloadSpecificModel(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	c := newFakeProcess("c")
	c.markReady()

	b := newTestBase(t, map[string]process.Process{"a": a, "c": c}, &stubPlanner{})
	b.Unload(time.Second, "a")

	if a.State() != process.StateStopped {
		t.Errorf("a should be stopped, got %q", a.State())
	}
	if c.State() != process.StateReady {
		t.Errorf("c should remain ready, got %q", c.State())
	}
}

func TestBaseRouter_UnloadSpecificModelUsesConfiguredTimeout(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	c := newFakeProcess("c")
	c.markReady()

	conf := config.Config{
		HealthCheckTimeout: 5,
		UnloadTimeout:      25,
		Models: map[string]config.ModelConfig{
			"a": {UnloadTimeout: 45},
			"c": {UnloadTimeout: 25},
		},
	}
	b := newTestBaseWithConfig(t, conf, map[string]process.Process{"a": a, "c": c}, &stubPlanner{})
	b.Unload(0, "a")

	if a.lastStopTimeout() != 45*time.Second {
		t.Errorf("a stop timeout=%v want 45s", a.lastStopTimeout())
	}
	if got := c.stopCalls.Load(); got != 0 {
		t.Errorf("c stopCalls=%d want 0", got)
	}
}

func TestBaseRouter_UnloadAllUsesConfiguredTimeouts(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	c := newFakeProcess("c")
	c.markReady()

	conf := config.Config{
		HealthCheckTimeout: 5,
		UnloadTimeout:      25,
		Models: map[string]config.ModelConfig{
			"a": {UnloadTimeout: 45},
			"c": {UnloadTimeout: 25},
		},
	}
	b := newTestBaseWithConfig(t, conf, map[string]process.Process{"a": a, "c": c}, &stubPlanner{})
	b.Unload(0)

	if a.lastStopTimeout() != 45*time.Second {
		t.Errorf("a stop timeout=%v want 45s", a.lastStopTimeout())
	}
	if c.lastStopTimeout() != 25*time.Second {
		t.Errorf("c stop timeout=%v want 25s", c.lastStopTimeout())
	}
}

func TestBaseRouter_UnloadStopsSmallestTimeoutFirst(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	c := newFakeProcess("c")
	c.markReady()
	e := newFakeProcess("e")
	e.markReady()

	var mu sync.Mutex
	var order []string
	record := func(id string) {
		mu.Lock()
		order = append(order, id)
		mu.Unlock()
	}
	a.onStop = record
	c.onStop = record
	e.onStop = record

	conf := config.Config{
		HealthCheckTimeout: 5,
		UnloadTimeout:      25,
		Models: map[string]config.ModelConfig{
			"a": {UnloadTimeout: 45},
			"c": {UnloadTimeout: 10},
			"e": {UnloadTimeout: 25},
		},
	}
	b := newTestBaseWithConfig(t, conf, map[string]process.Process{"a": a, "c": c, "e": e}, &stubPlanner{})
	// Named in descending timeout order; Unload must re-order ascending.
	b.Unload(0, "a", "e", "c")

	mu.Lock()
	defer mu.Unlock()
	if want := []string{"c", "e", "a"}; !slices.Equal(order, want) {
		t.Errorf("stop order=%v want %v", order, want)
	}
}

// TestBaseRouter_UnloadZeroStopsSameTimeoutInParallel verifies that models
// resolving to the same unloadTimeout share one unload request and stop
// concurrently, rather than one request per model. Both fakeProcess.Stop
// calls are pinned via stopBlock; the test only releases them after
// observing both stopStarted, which deadlocks if the stops were sequential.
func TestBaseRouter_UnloadZeroStopsSameTimeoutInParallel(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	a.stopBlock = make(chan struct{})
	c := newFakeProcess("c")
	c.markReady()
	c.stopBlock = make(chan struct{})

	conf := config.Config{
		HealthCheckTimeout: 5,
		UnloadTimeout:      25,
		// no per-model values: both models inherit the global 25s
	}
	b := newTestBaseWithConfig(t, conf, map[string]process.Process{"a": a, "c": c}, &stubPlanner{})

	unloadDone := make(chan struct{})
	go func() {
		b.Unload(0)
		close(unloadDone)
	}()

	for _, p := range []*fakeProcess{a, c} {
		select {
		case <-p.stopStarted:
		case <-time.After(2 * time.Second):
			t.Fatalf("Stop on %s never started — same-timeout unloads are not parallel", p.id)
		}
	}
	close(a.stopBlock)
	close(c.stopBlock)

	select {
	case <-unloadDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Unload did not return after stops were released")
	}
	if a.lastStopTimeout() != 25*time.Second || c.lastStopTimeout() != 25*time.Second {
		t.Errorf("stop timeouts a=%v c=%v want 25s each", a.lastStopTimeout(), c.lastStopTimeout())
	}
}

func TestBaseRouter_UnloadPositiveTimeoutOverridesConfigured(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()

	conf := config.Config{
		HealthCheckTimeout: 5,
		UnloadTimeout:      25,
		Models: map[string]config.ModelConfig{
			"a": {UnloadTimeout: 45},
		},
	}
	b := newTestBaseWithConfig(t, conf, map[string]process.Process{"a": a}, &stubPlanner{})
	b.Unload(time.Second, "a")

	if a.lastStopTimeout() != time.Second {
		t.Errorf("a stop timeout=%v want 1s", a.lastStopTimeout())
	}
}

// TestBaseRouter_Unload_StopsInParallel verifies that Unload fans out its
// Stop calls concurrently rather than stopping each process serially. Each
// fakeProcess.Stop is pinned via stopBlock; the test only releases them
// after observing every stopStarted, proving all three Stops were in
// flight simultaneously.
func TestBaseRouter_Unload_StopsInParallel(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	a.stopBlock = make(chan struct{})
	pb := newFakeProcess("b")
	pb.markReady()
	pb.stopBlock = make(chan struct{})
	pc := newFakeProcess("c")
	pc.markReady()
	pc.stopBlock = make(chan struct{})

	b := newTestBase(t, map[string]process.Process{"a": a, "b": pb, "c": pc}, &stubPlanner{})

	unloadDone := make(chan struct{})
	go func() {
		b.Unload(time.Second, "a", "b", "c")
		close(unloadDone)
	}()

	// All three Stop calls must start before any of them are allowed to
	// complete. If Unload was serial, only one stopStarted would fire
	// until we released its stopBlock, and this would deadlock.
	for _, p := range []*fakeProcess{a, pb, pc} {
		select {
		case <-p.stopStarted:
		case <-time.After(2 * time.Second):
			t.Fatalf("Stop on %s never started — Unload is not parallel", p.id)
		}
	}

	// Release them; Unload should now return.
	close(a.stopBlock)
	close(pb.stopBlock)
	close(pc.stopBlock)

	select {
	case <-unloadDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Unload did not return after stops released")
	}

	for _, p := range []*fakeProcess{a, pb, pc} {
		if p.State() != process.StateStopped {
			t.Errorf("%s state=%q want stopped", p.id, p.State())
		}
		if got := p.stopCalls.Load(); got != 1 {
			t.Errorf("%s stopCalls=%d want 1", p.id, got)
		}
	}
}

func TestBaseRouter_OnDemandStart(t *testing.T) {
	a := newFakeProcess("a")
	a.autoReady = true

	b := newTestBase(t, map[string]process.Process{"a": a}, &stubPlanner{})

	w := httptest.NewRecorder()
	b.ServeHTTP(w, newRequest("a"))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if got := a.runCalls.Load(); got != 1 {
		t.Errorf("runCalls=%d want 1", got)
	}
	if got := a.serveCalls.Load(); got != 1 {
		t.Errorf("serveCalls=%d want 1", got)
	}
}

func TestBaseRouter_IgnoreWebsocketsRejectsModelUnlessReady(t *testing.T) {
	for _, state := range []process.ProcessState{process.StateStopped, process.StateStarting} {
		t.Run(string(state), func(t *testing.T) {
			a := newFakeProcess("a")
			if state != process.StateStopped {
				a.setState(state)
			}
			conf := config.Config{
				HealthCheckTimeout: 5,
				Models: map[string]config.ModelConfig{
					"a": {Compat: config.CompatConfig{IgnoreWebsockets: true}},
				},
			}
			b := newTestBaseWithConfig(t, conf, map[string]process.Process{"a": a}, &stubPlanner{})

			r := httptest.NewRequest(http.MethodGet, "/props?model=a", nil)
			r.Header.Set("Connection", "keep-alive, Upgrade")
			r.Header.Set("Upgrade", "websocket")
			w := httptest.NewRecorder()
			b.ServeHTTP(w, r)

			if w.Code != http.StatusConflict {
				t.Fatalf("status=%d want %d body=%q", w.Code, http.StatusConflict, w.Body.String())
			}
			if got := a.runCalls.Load(); got != 0 {
				t.Errorf("runCalls=%d want 0", got)
			}
			if got := a.serveCalls.Load(); got != 0 {
				t.Errorf("serveCalls=%d want 0", got)
			}
		})
	}
}

func TestBaseRouter_WebsocketStartsModelWhenCompatDisabled(t *testing.T) {
	a := newFakeProcess("a")
	a.autoReady = true
	conf := config.Config{
		HealthCheckTimeout: 5,
		Models:             map[string]config.ModelConfig{"a": {}},
	}
	b := newTestBaseWithConfig(t, conf, map[string]process.Process{"a": a}, &stubPlanner{})

	r := httptest.NewRequest(http.MethodGet, "/props?model=a", nil)
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	w := httptest.NewRecorder()
	b.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want %d body=%q", w.Code, http.StatusOK, w.Body.String())
	}
	if got := a.runCalls.Load(); got != 1 {
		t.Errorf("runCalls=%d want 1", got)
	}
}

func TestBaseRouter_IgnoreWebsocketsDoesNotBlockSwap(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	a.serveBlock = make(chan struct{})
	pb := newFakeProcess("b")
	pb.autoReady = true
	conf := config.Config{
		HealthCheckTimeout: 5,
		Models: map[string]config.ModelConfig{
			"a": {Compat: config.CompatConfig{IgnoreWebsockets: true}},
			"b": {},
		},
	}
	b := newTestBaseWithConfig(t, conf, map[string]process.Process{"a": a, "b": pb}, &stubPlanner{
		evict: map[string][]string{"b": {"a"}},
	})

	websocketDone := make(chan struct{})
	go func() {
		defer close(websocketDone)
		r := httptest.NewRequest(http.MethodGet, "/props?model=a", nil)
		r.Header.Set("Connection", "Upgrade")
		r.Header.Set("Upgrade", "websocket")
		b.ServeHTTP(httptest.NewRecorder(), r)
	}()
	waitSignal(t, a.serveStarted, "websocket request start")

	w := httptest.NewRecorder()
	b.ServeHTTP(w, newRequest("b"))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if !a.stoppedWhileServing.Load() {
		t.Fatal("ignored websocket prevented the conflicting model from swapping in")
	}

	close(a.serveBlock)
	waitSignal(t, websocketDone, "websocket request finish")
}

func TestBaseRouter_UnloadDoesNotWaitForIgnoredWebsocket(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	a.serveBlock = make(chan struct{})
	conf := config.Config{
		HealthCheckTimeout: 5,
		Models: map[string]config.ModelConfig{
			"a": {Compat: config.CompatConfig{IgnoreWebsockets: true}},
		},
	}
	b := newTestBaseWithConfig(t, conf, map[string]process.Process{"a": a}, &stubPlanner{})

	websocketDone := make(chan struct{})
	go func() {
		defer close(websocketDone)
		r := httptest.NewRequest(http.MethodGet, "/props?model=a", nil)
		r.Header.Set("Connection", "Upgrade")
		r.Header.Set("Upgrade", "websocket")
		b.ServeHTTP(httptest.NewRecorder(), r)
	}()
	waitSignal(t, a.serveStarted, "ignored websocket start")

	unloadDone := make(chan struct{})
	go func() {
		b.Unload(time.Second, "a")
		close(unloadDone)
	}()
	select {
	case <-unloadDone:
	case <-time.After(time.Second):
		close(a.serveBlock)
		t.Fatal("ordinary unload waited for ignored websocket")
	}
	if !a.stoppedWhileServing.Load() {
		close(a.serveBlock)
		t.Fatal("ordinary unload did not preserve compatibility websocket behavior")
	}

	close(a.serveBlock)
	waitSignal(t, websocketDone, "ignored websocket finish")
}

// TestBaseRouter_RequestDuringStop is the router-level regression test for
// issue #946. A process being stopped outside the router's knowledge (a TTL
// unload, a crash, an operator kill) must not wedge the swap machinery: the
// request has to wait for the stop to finish and then start the model.
//
// Before the fix doSwap read State(), saw StateStopping, skipped the start, and
// then subscribed to a process nobody would ever start — stranding the swap, so
// every later request for the model joined the same zombie swap and hung.
func TestBaseRouter_RequestDuringStop(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	a.autoReady = true
	// Pin Stop so the process sits in StateStopping while the request arrives.
	a.stopBlock = make(chan struct{})

	b := newTestBase(t, map[string]process.Process{"a": a}, &stubPlanner{})

	// Stop the process directly, the way the process's own TTL goroutine does —
	// the router is never told about it.
	stopDone := make(chan struct{})
	go func() {
		defer close(stopDone)
		_ = a.Stop(time.Second)
	}()
	waitSignal(t, a.stopStarted, "a.stopStarted")

	if got := a.State(); got != process.StateStopping {
		t.Fatalf("State()=%s want %s before request", got, process.StateStopping)
	}

	w := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		defer close(served)
		b.ServeHTTP(w, newRequest("a"))
	}()

	// The router must ask the process to start even though it is mid-stop, and
	// leave the process to decide when. The stop is still pinned here, so this
	// signal can only arrive from a start requested during StateStopping —
	// which is precisely what the old State()-then-Run code refused to do.
	waitSignal(t, a.ensureAsked, "a.ensureAsked")

	// Let the unload complete. The request must now start the model itself.
	close(a.stopBlock)
	<-stopDone

	select {
	case <-served:
	case <-t.Context().Done():
		t.Fatalf("request during stop never completed: %v", context.Cause(t.Context()))
	}

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if got := a.runCalls.Load(); got != 1 {
		t.Errorf("runCalls=%d want 1 (model must be restarted after the unload)", got)
	}
	if got := a.serveCalls.Load(); got != 1 {
		t.Errorf("serveCalls=%d want 1", got)
	}
}

func TestBaseRouter_ContextCancel(t *testing.T) {
	a := newFakeProcess("a")
	// autoReady=false so swap parks forever until we mark ready.

	b := newTestBase(t, map[string]process.Process{"a": a}, &stubPlanner{})

	ctx, cancel := context.WithCancel(context.Background())
	w1 := httptest.NewRecorder()
	done1 := make(chan struct{})
	go func() {
		b.ServeHTTP(w1, newRequestCtx(ctx, "a"))
		close(done1)
	}()

	w2 := httptest.NewRecorder()
	done2 := make(chan struct{})
	go func() {
		b.ServeHTTP(w2, newRequest("a"))
		close(done2)
	}()

	waitProcessed(t, b.testProcessed, 2) // both requests joined the active swap
	<-a.runStarted

	cancel()
	select {
	case <-done1:
	case <-time.After(time.Second):
		t.Fatal("cancelled ServeHTTP did not return after ctx cancel")
	}

	a.markReady()
	select {
	case <-done2:
	case <-time.After(time.Second):
		t.Fatal("non-cancelled ServeHTTP did not complete after swap")
	}
	if w2.Code != http.StatusOK {
		t.Errorf("second request status=%d body=%q", w2.Code, w2.Body.String())
	}
}

func TestBaseRouter_ModelNotFound(t *testing.T) {
	a := newFakeProcess("a")
	b := newTestBase(t, map[string]process.Process{"a": a}, &stubPlanner{})

	w := httptest.NewRecorder()
	b.ServeHTTP(w, newRequest("unknown"))

	if w.Code != http.StatusNotFound {
		t.Errorf("status=%d want %d body=%q", w.Code, http.StatusNotFound, w.Body.String())
	}
}

func TestBaseRouter_ConcurrencyLimitRejectsBeforeLoadingStream(t *testing.T) {
	sendLoading := true
	conf := config.Config{
		HealthCheckTimeout: 5,
		Models: map[string]config.ModelConfig{
			"a": {ConcurrencyLimit: 2, SendLoadingState: &sendLoading},
			"b": {},
		},
	}
	a := newFakeProcess("a")
	a.autoReady = true
	bProc := newFakeProcess("b")
	bProc.autoReady = true
	bProc.serveBlock = make(chan struct{})

	b := newTestBaseWithConfig(t, conf, map[string]process.Process{"a": a, "b": bProc}, &stubPlanner{
		evict: map[string][]string{"a": {"b"}},
	})

	bDone := make(chan struct{})
	go func() {
		b.ServeHTTP(httptest.NewRecorder(), newStreamRequest("b"))
		close(bDone)
	}()
	waitSignal(t, bProc.serveStarted, "b request start")
	waitProcessed(t, b.testProcessed, 2)

	aDone1 := make(chan struct{})
	aDone2 := make(chan struct{})
	go func() {
		b.ServeHTTP(httptest.NewRecorder(), newStreamRequest("a"))
		close(aDone1)
	}()
	go func() {
		b.ServeHTTP(httptest.NewRecorder(), newStreamRequest("a"))
		close(aDone2)
	}()
	waitProcessed(t, b.testProcessed, 2)

	w := httptest.NewRecorder()
	b.ServeHTTP(w, newStreamRequest("a"))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429 body=%q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type=%q want application/json", got)
	}
	if strings.Contains(w.Body.String(), "llama-swap loading model") {
		t.Fatalf("429 body contains loading stream: %q", w.Body.String())
	}
	// OpenAI clients read body["error"]["message"], so "error" must decode as
	// an object rather than a bare string.
	var envelope swaputil.ErrorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("429 body is not an OpenAI error envelope: %v, body=%q", err, w.Body.String())
	}
	if envelope.Error.Message == "" || envelope.Error.Type != swaputil.ErrorTypeRateLimit {
		t.Fatalf("429 error=%+v, want a rate_limit_error with a message", envelope.Error)
	}

	close(bProc.serveBlock)
	for name, ch := range map[string]chan struct{}{"b": bDone, "a1": aDone1, "a2": aDone2} {
		waitSignal(t, ch, name+" request finish")
	}
}

// TestBaseRouter_DispatchErrorFramedIntoLoadingStream covers the second half of
// #1029. Once the loading stream has committed its 200, an error can only reach
// the client in-band: swaputil.SendError's status is dropped and its JSON body
// lands as a bare line that every SSE parser discards, leaving the caller with
// a truncated stream, no [DONE], and no reason.
func TestBaseRouter_DispatchErrorFramedIntoLoadingStream(t *testing.T) {
	sendLoading := true
	conf := config.Config{
		HealthCheckTimeout: 5,
		Models:             map[string]config.ModelConfig{"a": {SendLoadingState: &sendLoading}},
	}
	a := newFakeProcess("a")
	a.ensureErr = fmt.Errorf("upstream command exited prematurely")

	b := newTestBaseWithConfig(t, conf, map[string]process.Process{"a": a}, &stubPlanner{})

	w := httptest.NewRecorder()
	b.ServeHTTP(w, newStreamRequest("a"))

	body := w.Body.String()
	// The loading text is streamed a few characters per frame, so reassemble it.
	if content := extractStreamedContent(body); !strings.Contains(content, "llama-swap loading model") {
		t.Fatalf("loading stream did not start, so this is not the path under test: %q", content)
	}
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if line != "" && !strings.HasPrefix(line, "data: ") {
			t.Errorf("line %q is not an SSE field; a client would silently ignore it", line)
		}
	}
	if !strings.Contains(body, "upstream command exited prematurely") {
		t.Errorf("dispatch error never reached the client: %q", body)
	}
	if !strings.HasSuffix(strings.TrimRight(body, "\n"), "data: [DONE]") {
		t.Errorf("stream not terminated with [DONE]: %q", body)
	}
}

func TestBaseRouter_ConfigRestartDrainsOldRequestAndServesWaitingRequest(t *testing.T) {
	oldConfig := config.Config{
		HealthCheckTimeout: 1,
		Models: map[string]config.ModelConfig{
			"a": {Cmd: "old"},
		},
	}
	oldProcess := newFakeProcess("old-generation")
	oldProcess.markReady()
	oldProcess.serveBlock = make(chan struct{})
	replacement := newFakeProcess("new-generation")
	replacement.autoReady = true

	b := newTestBaseWithConfig(t, oldConfig, map[string]process.Process{"a": oldProcess}, &stubPlanner{})
	var factoryMu sync.Mutex
	var factoryConfigs []config.ModelConfig
	b.setProcessFactory(func(_ context.Context, _ string, modelConfig config.ModelConfig) (process.Process, error) {
		factoryMu.Lock()
		factoryConfigs = append(factoryConfigs, modelConfig)
		factoryMu.Unlock()
		return replacement, nil
	})

	oldResponse := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		b.ServeHTTP(response, newRequest("a"))
		oldResponse <- response
	}()
	waitSignal(t, oldProcess.serveStarted, "old generation request")

	desired := oldConfig
	desired.Models = map[string]config.ModelConfig{"a": {Cmd: "new"}}
	if err := b.Reconfigure(desired, &stubPlanner{}, map[string]struct{}{"a": {}}); err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}
	if status := b.ModelLifecycleStatuses()["a"]; status.ConfigStatus != scheduler.ConfigStatusModified {
		t.Fatalf("config status=%q want %q", status.ConfigStatus, scheduler.ConfigStatusModified)
	}
	if err := b.RestartModel("a"); err != nil {
		t.Fatalf("RestartModel: %v", err)
	}

	waitingResponse := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		b.ServeHTTP(response, newRequest("a"))
		waitingResponse <- response
	}()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		status := b.ModelLifecycleStatuses()["a"]
		if status.ConfigStatus == scheduler.ConfigStatusDraining && status.WaitingRequests == 1 {
			break
		}
		select {
		case <-deadline.C:
			t.Fatalf("restart waiting status=%+v", status)
		case <-time.After(time.Millisecond):
		}
	}
	if replacement.serveCalls.Load() != 0 {
		t.Fatal("replacement served a request before the old generation drained")
	}

	close(oldProcess.serveBlock)
	var oldResult *httptest.ResponseRecorder
	select {
	case oldResult = <-oldResponse:
	case <-time.After(time.Second):
		t.Fatal("old generation request did not finish")
	}
	if got := oldResult.Body.String(); got != "ok:old-generation" {
		t.Fatalf("old response=%q want old generation", got)
	}
	waitSignal(t, replacement.ensureAsked, "replacement readiness")

	var newResult *httptest.ResponseRecorder
	select {
	case newResult = <-waitingResponse:
	case <-time.After(time.Second):
		t.Fatal("waiting request did not continue after replacement became ready")
	}
	if got := newResult.Body.String(); got != "ok:new-generation" {
		t.Fatalf("waiting response=%q want new generation", got)
	}
	if oldProcess.stoppedWhileServing.Load() {
		t.Fatal("old process was stopped while its request was still serving")
	}
	factoryMu.Lock()
	defer factoryMu.Unlock()
	if len(factoryConfigs) != 1 || factoryConfigs[0].Cmd != "new" {
		t.Fatalf("factory configs=%v want one desired process config", factoryConfigs)
	}
}

func TestBaseRouter_ForceRestartFencesOldGeneration(t *testing.T) {
	oldConfig := config.Config{
		HealthCheckTimeout: 1,
		Models: map[string]config.ModelConfig{
			"a": {Cmd: "old"},
		},
	}
	oldProcess := newFakeProcess("old-generation")
	oldProcess.markReady()
	oldProcess.serveBlock = make(chan struct{})
	replacement := newFakeProcess("new-generation")
	replacement.autoReady = true

	b := newTestBaseWithConfig(t, oldConfig, map[string]process.Process{"a": oldProcess}, &stubPlanner{})
	b.setProcessFactory(func(_ context.Context, _ string, modelConfig config.ModelConfig) (process.Process, error) {
		if modelConfig.Cmd != "new" {
			return nil, fmt.Errorf("unexpected restart command %q", modelConfig.Cmd)
		}
		return replacement, nil
	})

	oldResponse := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		b.ServeHTTP(response, newRequest("a"))
		oldResponse <- response
	}()
	waitSignal(t, oldProcess.serveStarted, "old generation request")

	desired := oldConfig
	desired.Models = map[string]config.ModelConfig{"a": {Cmd: "new"}}
	if err := b.Reconfigure(desired, &stubPlanner{}, map[string]struct{}{"a": {}}); err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}
	if err := b.RestartModel("a"); err != nil {
		t.Fatalf("RestartModel: %v", err)
	}

	waitingResponse := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		b.ServeHTTP(response, newRequest("a"))
		waitingResponse <- response
	}()
	deadline := time.Now().Add(time.Second)
	for {
		status := b.ModelLifecycleStatuses()["a"]
		if status.ConfigStatus == scheduler.ConfigStatusDraining && status.WaitingRequests == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("restart waiting status=%+v", status)
		}
		time.Sleep(time.Millisecond)
	}

	if err := b.ForceRestartModel("a"); err != nil {
		t.Fatalf("ForceRestartModel: %v", err)
	}
	waitSignal(t, replacement.ensureAsked, "forced replacement readiness")

	select {
	case response := <-waitingResponse:
		if got := response.Body.String(); got != "ok:new-generation" {
			t.Fatalf("waiting response=%q want new generation", got)
		}
	case <-time.After(time.Second):
		t.Fatal("waiting request did not reach the replacement generation")
	}
	if oldProcess.State() != process.StateStopped {
		t.Fatalf("old process state=%q want stopped after force restart", oldProcess.State())
	}

	// The old handler is deliberately released after the replacement has served.
	// Its generation-0 completion must retire only the fenced old counters.
	close(oldProcess.serveBlock)
	select {
	case response := <-oldResponse:
		if got := response.Body.String(); got != "ok:old-generation" {
			t.Fatalf("old response=%q want old generation", got)
		}
	case <-time.After(time.Second):
		t.Fatal("old generation request did not finish after force restart")
	}

	deadline = time.Now().Add(time.Second)
	for {
		if _, ok := b.ModelLifecycleStatuses()["a"]; !ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("force restart lifecycle remained pending: %+v", b.ModelLifecycleStatuses()["a"])
		}
		time.Sleep(time.Millisecond)
	}
}

func TestBaseRouter_ConfigChangeKeepsPendingRestartAfterOnlineEdit(t *testing.T) {
	oldConfig := config.Config{
		HealthCheckTimeout: 1,
		Models: map[string]config.ModelConfig{
			"a": {Cmd: "old"},
		},
	}
	a := newFakeProcess("a")
	a.markReady()
	b := newTestBaseWithConfig(t, oldConfig, map[string]process.Process{"a": a}, &stubPlanner{})

	first := oldConfig
	first.Models = map[string]config.ModelConfig{"a": {Cmd: "new"}}
	if err := b.Reconfigure(first, &stubPlanner{}, map[string]struct{}{"a": {}}); err != nil {
		t.Fatalf("first Reconfigure: %v", err)
	}
	second := first
	second.Models = map[string]config.ModelConfig{"a": {Cmd: "new", Name: "display-only"}}
	if err := b.Reconfigure(second, &stubPlanner{}, map[string]struct{}{"a": {}}); err != nil {
		t.Fatalf("second Reconfigure: %v", err)
	}
	status := b.ModelLifecycleStatuses()["a"]
	if status.ConfigStatus != scheduler.ConfigStatusModified {
		t.Fatalf("config status=%q want pending modified after online edit", status.ConfigStatus)
	}
}

func TestBaseRouter_ReaddDuringRemovalKeepsReplacement(t *testing.T) {
	oldConfig := config.Config{
		HealthCheckTimeout: 1,
		UnloadTimeout:      1,
		Models: map[string]config.ModelConfig{
			"a": {Cmd: "old"},
		},
	}
	oldProcess := newFakeProcess("old-generation")
	oldProcess.markReady()
	oldProcess.stopBlock = make(chan struct{})
	b := newTestBaseWithConfig(t, oldConfig, map[string]process.Process{"a": oldProcess}, &stubPlanner{})
	b.testRemovalProcessed = make(chan struct{}, 1)

	replacement := newFakeProcess("re-added-generation")
	replacement.autoReady = true
	b.setProcessFactory(func(_ context.Context, _ string, modelConfig config.ModelConfig) (process.Process, error) {
		if modelConfig.Cmd != "new" {
			return nil, fmt.Errorf("unexpected re-add command %q", modelConfig.Cmd)
		}
		return replacement, nil
	})

	deleted := oldConfig
	deleted.Models = nil
	if err := b.Reconfigure(deleted, &stubPlanner{}, nil); err != nil {
		t.Fatalf("delete Reconfigure: %v", err)
	}
	waitSignal(t, oldProcess.stopStarted, "removal stop start")

	readded := oldConfig
	readded.Models = map[string]config.ModelConfig{"a": {Cmd: "new"}}
	if err := b.Reconfigure(readded, &stubPlanner{}, map[string]struct{}{"a": {}}); err != nil {
		t.Fatalf("re-add Reconfigure: %v", err)
	}

	b.processMu.RLock()
	got := b.processes["a"]
	b.processMu.RUnlock()
	if got != replacement {
		t.Fatalf("process registry contains %v, want re-added replacement %v", got, replacement)
	}

	newResponse := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		b.ServeHTTP(response, newRequest("a"))
		newResponse <- response
	}()
	waitProcessed(t, b.testProcessed, 1)
	select {
	case <-replacement.ensureAsked:
		t.Fatal("replacement started before the old removal finished")
	default:
	}

	// Let the old asynchronous removal finish. Its generation is stale now and
	// must not delete the replacement that was installed by the re-add; only
	// after that stop completes may the replacement begin serving.
	close(oldProcess.stopBlock)
	waitSignal(t, b.testRemovalProcessed, "stale removal completion")
	waitSignal(t, replacement.ensureAsked, "replacement readiness")
	select {
	case response := <-newResponse:
		if response.Code != http.StatusOK || response.Body.String() != "ok:re-added-generation" {
			t.Fatalf("re-added response=%d %q", response.Code, response.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("re-added request did not complete")
	}
	b.processMu.RLock()
	got = b.processes["a"]
	b.processMu.RUnlock()
	if got != replacement {
		t.Fatal("stale removal completion deleted the re-added process")
	}
}

func TestBaseRouter_IgnoredWebsocketDrainsBeforeConfigRestart(t *testing.T) {
	oldConfig := config.Config{
		HealthCheckTimeout: 1,
		Models: map[string]config.ModelConfig{
			"a": {Cmd: "old", Compat: config.CompatConfig{IgnoreWebsockets: true}},
		},
	}
	oldProcess := newFakeProcess("old-generation")
	oldProcess.markReady()
	oldProcess.serveBlock = make(chan struct{})
	replacement := newFakeProcess("new-generation")
	replacement.autoReady = true
	b := newTestBaseWithConfig(t, oldConfig, map[string]process.Process{"a": oldProcess}, &stubPlanner{})
	b.setProcessFactory(func(_ context.Context, _ string, modelConfig config.ModelConfig) (process.Process, error) {
		if modelConfig.Cmd != "new" {
			return nil, fmt.Errorf("unexpected restart command %q", modelConfig.Cmd)
		}
		return replacement, nil
	})

	websocketDone := make(chan struct{})
	go func() {
		defer close(websocketDone)
		r := httptest.NewRequest(http.MethodGet, "/props?model=a", nil)
		r.Header.Set("Connection", "Upgrade")
		r.Header.Set("Upgrade", "websocket")
		b.ServeHTTP(httptest.NewRecorder(), r)
	}()
	waitSignal(t, oldProcess.serveStarted, "ignored websocket start")

	desired := oldConfig
	desired.Models = map[string]config.ModelConfig{
		"a": {Cmd: "new", Compat: config.CompatConfig{IgnoreWebsockets: true}},
	}
	if err := b.Reconfigure(desired, &stubPlanner{}, map[string]struct{}{"a": {}}); err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}
	if err := b.RestartModel("a"); err != nil {
		t.Fatalf("RestartModel: %v", err)
	}
	if got := oldProcess.stopCalls.Load(); got != 0 {
		t.Fatalf("old process stopped before websocket drained: %d calls", got)
	}
	select {
	case <-replacement.ensureAsked:
		t.Fatal("replacement started before the existing websocket drained")
	default:
	}

	close(oldProcess.serveBlock)
	waitSignal(t, websocketDone, "ignored websocket finish")
	waitSignal(t, oldProcess.stopStarted, "restart stop start")
	waitSignal(t, replacement.ensureAsked, "replacement readiness")
	if oldProcess.stoppedWhileServing.Load() {
		t.Fatal("old process stopped while ignored websocket was serving")
	}
}

func TestBaseRouter_RuntimeRestartFailureRestoresExactOldGeneration(t *testing.T) {
	oldConfig := config.Config{
		HealthCheckTimeout: 1,
		Models: map[string]config.ModelConfig{
			"a": {Backend: config.BackendConfig{Type: "vllm", Runtime: "vllm", Arguments: []string{"/runtime/vllm/versions/old/.venv/bin/vllm", "serve", "model"}}},
		},
	}
	oldProcess := newFakeProcess("old-generation")
	oldProcess.markReady()
	oldProcess.serveBlock = make(chan struct{})
	candidate := newFakeProcess("candidate-generation")
	candidate.ensureErr = fmt.Errorf("candidate generation failed")
	rollback := newFakeProcess("rollback-generation")
	rollback.autoReady = true

	b := newTestBaseWithConfig(t, oldConfig, map[string]process.Process{"a": oldProcess}, &stubPlanner{})
	var factoryMu sync.Mutex
	var factoryConfigs []config.ModelConfig
	b.setProcessFactory(func(_ context.Context, _ string, modelConfig config.ModelConfig) (process.Process, error) {
		factoryMu.Lock()
		factoryConfigs = append(factoryConfigs, modelConfig)
		factoryMu.Unlock()
		if strings.Contains(modelConfig.Backend.Arguments[0], "/versions/new/") {
			return candidate, nil
		}
		if strings.Contains(modelConfig.Backend.Arguments[0], "/versions/old/") {
			return rollback, nil
		}
		return nil, fmt.Errorf("unexpected runtime executable %q", modelConfig.Backend.Arguments)
	})

	oldResponse := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		b.ServeHTTP(response, newRequest("a"))
		oldResponse <- response
	}()
	waitSignal(t, oldProcess.serveStarted, "old runtime generation request")

	desired := oldConfig
	desired.Models = map[string]config.ModelConfig{
		"a": {Backend: config.BackendConfig{Type: "vllm", Runtime: "vllm", Arguments: []string{"/runtime/vllm/versions/new/.venv/bin/vllm", "serve", "model"}}},
	}
	if err := b.Reconfigure(desired, &stubPlanner{}, map[string]struct{}{"a": {}}); err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}
	if err := b.RestartModel("a"); err != nil {
		t.Fatalf("RestartModel: %v", err)
	}
	waitingResponse := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		b.ServeHTTP(response, newRequest("a"))
		waitingResponse <- response
	}()
	select {
	case <-candidate.ensureAsked:
		t.Fatal("candidate started before the in-flight old request drained")
	default:
	}
	if oldProcess.stopCalls.Load() != 0 {
		t.Fatal("old runtime generation stopped while its request was in flight")
	}

	close(oldProcess.serveBlock)
	select {
	case response := <-oldResponse:
		if got := response.Body.String(); got != "ok:old-generation" {
			t.Fatalf("old response=%q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("old runtime generation request did not finish")
	}
	waitSignal(t, candidate.ensureAsked, "candidate readiness attempt")
	waitSignal(t, rollback.ensureAsked, "rollback runtime generation")
	select {
	case response := <-waitingResponse:
		if response.Code != http.StatusOK || response.Body.String() != "ok:rollback-generation" {
			t.Fatalf("waiting response=%d %q", response.Code, response.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("waiting request did not reach the restored runtime generation")
	}
	if oldProcess.stoppedWhileServing.Load() {
		t.Fatal("old runtime generation was stopped while serving")
	}
	if status := b.ModelLifecycleStatuses()["a"]; status.ConfigStatus != scheduler.ConfigStatusApplyFailed {
		t.Fatalf("lifecycle status=%+v want apply_failed after rollback", status)
	}
	factoryMu.Lock()
	defer factoryMu.Unlock()
	if len(factoryConfigs) != 2 || !strings.Contains(factoryConfigs[0].Backend.Arguments[0], "/versions/new/") || !strings.Contains(factoryConfigs[1].Backend.Arguments[0], "/versions/old/") {
		t.Fatalf("runtime factory configs=%v want candidate then exact old generation", factoryConfigs)
	}
}

func TestBaseRouter_Shutdown_StopsAllProcesses(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	go a.Run(0)
	pb := newFakeProcess("b")
	pb.markReady()
	go pb.Run(0)

	b := newTestBase(t, map[string]process.Process{"a": a, "b": pb}, &stubPlanner{})

	if err := b.Shutdown(time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := a.stopCalls.Load(); got != 1 {
		t.Errorf("a.stopCalls=%d want 1", got)
	}
	if got := pb.stopCalls.Load(); got != 1 {
		t.Errorf("b.stopCalls=%d want 1", got)
	}

	// Subsequent ServeHTTP should report 5xx.
	w := httptest.NewRecorder()
	b.ServeHTTP(w, newRequest("a"))
	if w.Code != http.StatusInternalServerError && w.Code != http.StatusServiceUnavailable {
		t.Errorf("post-shutdown status=%d want 5xx body=%q", w.Code, w.Body.String())
	}

	// Second Shutdown should report already in progress.
	if err := b.Shutdown(0); err == nil {
		t.Errorf("second Shutdown returned nil, want error")
	}
}

// TestBaseRouter_SetProcessDecorator covers the server seam: existing
// *ProcessCommands are decorated synchronously (non-ProcessCommand doubles
// are skipped) and every later process creation goes through the decorator.
func TestBaseRouter_SetProcessDecorator(t *testing.T) {
	logger := logmon.NewWriter(io.Discard)
	existing, err := process.New(context.Background(), "existing", config.ModelConfig{Cmd: "true"}, logger, logger)
	if err != nil {
		t.Fatalf("process.New: %v", err)
	}
	t.Cleanup(func() { existing.Stop(time.Second) }) //nolint: errcheck

	b := newTestBase(t, map[string]process.Process{
		"existing": existing,
		"fake":     newFakeProcess("fake"),
	}, &stubPlanner{})

	var (
		mu        sync.Mutex
		decorated []string
	)
	b.SetProcessDecorator(func(modelID string, p *process.ProcessCommand) {
		mu.Lock()
		decorated = append(decorated, modelID)
		mu.Unlock()
	})

	mu.Lock()
	got := append([]string(nil), decorated...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "existing" {
		t.Fatalf("existing decorations = %v, want [existing] (fake double skipped)", got)
	}

	created, err := b.newManagedProcess("created", config.ModelConfig{Cmd: "true"})
	if err != nil {
		t.Fatalf("newManagedProcess: %v", err)
	}
	if _, ok := created.(*process.ProcessCommand); !ok {
		t.Fatalf("newManagedProcess returned %T, want *process.ProcessCommand", created)
	}
	created.Stop(time.Second) //nolint: errcheck

	mu.Lock()
	got = append([]string(nil), decorated...)
	mu.Unlock()
	if len(got) != 2 || got[0] != "existing" || got[1] != "created" {
		t.Fatalf("decorations = %v, want [existing created]", got)
	}
}

func TestBaseRouter_ReconfigureReplacementCarriesLogHistory(t *testing.T) {
	oldConfig := config.Config{
		HealthCheckTimeout: 1,
		Models: map[string]config.ModelConfig{
			"a": {Cmd: "old"},
		},
	}
	oldMonitor := logmon.NewWriter(io.Discard)
	oldMonitor.Write([]byte("previous run output"))
	oldProcess := newFakeProcess("old-generation")
	oldProcess.logger = oldMonitor // stopped by default, so reconfigure refreshes the object
	b := newTestBaseWithConfig(t, oldConfig, map[string]process.Process{"a": oldProcess}, &stubPlanner{})

	desired := oldConfig
	desired.Models = map[string]config.ModelConfig{"a": {Cmd: "new"}}
	if err := b.Reconfigure(desired, &stubPlanner{}, map[string]struct{}{"a": {}}); err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}

	replacementLog, ok := b.ProcessLogger("a")
	if !ok {
		t.Fatal("model a has no process logger after reconfigure")
	}
	if replacementLog == oldMonitor {
		t.Fatal("reconfigure kept the replaced process's monitor")
	}
	history := string(replacementLog.GetHistory())
	if !strings.Contains(history, "previous run output") {
		t.Fatalf("replacement history = %q, want inherited previous-run output", history)
	}
	if !strings.Contains(history, processReplacedMarker) {
		t.Fatalf("replacement history = %q, want the replacement marker", history)
	}
	if !oldMonitor.IsClosed() {
		t.Fatal("replaced process's log monitor was not closed")
	}
}

func TestBaseRouter_ConfigRestartCarriesLogHistory(t *testing.T) {
	oldConfig := config.Config{
		HealthCheckTimeout: 1,
		Models: map[string]config.ModelConfig{
			"a": {Cmd: "old"},
		},
	}
	oldMonitor := logmon.NewWriter(io.Discard)
	oldMonitor.Write([]byte("previous generation output"))
	oldProcess := newFakeProcess("old-generation")
	oldProcess.logger = oldMonitor
	oldProcess.markReady()
	b := newTestBaseWithConfig(t, oldConfig, map[string]process.Process{"a": oldProcess}, &stubPlanner{})

	replacementMonitor := logmon.NewWriter(io.Discard)
	replacement := newFakeProcess("new-generation")
	replacement.autoReady = true
	replacement.logger = replacementMonitor
	b.setProcessFactory(func(_ context.Context, _ string, _ config.ModelConfig) (process.Process, error) {
		return replacement, nil
	})

	desired := oldConfig
	desired.Models = map[string]config.ModelConfig{"a": {Cmd: "new"}}
	if err := b.Reconfigure(desired, &stubPlanner{}, map[string]struct{}{"a": {}}); err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}
	if err := b.RestartModel("a"); err != nil {
		t.Fatalf("RestartModel: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if log, ok := b.ProcessLogger("a"); ok && log == replacementMonitor {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("restart did not install the replacement process")
		}
		time.Sleep(time.Millisecond)
	}
	history := string(replacementMonitor.GetHistory())
	if !strings.Contains(history, "previous generation output") {
		t.Fatalf("replacement history = %q, want inherited previous-generation output", history)
	}
	if !strings.Contains(history, processReplacedMarker) {
		t.Fatalf("replacement history = %q, want the replacement marker", history)
	}
	if !oldMonitor.IsClosed() {
		t.Fatal("replaced process's log monitor was not closed after the restart")
	}
}

func TestBaseRouter_ConfigRestartExposesPendingProcess(t *testing.T) {
	oldConfig := config.Config{
		HealthCheckTimeout: 5,
		Models: map[string]config.ModelConfig{
			"a": {Cmd: "old"},
		},
	}
	oldProcess := newFakeProcess("old-generation")
	oldProcess.markReady()
	candidate := newFakeProcess("new-generation")
	b := newTestBaseWithConfig(t, oldConfig, map[string]process.Process{"a": oldProcess}, &stubPlanner{})
	b.setProcessFactory(func(_ context.Context, _ string, modelConfig config.ModelConfig) (process.Process, error) {
		if modelConfig.Cmd != "new" {
			return nil, fmt.Errorf("unexpected restart command %q", modelConfig.Cmd)
		}
		return candidate, nil
	})

	desired := oldConfig
	desired.Models = map[string]config.ModelConfig{"a": {Cmd: "new"}}
	if err := b.Reconfigure(desired, &stubPlanner{}, map[string]struct{}{"a": {}}); err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}
	if err := b.RestartModel("a"); err != nil {
		t.Fatalf("RestartModel: %v", err)
	}
	waitSignal(t, candidate.ensureAsked, "candidate readiness")

	if state := b.RunningModels()["a"]; state != process.StateStarting {
		t.Fatalf("running state=%q want candidate starting state", state)
	}
	log, ok := b.ProcessLogger("a")
	if !ok || log != candidate.Logger() {
		t.Fatalf("process logger=%p ok=%v want candidate logger", log, ok)
	}

	candidate.markReady()
	deadline := time.Now().Add(time.Second)
	for {
		b.processMu.RLock()
		installed := b.processes["a"] == candidate
		b.processMu.RUnlock()
		if installed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("candidate process was not installed after readiness")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestBaseRouter_CanceledRestartPublishesTerminalStatus(t *testing.T) {
	oldConfig := config.Config{
		HealthCheckTimeout: 1,
		Models: map[string]config.ModelConfig{
			"a": {Cmd: "old"},
		},
	}
	oldProcess := newFakeProcess("old-generation")
	oldProcess.markReady()
	oldProcess.stopBlock = make(chan struct{})
	b := newTestBaseWithConfig(t, oldConfig, map[string]process.Process{"a": oldProcess}, &stubPlanner{})
	b.setProcessFactory(func(_ context.Context, _ string, _ config.ModelConfig) (process.Process, error) {
		return newFakeProcess("unexpected-generation"), nil
	})

	desired := oldConfig
	desired.Models = map[string]config.ModelConfig{"a": {Cmd: "new"}}
	if err := b.Reconfigure(desired, &stubPlanner{}, map[string]struct{}{"a": {}}); err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}
	if err := b.RestartModel("a"); err != nil {
		t.Fatalf("RestartModel: %v", err)
	}
	waitSignal(t, oldProcess.stopStarted, "restart stop")

	b.restartMu.Lock()
	control, ok := b.restartCancels["a"]
	if ok {
		control.cancel()
	}
	b.restartMu.Unlock()
	if !ok {
		t.Fatal("restart control was not registered")
	}
	close(oldProcess.stopBlock)

	deadline := time.Now().Add(time.Second)
	for {
		status := b.ModelLifecycleStatuses()["a"]
		if status.ConfigStatus == scheduler.ConfigStatusApplyFailed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("restart remained non-terminal: %+v", status)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestBaseRouter_RemoveProcessClosesLogMonitor(t *testing.T) {
	oldConfig := config.Config{
		HealthCheckTimeout: 1,
		UnloadTimeout:      1,
		Models: map[string]config.ModelConfig{
			"a": {Cmd: "old"},
		},
	}
	monitor := logmon.NewWriter(io.Discard)
	monitor.Write([]byte("removed generation output"))
	oldProcess := newFakeProcess("a")
	oldProcess.logger = monitor
	oldProcess.markReady()
	b := newTestBaseWithConfig(t, oldConfig, map[string]process.Process{"a": oldProcess}, &stubPlanner{})
	b.testRemovalProcessed = make(chan struct{}, 1)

	deleted := oldConfig
	deleted.Models = nil
	if err := b.Reconfigure(deleted, &stubPlanner{}, nil); err != nil {
		t.Fatalf("delete Reconfigure: %v", err)
	}
	waitSignal(t, b.testRemovalProcessed, "removal completion")

	if _, ok := b.ProcessLogger("a"); ok {
		t.Fatal("removed model still resolves to a process logger")
	}
	if !monitor.IsClosed() {
		t.Fatal("removed process's log monitor was not closed")
	}
	// History stays readable for late callers such as incident archives.
	if got := string(monitor.GetHistory()); !strings.Contains(got, "removed generation output") {
		t.Fatalf("closed monitor history = %q, want the captured output", got)
	}
}

// TestBaseRouter_ModelLogsAttributedUpstream pins the merged upstream log's
// model attribution: a model's monitor forwards its output through a prefix
// writer, so the combined log can tell which model produced each line.
func TestBaseRouter_ModelLogsAttributedUpstream(t *testing.T) {
	conf := config.Config{HealthCheckTimeout: 1}
	upstream := logmon.NewWriter(io.Discard)
	b, err := newBaseRouter("test", conf, map[string]process.Process{}, upstream, &stubPlanner{})
	if err != nil {
		t.Fatalf("newBaseRouter: %v", err)
	}
	created, err := b.newManagedProcess("attributed-model", config.ModelConfig{Cmd: "true"})
	if err != nil {
		t.Fatalf("newManagedProcess: %v", err)
	}
	created.Logger().Write([]byte("inference output line\n"))

	history := string(upstream.GetHistory())
	if !strings.Contains(history, "[attributed-model] inference output line") {
		t.Fatalf("upstream history = %q, want the model-prefixed line", history)
	}
}

// TestBaseRouter_ReconfigureDuringManualStartKeepsProcess verifies that a
// config reconcile arriving while a manual start's swap goroutine is between
// its registry read and EnsureReady does not replace the process object. The
// swap goroutine may still read the registry and start the old generation;
// replacing the object in that window orphans that process outside every
// lifecycle fence while the registry shows a stopped replacement.
func TestBaseRouter_ReconfigureDuringManualStartKeepsProcess(t *testing.T) {
	oldConfig := config.Config{
		HealthCheckTimeout: 5,
		Models: map[string]config.ModelConfig{
			"a": {Cmd: "old"},
		},
	}
	oldProcess := newFakeProcess("a")
	// autoReady=false: EnsureReady blocks until the test releases it, and the
	// test parks the swap goroutine before it can flip the state to starting
	// by holding the fake's opMu (the same serialization point EnsureReady
	// crosses after closing ensureAsked).
	b := newTestBaseWithConfig(t, oldConfig, map[string]process.Process{"a": oldProcess}, &stubPlanner{})
	replacement := newFakeProcess("new-generation")
	replacement.autoReady = true
	var factoryCalls atomic.Int32
	b.setProcessFactory(func(_ context.Context, _ string, _ config.ModelConfig) (process.Process, error) {
		factoryCalls.Add(1)
		return replacement, nil
	})

	oldProcess.opMu.Lock()
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		b.ServeHTTP(rec, newRequest("a"))
		response <- rec
	}()
	// The swap goroutine reached EnsureReady (ensureAsked closed) while the
	// process still reads StateStopped: the exact race window.
	waitSignal(t, oldProcess.ensureAsked, "swap goroutine EnsureReady")
	select {
	case <-b.testProcessed:
	default:
		t.Fatal("request not yet processed by the run loop")
	}

	desired := oldConfig
	desired.Models = map[string]config.ModelConfig{"a": {Cmd: "new"}}
	if err := b.Reconfigure(desired, &stubPlanner{}, map[string]struct{}{"a": {}}); err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}

	if got := factoryCalls.Load(); got != 0 {
		t.Fatalf("factory called %d times during in-flight swap; the process object must not be replaced", got)
	}
	b.processMu.RLock()
	registered := b.processes["a"]
	b.processMu.RUnlock()
	if registered != process.Process(oldProcess) {
		t.Fatalf("registry holds %v, want the old-generation process", registered)
	}
	if status := b.ModelLifecycleStatuses()["a"]; status.ConfigStatus != scheduler.ConfigStatusModified {
		t.Fatalf("config status=%q want %q", status.ConfigStatus, scheduler.ConfigStatusModified)
	}

	// Release the swap goroutine: the old-generation start completes and the
	// manually started request is served normally.
	oldProcess.opMu.Unlock()
	oldProcess.markReady()
	rec := <-response
	if rec.Code != http.StatusOK {
		t.Fatalf("manual start request status=%d body=%q", rec.Code, rec.Body.String())
	}

	// The pending modified config stays restartable after the swap settles.
	if err := b.RestartModel("a"); err != nil {
		t.Fatalf("RestartModel after swap settled: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		b.processMu.RLock()
		state := b.processes["a"].State()
		b.processMu.RUnlock()
		if state == process.StateReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("replacement process never became ready, state=%v", state)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestBaseRouter_StopAdoptsPendingConfig pins the rule an operator expects: once
// a model is stopped there is nothing left to disturb, so its next start has to
// use the configuration saved after it last ran — and the pending-restart flag
// has nothing left to describe.
//
// Before this, stopping kept both the flag and the stale process object, so a
// manual stop-then-start silently ran the previous configuration while the UI
// kept asking for a restart the operator had just performed.
func TestBaseRouter_StopAdoptsPendingConfig(t *testing.T) {
	original := config.Config{
		HealthCheckTimeout: 1,
		Models:             map[string]config.ModelConfig{"a": {Cmd: "a-one"}},
	}
	running := newFakeProcess("running")
	running.markReady()
	b := newTestBaseWithConfig(t, original, map[string]process.Process{"a": running}, &stubPlanner{})

	adopted := newFakeProcess("adopted")
	var mu sync.Mutex
	var built []config.ModelConfig
	b.setProcessFactory(func(_ context.Context, _ string, modelConfig config.ModelConfig) (process.Process, error) {
		mu.Lock()
		built = append(built, modelConfig)
		mu.Unlock()
		return adopted, nil
	})

	pending := original
	pending.Models = map[string]config.ModelConfig{"a": {Cmd: "a-two"}}
	if err := b.Reconfigure(pending, &stubPlanner{}, map[string]struct{}{"a": {}}); err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}
	if got := b.ModelLifecycleStatuses()["a"].ConfigStatus; got != scheduler.ConfigStatusModified {
		t.Fatalf("config status=%q, want %q while the change is pending", got, scheduler.ConfigStatusModified)
	}

	b.Unload(time.Second, "a")

	if got := b.ModelLifecycleStatuses()["a"].ConfigStatus; got == scheduler.ConfigStatusModified {
		t.Fatalf("config status=%q, want the pending restart cleared once the model stopped", got)
	}
	b.processMu.RLock()
	registered := b.processes["a"]
	b.processMu.RUnlock()
	if registered != process.Process(adopted) {
		t.Fatalf("registered process=%v, want the replacement built from the saved configuration", registered)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(built) != 1 || built[0].Cmd != "a-two" {
		t.Fatalf("built=%+v, want exactly one process from the pending configuration (Cmd=a-two)", built)
	}
}
