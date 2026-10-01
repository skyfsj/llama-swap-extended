package process

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

const (
	testStartTimeout    = 3 * time.Second
	testStopTimeout     = 2 * time.Second
	testReturnTimeout   = 1 * time.Second
	testPollInterval    = 20 * time.Millisecond
	testLogPollInterval = 10 * time.Millisecond
)

func newProcessCommand(t *testing.T, conf config.ModelConfig) *ProcessCommand {
	t.Helper()
	logger := logmon.NewWriter(io.Discard)
	p, err := New(context.Background(), t.Name(), conf, logger, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// runAsyncBudget bounds the start request and the readiness wait for tests that
// only need the process running. It is deliberately far above the roughly one
// second a local start takes: those tests assert behaviour (a stop that
// returns, a forking wrapper that gets reaped), and the shared 3s
// testStartTimeout is tight enough that CPU contention during a full
// `-race ./...` run turns them into flakes. The failures they guard against were
// unbounded hangs, so any reasonable cap still distinguishes pass from fail.
const runAsyncBudget = 30 * time.Second

// runAsync starts Run in a goroutine and waits until the process is ready,
// matching the new interface contract where Run blocks until the process is
// terminated. Returns a channel that delivers Run's eventual error.
func runAsync(t *testing.T, p *ProcessCommand) <-chan error {
	t.Helper()
	ch := make(chan error, 1)
	go func() { ch <- p.Run(runAsyncBudget) }()
	ctx, cancel := context.WithTimeout(context.Background(), runAsyncBudget)
	defer cancel()
	if err := p.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	return ch
}

// waitForState polls until the process reaches want, failing the test if it
// does not get there in time.
func waitForState(t *testing.T, p *ProcessCommand, want ProcessState) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := p.State(); got == want {
			return
		}
		time.Sleep(testPollInterval)
	}
	t.Fatalf("state is %s, want %s", p.State(), want)
}

// TestProcessCommand_EnsureReadyDuringStop is the regression test for issue
// #946: a caller that wants the process serving while it is being stopped must
// wait for the stop to finish and then get a freshly started process, instead
// of hanging forever.
//
// -ignore-sig-term makes the upstream survive the graceful signal, so the
// process sits in StateStopping for the whole unload timeout rather than
// milliseconds. That is the deterministic form of the reporter's `kill -STOP`
// reproduction: EnsureReady is provably called mid-stop.
func TestProcessCommand_EnsureReadyDuringStop(t *testing.T) {
	skipIfNoSimpleResponder(t)

	cmd, port := simpleResponderCmd(t, "-silent", "-ignore-sig-term")
	p := newProcessCommand(t, config.ModelConfig{
		Cmd:                cmd,
		Proxy:              fmt.Sprintf("http://127.0.0.1:%d", port),
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
	})
	t.Cleanup(func() { p.Stop(testStopTimeout) }) //nolint: errcheck

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := p.EnsureReady(ctx, testStartTimeout); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}

	stopDone := make(chan struct{})
	go func() {
		defer close(stopDone)
		p.Stop(testStopTimeout) //nolint: errcheck
	}()

	waitForState(t, p, StateStopping)

	if err := p.EnsureReady(ctx, testStartTimeout); err != nil {
		t.Fatalf("EnsureReady during stop: %v", err)
	}
	if got := p.State(); got != StateReady {
		t.Errorf("State() = %s, want %s", got, StateReady)
	}

	select {
	case <-stopDone:
	case <-time.After(testReturnTimeout):
		t.Error("Stop did not return")
	}
}

// TestProcessCommand_EnsureReadyIsIdempotent covers the settled states:
// EnsureReady on a ready process is a no-op, and it reports an error once the
// process has been shut down.
func TestProcessCommand_EnsureReadyIsIdempotent(t *testing.T) {
	skipIfNoSimpleResponder(t)

	parentCtx, cancelParent := context.WithCancel(context.Background())
	logger := logmon.NewWriter(io.Discard)
	cmd, port := simpleResponderCmd(t, "-silent")
	p, err := New(parentCtx, t.Name(), config.ModelConfig{
		Cmd:                cmd,
		Proxy:              fmt.Sprintf("http://127.0.0.1:%d", port),
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
	}, logger, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { p.Stop(testStopTimeout) }) //nolint: errcheck

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for i := 0; i < 3; i++ {
		if err := p.EnsureReady(ctx, testStartTimeout); err != nil {
			t.Fatalf("EnsureReady call %d: %v", i+1, err)
		}
		if got := p.State(); got != StateReady {
			t.Fatalf("State() = %s after call %d, want %s", got, i+1, StateReady)
		}
	}

	cancelParent()
	waitForState(t, p, StateShutdown)
	if err := p.EnsureReady(ctx, testStartTimeout); err == nil {
		t.Error("EnsureReady after shutdown: expected error, got nil")
	}
}

