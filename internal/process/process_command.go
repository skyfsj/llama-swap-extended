package process

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

var ErrStartAborted = fmt.Errorf("aborted")

// CrashRecorder receives a snapshot when an upstream inference process exits
// without an explicit Stop or shutdown. The log slice is an owned copy and
// the callback must return quickly; callers that persist to disk should queue
// that work themselves.
type CrashRecorder func(modelID string, processLog []byte, exitErr error)

// healthCheckKey marks requests issued by the health check loop, which polls
// the upstream through the same reverse proxy. Their failures are the expected
// shape of a model still booting, so they must not be logged as proxy errors.
type healthCheckKey struct{}

// newProxyErrorHandler builds the ErrorHandler for a model's reverse proxy.
//
// httputil.ReverseProxy's default handler answers every failure with 502, so a
// client hanging up mid-generation is logged as a Bad Gateway and sends
// operators looking at an inference server that was healthy the whole time.
// Cancellation is classified here, where the error is actually known: the
// request is recorded with the client-closed sentinel rather than blamed on the
// upstream, and at debug level because an impatient caller is normal traffic.
// Real upstream failures keep the 502. See #1029.
func newProxyErrorHandler(id string, proxyLogger *logmon.Monitor) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		// Pick the log level first: a cancelled request is never an upstream
		// fault, whether or not the sentinel ends up applying below.
		switch {
		case errors.Is(err, context.Canceled) || r.Context().Err() != nil:
			proxyLogger.Debugf("<%s> request cancelled: %v", id, err)
		case r.Context().Value(healthCheckKey{}) != nil:
			proxyLogger.Debugf("<%s> health check not ready: %v", id, err)
		default:
			proxyLogger.Warnf("<%s> proxy error: %v", id, err)
		}

		// Only a client that actually hung up gets the recorded-only sentinel.
		// A request cancelled server-side (an operator cancelling it from the
		// UI, or shutdown) still has a client waiting, and must be answered —
		// otherwise net/http finalizes it as an empty 200, telling the caller
		// the request succeeded.
		if swaputil.MarkClientClosed(w, r) || swaputil.ResponseStarted(w) {
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	}
}

// cmdWaitDelay is the upper bound the runtime will wait for child I/O to
// drain after the process exits before force-closing the stdout/stderr
// pipes. Required so that cmd.Wait() returns even when a forked grandchild
// inherits and holds the pipes open (e.g. a shell wrapper that backgrounds
// the real binary). killProcess sends the stop signal directly (not via the
// cmd context), so this delay is measured from process exit rather than from
// the stop request, and stays independent of the caller's graceful timeout.
const cmdWaitDelay = 10 * time.Second

// parentCancelGraceTimeout is the graceful timeout used when the process is
// torn down because parentCtx was cancelled (final router teardown or app
// shutdown). In the normal flow the process has already been stopped via
// Stop() by this point, so killProcess is a no-op kill; the short grace just
// bounds the rare case where a process is still alive when its context is cut.
const parentCancelGraceTimeout = time.Second

// stopCommandTimeout bounds the external CmdStop subprocess. It runs on
// detached goroutines (killProcess) and on os/exec's context watcher
// (cmd.Cancel), so a stop script that waits on a lock forever would otherwise
// leak those goroutines permanently.
const stopCommandTimeout = 30 * time.Second

// startReq asks the run loop to bring the process up. Run and EnsureReady share
// this one request type — and therefore one code path — so there is only ever a
// single way to start a process. block selects the caller's semantics: Run parks
// its response until the process terminates, EnsureReady is answered as soon as
// the process is ready or the start fails.
type startReq struct {
	ctx     context.Context
	timeout time.Duration
	respond chan error
	block   bool
}

type stopReq struct {
	timeout time.Duration
	respond chan error
}

type sleepReq struct {
	ctx     context.Context
	level   int
	respond chan error
}

type wakeReq struct {
	ctx     context.Context
	respond chan error
}

type waitReadyReq struct {
	respond chan error
}

type startResult struct {
	cmd       *exec.Cmd
	cmdDone   chan struct{}
	cancel    context.CancelFunc
	handlerFn http.HandlerFunc
	err       error
}

type ProcessCommand struct {
	id        string
	config    config.ModelConfig
	parentCtx context.Context

	processLogger *logmon.Monitor
	proxyLogger   *logmon.Monitor

	// waitDelay is assigned to cmd.WaitDelay when starting the upstream
	// process. Defaults to cmdWaitDelay; tests override it to keep the
	// pipe-close backstop from dominating their runtime.
	waitDelay time.Duration

	startCh     chan startReq
	stopCh      chan stopReq
	sleepCh     chan sleepReq
	wakeCh      chan wakeReq
	waitReadyCh chan waitReadyReq

	// current ProcessState. Written only by run(); read by State() via atomic load.
	state atomic.Value

	// stores the active reverse-proxy handler when the process is running.
	// Written only by run(); read by ServeHTTP via atomic load.
	handler atomic.Pointer[http.HandlerFunc]

	lastUse  atomic.Int64 // unix nano timestamp of last ServeHTTP completion
	inflight atomic.Int64 // current in-flight ServeHTTP calls

	// preStartHook is a dependency gate executed inside the run loop right
	// before a start request enters doStart. A non-nil error fails the start
	// without spawning the upstream process. Installed atomically because the
	// decorator may run on a different goroutine than the run loop.
	preStartHook atomic.Pointer[func(context.Context) error]

	// crashRecorder is installed by the server's process decorator. It is
	// atomic because the router may decorate existing processes while their run
	// goroutine is handling lifecycle events.
	crashRecorder atomic.Pointer[CrashRecorder]
	exitMu        sync.Mutex
	lastExitErr   error

	// Sleep Mode keeps the process and HTTP handler alive while moving vLLM
	// weights out of device memory. Serialize control calls so concurrent
	// requests cannot issue overlapping wake-up operations.
	sleepMu  sync.Mutex
	sleeping atomic.Bool
}