func TestProcessCommand_StartStop(t *testing.T) {
	skipIfNoSimpleResponder(t)

	cmd, port := simpleResponderCmd(t, "-silent", "-respond hello")
	p := newProcessCommand(t, config.ModelConfig{
		Cmd:                cmd,
		Proxy:              fmt.Sprintf("http://127.0.0.1:%d", port),
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
	})
	t.Cleanup(func() { p.Stop(testStopTimeout) })

	req := httptest.NewRequest("GET", "/test", nil)

	// before start: no handler
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("before start: expected 503, got %d", rr.Code)
	}
	if body := rr.Body.String(); !strings.Contains(body, `"src":"llama-swap"`) || !strings.Contains(body, "process is not ready") {
		t.Errorf("before start: expected llama-swap error envelope, got %q", body)
	}

	runErr := runAsync(t, p)
	if got := p.State(); got != StateReady {
		t.Errorf("after Run: expected state %s, got %s", StateReady, got)
	}

	rr = httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("after Run: expected 200, got %d", rr.Code)
	}
	if body := rr.Body.String(); body != "hello" {
		t.Errorf("expected body %q, got %q", "hello", body)
	}

	if err := p.Stop(testStopTimeout); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
	if got := p.State(); got != StateStopped {
		t.Errorf("after Stop: expected state %s, got %s", StateStopped, got)
	}
	select {
	case err := <-runErr:
		if err != nil {
			t.Errorf("Run() after Stop: expected nil, got %v", err)
		}
	case <-time.After(testReturnTimeout):
		t.Fatal("Run() did not return after Stop")
	}

	// after stop: handler cleared
	rr = httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("after stop: expected 503, got %d", rr.Code)
	}
	if body := rr.Body.String(); !strings.Contains(body, `"src":"llama-swap"`) || !strings.Contains(body, "process is not ready") {
		t.Errorf("after stop: expected llama-swap error envelope, got %q", body)
	}
}

func TestProcessCommand_Run_Idempotent(t *testing.T) {
	skipIfNoSimpleResponder(t)

	cmd, port := simpleResponderCmd(t, "-silent")
	p := newProcessCommand(t, config.ModelConfig{
		Cmd:                cmd,
		Proxy:              fmt.Sprintf("http://127.0.0.1:%d", port),
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
	})
	t.Cleanup(func() { p.Stop(testStopTimeout) })

	runErr := runAsync(t, p)

	if err := p.Run(testStartTimeout); err == nil {
		t.Error("second Run() while running: expected error, got nil")
	}

	if err := p.Stop(testStopTimeout); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
	select {
	case <-runErr:
	case <-time.After(testReturnTimeout):
		t.Fatal("Run() did not return after Stop")
	}
}

func TestProcessCommand_Stop_Idempotent(t *testing.T) {
	skipIfNoSimpleResponder(t)

	cmd, port := simpleResponderCmd(t, "-silent")
	p := newProcessCommand(t, config.ModelConfig{
		Cmd:                cmd,
		Proxy:              fmt.Sprintf("http://127.0.0.1:%d", port),
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
	})

	if err := p.Stop(testStopTimeout); err != nil {
		t.Fatalf("Stop() before Run(): %v", err)
	}

	runErr := runAsync(t, p)

	if err := p.Stop(testStopTimeout); err != nil {
		t.Fatalf("first Stop() error: %v", err)
	}
	select {
	case <-runErr:
	case <-time.After(testReturnTimeout):
		t.Fatal("Run() did not return after Stop")
	}

	if err := p.Stop(testStopTimeout); err != nil {
		t.Fatalf("second Stop() error: %v", err)
	}
}

// TestProcessCommand_StopCancelsRun verifies that a Stop sent while Run is
// executing its health-check loop returns ErrAbort to the Run caller.
//
// A blocking mock HTTP server is used as the proxy so the test can deterministically
// know when doStart is inside the health-check loop before issuing Stop.
func TestProcessCommand_StopCancelsRun(t *testing.T) {
	skipIfNoSimpleResponder(t)

	healthCheckStarted := make(chan struct{}, 1)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Signal that a health check is in-flight, then block until the client
		// cancels (which happens when Stop cancels the start context).
		select {
		case healthCheckStarted <- struct{}{}:
		default:
		}
		<-r.Context().Done()
		http.Error(w, "mock cancelled", http.StatusServiceUnavailable)
	}))
	defer mock.Close()

	// simple-responder is the real process; health checks go to the blocking mock.
	cmd, _ := simpleResponderCmd(t, "-silent")
	p := newProcessCommand(t, config.ModelConfig{
		Cmd:                cmd,
		Proxy:              mock.URL,
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 30,
	})

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- p.Run(testStartTimeout)
	}()

	// Block until doStart is actually performing a health check, guaranteeing
	// that Run is in-flight when Stop is called.
	<-healthCheckStarted

	if err := p.Stop(testStopTimeout); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}

	if err := <-runErrCh; !errors.Is(err, ErrStartAborted) {
		t.Errorf("expected ErrStartAborted from Run, got %v", err)
	}
}

// TestProcessCommand_ParentCtxCancelDuringStart verifies that cancelling the
// parent context while doStart is health-checking causes the process to
// transition to StateShutdown promptly, not wait for the health-check timeout.
//
// This is the config-reload race: Stop() returns early when parentCtx is
// already done and never writes to stopCh, so without a parentCtx.Done()
// case in the inner select, the process would keep loading indefinitely.
func TestProcessCommand_ParentCtxCancelDuringStart(t *testing.T) {
	skipIfNoSimpleResponder(t)

	healthCheckStarted := make(chan struct{}, 1)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case healthCheckStarted <- struct{}{}:
		default:
		}
		<-r.Context().Done()
		http.Error(w, "mock cancelled", http.StatusServiceUnavailable)
	}))
	defer mock.Close()

	parentCtx, cancelParent := context.WithCancel(context.Background())
	logger := logmon.NewWriter(io.Discard)
	cmd, _ := simpleResponderCmd(t, "-silent")
	p, err := New(parentCtx, t.Name(), config.ModelConfig{
		Cmd:                cmd,
		Proxy:              mock.URL,
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 60,
	}, logger, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	runErrCh := make(chan error, 1)
	go func() { runErrCh <- p.Run(60 * time.Second) }()

	<-healthCheckStarted

	// Cancel parent context to simulate a config reload tearing down the old server.
	cancelParent()

	select {
	case err := <-runErrCh:
		if !strings.Contains(err.Error(), "shutdown") {
			t.Errorf("Run error = %v, want shutdown error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("process did not shut down within 5s after parent context cancel during start")
	}

	// Run() may return before the run() goroutine writes StateShutdown;
	// poll briefly to avoid a spurious race in the assertion.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.State() == StateShutdown {
			break
		}
		time.Sleep(testPollInterval)
	}
	if got := p.State(); got != StateShutdown {
		t.Errorf("after cancel: expected StateShutdown, got %s", got)
	}
}

// TestProcessCommand_EnsureReadyTimeoutCancelsBlockedHealthCheck verifies that
// the readiness timeout also cancels a health request which is waiting for
// response headers. A zero ResponseHeader timeout is valid configuration, so
// the start context — rather than the transport — must provide the bound.
func TestProcessCommand_EnsureReadyTimeoutCancelsBlockedHealthCheck(t *testing.T) {
	skipIfNoSimpleResponder(t)

	healthCheckStarted := make(chan struct{}, 1)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case healthCheckStarted <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer mock.Close()

	cmd, _ := simpleResponderCmd(t, "-silent")
	p := newProcessCommand(t, config.ModelConfig{
		Cmd:                cmd,
		Proxy:              mock.URL,
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
		// The default is zero (no response-header timeout); the process start
		// deadline must still interrupt the probe.
		Timeouts: config.TimeoutsConfig{ResponseHeader: 0},
	})
	t.Cleanup(func() { _ = p.Stop(testStopTimeout) })

	const startTimeout = 600 * time.Millisecond
	result := make(chan error, 1)
	go func() {
		result <- p.EnsureReady(context.Background(), startTimeout)
	}()

	select {
	case <-healthCheckStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("health check did not start")
	}

	select {
	case err := <-result:
		// The startup deadline must be attributed to the health-check window
		// rather than reported as a bare "aborted": a bare abort is
		// indistinguishable from an operator stop and sends anyone reading the
		// lifecycle error looking for a cancellation that never happened.
		if !errors.Is(err, ErrStartAborted) {
			t.Fatalf("EnsureReady error = %v, want ErrStartAborted", err)
		}
		if !strings.Contains(err.Error(), "health check timed out") {
			t.Fatalf("EnsureReady error = %v, want an explicit health check timeout", err)
		}
	case <-time.After(2 * time.Second):
		_ = p.Stop(testStopTimeout)
		t.Fatal("EnsureReady remained blocked after its startup timeout")
	}

	waitForState(t, p, StateStopped)
}