var _ Process = (*ProcessCommand)(nil)
var _ SleepController = (*ProcessCommand)(nil)

func New(
	parentCtx context.Context,
	id string,
	conf config.ModelConfig,
	processLogger *logmon.Monitor,
	proxyLogger *logmon.Monitor,
) (*ProcessCommand, error) {
	p := &ProcessCommand{
		id:            id,
		config:        conf,
		parentCtx:     parentCtx,
		processLogger: processLogger,
		proxyLogger:   proxyLogger,

		startCh:     make(chan startReq),
		stopCh:      make(chan stopReq),
		sleepCh:     make(chan sleepReq),
		wakeCh:      make(chan wakeReq),
		waitReadyCh: make(chan waitReadyReq),
		waitDelay:   cmdWaitDelay,
	}
	p.state.Store(StateStopped)

	go p.run()
	return p, nil
}

func (p *ProcessCommand) Logger() *logmon.Monitor { return p.processLogger }

// SetPreStartHook installs a dependency gate that runs before every start
// attempt. A non-nil error from the hook fails the start (Run and EnsureReady
// both report it) and the state stays Stopped. The hook runs on its own
// goroutine — it can legitimately take minutes (dependency installs) — while
// the run loop keeps servicing Stop and shutdown: a Stop during the gate
// cancels the start context (the hook must honor it) and aborts the start.
// The start request's timeout bounds the complete dependency-preparation and
// process-start path.
func (p *ProcessCommand) SetPreStartHook(hook func(context.Context) error) {
	if hook == nil {
		p.preStartHook.Store(nil)
		return
	}
	p.preStartHook.Store(&hook)
}

// SetCrashRecorder installs or clears the callback used for unexpected
// upstream exits. The callback itself is not invoked while holding any
// ProcessCommand lock.
func (p *ProcessCommand) SetCrashRecorder(rec CrashRecorder) {
	if rec == nil {
		p.crashRecorder.Store(nil)
		return
	}
	p.crashRecorder.Store(&rec)
}

func (p *ProcessCommand) setLastExitErr(err error) {
	p.exitMu.Lock()
	p.lastExitErr = err
	p.exitMu.Unlock()
}

func (p *ProcessCommand) getLastExitErr() error {
	p.exitMu.Lock()
	defer p.exitMu.Unlock()
	return p.lastExitErr
}

// reportExit writes a lifecycle diagnostic to the model's own log.
//
// The model log panel reads the process monitor, not the proxy logger, so a
// diagnostic written only to the proxy logger leaves the panel ending mid-line
// with no explanation of why the process stopped — the operator sees output
// "just stop". Writing here reaches the panel directly and, because the process
// monitor forwards through a prefix writer, the shared log as well.
func (p *ProcessCommand) reportExit(format string, args ...any) {
	if p.processLogger != nil {
		p.processLogger.Errorf(format, args...)
		return
	}
	if p.proxyLogger != nil {
		p.proxyLogger.Errorf("<%s> %s", p.id, fmt.Sprintf(format, args...))
	}
}

func (p *ProcessCommand) recordCrash(exitErr error) {
	recorder := p.crashRecorder.Load()
	if recorder == nil {
		return
	}
	var processLog []byte
	if p.processLogger != nil {
		processLog = p.processLogger.GetHistory()
	}
	(*recorder)(p.id, processLog, exitErr)
}