// TestProcessCommand_RunStopCycle runs several sequential start/stop pairs on
// fresh processes to confirm they are reusable.
func TestProcessCommand_RunStopCycle(t *testing.T) {
	skipIfNoSimpleResponder(t)

	for i := range 3 {
		cmd, port := simpleResponderCmd(t, "-silent")
		p := newProcessCommand(t, config.ModelConfig{
			Cmd:                cmd,
			Proxy:              fmt.Sprintf("http://127.0.0.1:%d", port),
			CheckEndpoint:      "/health",
			HealthCheckTimeout: 10,
		})

		runErr := runAsync(t, p)

		req := httptest.NewRequest("GET", "/health", nil)
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("cycle %d: expected 200 from /health, got %d", i, rr.Code)
		}

		if err := p.Stop(testStopTimeout); err != nil {
			t.Fatalf("cycle %d Stop() error: %v", i, err)
		}
		select {
		case <-runErr:
		case <-time.After(testReturnTimeout):
			t.Fatalf("cycle %d: Run() did not return after Stop", i)
		}
	}
}

// TestProcessCommand_ReverseProxyPanicIsRecovered drives the full proxy path:
// the upstream responds healthy on /health (so Run completes), then on the
// actual proxied request it hijacks the connection and closes it mid-body.
// That upstream EOF makes httputil.ReverseProxy.copyResponse return an error,
// which panics with http.ErrAbortHandler — the wrapped handlerFn must recover
// and log the disconnect.
//
// Requests are issued through an httptest.NewServer wrapping the process so
// the panic actually fires (httputil only panics on copy errors when the
// request carries http.ServerContextKey, which a real server sets).
//
// see: https://github.com/golang/go/issues/23643
func TestProcessCommand_ReverseProxyPanicIsRecovered(t *testing.T) {
	skipIfNoSimpleResponder(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		// Send a Content-Length that promises 100 bytes, deliver only a few,
		// then slam the connection shut. The reverse proxy will see EOF
		// before the body is fully copied and panic with ErrAbortHandler.
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("upstream: hijack not supported")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("upstream: hijack: %v", err)
			return
		}
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 100\r\nContent-Type: text/plain\r\n\r\npartial"))
		_ = conn.Close()
	}))
	t.Cleanup(upstream.Close)

	// Capture proxy log output so we can assert the recover message was
	// emitted by handlerFn.
	logBuf := &syncBuffer{}
	proxyLogger := logmon.NewWriter(logBuf)
	procLogger := logmon.NewWriter(io.Discard)

	cmd, _ := simpleResponderCmd(t, "-silent")
	p, err := New(context.Background(), t.Name(), config.ModelConfig{
		Cmd:                cmd,
		Proxy:              upstream.URL,
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
	}, procLogger, proxyLogger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { p.Stop(testStopTimeout) })

	_ = runAsync(t, p)

	// Wrap p in an httptest server so requests get http.ServerContextKey
	// automatically — that is what makes httputil.ReverseProxy raise the panic.
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	resp, err := http.Get(front.URL + "/disconnect")
	if err == nil {
		resp.Body.Close()
	}

	const want = "recovered from upstream disconnection"
	deadline := time.Now().Add(testReturnTimeout)
	for time.Now().Before(deadline) {
		if strings.Contains(logBuf.String(), want) {
			return
		}
		time.Sleep(testLogPollInterval)
	}
	t.Errorf("expected proxy log to contain %q; got:\n%s", want, logBuf.String())
}

// syncBuffer is a concurrent-safe bytes.Buffer for capturing logmon output.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestProcessCommand_TTL_StopsAfterIdle verifies that a process with a TTL
// automatically stops itself after the idle timeout has elapsed following its
// last request.
func TestProcessCommand_TTL_StopsAfterIdle(t *testing.T) {
	skipIfNoSimpleResponder(t)

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(mock.Close)

	cmd, _ := simpleResponderCmd(t, "-silent")

	cfg := config.ModelConfig{
		Cmd:                cmd,
		Proxy:              mock.URL,
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
		UnloadAfter:        1, // 1-second TTL
	}
	if runtime.GOOS == "windows" {
		cfg.CmdStop = "taskkill /f /t /pid ${PID}"
	}

	p := newProcessCommand(t, cfg)

	runErr := runAsync(t, p)
	defer func() {
		if p.State() == StateReady {
			p.Stop(testStopTimeout)
		}
	}()

	if got := p.State(); got != StateReady {
		t.Fatalf("expected StateReady, got %s", got)
	}

	// Make one request to prime the last-use timestamp.
	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 after request, got %d", rr.Code)
	}

	// Wait for the TTL goroutine to fire and the process to fully stop.
	// Poll for StateStopped directly to avoid racing the StateStopping
	// intermediate state that sits between StateReady and StateStopped.
	deadline := time.Now().Add(5 * time.Second)
	for p.State() != StateStopped && time.Now().Before(deadline) {
		time.Sleep(testPollInterval)
	}

	if got := p.State(); got != StateStopped {
		t.Fatalf("TTL did not stop process; state is %s (expected %s)", got, StateStopped)
	}

	// Run() should have returned nil (clean stop from TTL).
	select {
	case err := <-runErr:
		if err != nil {
			t.Errorf("Run() after TTL stop: expected nil, got %v", err)
		}
	case <-time.After(testReturnTimeout):
		t.Fatal("Run() did not return after TTL-induced stop")
	}
}

// TestProcessCommand_TTL_VLLMSleepMode keeps the process alive after TTL and
// wakes the backend before the first request that follows the idle period.
func TestProcessCommand_TTL_VLLMSleepMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("vLLM lifecycle test requires a POSIX runtime")
	}

	var sleepRequests atomic.Int32
	var wakeRequests atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/sleep":
			if r.Method != http.MethodPost || r.URL.Query().Get("level") != "1" {
				http.Error(w, "invalid sleep request", http.StatusBadRequest)
				return
			}
			sleepRequests.Add(1)
			w.WriteHeader(http.StatusOK)
		case "/wake_up":
			if r.Method != http.MethodPost {
				http.Error(w, "invalid wake request", http.StatusBadRequest)
				return
			}
			wakeRequests.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			_, _ = w.Write([]byte("awake"))
		}
	}))
	defer backend.Close()

	p := newProcessCommand(t, config.ModelConfig{
		Cmd:                "sh -c 'sleep 30'",
		Proxy:              backend.URL,
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 3,
		UnloadAfter:        1,
		Backend: config.BackendConfig{
			Type: "vllm",
			Lifecycle: config.LifecycleConfig{
				Mode:       "sleep",
				SleepLevel: 1,
			},
		},
	})
	t.Cleanup(func() { _ = p.Stop(testStopTimeout) })
	runAsync(t, p)

	deadline := time.Now().Add(5 * time.Second)
	for sleepRequests.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(testPollInterval)
	}
	if got := sleepRequests.Load(); got != 1 {
		t.Fatalf("sleep requests = %d, want 1", got)
	}
	if !p.sleeping.Load() || p.State() != StateReady {
		t.Fatalf("after TTL: sleeping=%v state=%s, want sleeping=true state=%s", p.sleeping.Load(), p.State(), StateReady)
	}

	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/completion", nil))
	if rr.Code != http.StatusOK || rr.Body.String() != "awake" {
		t.Fatalf("wake request: status=%d body=%q", rr.Code, rr.Body.String())
	}
	if got := wakeRequests.Load(); got != 1 || p.sleeping.Load() {
		t.Fatalf("after wake: wake requests=%d sleeping=%v", got, p.sleeping.Load())
	}
}

func TestProcessCommand_ManualVLLMSleepAndWake(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("vLLM lifecycle test requires a POSIX runtime")
	}

	var sleepRequests atomic.Int32
	var wakeRequests atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/sleep":
			if r.Method != http.MethodPost || r.URL.Query().Get("level") != "2" {
				http.Error(w, "invalid sleep request", http.StatusBadRequest)
				return
			}
			sleepRequests.Add(1)
			w.WriteHeader(http.StatusOK)
		case "/wake_up":
			if r.Method != http.MethodPost {
				http.Error(w, "invalid wake request", http.StatusBadRequest)
				return
			}
			wakeRequests.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			_, _ = w.Write([]byte("awake"))
		}
	}))
	defer backend.Close()

	p := newProcessCommand(t, config.ModelConfig{
		Cmd:                "sleep 30",
		Proxy:              backend.URL,
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 3,
		Backend: config.BackendConfig{
			Type: "vllm",
		},
	})
	t.Cleanup(func() { _ = p.Stop(testStopTimeout) })
	runAsync(t, p)

	ctx, cancel := context.WithTimeout(context.Background(), testStartTimeout)
	defer cancel()
	if err := p.Sleep(ctx, 2); err != nil {
		t.Fatalf("manual Sleep: %v", err)
	}
	if got := sleepRequests.Load(); got != 1 {
		t.Fatalf("sleep requests=%d, want 1", got)
	}
	if !p.Sleeping() || p.State() != StateReady {
		t.Fatalf("after manual sleep: sleeping=%v state=%s, want sleeping=true state=%s", p.Sleeping(), p.State(), StateReady)
	}

	if err := p.EnsureReady(ctx, testStartTimeout); err != nil {
		t.Fatalf("EnsureReady should wake sleeping process: %v", err)
	}
	if got := wakeRequests.Load(); got != 1 {
		t.Fatalf("wake requests=%d, want 1", got)
	}
	if p.Sleeping() {
		t.Fatal("process remained sleeping after EnsureReady")
	}

	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/completion", nil))
	if rr.Code != http.StatusOK || rr.Body.String() != "awake" {
		t.Fatalf("request after manual wake: status=%d body=%q", rr.Code, rr.Body.String())
	}
	if got := wakeRequests.Load(); got != 1 {
		t.Fatalf("ordinary request issued an unexpected second wake: %d", got)
	}
}