// run is the single-writer goroutine that owns all mutable lifecycle state
// (current ProcessState, the running *exec.Cmd, the active reverse-proxy
// handler, and the list of WaitReady subscribers). Every public method
// (Run / Stop / State / WaitReady) is a thin client that sends a request on
// one of the channels below and waits for a response — this funnels concurrent
// callers through a single serialization point so the state machine never
// observes a race.
func (p *ProcessCommand) run() {
	// Mutable state — only read/written from this goroutine. ServeHTTP reads
	// p.handler concurrently, which is why handler is an atomic.Pointer.
	// p.state mirrors `state` so State() can observe transitions; setState
	// writes both.
	state := StateStopped
	setState := func(s ProcessState) {
		old := state
		state = s
		p.state.Store(s)
		if old != s {
			event.Emit(swaputil.ProcessStateChangeEvent{
				ProcessName: p.id,
				OldState:    string(old),
				NewState:    string(s),
			})
		}
	}
	setSleeping := func(sleeping bool) {
		old := p.sleeping.Swap(sleeping)
		if old == sleeping {
			return
		}
		oldState, newState := string(StateReady), string(StateSleeping)
		if !sleeping {
			oldState, newState = newState, oldState
		}
		event.Emit(swaputil.ProcessStateChangeEvent{
			ProcessName: p.id,
			OldState:    oldState,
			NewState:    newState,
		})
	}
	var (
		cmd          *exec.Cmd
		cmdDone      <-chan struct{}
		cmdCancel    context.CancelFunc
		readyWaiters []waitReadyReq
		// runResp parks the in-flight Run caller's response channel. The
		// interface contract is that Run blocks until the process is
		// terminated, so we hold this until Stop, parentCtx, or an
		// upstream exit unblocks it via respondRun.
		runResp chan<- error
	)

	// notifyWaiters wakes every blocked WaitReady caller with the given result.
	// It must be called on every transition into a state that resolves the
	// "is it ready yet?" question — ready, failed, aborted, shutdown, stopped,
	// or exited. Missing one strands the subscriber forever (issue #946).
	notifyWaiters := func(err error) {
		for _, w := range readyWaiters {
			select {
			case w.respond <- err:
			default:
			}
		}
		readyWaiters = nil
	}

	// respondRun delivers the final Run result, if a Run caller is parked.
	respondRun := func(err error) {
		if runResp != nil {
			runResp <- err
			runResp = nil
		}
	}

	for {
		select {
		// Shutdown: parent context cancelled. Tear down any running process,
		// wake any pending WaitReady callers with an error, then exit the
		// goroutine permanently. Subsequent public-method calls will fail
		// because parentCtx.Done() unblocks their send-side selects.
		case <-p.parentCtx.Done():
			// Mark shutdown before killProcess so concurrent State() readers
			// stop treating this process as ready while the (possibly slow)
			// teardown is in progress.
			p.sleeping.Store(false)
			setState(StateShutdown)
			if cmd != nil {
				p.handler.Store(nil)
				p.killProcess(cmd, cmdCancel, cmdDone, parentCancelGraceTimeout)
				cmd = nil
				cmdDone = nil
				cmdCancel = nil
			}
			notifyWaiters(fmt.Errorf("[%s] shutdown", p.id))
			respondRun(fmt.Errorf("[%s] shutdown", p.id))
			return

		// Upstream exited on its own (not via Stop). Drop handler state,
		// transition to Stopped, and unblock the parked Run caller.
		// cmdDone is nil while no process is running, so this case is
		// dormant outside of StateReady.
		case <-cmdDone:
			waitErr := p.getLastExitErr()
			if cmdCancel != nil {
				cmdCancel()
			}
			cmd = nil
			cmdDone = nil
			cmdCancel = nil
			p.sleeping.Store(false)
			p.handler.Store(nil)
			setState(StateStopped)
			p.proxyLogger.Warnf("<%s> upstream process exited unexpectedly", p.id)
			p.recordCrash(waitErr)
			// Safety net: readyWaiters is normally empty here because
			// WaitReady is answered immediately while StateReady. Notifying
			// anyway keeps the invariant that no transition into a settled
			// state can leave a subscriber parked forever.
			notifyWaiters(fmt.Errorf("[%s] upstream exited unexpectedly", p.id))
			respondRun(fmt.Errorf("[%s] upstream exited unexpectedly", p.id))

		// WaitReady: if we're already in a terminal-for-this-question state,
		// respond immediately; otherwise queue the caller and let a future
		// state transition wake them via notifyWaiters.
		case req := <-p.waitReadyCh:
			switch state {
			case StateReady:
				req.respond <- nil
			case StateShutdown:
				req.respond <- fmt.Errorf("[%s] shutdown", p.id)
			default:
				readyWaiters = append(readyWaiters, req)
			}

		// Sleep and wake are handled by the same run loop that owns start and
		// stop. The upstream listener stays alive in StateReady while its vLLM
		// weights are released, so the next request can wake it without racing a
		// second process generation into the same proxy.
		case req := <-p.sleepCh:
			if !p.isVLLMBackend() {
				req.respond <- ErrSleepUnsupported
				continue
			}
			if state != StateReady || p.handler.Load() == nil {
				req.respond <- ErrSleepUnavailable
				continue
			}
			if req.level == 0 {
				req.level = 1
			}
			if req.level < 1 || req.level > 2 {
				req.respond <- fmt.Errorf("sleep level must be 1 or 2")
				continue
			}
			if p.sleeping.Load() {
				req.respond <- nil
				continue
			}
			err := p.sleepVLLM(req.ctx, req.level, time.Duration(p.config.UnloadTimeout)*time.Second)
			if err == nil {
				setSleeping(true)
			}
			req.respond <- err

		case req := <-p.wakeCh:
			if !p.isVLLMBackend() {
				req.respond <- ErrSleepUnsupported
				continue
			}
			if state != StateReady || p.handler.Load() == nil {
				req.respond <- ErrSleepUnavailable
				continue
			}
			if !p.sleeping.Load() {
				req.respond <- nil
				continue
			}
			err := p.wakeVLLM(req.ctx)
			if err == nil {
				setSleeping(false)
			}
			req.respond <- err

		// Start the upstream process (Run or EnsureReady — see startReq).
		// doStart can take a long time (health-check polling), so it runs in
		// a separate goroutine and we wait on resultCh. While waiting we also
		// listen for an incoming Stop — that's how callers cancel an in-flight
		// start.
		case req := <-p.startCh:
			// EnsureReady answers straight from the current state when the
			// "is it ready?" question is already settled. There is deliberately
			// no StateStopping case: a stop keeps this loop parked inside
			// killProcess and out of the select, so a request can only ever be
			// received once the stop has finished and state is Stopped again.
			// Waiting on the channel IS the synchronisation — the caller never
			// has to guess the state from outside.
			if !req.block {
				switch state {
				case StateReady:
					if p.sleeping.Load() {
						wakeCtx := req.ctx
						cancelWake := func() {}
						if req.timeout > 0 {
							wakeCtx, cancelWake = context.WithTimeout(req.ctx, req.timeout)
						}
						err := p.wakeVLLM(wakeCtx)
						cancelWake()
						if err != nil {
							req.respond <- err
							continue
						}
						setSleeping(false)
					}
					req.respond <- nil
					continue
				case StateShutdown:
					req.respond <- fmt.Errorf("[%s] shutdown", p.id)
					continue
				}
			}
			// Only valid from StateStopped. For Run this is also the "second
			// Run while already running" rejection.
			if state != StateStopped {
				req.respond <- fmt.Errorf("[%s] could not be started in %s state", p.id, state)
				continue
			}
			// Bound dependency preparation and the actual start with the same
			// caller-provided context. The pre-start hook runs before StateStarting
			// and doStart, so passing only parentCtx here can leave a restart stuck
			// in Stopped without ever spawning the replacement process.
			var startCtx context.Context
			var cancelStart context.CancelFunc
			if req.timeout > 0 {
				startCtx, cancelStart = context.WithTimeout(req.ctx, req.timeout)
			} else {
				startCtx, cancelStart = context.WithCancel(req.ctx)
			}
			// The pre-start hook is the dependency gate: a model whose
			// runtime dependencies are not ready must fail here, before a
			// single upstream process is spawned. State stays Stopped.
			// The gate can legitimately run for minutes (dependency
			// installs), so it runs in its own goroutine and this loop keeps
			// servicing Stop and shutdown — the same contract as the
			// doStart wait below.
			hookDone := make(chan error, 1)
			if hookPtr := p.preStartHook.Load(); hookPtr != nil {
				hook := *hookPtr
				go func() { hookDone <- hook(startCtx) }()
			} else {
				hookDone <- nil
			}
			select {
			case hookErr := <-hookDone:
				if hookErr != nil {
					cancelStart()
					p.proxyLogger.Warnf("<%s> pre-start check failed: %v", p.id, hookErr)
					notifyWaiters(hookErr)
					req.respond <- hookErr
					continue
				}

			// Stop arrived while the dependency gate was still running. Cancel
			// the start context (the hook honours it), drain the gate so it
			// stays single-flight, and answer both callers.
			case stop := <-p.stopCh:
				cancelStart()
				hookErr := <-hookDone
				setState(StateStopped)
				if hookErr == nil {
					// The gate completed successfully, and a hook only acquires
					// what it needs after succeeding (the LMCache reference is
					// taken once the gate is fully satisfied). setState is a
					// no-op here — the process never left Stopped — so no
					// transition event is emitted, and subscribers that release
					// on leaving the running set would never run, stranding the
					// reference until the next successful start/stop cycle.
					event.Emit(swaputil.ProcessStateChangeEvent{
						ProcessName: p.id,
						OldState:    string(StateStopped),
						NewState:    string(StateStopped),
					})
				}
				notifyWaiters(ErrStartAborted)
				req.respond <- ErrStartAborted
				stop.respond <- nil
				continue

			// Parent context cancelled (e.g. config reload) while the gate was
			// still running. Mirrors the shutdown contract of the doStart wait
			// below: the start requester unblocks through its own parentCtx
			// send-side select.
			case <-p.parentCtx.Done():
				cancelStart()
				setState(StateShutdown)
				<-hookDone
				notifyWaiters(fmt.Errorf("[%s] shutdown", p.id))
				respondRun(fmt.Errorf("[%s] shutdown", p.id))
				return
			}
			setState(StateStarting)

			resultCh := make(chan startResult, 1)
			go func() {
				resultCh <- p.doStart(startCtx, req.timeout)
			}()

			// pendingStop holds a Stop request that arrived mid-start, so we
			// can respond to it AFTER we've finished tearing the start down.
			var pendingStop *stopReq
			select {
			// doStart finished on its own — either successfully (latch
			// cmd/handler and move to Ready) or with an error (back to
			// Stopped). Either way wake WaitReady subscribers and reply
			// to the Run caller.
			case res := <-resultCh:
				if res.err == nil {
					cmd = res.cmd
					cmdDone = res.cmdDone
					cmdCancel = res.cancel
					p.sleeping.Store(false)
					p.lastUse.Store(time.Now().UnixNano())
					fn := res.handlerFn
					p.handler.Store(&fn)
					setState(StateReady)
					notifyWaiters(nil)
					if req.block {
						// Park the Run response — Run blocks until the process
						// terminates, so we only fire this when Stop, parentCtx,
						// or the upstream exit takes the process down.
						runResp = req.respond
					} else {
						// EnsureReady's question is answered: it's ready.
						req.respond <- nil
					}

					// Start TTL goroutine if configured — self-terminates
					// when state leaves StateReady.
					if p.config.UnloadAfter > 0 {
						ttlDuration := time.Duration(p.config.UnloadAfter) * time.Second
						go func() {
							ticker := time.NewTicker(time.Second)
							defer ticker.Stop()
							for range ticker.C {
								if p.State() != StateReady {
									return
								}
								if p.sleeping.Load() {
									continue
								}
								if p.inflight.Load() != 0 {
									continue
								}
								if time.Since(time.Unix(0, p.lastUse.Load())) > ttlDuration {
									if p.usesVLLMSleepMode() {
										level := p.config.Backend.Lifecycle.SleepLevel
										if level == 0 {
											level = 1
										}
										p.proxyLogger.Infof("<%s> Entering vLLM Sleep Mode level %d, TTL of %ds reached", p.id, level, p.config.UnloadAfter)
										if err := p.Sleep(context.Background(), level); err != nil {
											p.proxyLogger.Warnf("<%s> vLLM Sleep Mode failed: %v", p.id, err)
										}
										continue
									}
									p.proxyLogger.Infof("<%s> Unloading model, TTL of %ds reached", p.id, p.config.UnloadAfter)
									if err := p.Stop(time.Duration(p.config.UnloadTimeout) * time.Second); err != nil {
										// The line above promises an unload. Without this
										// the operator only sees that promise and keeps
										// wondering why VRAM is still held.
										p.proxyLogger.Warnf("<%s> TTL unload failed: %v", p.id, err)
									}
									return
								}
							}
						}()
					}
				} else {
					setState(StateStopped)
					notifyWaiters(res.err)
					req.respond <- res.err
				}

			// Stop arrived while doStart was still running. Cancel the
			// start context to abort it, then wait for doStart to return.
			// If doStart had already crossed the finish line before
			// cancellation took effect, it returns a live cmd that we
			// must kill ourselves. The Run caller gets ErrAbort; the Stop
			// caller is parked in pendingStop and answered below.
			case stop := <-p.stopCh:
				cancelStart()
				res := <-resultCh
				if res.cmd != nil {
					p.killProcess(res.cmd, res.cancel, res.cmdDone, stop.timeout)
				}
				setState(StateStopped)
				notifyWaiters(ErrStartAborted)
				req.respond <- ErrStartAborted
				pendingStop = &stop

			// Parent context cancelled (e.g. config reload) while doStart
			// was still running. Stop() returns early when parentCtx is
			// done and never sends on stopCh, so we must handle shutdown
			// here to avoid leaving doStart running indefinitely.
			case <-p.parentCtx.Done():
				cancelStart()
				// Mark shutdown before tearing the process down: killProcess
				// may block (e.g. taskkill on Windows is slow to spawn), and
				// callers observing State() should see StateShutdown promptly
				// rather than a stale StateStarting.
				setState(StateShutdown)
				res := <-resultCh
				if res.cmd != nil {
					p.killProcess(res.cmd, res.cancel, res.cmdDone, parentCancelGraceTimeout)
				}
				notifyWaiters(fmt.Errorf("[%s] shutdown", p.id))
				respondRun(fmt.Errorf("[%s] shutdown", p.id))
				return
			}
			// cancelStart is idempotent; calling it again here ensures the
			// context is released even on the success path (govet leak check).
			cancelStart()
			if pendingStop != nil {
				pendingStop.respond <- nil
			}

		// Stop: tear down a running process.
		case stop := <-p.stopCh:
			toreDown := cmd != nil
			if cmd != nil {
				// Clear the public sleep projection before publishing Stopping;
				// otherwise the state event can be rendered as sleeping while the
				// process is already being torn down.
				p.sleeping.Store(false)
				setState(StateStopping)
				p.killProcess(cmd, cmdCancel, cmdDone, stop.timeout)
				cmd = nil
				cmdDone = nil
				cmdCancel = nil
				p.handler.Store(nil)
			}
			// Stop is a no-op (and not an error) when already Stopped — this
			// is what makes it idempotent for callers that don't track state.
			setState(StateStopped)
			if toreDown {
				// A process we just killed is not going to become ready, so
				// release anyone parked on that question rather than leaving
				// them stranded (issue #946). Guarded on having actually torn
				// something down: an idempotent no-op Stop must not cancel a
				// subscriber waiting on a start that hasn't happened yet, which
				// is the legitimate `go Run(); WaitReady()` ordering.
				notifyWaiters(fmt.Errorf("[%s] stopped", p.id))
			}
			respondRun(nil)
			stop.respond <- nil
		}
	}
}