// TestProcessCommand_TTL_ResetsOnRequest verifies that inflight requests
// prevent the TTL goroutine from stopping the process, and that the TTL timer
// resets after each request completes.
func TestProcessCommand_TTL_ResetsOnRequest(t *testing.T) {
	skipIfNoSimpleResponder(t)

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(mock.Close)

	cmd, _ := simpleResponderCmd(t, "-silent")
	p := newProcessCommand(t, config.ModelConfig{
		Cmd:                cmd,
		Proxy:              mock.URL,
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
		UnloadAfter:        1, // 1-second TTL
	})

	runErr := runAsync(t, p)
	defer func() {
		if p.State() == StateReady {
			p.Stop(testStopTimeout)
		}
	}()

	// Keep sending requests for 1.5s — past the 1s TTL — and verify
	// the process never stops while traffic is flowing.
	stopAt := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(stopAt) {
		req := httptest.NewRequest("GET", "/", nil)
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rr.Code)
		}
		if p.State() != StateReady {
			t.Fatalf("process was stopped during active traffic (state=%s)", p.State())
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := p.State(); got != StateReady {
		t.Fatalf("expected StateReady while traffic was active, got %s", got)
	}

	// Now stop manually to clean up.
	if err := p.Stop(testStopTimeout); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
	select {
	case <-runErr:
	case <-time.After(testReturnTimeout):
		t.Fatal("Run() did not return after Stop")
	}
}

func TestProcessCommand_TTL_IgnoresWebsocket(t *testing.T) {
	skipIfNoSimpleResponder(t)

	websocketStarted := make(chan struct{})
	releaseWebsocket := make(chan struct{})
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		close(websocketStarted)
		<-releaseWebsocket
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(mock.Close)

	cmd, _ := simpleResponderCmd(t, "-silent")
	p := newProcessCommand(t, config.ModelConfig{
		Cmd:                cmd,
		Proxy:              mock.URL,
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
		UnloadAfter:        1,
		UnloadTimeout:      1,
		Compat:             config.CompatConfig{IgnoreWebsockets: true},
	})
	runErr := runAsync(t, p)

	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		r := httptest.NewRequest(http.MethodGet, "/socket", nil)
		r.Header.Set("Connection", "Upgrade")
		r.Header.Set("Upgrade", "websocket")
		p.ServeHTTP(httptest.NewRecorder(), r)
	}()

	select {
	case <-websocketStarted:
	case <-time.After(testReturnTimeout):
		t.Fatal("websocket request did not reach upstream")
	}
	waitForState(t, p, StateStopped)
	select {
	case <-requestDone:
		t.Fatal("websocket request completed before it was released")
	default:
	}

	close(releaseWebsocket)
	select {
	case <-requestDone:
	case <-time.After(testReturnTimeout):
		t.Fatal("websocket request did not finish after release")
	}
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() after TTL stop: %v", err)
		}
	case <-time.After(testReturnTimeout):
		t.Fatal("Run() did not return after TTL stop")
	}
}

// TestProcessCommand_TTL_ZeroDisables verifies that UnloadAfter=0 does not
// spawn a TTL goroutine — the process stays ready until explicitly stopped.
func TestProcessCommand_TTL_ZeroDisables(t *testing.T) {
	skipIfNoSimpleResponder(t)

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(mock.Close)

	cmd, _ := simpleResponderCmd(t, "-silent")
	p := newProcessCommand(t, config.ModelConfig{
		Cmd:                cmd,
		Proxy:              mock.URL,
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
		UnloadAfter:        0, // disabled
	})

	runErr := runAsync(t, p)
	defer func() {
		if p.State() == StateReady {
			p.Stop(testStopTimeout)
		}
	}()

	if got := p.State(); got != StateReady {
		t.Fatalf("expected StateReady, got %s", got)
	}

	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 after request, got %d", rr.Code)
	}

	// No TTL goroutine is spawned when UnloadAfter=0, so a brief sleep is
	// enough to confirm the process remains ready.
	time.Sleep(100 * time.Millisecond)

	if got := p.State(); got != StateReady {
		t.Fatalf("process was stopped unexpectedly (state=%s) with TTL=0", got)
	}

	// Cleanly stop.
	if err := p.Stop(testStopTimeout); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
	select {
	case <-runErr:
	case <-time.After(testReturnTimeout):
		t.Fatal("Run() did not return after Stop")
	}
}