func (p *ProcessCommand) doStart(startCtx context.Context, healthCheckTimeout time.Duration) startResult {
	if p.config.Proxy == "" {
		return startResult{err: fmt.Errorf("upstream proxy missing")}
	}

	args, err := p.config.SanitizedCommand()
	if err != nil {
		return startResult{err: fmt.Errorf("unable to get sanitized command: %w", err)}
	}

	proxyURL, err := url.Parse(p.config.Proxy)
	if err != nil {
		return startResult{err: fmt.Errorf("invalid proxy URL %q: %w", p.config.Proxy, err)}
	}

	reverseProxy := httputil.NewSingleHostReverseProxy(proxyURL)
	reverseProxy.Transport = &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   time.Duration(p.config.Timeouts.Connect) * time.Second,
			KeepAlive: time.Duration(p.config.Timeouts.KeepAlive) * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   time.Duration(p.config.Timeouts.TLSHandshake) * time.Second,
		ResponseHeaderTimeout: time.Duration(p.config.Timeouts.ResponseHeader) * time.Second,
		ExpectContinueTimeout: time.Duration(p.config.Timeouts.ExpectContinue) * time.Second,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       time.Duration(p.config.Timeouts.IdleConn) * time.Second,
	}
	reverseProxy.ErrorHandler = newProxyErrorHandler(p.id, p.proxyLogger)
	reverseProxy.ModifyResponse = func(resp *http.Response) error {
		if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			resp.Header.Set("X-Accel-Buffering", "no")
		}
		return nil
	}
	// httputil.ReverseProxy panics with http.ErrAbortHandler when the upstream
	// disconnects after response headers have been sent. Recover here so the
	// streaming termination is treated as a normal client/upstream disconnect.
	// see: https://github.com/golang/go/issues/23643
	handlerFn := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					p.proxyLogger.Infof("<%s> recovered from upstream disconnection during streaming", p.id)
				} else {
					p.proxyLogger.Warnf("<%s> recovered from panic: %v", p.id, rec)
				}
			}
		}()
		reverseProxy.ServeHTTP(w, r)
	})

	// cmdCtx + cmd.Cancel are wired as a safety net: if the context is ever
	// cancelled while the process is alive, cmd.Cancel sends SIGTERM / CmdStop
	// and the runtime escalates to SIGKILL after cmd.WaitDelay. In the normal
	// teardown path killProcess sends the stop signal directly instead, so
	// cmd.WaitDelay only acts as the inherited-pipe backstop measured from
	// process exit (see killProcess).
	cmdCtx, cmdCancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(cmdCtx, args[0], args[1:]...)
	// os/exec drains these pipes with io.Copy from its own goroutine, so the
	// writer bound here runs on the child's stdout drain path. Bind a
	// DrainWriter rather than the logger itself: the child then only ever waits
	// on a memory copy, while line prefixing, the merge into the shared
	// upstream log, and the terminal write happen afterwards, off that path.
	// Bound inline, a slow or failing sink stops the copier, the pipe fills up,
	// and the child dies of SIGPIPE on its next write — a logging fault that is
	// indistinguishable from a model crash.
	childOutput := logmon.NewDrainWriter(p.processLogger)
	cmd.Stderr = childOutput
	cmd.Stdout = childOutput
	cmd.Env = append(cmd.Environ(), p.config.Env...)
	cmd.Cancel = func() error { return p.sendStopSignal(cmd) }
	cmd.WaitDelay = p.waitDelay
	setProcAttributes(cmd)

	p.proxyLogger.Debugf("<%s> Executing start command: %s, env: %s", p.id, strings.Join(args, " "), strings.Join(p.config.Env, ", "))

	cmdDone := make(chan struct{})
	p.setLastExitErr(nil)
	if err := cmd.Start(); err != nil {
		cmdCancel()
		return startResult{err: fmt.Errorf("failed to start command '%s': %w", strings.Join(args, " "), err)}
	}

	go func() {
		waitErr := cmd.Wait()
		// cmd.Wait has returned, so both copier goroutines are done and every
		// byte the child wrote is either delivered or queued. Flush before
		// reporting: the diagnostics below decide whether to start a new line by
		// looking at what the log currently ends with, and a crash archive reads
		// the same buffer.
		childOutput.Close()
		p.setLastExitErr(waitErr)
		switch st := p.State(); {
		case waitErr == nil:
			p.proxyLogger.Debugf("<%s> process exited cleanly", p.id)
		case st == StateStopping || st == StateShutdown:
			// Expected: we force-terminated the process. A forced kill exits
			// the child with a non-zero code (e.g. taskkill /f on Windows
			// yields exit status 1), so this is not an error.
			p.proxyLogger.Debugf("<%s> process stopped by llama-swap: %v", p.id, waitErr)
		case st == StateStarting:
			// A death while a start is still in flight belongs to the start path,
			// which reports it with the context that matters ("exited before
			// becoming ready") and archives the crash. Reporting it here as well
			// printed the same death twice, and it also misfired on the kills
			// llama-swap itself performs against a starting process — a start
			// deadline or an explicit cancel — which are deliberate teardowns,
			// not surprises. The run loop still reports a process that dies after
			// reaching ready, so nothing is lost.
			p.proxyLogger.Debugf("<%s> process exited during start: %v", p.id, waitErr)
		default:
			// An unexpected death must be visible at the default log level. It
			// used to be logged at debug, which the default level discards, so
			// the operator-visible log jumped from progress output straight to a
			// bare "exit status N" with no record of what actually happened.
			// DescribeExitWithMemory names the mechanism (signal vs exit code)
			// and appends any memory evidence, because memory exhaustion is the
			// silent case: no traceback, no signal, output just stops.
			p.reportExit("process exited unexpectedly: %s",
				DescribeExitWithMemory(waitErr, false))
		}
		close(cmdDone)
	}()

	// abort tears down a start that never reached ready. A bare ctx
	// cancellation and a health-check deadline produce the same startCtx.Err()
	// value, so report the deadline explicitly: "aborted" alone tells an
	// operator nothing, while "health check timed out after 15m0s" points
	// straight at a model that is simply slower to warm up than the configured
	// window.
	abort := func(err error) startResult {
		// Keep ErrStartAborted in the chain — callers classify a pre-ready stop
		// with errors.Is — while stating the deadline explicitly. "aborted"
		// alone is indistinguishable from an operator stop and sends anyone
		// reading the lifecycle error hunting for a cancellation that never
		// happened.
		if errors.Is(startCtx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("health check timed out after %v: %w", healthCheckTimeout, ErrStartAborted)
		}
		p.killProcess(cmd, cmdCancel, cmdDone, 5*time.Second)
		return startResult{err: err}
	}
	prematureExit := func() startResult {
		exitErr := p.getLastExitErr()
		// Report before archiving so the snapshot an operator downloads carries
		// the same explanation the live panel shows.
		cause := DescribeExitWithMemory(exitErr, false)
		p.reportExit("process exited before becoming ready: %s", cause)
		if startCtx.Err() == nil {
			p.recordCrash(exitErr)
		}
		cmdCancel()
		// The lifecycle error is what the control plane surfaces to the caller;
		// the same sentence was already written to the model log above.
		return startResult{err: fmt.Errorf("upstream command %s", cause)}
	}

	if startCtx.Err() != nil {
		return abort(ErrStartAborted)
	}

	checkEndpoint := strings.TrimSpace(p.config.CheckEndpoint)
	if checkEndpoint == "none" {
		return p.finishVLLMStart(startCtx, cmd, cmdDone, cmdCancel, handlerFn, reverseProxy, prematureExit)
	}

	// Wait 250ms for the command to start up before health checking
	select {
	case <-startCtx.Done():
		return abort(ErrStartAborted)
	case <-time.After(250 * time.Millisecond):
	}

	deadline := time.Now().Add(healthCheckTimeout)
	for {
		select {
		case <-startCtx.Done():
			return abort(ErrStartAborted)
		case <-cmdDone:
			return prematureExit()
		default:
		}

		if time.Now().After(deadline) {
			return abort(fmt.Errorf("health check timed out after %v", healthCheckTimeout))
		}

		// Tagged so the proxy ErrorHandler logs a not-yet-listening upstream
		// at debug rather than as a proxy error once per poll.
		checkCtx := context.WithValue(startCtx, healthCheckKey{}, true)
		req, err := http.NewRequestWithContext(checkCtx, "GET", p.config.CheckEndpoint, nil)
		if err != nil {
			// A nil request handed to reverseProxy.ServeHTTP would panic and
			// take the whole daemon down (this runs on a bare goroutine).
			return abort(fmt.Errorf("invalid checkEndpoint %q: %w", p.config.CheckEndpoint, err))
		}
		rr := httptest.NewRecorder()
		reverseProxy.ServeHTTP(rr, req)
		resp := rr.Result()
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			p.proxyLogger.Infof("<%s> Health check passed on %s%s", p.id, p.config.Proxy, p.config.CheckEndpoint)
			break
		} else if startCtx.Err() != nil {
			return abort(ErrStartAborted)
		}

		select {
		case <-startCtx.Done():
			return abort(ErrStartAborted)
		case <-cmdDone:
			return prematureExit()
		case <-time.After(time.Second):
		}
	}

	return p.finishVLLMStart(startCtx, cmd, cmdDone, cmdCancel, handlerFn, reverseProxy, prematureExit)
}