// TestProcessCommand_ConcurrentRunStop launches many concurrent run/stop racing
// pairs to exercise the race detector and verify no deadlocks occur.
func TestProcessCommand_ConcurrentRunStop(t *testing.T) {
	skipIfNoSimpleResponder(t)

	for range 10 {
		cmd, port := simpleResponderCmd(t, "-silent")
		cfg := config.ModelConfig{
			Cmd:                cmd,
			Proxy:              fmt.Sprintf("http://127.0.0.1:%d", port),
			CheckEndpoint:      "/health",
			HealthCheckTimeout: 10,
		}

		if runtime.GOOS == "windows" {
			cfg.CmdStop = "taskkill /f /t /pid ${PID}"
		}

		p := newProcessCommand(t, cfg)

		runDone := make(chan struct{})
		go func() {
			defer close(runDone)
			p.Run(testStartTimeout) //nolint: errcheck — one goroutine wins the race
		}()
		go func() {
			p.Stop(testStopTimeout) //nolint: errcheck
		}()

		// Backstop: the racing Stop may have arrived before Run got on the
		// channel (making it a no-op), so keep stopping until Run unblocks.
		deadline := time.After(testStartTimeout)
		for done := false; !done; {
			select {
			case <-runDone:
				done = true
			case <-deadline:
				t.Fatal("Run did not return")
			case <-time.After(testPollInterval):
				p.Stop(testStopTimeout) //nolint: errcheck
			}
		}
	}
}