// sendStopSignal runs the configured CmdStop (if any) or sends SIGTERM to
// the upstream process. Wired up as cmd.Cancel so it fires whenever the
// cmd's context is cancelled.
func (p *ProcessCommand) sendStopSignal(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		p.processLogger.Debugf("<%s> sendStopSignal() called with nil cmd or process, nothing to stop", p.id)
		return nil
	}
	pid := cmd.Process.Pid
	if p.config.CmdStop != "" {
		p.processLogger.Debugf("<%s> sendStopSignal() using CmdStop %q for pid %d", p.id, p.config.CmdStop, pid)
		stopArgs, err := config.SanitizeCommand(
			strings.ReplaceAll(p.config.CmdStop, "${PID}", fmt.Sprintf("%d", pid)),
		)
		if err == nil {
			p.processLogger.Debugf("<%s> sendStopSignal() running stop command: %s", p.id, strings.Join(stopArgs, " "))
			// Bound the stop command: it runs on detached goroutines (killProcess)
			// and on exec's ctx watcher (cmd.Cancel), so a hanging stop script
			// would otherwise leak both permanently.
			stopCtx, stopCancel := context.WithTimeout(context.Background(), stopCommandTimeout)
			defer stopCancel()
			stopCmd := exec.CommandContext(stopCtx, stopArgs[0], stopArgs[1:]...)
			stopCmd.Env = cmd.Env
			setProcAttributes(stopCmd)
			runErr := stopCmd.Run()
			if runErr != nil {
				p.processLogger.Errorf("<%s> sendStopSignal() stop command failed: %v", p.id, runErr)
			} else {
				p.processLogger.Debugf("<%s> sendStopSignal() stop command completed for pid %d", p.id, pid)
			}
			return runErr
		}
		// fall through to SIGTERM if sanitize failed
		p.processLogger.Errorf("<%s> sendStopSignal() failed to sanitize CmdStop %q: %v, falling back to terminateProcessTree", p.id, p.config.CmdStop, err)
	}
	// On Unix this SIGTERMs the whole process group so a forked grandchild
	// (e.g. a shell wrapper that backgrounds the real binary) is taken down
	// with the parent rather than orphaned.
	p.processLogger.Debugf("<%s> sendStopSignal() no CmdStop configured, calling terminateProcessTree for pid %d", p.id, pid)
	termErr := terminateProcessTree(cmd)
	if termErr != nil {
		p.processLogger.Errorf("<%s> sendStopSignal() terminateProcessTree failed for pid %d: %v", p.id, pid, termErr)
	}
	return termErr
}

// killProcess terminates the upstream process. The flow:
//
//  1. Send the graceful stop signal (CmdStop / SIGTERM) directly — NOT by
//     cancelling cmdCtx. Cancelling the context would start cmd.WaitDelay
//     immediately, which force-kills the process WaitDelay after the signal
//     and would silently cap gracefulTimeout at WaitDelay whenever
//     gracefulTimeout is the longer of the two.
//  2. We wait up to gracefulTimeout for the process to exit on its own.
//  3. If still alive, we SIGKILL the process group directly (Unix) so any
//     forked descendant is force-terminated alongside the parent.
//  4. We wait on cmdDone. cmd.WaitDelay (set when the cmd was built) is the
//     critical backstop here: once the process exits, if a forked grandchild
//     inherited the stdout/stderr pipes and is still holding them, the runtime
//     force-closes the pipes WaitDelay after the exit and cmd.Wait() unblocks.
//     Because we never cancelled the context, that WaitDelay timer measures
//     from process exit (see os/exec awaitGoroutines), not from this call.
//     Without WaitDelay this select would hang forever (the v219 bug).
//
// cancel() is still invoked (deferred) to release the context, but only after
// the process has exited and os/exec's ctx watcher has already torn down, so it
// never re-fires cmd.Cancel.
func (p *ProcessCommand) killProcess(cmd *exec.Cmd, cancel context.CancelFunc, cmdDone <-chan struct{}, gracefulTimeout time.Duration) {
	if cancel == nil {
		return
	}
	defer cancel()

	// Deliver CmdStop / SIGTERM in a goroutine so a slow or hanging CmdStop
	// cannot block the run() goroutine; the gracefulTimeout + Process.Kill
	// path below still guarantees teardown.
	if cmd != nil {
		go func() {
			p.proxyLogger.Debugf("[%s] sending stop signal with timeout %v", p.id, gracefulTimeout)
			if err := p.sendStopSignal(cmd); err != nil {
				p.proxyLogger.Warnf("[%s] stop signal failed: %v", p.id, err)
			}
		}()
	}

	timer := time.NewTimer(gracefulTimeout)
	defer timer.Stop()

	select {
	case <-cmdDone:
		return
	case <-timer.C:
	}

	if cmd != nil {
		// SIGKILL the whole process group on Unix so any descendant that
		// ignored or outlived the graceful signal is force-terminated too.
		_ = killProcessTree(cmd)
	}
	<-cmdDone
}