// TestProcessCommand_PreStartHookBlocksStart covers the dependency gate: a
// failing hook must fail EnsureReady with its own error, leave the state
// Stopped, and run once per start attempt without touching the upstream.
func TestProcessCommand_PreStartHookBlocksStart(t *testing.T) {
	// The command is deliberately broken: reaching it would fail in a
	// different way, so the hook error identity proves the gate held.
	p := newProcessCommand(t, config.ModelConfig{
		Cmd:   "definitely-missing-binary-xyz",
		Proxy: "http://127.0.0.1:1",
	})
	hookErr := errors.New("dependency not ready")
	var calls atomic.Int32
	p.SetPreStartHook(func(context.Context) error {
		calls.Add(1)
		return hookErr
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		err := p.EnsureReady(ctx, testStartTimeout)
		if !errors.Is(err, hookErr) {
			t.Fatalf("EnsureReady call %d = %v, want the hook error", i+1, err)
		}
		if got := p.State(); got != StateStopped {
			t.Fatalf("state after failed pre-start = %s, want stopped", got)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("hook calls = %d, want 2 (one per start attempt)", got)
	}
}

// TestProcessCommand_PreStartHookAllowsStart verifies a passing hook lets the
// normal start path proceed untouched.
func TestProcessCommand_PreStartHookAllowsStart(t *testing.T) {
	skipIfNoSimpleResponder(t)

	cmd, port := simpleResponderCmd(t, "-silent")
	p := newProcessCommand(t, config.ModelConfig{
		Cmd:                cmd,
		Proxy:              fmt.Sprintf("http://127.0.0.1:%d", port),
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
	})
	t.Cleanup(func() { p.Stop(testStopTimeout) }) //nolint: errcheck

	var calls atomic.Int32
	p.SetPreStartHook(func(context.Context) error {
		calls.Add(1)
		return nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.EnsureReady(ctx, testStartTimeout); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	if got := p.State(); got != StateReady {
		t.Fatalf("state = %s, want ready", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("hook calls = %d, want 1", got)
	}
}

// TestProcessCommand_StopDuringPreStartHook verifies a Stop request is
// honoured while the dependency gate is still running instead of queueing
// behind it on the run loop.
func TestProcessCommand_StopDuringPreStartHook(t *testing.T) {
	p := newProcessCommand(t, config.ModelConfig{
		Cmd:   "definitely-missing-binary-xyz",
		Proxy: "http://127.0.0.1:1",
	})

	started := make(chan struct{})
	release := make(chan struct{})
	p.SetPreStartHook(func(ctx context.Context) error {
		close(started)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})

	startCtx, startCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer startCancel()
	startErr := make(chan error, 1)
	go func() { startErr <- p.EnsureReady(startCtx, testStartTimeout) }()

	<-started
	stopErr := make(chan error, 1)
	go func() { stopErr <- p.Stop(testStopTimeout) }()
	select {
	case err := <-stopErr:
		if err != nil {
			t.Fatalf("Stop during pre-start hook = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return while the pre-start hook was running")
	}

	close(release)
	if err := <-startErr; !errors.Is(err, ErrStartAborted) {
		t.Fatalf("EnsureReady after stop = %v, want ErrStartAborted", err)
	}
	if got := p.State(); got != StateStopped {
		t.Fatalf("state = %s, want stopped", got)
	}
}

// TestProcessCommand_EnsureReadyAfterStopSpawnsSimulatedLLM uses the actual
// simple-responder subprocess as a stand-in LLM. It verifies the complete
// restart path: the first process is stopped and its listener disappears,
// EnsureReady then launches a second process on the same port, and the log
// contains two distinct start commands.
func TestProcessCommand_EnsureReadyAfterStopSpawnsSimulatedLLM(t *testing.T) {
	skipIfNoSimpleResponder(t)

	cmd, port := simpleResponderCmd(t, "-silent", "-respond simulated-llm")
	logBuf := &syncBuffer{}
	logger := logmon.NewWriter(logBuf)
	logger.SetLogLevel(logmon.LevelDebug)
	p, err := New(context.Background(), t.Name(), config.ModelConfig{
		Cmd:                cmd,
		Proxy:              fmt.Sprintf("http://127.0.0.1:%d", port),
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
	}, logger, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(testStopTimeout) })

	var hookCalls atomic.Int32
	p.SetPreStartHook(func(context.Context) error {
		hookCalls.Add(1)
		return nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for cycle := 1; cycle <= 2; cycle++ {
		if err := p.EnsureReady(ctx, testStartTimeout); err != nil {
			t.Fatalf("cycle %d EnsureReady: %v", cycle, err)
		}
		if got := p.State(); got != StateReady {
			t.Fatalf("cycle %d state = %s, want ready", cycle, got)
		}

		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK || rr.Body.String() != "simulated-llm" {
			t.Fatalf("cycle %d response = %d %q, want 200 %q", cycle, rr.Code, rr.Body.String(), "simulated-llm")
		}

		if err := p.Stop(testStopTimeout); err != nil {
			t.Fatalf("cycle %d Stop: %v", cycle, err)
		}
		if got := p.State(); got != StateStopped {
			t.Fatalf("cycle %d state after stop = %s, want stopped", cycle, got)
		}
		if resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port)); err == nil {
			resp.Body.Close()
			t.Fatalf("cycle %d simulated LLM listener still answered after Stop", cycle)
		}
	}

	if got := hookCalls.Load(); got != 2 {
		t.Fatalf("pre-start hook calls = %d, want one per restart", got)
	}
	if got := strings.Count(logBuf.String(), "Executing start command:"); got != 2 {
		t.Fatalf("start command log count = %d, want 2; log=%q", got, logBuf.String())
	}
}

// TestProcessCommand_PreStartHookUsesStartContext verifies that the startup
// timeout also bounds dependency preparation. A hook runs before StateStarting
// and before doStart, so using the long-lived parent context here can leave a
// restart looking stopped forever without ever spawning the upstream.
func TestProcessCommand_PreStartHookUsesStartContext(t *testing.T) {
	parentCtx, cancelParent := context.WithCancel(context.Background())
	t.Cleanup(cancelParent)
	logger := logmon.NewWriter(io.Discard)
	p, err := New(parentCtx, t.Name(), config.ModelConfig{
		Cmd:   "definitely-missing-binary-xyz",
		Proxy: "http://127.0.0.1:1",
	}, logger, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	hookStarted := make(chan struct{})
	hookDone := make(chan struct{})
	p.SetPreStartHook(func(ctx context.Context) error {
		close(hookStarted)
		<-ctx.Done()
		close(hookDone)
		return ctx.Err()
	})

	const startTimeout = 150 * time.Millisecond
	result := make(chan error, 1)
	go func() {
		result <- p.EnsureReady(context.Background(), startTimeout)
	}()

	select {
	case <-hookStarted:
	case <-time.After(testReturnTimeout):
		t.Fatal("pre-start hook did not run")
	}

	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("EnsureReady error = %v, want context deadline exceeded", err)
		}
	case <-time.After(testReturnTimeout):
		t.Fatal("EnsureReady remained blocked in pre-start hook")
	}

	select {
	case <-hookDone:
	case <-time.After(testReturnTimeout):
		t.Fatal("pre-start hook did not receive the startup timeout")
	}
	if got := p.State(); got != StateStopped {
		t.Fatalf("state after bounded pre-start failure = %s, want stopped", got)
	}
}