func (p *ProcessCommand) ID() string {
	return p.id
}

func (p *ProcessCommand) Run(timeout time.Duration) error {
	req := startReq{
		ctx:     context.Background(),
		timeout: timeout,
		respond: make(chan error, 1),
		block:   true,
	}
	select {
	case p.startCh <- req:
	case <-p.parentCtx.Done():
		return fmt.Errorf("[%s] shutdown", p.id)
	}
	select {
	case err := <-req.respond:
		return err
	case <-p.parentCtx.Done():
		return fmt.Errorf("[%s] shutdown", p.id)
	}
}

// EnsureReady brings the process to a ready state and blocks until it is
// serving. See the Process interface for the full state-by-state contract.
//
// The send on startCh is the synchronisation point: it can only be received
// when the run loop is back at its select, so a stop that is still in progress
// naturally holds the request until the process is really stopped. Callers must
// not inspect State() first — that read races the run loop and is what caused
// issue #946.
func (p *ProcessCommand) EnsureReady(ctx context.Context, timeout time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	req := startReq{
		ctx:     ctx,
		timeout: timeout,
		respond: make(chan error, 1),
	}
	select {
	case p.startCh <- req:
	case <-ctx.Done():
		return ctx.Err()
	case <-p.parentCtx.Done():
		return fmt.Errorf("[%s] shutdown", p.id)
	}
	select {
	case err := <-req.respond:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-p.parentCtx.Done():
		return fmt.Errorf("[%s] shutdown", p.id)
	}
}

func (p *ProcessCommand) WaitReady(ctx context.Context) error {
	req := waitReadyReq{respond: make(chan error, 1)}
	select {
	case p.waitReadyCh <- req:
	case <-ctx.Done():
		return ctx.Err()
	case <-p.parentCtx.Done():
		return fmt.Errorf("[%s] shutdown", p.id)
	}
	select {
	case err := <-req.respond:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *ProcessCommand) Stop(timeout time.Duration) error {
	req := stopReq{
		timeout: timeout,
		respond: make(chan error, 1),
	}
	select {
	case p.stopCh <- req:
	case <-p.parentCtx.Done():
		return fmt.Errorf("[%s] shutdown", p.id)
	}
	return <-req.respond
}

func (p *ProcessCommand) State() ProcessState {
	if s, ok := p.state.Load().(ProcessState); ok {
		return s
	}
	return StateStopped
}

// Sleep asks a ready vLLM process to release its model weights while keeping
// the listener and process generation alive. The request is serialized by the
// process run loop with starts and stops.
func (p *ProcessCommand) Sleep(ctx context.Context, level int) error {
	if ctx == nil {
		ctx = context.Background()
	}
	req := sleepReq{ctx: ctx, level: level, respond: make(chan error, 1)}
	select {
	case p.sleepCh <- req:
	case <-ctx.Done():
		return ctx.Err()
	case <-p.parentCtx.Done():
		return fmt.Errorf("[%s] shutdown", p.id)
	}
	select {
	case err := <-req.respond:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-p.parentCtx.Done():
		return fmt.Errorf("[%s] shutdown", p.id)
	}
}

// Wake restores a sleeping vLLM process before traffic is forwarded to it.
// Like Sleep, the operation is serialized by the process run loop.
func (p *ProcessCommand) Wake(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	req := wakeReq{ctx: ctx, respond: make(chan error, 1)}
	select {
	case p.wakeCh <- req:
	case <-ctx.Done():
		return ctx.Err()
	case <-p.parentCtx.Done():
		return fmt.Errorf("[%s] shutdown", p.id)
	}
	select {
	case err := <-req.respond:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-p.parentCtx.Done():
		return fmt.Errorf("[%s] shutdown", p.id)
	}
}

func (p *ProcessCommand) Sleeping() bool {
	return p.sleeping.Load()
}

func (p *ProcessCommand) usesVLLMSleepMode() bool {
	return p.isVLLMBackend() && p.config.UnloadAfter > 0 &&
		!strings.EqualFold(strings.TrimSpace(p.config.Backend.Lifecycle.Mode), "process")
}

func (p *ProcessCommand) lifecycleEndpoint(path string) (string, error) {
	base, err := url.Parse(strings.TrimSpace(p.config.Proxy))
	if err != nil || base.Scheme == "" || base.Host == "" {
		if err == nil {
			err = fmt.Errorf("proxy URL has no scheme or host")
		}
		return "", fmt.Errorf("invalid vLLM lifecycle proxy %q: %w", p.config.Proxy, err)
	}
	base.Path = strings.TrimRight(base.Path, "/") + path
	base.RawQuery = ""
	return base.String(), nil
}

func lifecycleHTTPTimeout(timeout time.Duration) time.Duration {
	if timeout > 0 {
		return timeout
	}
	return 30 * time.Second
}

func (p *ProcessCommand) sleepVLLM(ctx context.Context, level int, timeout time.Duration) error {
	p.sleepMu.Lock()
	defer p.sleepMu.Unlock()
	if p.sleeping.Load() {
		return nil
	}
	endpoint, err := p.lifecycleEndpoint("/sleep")
	if err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, lifecycleHTTPTimeout(timeout))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return err
	}
	query := "?level=" + fmt.Sprintf("%d", level)
	client := &http.Client{Timeout: lifecycleHTTPTimeout(timeout)}
	req.URL.RawQuery = "level=" + fmt.Sprintf("%d", level)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s%s: %w", endpoint, query, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("POST %s%s returned HTTP %d", endpoint, query, resp.StatusCode)
	}
	return nil
}

func (p *ProcessCommand) wakeVLLM(ctx context.Context) error {
	p.sleepMu.Lock()
	defer p.sleepMu.Unlock()
	if !p.sleeping.Load() {
		return nil
	}
	endpoint, err := p.lifecycleEndpoint("/wake_up")
	if err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	wakeCtx, cancel := context.WithTimeout(ctx, lifecycleHTTPTimeout(time.Duration(p.config.UnloadTimeout)*time.Second))
	defer cancel()
	req, err := http.NewRequestWithContext(wakeCtx, http.MethodPost, endpoint, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: lifecycleHTTPTimeout(time.Duration(p.config.UnloadTimeout) * time.Second)}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("POST %s returned HTTP %d", endpoint, resp.StatusCode)
	}
	return nil
}

func (p *ProcessCommand) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fn := p.handler.Load()
	if fn == nil {
		swaputil.SendResponse(w, r, http.StatusServiceUnavailable, fmt.Sprintf("[%s] process is not ready", p.id))
		return
	}
	if p.sleeping.Load() {
		if err := p.Wake(r.Context()); err != nil {
			p.proxyLogger.Warnf("<%s> vLLM Sleep Mode wake failed: %v", p.id, err)
			swaputil.SendResponse(w, r, http.StatusServiceUnavailable, fmt.Sprintf("[%s] vLLM backend is sleeping and could not be woken: %v", p.id, err))
			return
		}
	}
	if p.config.Compat.IgnoreWebsockets && swaputil.IsWebSocketUpgrade(r) {
		(*fn)(w, r)
		return
	}
	p.inflight.Add(1)
	defer func() {
		p.lastUse.Store(time.Now().UnixNano())
		p.inflight.Add(-1)
	}()
	(*fn)(w, r)
}
