package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mostlygeek/llama-swap/internal/backend"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
	runtimeManager "github.com/mostlygeek/llama-swap/internal/runtime"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

const (
	// lmcacheProgressKey is the BackendProgress key the UI renders for this
	// module. It is not a model or runtime name, which keeps the card's
	// progress bar independent of per-runtime operation progress.
	lmcacheProgressKey  = "lmcache"
	lmcacheStateDir     = "lmcache"
	lmcacheProbeTimeout = 15 * time.Second
	lmcacheStopTimeout  = 10 * time.Second
	// lmcacheConnectorInstallTimeout bounds the uv install a first model start
	// may block on: a cold cache download of the connector package.
	lmcacheConnectorInstallTimeout = 15 * time.Minute
	// lmcacheOutputTailBounds error diagnostics so a verbose uv failure cannot
	// flood the SSE stream or the response body.
	lmcacheOutputTail = 400
	// lmcacheReadyTimeout bounds the STARTING→RUNNING window: the server must
	// answer /healthcheck within it or the start is declared failed. A cold
	// GPU host imports torch before the management endpoint answers.
	lmcacheReadyTimeout = 120 * time.Second
	// lmcacheStartingProbeInterval is the health poll cadence while STARTING.
	lmcacheStartingProbeInterval = 2 * time.Second
	// lmcacheHealthInterval is the periodic re-probe cadence that keeps
	// RUNNING honest: a RUNNING server that stops answering degrades to
	// ERROR instead of pretending to serve.
	lmcacheHealthInterval = 10 * time.Second
	// lmcacheUnhealthyFailures is how many consecutive RUNNING probes must
	// fail before the server is declared down and terminated. The management
	// frontend is a single HTTP process that legitimate heavy work can starve
	// for seconds at a time — registering 64 KV cache tensors across four TP
	// workers is the observed case. Killing a merely-busy server strands those
	// workers until their own 300s register timeout fails the whole model
	// start, so a lone missed probe must never tear the server down.
	lmcacheUnhealthyFailures = 3
	// lmcacheProbeHTTPTimeout bounds a single management-frontend probe.
	lmcacheProbeHTTPTimeout = 3 * time.Second
)

// errLMCacheBusy is returned when an install/uninstall operation is already
// running; the API maps it to 409.
var errLMCacheBusy = errors.New("lmcache operation already in progress")

// errLMCacheInUse is wrapped by lmcacheInUseError; the API maps it to 409 so
// the UI can distinguish "in use by models" from a plain failure.
var errLMCacheInUse = errors.New("lmcache is in use by running models")

// lmcacheState is the explicit lifecycle state of the supervised LMCache
// server. RUNNING requires the process to be alive AND the management
// frontend to answer healthy — a PID alone never counts as serving.
type lmcacheState string

const (
	LMCacheStateNotInstalled lmcacheState = "NOT_INSTALLED"
	LMCacheStateStopped      lmcacheState = "STOPPED"
	LMCacheStateStarting     lmcacheState = "STARTING"
	LMCacheStateRunning      lmcacheState = "RUNNING"
	LMCacheStateStopping     lmcacheState = "STOPPING"
	LMCacheStateError        lmcacheState = "ERROR"
	LMCacheStateUpdating     lmcacheState = "UPDATING"
)

// lmcacheVenvTarget is one active native vLLM runtime venv the module can
// install the connector package into.
type lmcacheVenvTarget struct {
	Name   string
	BinDir string
}

// activeLMCacheVenvs resolves the venv bin directory of every configured
// native vLLM runtime that has an activated version. Runtimes without a
// current pointer or with a missing venv are skipped, so the module can be
// enabled incrementally as runtimes come online.
func (s *Server) activeLMCacheVenvs(cfg config.Config) ([]lmcacheVenvTarget, error) {
	root := managedRuntimeRoot(cfg)
	targets := make([]lmcacheVenvTarget, 0, len(cfg.Runtimes))
	for name, runtimeCfg := range cfg.Runtimes {
		kind := strings.ToLower(strings.TrimSpace(runtimeCfg.Kind))
		if kind != "vllm" || configuredRuntimeMode(runtimeCfg) == "container" {
			continue
		}
		binding, found, err := runtimeManager.CurrentVersionLaunchBinding(root, name, "vllm", "python")
		if err != nil || !found {
			continue
		}
		if _, err := os.Stat(filepath.Join(binding.BinDir, "python")); err != nil {
			continue
		}
		targets = append(targets, lmcacheVenvTarget{Name: name, BinDir: binding.BinDir})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })
	return targets, nil
}

// probeLMCacheVersion asks a venv python for the installed lmcache version.
// An empty version with a nil error means "not installed"; only operational
// failures (missing interpreter, timeout) surface as errors.
func probeLMCacheVersion(ctx context.Context, binDir string) (string, error) {
	const script = `import importlib.metadata as m; print(m.version("lmcache"))`
	probeCtx, cancel := context.WithTimeout(ctx, lmcacheProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, filepath.Join(binDir, "python"), "-c", script)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", nil
		}
		return "", fmt.Errorf("probe lmcache: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// tailOutput keeps the trailing bytes of a command output for diagnostics.
func tailOutput(output string, max int) string {
	output = strings.TrimSpace(output)
	if len(output) <= max {
		return output
	}
	return "…" + output[len(output)-max:]
}

func lmcacheProgressFraction(index, total int, lo, hi float64) float64 {
	if total <= 1 {
		return hi
	}
	return lo + (hi-lo)*float64(index)/float64(total)
}

// probeLMCacheHealth queries the management frontend /healthcheck endpoint.
// A 200 whose body reports healthy is healthy; everything else (503, refused
// connection, timeout) is unhealthy rather than an error because the probe
// feeds state transitions, not diagnostics.
func probeLMCacheHealth(client *http.Client, url string) bool {
	if client == nil || strings.TrimSpace(url) == "" {
		return false
	}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	if err != nil {
		return false
	}
	trimmed := strings.TrimSpace(string(body))
	if strings.EqualFold(trimmed, "healthy") || trimmed == "true" {
		return true
	}
	var payload struct {
		Status    string `json:"status"`
		Healthy   *bool  `json:"healthy"`
		OK        *bool  `json:"ok"`
		IsHealthy *bool  `json:"is_healthy"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false
	}
	if payload.Healthy != nil {
		return *payload.Healthy
	}
	if payload.OK != nil {
		return *payload.OK
	}
	if payload.IsHealthy != nil {
		return *payload.IsHealthy
	}
	return strings.EqualFold(strings.TrimSpace(payload.Status), "healthy")
}

// lmcacheProcess supervises one standalone lmcache server process. It is
// intentionally self-contained rather than built on the model process router:
// the LMCache server is a control-plane daemon, not an inference model. The
// state transitions are:
//
//	STOPPED → STARTING → RUNNING → STOPPING → STOPPED
//	                         ↘ ERROR (crash or health failure, process killed)
//
// RUNNING is only reached after /healthcheck answers and is continuously
// re-verified while the process lives.
type lmcacheProcess struct {
	mu              sync.Mutex
	state           lmcacheState
	cmd             *exec.Cmd
	done            chan struct{}
	intentional     bool
	startedAt       time.Time
	lastError       string
	healthy         bool
	healthCheckedAt time.Time
	healthClient    *http.Client
	healthInterval  time.Duration
	readyTimeout    time.Duration
}

func newLMCacheProcess() lmcacheProcess {
	return lmcacheProcess{
		state:          LMCacheStateStopped,
		healthClient:   &http.Client{Timeout: lmcacheProbeHTTPTimeout},
		healthInterval: lmcacheHealthInterval,
		readyTimeout:   lmcacheReadyTimeout,
	}
}

// Start launches the lmcache server with combined stdout/stderr appended to
// logPath, then watches /healthcheck in the background. Starting a second
// process while one runs is an error. Start returns once the child is
// spawned; the state settles to RUNNING or ERROR through the watcher.
func (p *lmcacheProcess) Start(executable string, args []string, workDir, logPath, healthURL string) error {
	p.mu.Lock()
	if p.cmd != nil {
		p.mu.Unlock()
		return errors.New("lmcache server is already running")
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o750); err != nil {
		p.mu.Unlock()
		return fmt.Errorf("create lmcache log directory: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		p.mu.Unlock()
		return fmt.Errorf("open lmcache server log: %w", err)
	}
	cmd := exec.Command(executable, args...)
	if workDir != "" {
		cmd.Dir = workDir
	}
	cmd.Env = os.Environ()
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		p.mu.Unlock()
		return fmt.Errorf("start lmcache server: %w", err)
	}
	done := make(chan struct{})
	p.cmd = cmd
	p.done = done
	p.intentional = false
	p.startedAt = time.Now()
	p.lastError = ""
	p.healthy = false
	p.healthCheckedAt = time.Time{}
	p.state = LMCacheStateStarting
	p.mu.Unlock()
	go func() {
		waitErr := cmd.Wait()
		logFile.Close()
		p.mu.Lock()
		intentional := p.intentional
		p.cmd = nil
		if !intentional {
			p.healthy = false
			p.healthCheckedAt = time.Now()
			p.lastError = fmt.Sprintf("lmcache server exited unexpectedly: %v", waitErr)
			p.state = LMCacheStateError
		} else if p.state == LMCacheStateStarting || p.state == LMCacheStateRunning || p.state == LMCacheStateStopping {
			p.state = LMCacheStateStopped
		}
		p.mu.Unlock()
		close(done)
	}()
	if healthURL != "" {
		go p.watchHealth(healthURL, done)
	}
	return nil
}

// watchHealth drives STARTING → RUNNING and keeps RUNNING honest. It exits
// quietly when the process is stopped intentionally; a crash or an unhealthy
// server degrades to ERROR with the process terminated so the state matches
// reality.
func (p *lmcacheProcess) watchHealth(healthURL string, done chan struct{}) {
	deadline := time.Now().Add(p.readyTimeout)
	for {
		if p.exited(done) || p.intentionallyStopping() {
			return
		}
		healthy := probeLMCacheHealth(p.healthClient, healthURL)
		p.recordHealth(healthy)
		if healthy {
			p.setState(LMCacheStateRunning, "")
			break
		}
		if time.Now().After(deadline) {
			p.fail("lmcache server did not become healthy within " + p.readyTimeout.String())
			return
		}
		time.Sleep(lmcacheStartingProbeInterval)
	}
	ticker := time.NewTicker(p.healthInterval)
	defer ticker.Stop()
	// Only a sustained run of misses counts as down. A transient miss keeps
	// the last healthy reading: the UI should not flap to unhealthy, and
	// isServerHealthy must stay true so a start request does not try to spawn
	// a second server while this one is simply busy.
	consecutiveFailures := 0
	for range ticker.C {
		if p.exited(done) || p.intentionallyStopping() {
			return
		}
		if probeLMCacheHealth(p.healthClient, healthURL) {
			consecutiveFailures = 0
			p.recordHealth(true)
			continue
		}
		consecutiveFailures++
		p.recordHealthCheck()
		if consecutiveFailures >= lmcacheUnhealthyFailures {
			p.recordHealth(false)
			p.fail("lmcache server health check failed")
			return
		}
	}
}

// fail records an ERROR state with the cause and terminates the process. The
// intentional flag is set first so the wait goroutine preserves lastError
// instead of overwriting it with a generic exit message.
func (p *lmcacheProcess) fail(reason string) {
	p.mu.Lock()
	if p.cmd == nil {
		p.healthy = false
		p.healthCheckedAt = time.Now()
		if p.state != LMCacheStateError {
			p.state = LMCacheStateError
			p.lastError = reason
		}
		p.mu.Unlock()
		return
	}
	p.healthy = false
	p.healthCheckedAt = time.Now()
	if p.state == LMCacheStateStopped || p.state == LMCacheStateNotInstalled {
		p.mu.Unlock()
		return
	}
	p.state = LMCacheStateError
	p.lastError = reason
	p.intentional = true
	p.healthy = false
	p.healthCheckedAt = time.Now()
	p.mu.Unlock()
	_ = p.Stop(lmcacheStopTimeout)
}

// Stop signals the server to terminate and waits up to timeout, escalating to
// SIGKILL. Stopping a process that is not running is a no-op.
func (p *lmcacheProcess) Stop(timeout time.Duration) error {
	p.mu.Lock()
	cmd := p.cmd
	done := p.done
	if cmd == nil {
		p.mu.Unlock()
		return nil
	}
	p.intentional = true
	p.healthy = false
	p.healthCheckedAt = time.Now()
	// A server that failed its health check (fail) is already ERROR; a stop
	// is the cleanup for that, not a fresh transition, so do not downgrade it.
	if p.state != LMCacheStateError && p.state != LMCacheStateUpdating {
		p.state = LMCacheStateStopping
	}
	p.mu.Unlock()
	if runtime.GOOS == "windows" {
		_ = cmd.Process.Kill()
	} else {
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			_ = cmd.Process.Kill()
		}
	}
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		return fmt.Errorf("lmcache server did not stop within %s; killed", timeout)
	}
}

// setExternalState records a service-level state (UPDATING) that outlives a
// single process run. It only applies while no process is alive so a stale
// update marker can never hide a live server.
func (p *lmcacheProcess) setExternalState(state lmcacheState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd == nil {
		p.healthy = false
		p.healthCheckedAt = time.Now()
		p.state = state
	}
}

// recordStartFailure records a failed start attempt that never spawned a
// process (bad L2/L3 configuration, missing executable): the state becomes
// ERROR with the reason so the status endpoint reports why instead of
// showing a healthy-looking STOPPED. It only applies while no process is
// alive — a live server's state is owned by the wait and health goroutines,
// and a failed restart leaves the running server untouched.
func (p *lmcacheProcess) recordStartFailure(lastError string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd != nil {
		return
	}
	p.healthy = false
	p.healthCheckedAt = time.Now()
	p.state = LMCacheStateError
	p.lastError = lastError
}

type lmcacheProcStatus struct {
	State           lmcacheState `json:"state"`
	Running         bool         `json:"running"`
	PID             int          `json:"pid,omitempty"`
	StartedAt       time.Time    `json:"startedAt,omitempty"`
	LastError       string       `json:"lastError,omitempty"`
	Healthy         bool         `json:"healthy"`
	HealthCheckedAt time.Time    `json:"healthCheckedAt,omitempty"`
}

// Status reports the supervised process state.
func (p *lmcacheProcess) Status() lmcacheProcStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	status := lmcacheProcStatus{State: p.state, LastError: p.lastError, Healthy: p.healthy, HealthCheckedAt: p.healthCheckedAt}
	if p.cmd != nil {
		status.Running = true
		status.PID = p.cmd.Process.Pid
		status.StartedAt = p.startedAt
	}
	return status
}

func (p *lmcacheProcess) recordHealth(healthy bool) {
	p.mu.Lock()
	p.healthy = healthy
	p.healthCheckedAt = time.Now()
	p.mu.Unlock()
}

// recordHealthCheck notes that a probe ran without changing the health verdict.
// Transient misses use it so a busy server keeps its last known-good state.
func (p *lmcacheProcess) recordHealthCheck() {
	p.mu.Lock()
	p.healthCheckedAt = time.Now()
	p.mu.Unlock()
}

// exited reports whether the wait goroutine has already closed the run.
func (p *lmcacheProcess) exited(done chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

func (p *lmcacheProcess) intentionallyStopping() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.intentional
}

// setState records a transition while holding no lock; it refuses to move a
// stopping or already-dead process so a late health answer cannot resurrect
// a server the operator just stopped.
func (p *lmcacheProcess) setState(state lmcacheState, lastError string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd == nil || p.intentional {
		return
	}
	p.state = state
	if lastError != "" {
		p.lastError = lastError
	}
}

type lmcacheRuntimeStatus struct {
	Name      string `json:"name"`
	Active    bool   `json:"active"`
	Installed bool   `json:"installed"`
	Version   string `json:"version,omitempty"`
	Error     string `json:"error,omitempty"`
}

type lmcacheServerStatus struct {
	Enabled         bool         `json:"enabled"`
	State           lmcacheState `json:"state"`
	Running         bool         `json:"running"`
	Version         string       `json:"version,omitempty"`
	PID             int          `json:"pid,omitempty"`
	StartedAt       time.Time    `json:"startedAt,omitempty"`
	LastError       string       `json:"lastError,omitempty"`
	Healthy         bool         `json:"healthy"`
	HealthCheckedAt time.Time    `json:"healthCheckedAt,omitempty"`
	Host            string       `json:"host"`
	Port            int          `json:"port"`
	HTTPHost        string       `json:"httpHost"`
	HTTPPort        int          `json:"httpPort"`
	L1SizeGB        int          `json:"l1SizeGB"`
	EvictionPolicy  string       `json:"evictionPolicy"`
	ChunkSize       int          `json:"chunkSize"`
	// L2 is the pinned-DRAM pool ("system memory"). L2UsedBytes is nil when
	// the usage number is unavailable; the UI renders that as unavailable.
	L2Enabled   bool   `json:"l2Enabled"`
	L2MaxBytes  int64  `json:"l2MaxBytes"`
	L2UsedBytes *int64 `json:"l2UsedBytes,omitempty"`
	// L3 is the disk tier. Its usage is not exposed by the LMCache fs
	// adapter (documented capability gap), so no used field.
	L3Enabled bool   `json:"l3Enabled"`
	L3Path    string `json:"l3Path,omitempty"`
	// VenvPath is the dedicated server virtualenv the running (or to-be-run)
	// process launches from. It identifies the isolation boundary: the server
	// never runs from a vLLM venv. Empty when the runtime is not staged.
	VenvPath string `json:"venvPath,omitempty"`
	// LogPath is the supervised server log. Error responses attach its tail so
	// a failure is diagnosable without leaving the UI.
	LogPath string `json:"logPath,omitempty"`
}

type lmcacheStatus struct {
	Installed      bool                   `json:"installed"`
	AllInstalled   bool                   `json:"allInstalled"`
	Version        string                 `json:"version,omitempty"`
	Runtimes       []lmcacheRuntimeStatus `json:"runtimes"`
	EnabledModels  []string               `json:"enabledModels"`
	UsingModels    []string               `json:"usingModels"`
	Installing     bool                   `json:"installing"`
	PendingRestart bool                   `json:"pendingRestart"`
	Server         lmcacheServerStatus    `json:"server"`
	Update         lmcacheUpdateStatus    `json:"update"`
	Error          string                 `json:"error,omitempty"`
}

type lmcacheUpdateStatus struct {
	Policy        string               `json:"policy"`
	Channel       string               `json:"channel"`
	TargetVersion string               `json:"targetVersion,omitempty"`
	Current       string               `json:"current,omitempty"`
	Previous      string               `json:"previous,omitempty"`
	Staged        string               `json:"staged,omitempty"`
	Available     string               `json:"available,omitempty"`
	Pinned        bool                 `json:"pinned"`
	PinnedVersion string               `json:"pinnedVersion,omitempty"`
	LastCheck     time.Time            `json:"lastCheck,omitempty"`
	LastUpdate    time.Time            `json:"lastUpdate,omitempty"`
	State         runtimeManager.State `json:"state"`
	LastError     string               `json:"lastError,omitempty"`
}

// lmcacheService owns the optional LMCache module: the derived server
// runtime (its own version-managed virtualenv), the pinned connector installs
// into native vLLM venvs, and supervision of the standalone lmcache server.
// lmcacheStartOp is the singleflight slot for a server start: concurrent
// model starts share one spawn and its settled result instead of racing on
// the supervised process. Like lmcacheConnectorOp it stores the outcome once
// and closes sig to wake every waiter: a buffered channel hands the value to a
// single receiver, so any other concurrent model start would block on it until
// its own start deadline expired and then fail a perfectly healthy server.
type lmcacheStartOp struct {
	mu   sync.Mutex
	err  error
	done bool
	sig  chan struct{}
}

func (op *lmcacheStartOp) finish(err error) {
	op.mu.Lock()
	op.err = err
	op.done = true
	op.mu.Unlock()
	close(op.sig)
}

// wait reports the start outcome, or the caller's context error when its own
// budget expires first. A start that settled in the same instant still reports
// its real result rather than the deadline.
func (op *lmcacheStartOp) wait(ctx context.Context) error {
	select {
	case <-op.sig:
	case <-ctx.Done():
		op.mu.Lock()
		settled := op.done
		op.mu.Unlock()
		if !settled {
			return ctx.Err()
		}
	}
	op.mu.Lock()
	defer op.mu.Unlock()
	return op.err
}

// lmcacheConnectorOp is one in-flight connector install shared by every
// caller for the same venv. The outcome is stored once and sig is closed to
// wake all waiters: a buffered channel would deliver the result to a single
// receiver, starving the others.
type lmcacheConnectorOp struct {
	mu   sync.Mutex
	err  error
	done bool
	sig  chan struct{}
}

func (op *lmcacheConnectorOp) finish(err error) {
	op.mu.Lock()
	op.err = err
	op.done = true
	op.mu.Unlock()
	close(op.sig)
}

// wait reports the install outcome, or a retryable error when the caller's
// budget (the model's start timeout) expires while the install is still
// running in the background.
func (op *lmcacheConnectorOp) wait(ctx context.Context) error {
	op.mu.Lock()
	if op.done {
		err := op.err
		op.mu.Unlock()
		return err
	}
	op.mu.Unlock()
	select {
	case <-op.sig:
		op.mu.Lock()
		defer op.mu.Unlock()
		return op.err
	case <-ctx.Done():
		// The install may have finished in the same instant; report its real
		// outcome instead of a stale retry hint.
		op.mu.Lock()
		if op.done {
			err := op.err
			op.mu.Unlock()
			return err
		}
		op.mu.Unlock()
		return fmt.Errorf("the LMCache connector install for this venv is still running in the background; retry the model start: %w", ctx.Err())
	}
}

type lmcacheService struct {
	srv         *Server
	installBusy atomic.Bool
	proc        lmcacheProcess
	// users is the live model reference count: acquired by each model
	// process's pre-start hook, released when the model leaves the running
	// states. The authoritative in-use view for destructive operations.
	users *lmcacheUsers
	// startMu guards startOp, the in-flight start that ensureServerRunning
	// callers share (singleflight).
	startMu sync.Mutex
	startOp *lmcacheStartOp
	// connectorMu guards connectorOps: per-venv in-flight connector installs
	// shared by concurrent model starts (singleflight per venv).
	connectorMu  sync.Mutex
	connectorOps map[string]*lmcacheConnectorOp
	// pendingRestart marks the D4 "saved, pending apply" state: a config
	// change requires a server restart, but in-use models block it.
	pendingRestart atomic.Bool
	// systemStopped records that the daemon itself stopped the server for a
	// config apply (parameter or version change) and intends it to come
	// back. A later reload finds this set after a failed restart and brings
	// the server up; an operator stop clears it so a deliberate stop is
	// never undone by a reload.
	systemStopped atomic.Bool
	// spawns counts server process starts since daemon boot. Observable in
	// tests to prove singleflight (concurrent gates yield exactly one spawn).
	spawns atomic.Int64
	// runCommand executes uv probe/install commands; tests inject a fake.
	runCommand func(ctx context.Context, name string, args ...string) (string, error)
	// diskFree reports the free bytes on the filesystem holding path; tests
	// inject a fake to exercise the L3 free-space check without a full disk.
	diskFree func(path string) (int64, error)
	// switchMu guards switchState: what the pre-activation hook stopped, so
	// the post-activation hook can restore the right serving version.
	switchMu    sync.Mutex
	switchState lmcacheSwitchState
	// reconcileMu carries the desired config while a config reload is applying
	// a version pointer. Runtime activation calls the same hooks as an API
	// upgrade, but the server's published config is updated only after the
	// router/peer reload succeeds. Keeping this short-lived override here makes
	// a disabled server stay stopped during that pointer switch.
	reconcileMu  sync.RWMutex
	reconcileCfg *config.Config
}

func newLMCacheService(srv *Server) *lmcacheService {
	return &lmcacheService{
		srv:        srv,
		proc:       newLMCacheProcess(),
		users:      newLMCacheUsers(),
		runCommand: defaultLMCacheCommand,
		diskFree:   lmcacheDiskFreeSpace,
	}
}

func (svc *lmcacheService) lifecycleConfig() config.Config {
	if svc == nil || svc.srv == nil {
		return config.Config{}
	}
	svc.reconcileMu.RLock()
	if svc.reconcileCfg != nil {
		cfg := *svc.reconcileCfg
		svc.reconcileMu.RUnlock()
		return cfg
	}
	svc.reconcileMu.RUnlock()
	return svc.srv.currentConfig()
}

func (svc *lmcacheService) setReconcileConfig(cfg *config.Config) {
	if svc == nil {
		return
	}
	svc.reconcileMu.Lock()
	if cfg == nil {
		svc.reconcileCfg = nil
	} else {
		copy := *cfg
		svc.reconcileCfg = &copy
	}
	svc.reconcileMu.Unlock()
}

func defaultLMCacheCommand(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// publish pushes module progress onto the shared BackendProgress stream. The
// UI's LMCache card renders the lmcacheProgressKey entry; empty error clears.
func (svc *lmcacheService) publish(phase string, progress float64, message string, progErr string) {
	backend.PublishProgress(swaputil.BackendProgressEvent{
		Runtime:  lmcacheProgressKey,
		Phase:    phase,
		Progress: progress,
		Message:  message,
		Error:    progErr,
	})
}

// lmcacheServerVersion reports the version of the currently activated server
// runtime without requiring the manager's in-memory state: the current
// pointer and manifest are the durable source of truth.
func lmcacheServerVersion(cfg config.Config) (string, bool, error) {
	root := managedRuntimeRoot(cfg)
	manifest, found, err := runtimeManager.CurrentManifest(root, config.LMCacheRuntimeName, "lmcache")
	if err != nil {
		return "", false, err
	}
	if !found {
		return "", false, nil
	}
	return manifest.Version, true, nil
}

// modelUsesLMCacheServer reports whether a model's LMCache attachment depends
// on the standalone server. A non-standalone (inProcess) model runs the
// library inside its own vLLM process, so it neither holds a server reference
// nor blocks stopping the service.
func modelUsesLMCacheServer(lm *config.LMCacheModelConfig) bool {
	if lm == nil || !lm.Enabled {
		return false
	}
	return lm.EffectiveMode() != config.LMCacheModeInProcess
}

// lmcacheInUseModels returns the models the CONFIG says use the server:
// configured with backend.lmcache and not stopped. It complements the live
// reference tracker (svc.users): together via inUseModels they reject
// destructive operations, and the union never under-reports.
func (svc *lmcacheService) lmcacheInUseModels(cfg config.Config) []string {
	if svc.srv.local == nil {
		return nil
	}
	running := svc.srv.local.RunningModels()
	if len(running) == 0 {
		return nil
	}
	ids := make([]string, 0, len(running))
	for id, model := range cfg.Models {
		if !modelUsesLMCacheServer(model.Backend.LMCache) {
			continue
		}
		if _, active := running[id]; active {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func lmcacheInUseError(models []string) error {
	list := strings.Join(models, ", ")
	return fmt.Errorf("%w: %d model(s) are using LMCache: %s (stop those models first)", errLMCacheInUse, len(models), list)
}

// inUseModels is the sorted union of the live reference tracker and the
// config-based running view. The tracker starts empty at daemon boot, so the
// config view covers models already running before tracking engaged; the
// union is the only view that is safe to act on.
func (svc *lmcacheService) inUseModels(cfg config.Config) []string {
	seen := make(map[string]struct{})
	var ids []string
	for _, id := range append(svc.users.users(), svc.lmcacheInUseModels(cfg)...) {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// requireNoUsers fails while any model still uses the server; the error
// wraps errLMCacheInUse so the API maps it to 409.
func (svc *lmcacheService) requireNoUsers(cfg config.Config) error {
	if models := svc.inUseModels(cfg); len(models) > 0 {
		return fmt.Errorf("%w: %w", runtimeManager.ErrRuntimeBusy, lmcacheInUseError(models))
	}
	return nil
}

// status computes the module state: the dedicated server runtime
// (installed/version), per-venv connector probes, the in-use models, and the
// supervised server.
func (svc *lmcacheService) status(cfg config.Config) lmcacheStatus {
	st := lmcacheStatus{Runtimes: []lmcacheRuntimeStatus{}, EnabledModels: []string{}, UsingModels: []string{}}
	st.Installing = svc.installBusy.Load()
	st.PendingRestart = svc.pendingRestart.Load()
	targets, _ := svc.srv.activeLMCacheVenvs(cfg)
	// Connector probes execute Python imports and may be slow on a cold GPU
	// host. Run them concurrently behind one bounded context so one broken venv
	// cannot serialize the whole /api/lmcache response.
	st.Runtimes = make([]lmcacheRuntimeStatus, len(targets))
	probeCtx, cancel := context.WithTimeout(context.Background(), lmcacheProbeTimeout)
	var probeWG sync.WaitGroup
	for index, target := range targets {
		probeWG.Add(1)
		go func(index int, target lmcacheVenvTarget) {
			defer probeWG.Done()
			entry := lmcacheRuntimeStatus{Name: target.Name, Active: true}
			version, err := probeLMCacheVersion(probeCtx, target.BinDir)
			if err != nil {
				entry.Error = err.Error()
			} else {
				entry.Installed = version != ""
				entry.Version = version
			}
			st.Runtimes[index] = entry
		}(index, target)
	}
	probeWG.Wait()
	cancel()
	for _, entry := range st.Runtimes {
		if entry.Error != "" && st.Error == "" {
			st.Error = fmt.Sprintf("%s: %s", entry.Name, entry.Error)
		}
	}
	// installed/version describe the dedicated server runtime, not the
	// connector probes above: the module is installed when its server venv
	// exists, independent of any vLLM runtime.
	serverVersion := svc.serverRuntimeVersion(cfg)
	st.Version = serverVersion
	st.Installed = serverVersion != ""
	st.AllInstalled = len(st.Runtimes) > 0
	for _, entry := range st.Runtimes {
		if !entry.Installed {
			st.AllInstalled = false
		}
	}
	for name, model := range cfg.Models {
		if model.Backend.LMCache != nil && model.Backend.LMCache.Enabled {
			st.EnabledModels = append(st.EnabledModels, name)
		}
	}
	sort.Strings(st.EnabledModels)
	st.UsingModels = svc.inUseModels(cfg)
	eff := cfg.LMCache.Server.Effective()
	proc := svc.proc.Status()
	// NOT_INSTALLED is a module-level state: the process state alone cannot
	// distinguish "never staged" from "staged and stopped".
	if !proc.Running {
		if _, found, err := lmcacheServerVersion(cfg); err == nil {
			switch {
			case !found:
				proc.State = LMCacheStateNotInstalled
			case proc.State == LMCacheStateNotInstalled:
				proc.State = LMCacheStateStopped
			}
		}
	}
	st.Server = lmcacheServerStatus{
		Enabled:         cfg.LMCache.Server.Enabled,
		State:           proc.State,
		Running:         proc.Running,
		Version:         serverVersion,
		PID:             proc.PID,
		StartedAt:       proc.StartedAt,
		LastError:       proc.LastError,
		Healthy:         proc.Healthy,
		HealthCheckedAt: proc.HealthCheckedAt,
		Host:            eff.Host,
		Port:            eff.Port,
		HTTPHost:        eff.HTTPHost,
		HTTPPort:        eff.HTTPPort,
		L1SizeGB:        eff.L1SizeGB,
		EvictionPolicy:  eff.EvictionPolicy,
		ChunkSize:       eff.ChunkSize,
	}
	if l2Bytes, err := eff.EffectiveL2MaxBytes(); err == nil {
		st.Server.L2Enabled = eff.L2.Enabled
		st.Server.L2MaxBytes = l2Bytes
	}
	st.Server.L2UsedBytes = svc.l2UsageBytes(cfg)
	st.Server.L3Enabled = eff.L3.Enabled
	if eff.L3.Enabled {
		st.Server.L3Path = eff.L3.Path
	}
	st.Server.VenvPath = svc.serverVenvPath(cfg)
	st.Server.LogPath = lmcacheServerLogPath(cfg)
	st.Update = svc.updateStatus(cfg, serverVersion)
	return st
}

func (svc *lmcacheService) updateStatus(cfg config.Config, serverVersion string) lmcacheUpdateStatus {
	update := lmcacheUpdateStatus{
		Policy:        "disabled",
		Channel:       "stable",
		TargetVersion: strings.TrimSpace(cfg.LMCache.EffectiveVersion()),
		Current:       serverVersion,
	}
	if svc == nil || svc.srv == nil || svc.srv.runtime == nil {
		return update
	}
	if definition, ok := svc.srv.runtime.Definition(config.LMCacheRuntimeName); ok {
		update.Policy = definition.Policy.Policy
		update.Channel = definition.Policy.Channel
		if strings.TrimSpace(definition.Spec.Version) != "" {
			update.TargetVersion = definition.Spec.Version
		}
	}
	if status, ok := svc.srv.runtime.Get(config.LMCacheRuntimeName); ok {
		update.Current = status.Current
		if update.Current == "" {
			update.Current = serverVersion
		}
		update.Previous = status.Previous
		update.Staged = status.Staged
		update.Available = status.Available
		update.Pinned = strings.TrimSpace(status.Pinned) != ""
		update.PinnedVersion = status.Pinned
		update.LastCheck = status.LastCheck
		update.LastUpdate = status.LastUpdate
		update.State = status.State
		update.LastError = status.LastError
	}
	return update
}

// lmcacheServerLogPath is the supervised server log location. It lives under
// the runtime root's lmcache state dir, distinct from per-model logs, so a
// server failure is attributable to the LMCache runtime (spec 25).
func lmcacheServerLogPath(cfg config.Config) string {
	return filepath.Join(managedRuntimeRoot(cfg), lmcacheStateDir, "server.log")
}

// serverVenvPath resolves the dedicated server virtualenv the current pointer
// names. It follows the same resolution as the launch path, so the reported
// venv always matches the executable that would actually run. Empty when the
// runtime is not staged or the pointer is invalid.
func (svc *lmcacheService) serverVenvPath(cfg config.Config) string {
	if _, workDir, err := resolveServerExecutable(cfg, ""); err == nil {
		return workDir
	}
	return ""
}

func lmcacheUpdateChannel(cfg config.Config) string {
	if cfg.LMCache.Update != nil {
		if channel := strings.TrimSpace(cfg.LMCache.Update.Channel); channel != "" {
			return channel
		}
	}
	return "stable"
}

// stageUpdate downloads and verifies a candidate lmcache server version into
// the versioned runtime without activating it (spec 16: stage without apply).
// The serving process and the current pointer are untouched, so staging is
// safe while the server is up. An explicit version wins over the configured
// pin, which wins over the newest published release. A version that is
// already installed is returned as-is: re-downloading an identical candidate
// is a no-op the manager itself would refuse.
func (svc *lmcacheService) stageUpdate(ctx context.Context, cfg config.Config, version string) (string, error) {
	version = strings.TrimSpace(version)
	if version == "" {
		version = strings.TrimSpace(cfg.LMCache.EffectiveVersion())
	}
	if version == "" {
		svc.publish("installing", 0.08, "resolving newest lmcache release", "")
		provider := runtimeManager.LMCacheProvider{
			IndexURL:        strings.TrimSpace(cfg.LMCache.IndexURL),
			UpdateURL:       lmcacheUpdateURL(cfg.LMCache.IndexURL, cfg.LMCache.PackageName()),
			SourceAllowlist: append([]string(nil), cfg.RuntimeManager.SourceAllowlist...),
		}
		resolved, err := provider.LatestVersionForChannel(ctx, cfg.LMCache.PackageName(), lmcacheUpdateChannel(cfg))
		if err != nil {
			return "", fmt.Errorf("resolve lmcache version: %w (set lmcache.update.version to pin an exact release)", err)
		}
		version = resolved
	}
	if _, _, err := resolveServerExecutable(cfg, version); err == nil {
		svc.publish("staged", 1, fmt.Sprintf("lmcache %s is already staged", version), "")
		return version, nil
	}
	manager := svc.srv.runtime
	if manager == nil {
		return "", errors.New("runtime manager is unavailable; restart the daemon")
	}
	build := map[string]string{
		"package": cfg.LMCache.PackageName(),
		"python":  cfg.LMCache.EffectivePythonVersion(),
	}
	if indexURL := strings.TrimSpace(cfg.LMCache.IndexURL); indexURL != "" {
		if !runtimeManager.ValidSourceURL(indexURL, cfg.RuntimeManager.SourceAllowlist) {
			return "", fmt.Errorf("lmcache.indexURL %q is not allowed by runtimeManager.sourceAllowlist", indexURL)
		}
		build["indexURL"] = indexURL
	}
	spec := runtimeManager.Spec{
		Name:       config.LMCacheRuntimeName,
		Kind:       "lmcache",
		Mode:       runtimeManager.RuntimeModeNative,
		Version:    version,
		SourceType: "pypi",
		Build:      build,
	}
	svc.publish("installing", 0.15, fmt.Sprintf("staging lmcache %s (not activated)", version), "")
	if err := manager.Register(config.LMCacheRuntimeName, "lmcache"); err != nil {
		return "", fmt.Errorf("register lmcache runtime: %w", err)
	}
	staged, err := manager.Stage(ctx, spec)
	if err != nil {
		svc.publish("error", 0, "", tailOutput(err.Error(), lmcacheOutputTail))
		return "", fmt.Errorf("stage lmcache %s: %w", version, err)
	}
	svc.publish("staged", 1, fmt.Sprintf("lmcache %s staged; activate to apply", staged.Version), "")
	return staged.Version, nil
}

// serverRuntimeVersion is the version the server executable resolves from,
// read directly from the durable current pointer.
func (svc *lmcacheService) serverRuntimeVersion(cfg config.Config) string {
	version, _, _ := lmcacheServerVersion(cfg)
	return version
}

// ensureServerRuntime stages and activates the derived lmcache server
// runtime when its current version does not satisfy the configured pin, and
// returns the active version. An empty configured pin accepts whatever is
// already activated (a reload must never re-stage a healthy runtime) and
// only resolves the newest published release for a first install.
func (svc *lmcacheService) ensureServerRuntime(ctx context.Context, cfg config.Config) (string, error) {
	root := managedRuntimeRoot(cfg)
	pinned := strings.TrimSpace(cfg.LMCache.EffectiveVersion())
	manifest, found, err := runtimeManager.CurrentManifest(root, config.LMCacheRuntimeName, "lmcache")
	if err != nil {
		return "", fmt.Errorf("read lmcache server runtime: %w", err)
	}
	if found && (pinned == "" || pinned == manifest.Version) {
		return manifest.Version, nil
	}
	version := pinned
	if version == "" {
		svc.publish("installing", 0.08, "resolving newest lmcache release", "")
		provider := runtimeManager.LMCacheProvider{
			IndexURL:        strings.TrimSpace(cfg.LMCache.IndexURL),
			UpdateURL:       lmcacheUpdateURL(cfg.LMCache.IndexURL, cfg.LMCache.PackageName()),
			SourceAllowlist: append([]string(nil), cfg.RuntimeManager.SourceAllowlist...),
		}
		version, err = provider.LatestVersionForChannel(ctx, cfg.LMCache.PackageName(), lmcacheUpdateChannel(cfg))
		if err != nil {
			return "", fmt.Errorf("resolve lmcache version: %w (set lmcache.update.version to pin an exact release)", err)
		}
	}
	if found && manifest.Version == version {
		return version, nil
	}
	manager := svc.srv.runtime
	if manager == nil {
		return "", errors.New("runtime manager is unavailable; restart the daemon")
	}
	build := map[string]string{
		"package": cfg.LMCache.PackageName(),
		"python":  cfg.LMCache.EffectivePythonVersion(),
	}
	if indexURL := strings.TrimSpace(cfg.LMCache.IndexURL); indexURL != "" {
		if !runtimeManager.ValidSourceURL(indexURL, cfg.RuntimeManager.SourceAllowlist) {
			return "", fmt.Errorf("lmcache.indexURL %q is not allowed by runtimeManager.sourceAllowlist", indexURL)
		}
		build["indexURL"] = indexURL
	}
	spec := runtimeManager.Spec{
		Name:       config.LMCacheRuntimeName,
		Kind:       "lmcache",
		Mode:       runtimeManager.RuntimeModeNative,
		Version:    version,
		SourceType: "pypi",
		Build:      build,
	}
	if found && svc.proc.Status().Running {
		// Replacing the serving venv underneath a live server is a restart
		// operation; the callers (enable, updates) own that decision.
		return "", fmt.Errorf("lmcache %s is already active and the server is running; stop the server before replacing it", manifest.Version)
	}
	svc.publish("installing", 0.15, fmt.Sprintf("staging lmcache %s into the dedicated server venv", version), "")
	if err := manager.Register(config.LMCacheRuntimeName, "lmcache"); err != nil {
		return "", fmt.Errorf("register lmcache runtime: %w", err)
	}
	staged, err := manager.Stage(ctx, spec)
	if err != nil {
		svc.publish("error", 0, "", tailOutput(err.Error(), lmcacheOutputTail))
		return "", fmt.Errorf("stage lmcache %s: %w", version, err)
	}
	svc.publish("installing", 0.9, fmt.Sprintf("activating lmcache %s", staged.Version), "")
	if err := manager.Activate(ctx, config.LMCacheRuntimeName, staged.Version); err != nil {
		svc.publish("error", 0, "", tailOutput(err.Error(), lmcacheOutputTail))
		return "", fmt.Errorf("activate lmcache %s: %w", staged.Version, err)
	}
	svc.pendingRestart.Store(false)
	return staged.Version, nil
}

// enable installs the dedicated LMCache server runtime (its own version-
// managed venv) and starts the server when configured. It never touches a
// vLLM venv: the runtime manager owns the server installation only, and the
// connector a model's vLLM venv needs is installed by the model's pre-start
// gate when a model actually uses it. The operation continues after a client
// disconnect like a runtime stage.
func (svc *lmcacheService) enable(ctx context.Context, cfg config.Config) error {
	if !svc.installBusy.CompareAndSwap(false, true) {
		return errLMCacheBusy
	}
	defer svc.installBusy.Store(false)
	svc.publish("installing", 0.1, "installing the LMCache server runtime", "")
	if _, err := svc.ensureServerRuntime(ctx, cfg); err != nil {
		return fmt.Errorf("lmcache server runtime: %w", err)
	}
	svc.publish("idle", 1, "LMCache installed", "")
	if cfg.LMCache.Server.Enabled {
		if startErr := svc.ensureServerRunning(ctx, cfg); startErr != nil {
			return fmt.Errorf("lmcache installed, but server start failed: %w", startErr)
		}
	}
	return nil
}

// disable stops the manager (if running) and uninstalls the connector from
// every active venv. It refuses while models still use the server.
// Uninstalling an absent package is treated as success so the operation
// stays idempotent.
func (svc *lmcacheService) disable(ctx context.Context, cfg config.Config) error {
	if !svc.installBusy.CompareAndSwap(false, true) {
		return errLMCacheBusy
	}
	defer svc.installBusy.Store(false)
	if err := svc.requireNoUsers(cfg); err != nil {
		return err
	}
	if stopErr := svc.stopServer(); stopErr != nil {
		if svc.srv.proxylog != nil {
			svc.srv.proxylog.Warnf("lmcache server stop during disable: %v", stopErr)
		}
	}
	targets, err := svc.srv.activeLMCacheVenvs(cfg)
	if err != nil {
		return err
	}
	for i, target := range targets {
		args := []string{"pip", "uninstall", "--python", filepath.Join(target.BinDir, "python"), "-y", "lmcache"}
		svc.publish("removing", lmcacheProgressFraction(i, len(targets), 0.05, 0.95), fmt.Sprintf("removing lmcache from %s", target.Name), "")
		if _, runErr := svc.runCommand(ctx, "uv", args...); runErr != nil {
			return fmt.Errorf("uninstall lmcache from %s: %v", target.Name, runErr)
		}
	}
	svc.publish("idle", 1, "LMCache removed", "")
	return nil
}

// lmcacheServerExecutable finds the lmcache console script in the dedicated
// server runtime's currently activated virtualenv. The server never runs
// from a vLLM venv: the two dependency trees are isolated by design.
func (svc *lmcacheService) lmcacheServerExecutable(cfg config.Config) (string, string, error) {
	root := managedRuntimeRoot(cfg)
	binding, found, err := runtimeManager.CurrentVersionLaunchBinding(root, config.LMCacheRuntimeName, "lmcache", "lmcache")
	if err != nil {
		return "", "", fmt.Errorf("resolve lmcache server runtime: %w", err)
	}
	if !found {
		return "", "", errors.New("lmcache server runtime is not staged; enable the LMCache module first")
	}
	if info, statErr := os.Stat(binding.Executable); statErr != nil || !info.Mode().IsRegular() {
		return "", "", fmt.Errorf("lmcache executable not found in the dedicated server venv; enable the LMCache module first")
	}
	workDir := filepath.Dir(binding.BinDir)
	return binding.Executable, workDir, nil
}

// healthCheckURL is the management frontend probe target for the configured
// server.
func (svc *lmcacheService) healthCheckURL(cfg config.Config) string {
	eff := cfg.LMCache.Server.Effective()
	return fmt.Sprintf("http://%s:%d/healthcheck", eff.HTTPHost, eff.HTTPPort)
}

// isServerHealthy is the fast path of the model gate: the state is RUNNING
// AND /healthcheck answers right now. The state alone can lag a just-failed
// probe by up to one health interval, so the gate re-probes.
func (svc *lmcacheService) isServerHealthy(cfg config.Config) bool {
	if svc.proc.Status().State != LMCacheStateRunning {
		return false
	}
	return probeLMCacheHealth(svc.proc.healthClient, svc.healthCheckURL(cfg))
}

// tailFile reads the trailing bytes of a log file for error diagnostics. A
// missing or unreadable file yields an empty tail, never an error.
func tailFile(path string, max int) string {
	info, err := os.Stat(path)
	if err != nil || info.Size() <= 0 {
		return ""
	}
	offset := int64(0)
	if info.Size() > int64(max) {
		offset = info.Size() - int64(max)
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// waitForServerSettled blocks until the supervised process leaves the
// starting states: RUNNING settles to nil, ERROR/STOPPED to an error. The
// health watcher already bounds STARTING with readyTimeout; the extra margin
// here is a backstop against a stalled watcher.
func (svc *lmcacheService) waitForServerSettled(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	waitCtx, cancel := context.WithTimeout(ctx, svc.proc.readyTimeout+30*time.Second)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		st := svc.proc.Status()
		switch st.State {
		case LMCacheStateRunning:
			return nil
		case LMCacheStateError:
			if st.LastError != "" {
				return errors.New(st.LastError)
			}
			return errors.New("lmcache server failed to start")
		case LMCacheStateStopped:
			if st.LastError != "" {
				return errors.New(st.LastError)
			}
			return errors.New("lmcache server stopped during start")
		}
		select {
		case <-waitCtx.Done():
			stopErr := svc.stopServer()
			waitErr := fmt.Errorf("lmcache server did not become healthy in time: %w", waitCtx.Err())
			if stopErr != nil {
				return errors.Join(waitErr, fmt.Errorf("stop unhealthy lmcache server: %w", stopErr))
			}
			return waitErr
		case <-ticker.C:
		}
	}
}

// ensureServerRunning is the fail-closed model gate (singleflight): it
// returns nil only when the server is RUNNING and healthy. A healthy server
// is reused as-is (no second instance); otherwise one in-flight start is
// shared by every concurrent caller, and the result is the settled state of
// that single start. It never restarts a live server: the in-use restart
// guard belongs to the operator-facing startServer, not to the model path.
func (svc *lmcacheService) ensureServerRunning(ctx context.Context, cfg config.Config) error {
	if svc.isServerHealthy(cfg) {
		return nil
	}
	svc.startMu.Lock()
	if op := svc.startOp; op != nil {
		svc.startMu.Unlock()
		return op.wait(ctx)
	}
	op := &lmcacheStartOp{sig: make(chan struct{})}
	svc.startOp = op
	svc.startMu.Unlock()

	svc.publish("starting", 0.2, "starting lmcache server for a model start", "")
	err := svc.spawnServer(cfg)
	if err == nil {
		err = svc.waitForServerSettled(ctx)
	}
	if err != nil {
		svc.publish("error", 0, "", tailOutput(err.Error(), lmcacheOutputTail))
	} else {
		svc.publish("idle", 1, "lmcache server running", "")
	}
	op.finish(err)
	svc.startMu.Lock()
	if svc.startOp == op {
		svc.startOp = nil
	}
	svc.startMu.Unlock()
	return err
}

// spawnServer resolves the dedicated-venv executable, tears down any
// residual process, and spawns the server. It returns once the child is
// spawned; readiness is settled by the caller through waitForServerSettled.
func (svc *lmcacheService) spawnServer(cfg config.Config) error {
	executable, workDir, err := svc.lmcacheServerExecutable(cfg)
	if err != nil {
		// The module may have a current manifest while its console entrypoint
		// was removed or became unreadable. That is a failed start, not an
		// uninstalled module: keep the durable log/error visible and let the
		// operator repair or retry it.
		svc.proc.recordStartFailure(err.Error())
		return err
	}
	eff := cfg.LMCache.Server.Effective()
	args, err := lmcacheServerArgs(eff)
	if err != nil {
		svc.proc.recordStartFailure(err.Error())
		return err
	}
	if err := svc.prepareL3Runtime(eff); err != nil {
		svc.proc.recordStartFailure(err.Error())
		return err
	}
	logPath := filepath.Join(managedRuntimeRoot(cfg), lmcacheStateDir, "server.log")
	if stopErr := svc.stopServer(); stopErr != nil && svc.srv.proxylog != nil {
		svc.srv.proxylog.Warnf("lmcache server pre-start stop: %v", stopErr)
	}
	svc.spawns.Add(1)
	if err := svc.proc.Start(executable, args, workDir, logPath, svc.healthCheckURL(cfg)); err != nil {
		return err
	}
	// Every successful spawn — boot autostart, config reload, model-start
	// bring-up or an explicit operator start — restores the running state the
	// systemStopped flag describes.
	svc.systemStopped.Store(false)
	return nil
}

// startServer launches (or restarts) the standalone lmcache server with the
// configured effective parameters from the dedicated server venv. It returns
// once the process is spawned; the health watcher settles the state to
// RUNNING or ERROR.
func (svc *lmcacheService) startServer(cfg config.Config) error {
	// A start that would replace a live or starting server is a restart:
	// refuse it while models still use the server instead of dropping their
	// KV cache. A cold start (nothing running) is always allowed — it is the
	// recovery path after a crash.
	if st := svc.proc.Status(); st.State == LMCacheStateRunning || st.State == LMCacheStateStarting {
		if err := svc.requireNoUsers(cfg); err != nil {
			return err
		}
	}
	eff := cfg.LMCache.Server.Effective()
	executable, workDir, err := svc.lmcacheServerExecutable(cfg)
	if err != nil {
		svc.proc.recordStartFailure(err.Error())
		return err
	}
	args, err := lmcacheServerArgs(eff)
	if err != nil {
		svc.proc.recordStartFailure(err.Error())
		svc.publish("error", 0, "", err.Error())
		return err
	}
	if err := svc.prepareL3Runtime(eff); err != nil {
		svc.proc.recordStartFailure(err.Error())
		svc.publish("error", 0, "", err.Error())
		return err
	}
	logPath := filepath.Join(managedRuntimeRoot(cfg), lmcacheStateDir, "server.log")
	if stopErr := svc.stopServer(); stopErr != nil && svc.srv.proxylog != nil {
		svc.srv.proxylog.Warnf("lmcache server pre-start stop: %v", stopErr)
	}
	healthURL := fmt.Sprintf("http://%s:%d/healthcheck", eff.HTTPHost, eff.HTTPPort)
	svc.publish("starting", 0.5, "starting lmcache server from the dedicated server venv", "")
	if startErr := svc.proc.Start(executable, args, workDir, logPath, healthURL); startErr != nil {
		svc.publish("error", 0, "", startErr.Error())
		return startErr
	}
	// The start/restart/reload path spawns here rather than through
	// spawnServer; a successful spawn restores the running state the
	// systemStopped flag describes (see spawnServer).
	svc.systemStopped.Store(false)
	return nil
}

// startServerAndWait is the public lifecycle boundary: a start is successful
// only after the supervised process answers the real /healthcheck endpoint.
// Keeping the spawn and wait together prevents enable/start/restart APIs from
// returning a false success while the process is still importing torch. The
// health watcher owns only the process state, so this function also settles
// the progress stream: the last event the UI saw was this start's
// "starting" phase, and leaving it behind would pin the card's progress bar.
func (svc *lmcacheService) startServerAndWait(ctx context.Context, cfg config.Config) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := svc.startServer(cfg); err != nil {
		return err
	}
	if err := svc.waitForServerSettled(ctx); err != nil {
		svc.publish("error", 0, "", tailOutput(err.Error(), lmcacheOutputTail))
		return err
	}
	svc.publish("idle", 1, "lmcache server running", "")
	return nil
}

// stopServer terminates the standalone lmcache server if it is running.
func (svc *lmcacheService) stopServer() error {
	return svc.proc.Stop(lmcacheStopTimeout)
}

// restartServer stops the supervised server and starts it again so a pending
// restart-class configuration change is applied (D4). The HTTP handler
// enforces the dependency guard first; a successful restart clears the
// pending flag because the new process serves the new parameters. A failed
// start keeps the flag set so the UI keeps offering the retry.
func (svc *lmcacheService) restartServer(ctx context.Context, cfg config.Config) error {
	svc.publish("stopping", 0.2, "stopping lmcache server to apply the pending configuration", "")
	if err := svc.stopServer(); err != nil {
		return err
	}
	if err := svc.startServerAndWait(ctx, cfg); err != nil {
		return err
	}
	svc.pendingRestart.Store(false)
	return nil
}

// diagnoseStartError formats a server start or version-switch failure for API
// responses: the root cause plus the supervised server log path and its most
// recent lines, so a failure is diagnosable without leaving the UI (spec 25).
// A missing log file contributes nothing rather than an error of its own.
func (svc *lmcacheService) diagnoseStartError(err error, cfg config.Config) string {
	msg := err.Error()
	logPath := lmcacheServerLogPath(cfg)
	if tail := tailFile(logPath, lmcacheOutputTail); tail != "" {
		msg += "\nlog: " + logPath + "\n" + tail
	}
	return msg
}

// syncLMCacheModule reconciles the supervised server after a config reload.
// Restart-class changes (disable, parameter change) are applied immediately
// when no model uses the server; otherwise the change is marked
// pending-restart (D4) and the live process is left alone.
func (s *Server) syncLMCacheModule(active, desired config.Config) error {
	if s.lmcacheMod == nil {
		return nil
	}
	s.lmcacheMod.setReconcileConfig(&desired)
	defer s.lmcacheMod.setReconcileConfig(nil)
	ctx := context.Background()
	if s.shutdownCtx != nil {
		ctx = s.shutdownCtx
	}
	timeout := desired.RuntimeManager.OperationTimeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	desiredEff := desired.LMCache.Server.Effective()
	activeEff := active.LMCache.Server.Effective()
	proc := s.lmcacheMod.proc.Status()
	wasRunning := proc.Running
	targetVersion := strings.TrimSpace(desired.LMCache.EffectiveVersion())
	currentVersion := s.lmcacheMod.serverRuntimeVersion(desired)
	runtimeNeedsEnsure := currentVersion == "" || (targetVersion != "" && currentVersion != targetVersion)

	// A disabled server may still move the durable runtime pointer, but the
	// lifecycle hook must not start the new process as a side effect. Stop the
	// old process first, then let ensureServerRuntime update only the pointer.
	if !desired.LMCache.Server.Enabled {
		if proc.Running {
			if err := s.lmcacheMod.requireNoUsers(desired); err != nil {
				s.lmcacheMod.pendingRestart.Store(true)
				if s.proxylog != nil {
					s.proxylog.Warnf("lmcache server disabled in config but remains in use; stop deferred: %v", err)
				}
				return nil
			}
			if err := s.lmcacheMod.stopServer(); err != nil {
				return err
			}
			proc = s.lmcacheMod.proc.Status()
		}
		if runtimeNeedsEnsure {
			if _, err := s.lmcacheMod.ensureServerRuntime(ctx, desired); err != nil {
				return err
			}
		}
		s.lmcacheMod.pendingRestart.Store(false)
		s.lmcacheMod.systemStopped.Store(false)
		return nil
	}
	if !desired.LMCache.Enabled && !desired.LMCache.Server.Enabled {
		return nil
	}

	if runtimeNeedsEnsure && proc.Running {
		if err := s.lmcacheMod.requireNoUsers(desired); err != nil {
			s.lmcacheMod.pendingRestart.Store(true)
			if s.proxylog != nil {
				s.proxylog.Warnf("lmcache runtime version change deferred while in use: %v", err)
			}
			return nil
		}
		s.lmcacheMod.systemStopped.Store(true)
		if err := s.lmcacheMod.stopServer(); err != nil {
			return err
		}
		proc = s.lmcacheMod.proc.Status()
	}
	if runtimeNeedsEnsure {
		if _, err := s.lmcacheMod.ensureServerRuntime(ctx, desired); err != nil {
			return err
		}
		proc = s.lmcacheMod.proc.Status()
	}
	if !proc.Running {
		// A previously running server is restarted even when autoStart is
		// false: that setting governs the daemon boot hook, not reloads. A
		// reload starts a stopped server when this change newly enabled it,
		// or when the daemon itself stopped it for a previous apply and the
		// start did not come back (a failed restart must not leave the
		// service down) — it must never resurrect one the operator
		// explicitly stopped.
		newlyEnabled := !active.LMCache.Enabled || !active.LMCache.Server.Enabled
		if wasRunning || newlyEnabled || s.lmcacheMod.systemStopped.Load() {
			if err := s.lmcacheMod.startServerAndWait(ctx, desired); err != nil {
				return err
			}
		}
		return nil
	}
	if !serverConfigsEqual(activeEff, desiredEff) {
		if err := s.lmcacheMod.requireNoUsers(desired); err != nil {
			s.lmcacheMod.pendingRestart.Store(true)
			if s.proxylog != nil {
				s.proxylog.Warnf("lmcache server parameters changed but remain in use; restart deferred: %v", err)
			}
			return nil
		}
		// The stop is the daemon's own doing, so a reload must bring the
		// server back if the start below fails; the pending flag stays set on
		// failure so the UI keeps offering the retry.
		s.lmcacheMod.systemStopped.Store(true)
		if err := s.lmcacheMod.stopServer(); err != nil {
			return err
		}
		if err := s.lmcacheMod.startServerAndWait(ctx, desired); err != nil {
			s.lmcacheMod.pendingRestart.Store(true)
			return err
		}
		s.lmcacheMod.pendingRestart.Store(false)
		return nil
	}
	return nil
}

// serverConfigsEqual compares effective server configurations; every field
// is restart-class (D4), so plain struct equality is the comparison — new
// parameters join the restart-required group by being added to the struct.
func serverConfigsEqual(a, b config.LMCacheServerConfig) bool {
	return a == b
}

// handleAPILMCacheStatus reports the module state.
func (s *Server) handleAPILMCacheStatus(w http.ResponseWriter, r *http.Request) {
	status := s.lmcacheMod.status(s.currentConfig())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

// handleAPILMCacheCheck performs the explicit, side-effect-free lifecycle
// check: verify the current artifact, probe the actual running server, and
// query the configured package metadata endpoint. It never stages, activates,
// removes, or changes a pointer. A failed check still returns the complete
// status object so the UI can render the durable error and log path.
func (s *Server) handleAPILMCacheCheck(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	if s.lmcacheMod == nil || s.runtime == nil {
		swaputil.SendResponse(w, r, http.StatusServiceUnavailable, "lmcache runtime is unavailable")
		return
	}
	ctx, cancel := s.runtimeOperationContext(r)
	defer cancel()
	var checkErrors []error
	if runtimeStatus, ok := s.runtime.Get(config.LMCacheRuntimeName); ok && runtimeStatus.Current != "" {
		if _, err := s.runtime.Check(ctx, config.LMCacheRuntimeName); err != nil {
			checkErrors = append(checkErrors, fmt.Errorf("current artifact: %w", err))
		}
	}
	if proc := s.lmcacheMod.proc.Status(); proc.Running && !s.lmcacheMod.isServerHealthy(cfg) {
		const errMessage = "lmcache server /healthcheck is not healthy"
		s.lmcacheMod.proc.recordHealth(false)
		checkErrors = append(checkErrors, errors.New(errMessage))
	}
	if _, configured := s.runtime.Definition(config.LMCacheRuntimeName); configured {
		if _, _, err := s.runtime.CheckForUpdate(ctx, config.LMCacheRuntimeName); err != nil {
			checkErrors = append(checkErrors, fmt.Errorf("remote version metadata: %w", err))
		}
	}
	status := s.lmcacheMod.status(cfg)
	if err := errors.Join(checkErrors...); err != nil {
		status.Error = err.Error()
		if status.Update.LastError == "" {
			status.Update.LastError = err.Error()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

func (s *Server) handleAPILMCacheEnable(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	ctx, cancel := s.stageOperationContext()
	defer cancel()
	if err := s.lmcacheMod.enable(ctx, cfg); err != nil {
		if errors.Is(err, errLMCacheBusy) || errors.Is(err, errLMCacheInUse) {
			swaputil.SendResponse(w, r, http.StatusConflict, err.Error())
			return
		}
		swaputil.SendResponse(w, r, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.lmcacheMod.status(cfg))
}

func (s *Server) handleAPILMCacheDisable(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	ctx, cancel := s.stageOperationContext()
	defer cancel()
	if err := s.lmcacheMod.disable(ctx, cfg); err != nil {
		if errors.Is(err, errLMCacheBusy) {
			swaputil.SendResponse(w, r, http.StatusConflict, err.Error())
			return
		}
		if errors.Is(err, errLMCacheInUse) {
			swaputil.SendResponse(w, r, http.StatusConflict, err.Error())
			return
		}
		swaputil.SendResponse(w, r, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.lmcacheMod.status(cfg))
}

func (s *Server) handleAPILMCacheServerStart(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	ctx, cancel := s.runtimeOperationContext(r)
	defer cancel()
	if err := s.lmcacheMod.startServerAndWait(ctx, cfg); err != nil {
		if errors.Is(err, errLMCacheInUse) || errors.Is(err, runtimeManager.ErrRuntimeBusy) {
			swaputil.SendResponse(w, r, http.StatusConflict, err.Error())
			return
		}
		swaputil.SendResponse(w, r, http.StatusBadGateway, s.lmcacheMod.diagnoseStartError(err, cfg))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAPILMCacheServerStop(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	if err := s.lmcacheMod.requireNoUsers(cfg); err != nil {
		swaputil.SendResponse(w, r, http.StatusConflict, err.Error())
		return
	}
	if err := s.lmcacheMod.stopServer(); err != nil {
		swaputil.SendResponse(w, r, http.StatusBadGateway, err.Error())
		return
	}
	// An operator stop must survive later config reloads untouched.
	s.lmcacheMod.systemStopped.Store(false)
	w.WriteHeader(http.StatusNoContent)
}

// handleAPILMCacheServerRestart stops and restarts the standalone server so a
// pending config change is applied. Restarting drops the in-memory KV cache,
// so it is refused while any model references the server (409 with the model
// list) — the same guard every destructive path uses. A cold start (no
// running server) is allowed: it is the crash-recovery path.
func (s *Server) handleAPILMCacheServerRestart(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	if s.lmcacheMod == nil {
		swaputil.SendResponse(w, r, http.StatusServiceUnavailable, "lmcache module is not available")
		return
	}
	if err := s.lmcacheMod.requireNoUsers(cfg); err != nil {
		swaputil.SendResponse(w, r, http.StatusConflict, err.Error())
		return
	}
	ctx, cancel := s.runtimeOperationContext(r)
	defer cancel()
	if err := s.lmcacheMod.restartServer(ctx, cfg); err != nil {
		if errors.Is(err, errLMCacheInUse) {
			swaputil.SendResponse(w, r, http.StatusConflict, err.Error())
			return
		}
		swaputil.SendResponse(w, r, http.StatusBadGateway, s.lmcacheMod.diagnoseStartError(err, cfg))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// lmcacheUpdateRequest is the body of /api/lmcache/update. The action selects
// one step of the versioned update flow; version is required for activate and
// optional for stage (omitted resolves the configured pin or the newest
// published release).
type lmcacheUpdateRequest struct {
	Action  string `json:"action"` // upgrade | stage | activate | rollback
	Version string `json:"version"`
}

// handleAPILMCacheUpdate drives one step of the versioned LMCache server
// update: stage a candidate without applying it, activate a staged version,
// or roll back to the previous one. Every step is guarded by the same
// dependency check the update hooks use — while a model references the server
// the operation is refused (409) and the serving process is untouched. The
// manager emits progress on the lmcache stream, which the UI's card renders.
func (s *Server) handleAPILMCacheUpdate(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	if s.lmcacheMod == nil {
		swaputil.SendResponse(w, r, http.StatusServiceUnavailable, "lmcache module is not available")
		return
	}
	if s.runtime == nil {
		swaputil.SendResponse(w, r, http.StatusServiceUnavailable, "runtime manager is unavailable")
		return
	}
	if err := s.lmcacheMod.requireNoUsers(cfg); err != nil {
		swaputil.SendResponse(w, r, http.StatusConflict, err.Error())
		return
	}
	var req lmcacheUpdateRequest
	if err := decodeJSONBody(w, r, &req, 4096); err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "invalid lmcache update request")
		return
	}
	action := strings.ToLower(strings.TrimSpace(req.Action))
	if action == "upgrade" && !s.runtimeIdle(config.LMCacheRuntimeName) {
		err := fmt.Errorf("%w: lmcache runtime is busy", runtimeManager.ErrRuntimeBusy)
		swaputil.SendResponse(w, r, runtimeOperationStatus(err, http.StatusConflict), err.Error())
		return
	}
	switch action {
	case "upgrade":
		// The one-click path deliberately uses the same stage and activation
		// primitives as the compatibility APIs. The long operation is detached
		// from the request, while the response is emitted only after the new
		// server has passed its real health check.
		ctx, cancel := s.stageOperationContext()
		defer cancel()
		version, err := s.lmcacheMod.stageUpdate(ctx, cfg, strings.TrimSpace(req.Version))
		if err != nil {
			swaputil.SendResponse(w, r, runtimeOperationStatus(err, http.StatusBadGateway), s.lmcacheMod.diagnoseStartError(err, cfg))
			return
		}
		if err := s.runtime.Activate(ctx, config.LMCacheRuntimeName, version); err != nil {
			swaputil.SendResponse(w, r, runtimeOperationStatus(err, http.StatusBadGateway), s.lmcacheMod.diagnoseStartError(err, cfg))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.lmcacheMod.status(cfg))
	case "stage":
		// Stage is the long step: it must outlive a disconnecting client the
		// same way every other stage operation does.
		ctx, cancel := s.stageOperationContext()
		version, err := s.lmcacheMod.stageUpdate(ctx, cfg, strings.TrimSpace(req.Version))
		cancel()
		if err != nil {
			swaputil.SendResponse(w, r, runtimeOperationStatus(err, http.StatusBadGateway), err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"staged": version})
	case "activate":
		version := strings.TrimSpace(req.Version)
		if version == "" {
			swaputil.SendResponse(w, r, http.StatusBadRequest, "version is required to activate")
			return
		}
		ctx, cancel := s.runtimeOperationContext(r)
		defer cancel()
		if err := s.runtime.Activate(ctx, config.LMCacheRuntimeName, version); err != nil {
			swaputil.SendResponse(w, r, runtimeOperationStatus(err, http.StatusConflict), s.lmcacheMod.diagnoseStartError(err, cfg))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case "rollback":
		ctx, cancel := s.runtimeOperationContext(r)
		defer cancel()
		if err := s.runtime.Rollback(ctx, config.LMCacheRuntimeName); err != nil {
			swaputil.SendResponse(w, r, runtimeOperationStatus(err, http.StatusConflict), err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		swaputil.SendResponse(w, r, http.StatusBadRequest, "action must be upgrade, stage, activate, or rollback")
	}
}

// lmcacheRuntimeOperationGuard refuses the generic runtime operations
// (stage/activate/rollback) on the derived LMCache server runtime while a
// model still uses the server. The derived runtime is not user-configurable,
// so only the reserved name is affected; nil service (tests) is a no-op.
func (s *Server) lmcacheRuntimeOperationGuard(cfg config.Config, name string) error {
	if s.lmcacheMod == nil || name != config.LMCacheRuntimeName {
		return nil
	}
	return s.lmcacheMod.requireNoUsers(cfg)
}

// installModelTracking wires the LMCache reference tracker into the local
// router: every LMCache-enabled model process acquires a reference right
// before it spawns (pre-start hook), and the process state stream releases
// it when the model leaves the running states (releaseLMCacheReference).
// Runs once at boot, before any model can start.
func (s *Server) installModelTracking() {
	if s.local == nil {
		return
	}
	if dec, ok := s.local.(interface {
		SetProcessDecorator(func(modelID string, p *process.ProcessCommand))
	}); ok {
		dec.SetProcessDecorator(s.decorateModelProcess)
	}
}

// decorateModelProcess installs the pre-start dependency gate on
// LMCache-enabled models only; other models are left untouched. A model
// whose config later stops enabling LMCache loses the gate the next time its
// process is replaced.
func (s *Server) decorateModelProcess(modelID string, p *process.ProcessCommand) {
	// Keep the crash recorder independent from the optional LMCache gate: every
	// managed inference process should leave a bounded diagnostic snapshot when
	// it exits unexpectedly.
	p.SetCrashRecorder(func(id string, processLog []byte, exitErr error) {
		go s.recordInferenceCrash(id, processLog, exitErr)
	})

	cfg := s.currentConfig()
	modelCfg, _, ok := cfg.FindConfig(modelID)
	if !ok || modelCfg.Backend.LMCache == nil || !modelCfg.Backend.LMCache.Enabled {
		return
	}
	p.SetPreStartHook(s.lmcacheMod.ensureReadyForModel(modelID))
}

// ensureReadyForModel builds the pre-start gate for one LMCache-enabled
// model: the server must be RUNNING and healthy before the upstream spawns,
// and the reference is acquired only after that (so a stop decision made
// during startup already sees this model). Every failure is returned to the
// model start — there is no fallback to a plain start.
//
// A non-standalone (inProcess) model is different by design: the LMCache
// library runs inside its own vLLM process, so there is no server to keep
// running and no shared dependency to guard. Only the connector in the
// model's venv has to be ready, and no server reference is held.
func (svc *lmcacheService) ensureReadyForModel(modelID string) func(context.Context) error {
	return func(ctx context.Context) error {
		cfg := svc.srv.currentConfig()
		modelCfg, _, ok := cfg.FindConfig(modelID)
		if !ok || modelCfg.Backend.LMCache == nil || !modelCfg.Backend.LMCache.Enabled {
			return nil
		}
		if modelCfg.Backend.LMCache.EffectiveMode() == config.LMCacheModeInProcess {
			if err := svc.ensureConnectorReady(ctx, cfg, modelID); err != nil {
				return fmt.Errorf("LMCache is not ready, so the model cannot start: %w", err)
			}
			return nil
		}
		if !cfg.LMCache.Server.Enabled {
			return errors.New("the model requires LMCache but the LMCache server is disabled (lmcache.server.enabled is false)")
		}
		// The first model start is also an installation entry point. Ensure the
		// durable LMCache runtime before acquiring the live model reference so
		// the runtime idle probe does not reject the bootstrap activation merely
		// because this model is waiting for it.
		if _, err := svc.ensureServerRuntime(ctx, cfg); err != nil {
			return svc.modelStartError(cfg, err)
		}
		// Acquire before waiting on the server: a stop decision made while
		// this model is starting must already see it, and the auto-stop must
		// not fire while a start still depends on the server.
		svc.users.acquire(modelID)
		if err := svc.ensureServerRunning(ctx, cfg); err != nil {
			svc.users.release(modelID)
			return svc.modelStartError(cfg, err)
		}
		if err := svc.ensureConnectorReady(ctx, cfg, modelID); err != nil {
			svc.users.release(modelID)
			return fmt.Errorf("LMCache is not ready, so the model cannot start: %w", err)
		}
		return nil
	}
}

// ensureConnectorReady guarantees that the model's own vLLM venv contains the
// lmcache connector before the model spawns: MP mode imports the connector
// inside the vLLM process and talks a versioned protocol to the server, so a
// missing or drifted package must be replaced, never left behind. The check is
// a fast import probe; only a mismatch triggers an install. Concurrent starts
// that share one venv share one install. This is the "the model owns how it
// uses LMCache" boundary: enabling the module installs the server runtime, and
// the connector follows the models that need it.
//
// MP models pin the connector to the server's current version. A
// non-standalone (inProcess) model has no server to pin against: it uses the
// module's configured version when one is set, and otherwise the newest
// published release, which uv resolves as an unpinned install.
func (svc *lmcacheService) ensureConnectorReady(ctx context.Context, cfg config.Config, modelID string) error {
	modelCfg, _, ok := cfg.FindConfig(modelID)
	if !ok || modelCfg.Backend.LMCache == nil || !modelCfg.Backend.LMCache.Enabled {
		// Config changed after the hook was decorated; nothing to guarantee.
		return nil
	}
	binDir, err := lmcacheModelVenvBinDir(managedRuntimeRoot(cfg), &modelCfg)
	if err != nil {
		return err
	}
	connectorVersion := svc.serverRuntimeVersion(cfg)
	if connectorVersion == "" {
		if modelCfg.Backend.LMCache.EffectiveMode() != config.LMCacheModeInProcess {
			return errors.New("the LMCache server runtime is not installed; enable the LMCache module first")
		}
		// No server runtime: the module's explicit pin (if any) is the only
		// version the operator asked for; an empty pin installs the newest
		// published release.
		connectorVersion = strings.TrimSpace(cfg.LMCache.EffectiveVersion())
	}
	// Fast path without the singleflight slot: the package is already the
	// expected version in this venv.
	existing, probeErr := probeLMCacheVersion(ctx, binDir)
	if probeErr != nil {
		return fmt.Errorf("probe lmcache in the model's vLLM venv: %w", probeErr)
	}
	if connectorVersion == "" || existing == connectorVersion {
		return nil
	}
	runtimeName := strings.TrimSpace(modelCfg.Backend.Runtime)
	return svc.installConnector(ctx, cfg, binDir, runtimeName, connectorVersion)
}

// installConnector installs (or replaces) the lmcache connector in one venv,
// pinned to the server's current version. Concurrent callers for the same
// venv share one uv run (singleflight per venv); callers for different venvs
// run in parallel. The install outlives the model start that triggered it
// (see runConnectorInstall): a caller whose start budget expires mid-install
// gets a retryable error while the shared install keeps running.
func (svc *lmcacheService) installConnector(ctx context.Context, cfg config.Config, binDir, runtimeName, serverVersion string) error {
	svc.connectorMu.Lock()
	if op, ok := svc.connectorOps[binDir]; ok {
		svc.connectorMu.Unlock()
		return op.wait(ctx)
	}
	if svc.connectorOps == nil {
		svc.connectorOps = make(map[string]*lmcacheConnectorOp)
	}
	op := &lmcacheConnectorOp{sig: make(chan struct{})}
	svc.connectorOps[binDir] = op
	svc.connectorMu.Unlock()

	go func() {
		err := svc.runConnectorInstall(cfg, binDir, runtimeName, serverVersion)
		op.finish(err)
		svc.connectorMu.Lock()
		if svc.connectorOps[binDir] == op {
			delete(svc.connectorOps, binDir)
		}
		svc.connectorMu.Unlock()
	}()
	return op.wait(ctx)
}

// runConnectorInstall performs the actual uv install of the connector into
// one venv and re-probes to verify the pinned version landed. A configured
// index URL must pass the source allowlist like every other LMCache source.
// An empty version installs the newest published release (a non-standalone
// model with no server runtime to pin against), so the exact-version
// verification is skipped for it.
//
// The install is deliberately not scoped to the model start that triggered
// it: that start is bounded by the model's health-check timeout, which a
// first-time install over slow egress can legitimately exceed. Killing uv at
// that point would doom every following start to the same cutoff, so the
// install is owned by the daemon's shutdown context and bounded by
// lmcacheConnectorInstallTimeout instead; callers only wait as long as their
// own budget allows.
func (svc *lmcacheService) runConnectorInstall(cfg config.Config, binDir, runtimeName, version string) error {
	parent := context.Background()
	if svc.srv != nil && svc.srv.shutdownCtx != nil {
		parent = svc.srv.shutdownCtx
	}
	installCtx, cancel := context.WithTimeout(parent, lmcacheConnectorInstallTimeout)
	defer cancel()
	module := cfg.LMCache
	packageSpec := module.PackageName()
	pinned := strings.TrimSpace(version)
	if pinned != "" {
		packageSpec += "==" + pinned
	}
	args := []string{"pip", "install", "--python", filepath.Join(binDir, "python")}
	if indexURL := strings.TrimSpace(module.IndexURL); indexURL != "" {
		if !runtimeManager.ValidSourceURL(indexURL, cfg.RuntimeManager.SourceAllowlist) {
			return fmt.Errorf("lmcache.indexURL %q is not allowed by runtimeManager.sourceAllowlist", indexURL)
		}
		args = append(args, "--index-url", indexURL)
	}
	args = append(args, packageSpec)
	svc.publish("installing", 0.3, fmt.Sprintf("installing %s into %s (first start of a LMCache model)", packageSpec, runtimeName), "")
	output, runErr := svc.runCommand(installCtx, "uv", args...)
	if runErr != nil {
		diagnostic := tailOutput(output, lmcacheOutputTail)
		svc.publish("error", 0, "", fmt.Sprintf("connector install into %s failed: %s", runtimeName, diagnostic))
		return fmt.Errorf("install lmcache connector into %s: %v: %s", runtimeName, runErr, diagnostic)
	}
	verified, probeErr := probeLMCacheVersion(installCtx, binDir)
	if probeErr != nil {
		return fmt.Errorf("verify lmcache in %s: %w", runtimeName, probeErr)
	}
	if pinned != "" && verified != pinned {
		return fmt.Errorf("lmcache %q is not the expected version %s after install into %s", verified, pinned, runtimeName)
	}
	svc.publish("idle", 1, fmt.Sprintf("LMCache connector ready in %s", runtimeName), "")
	return nil
}

// maybeAutoStop stops the server when the last referencing model has gone
// and the operator opted into auto-stop (default: off, cache stays warm).
// It runs off the event stream; the stop happens in its own goroutine under
// the start singleflight slot, so an in-flight model-started start is never
// torn down mid-spawn.
func (svc *lmcacheService) maybeAutoStop() {
	if !svc.srv.currentConfig().LMCache.EffectiveAutoStop() {
		return
	}
	go func() {
		svc.startMu.Lock()
		defer svc.startMu.Unlock()
		if len(svc.users.users()) > 0 || svc.startOp != nil {
			return
		}
		svc.publish("stopping", 0.5, "auto-stopping lmcache server: no models are using it", "")
		if err := svc.stopServer(); err != nil && svc.srv.proxylog != nil {
			svc.srv.proxylog.Warnf("lmcache auto-stop: %v", err)
		} else {
			svc.publish("idle", 1, "lmcache server stopped (autoStop)", "")
		}
	}()
}

// modelStartError enriches a failed gate with the server's last error, the
// tail of its log, and the log path, so the model start failure tells the
// operator where to look. It never hides the cause.
func (svc *lmcacheService) modelStartError(cfg config.Config, cause error) error {
	detail := ""
	if st := svc.proc.Status(); st.LastError != "" {
		detail += "; server: " + st.LastError
	}
	logPath := filepath.Join(managedRuntimeRoot(cfg), lmcacheStateDir, "server.log")
	if tail := tailFile(logPath, lmcacheOutputTail); tail != "" {
		detail += "; log tail: " + tail
	}
	return fmt.Errorf("LMCache is not ready, so the model cannot start: %w%s (log: %s)", cause, detail, logPath)
}

// releaseLMCacheReference drops a model's LMCache reference when it leaves
// the running states. Idempotent; invoked from the process state event
// stream on every transition.
func (s *Server) releaseLMCacheReference(change swaputil.ProcessStateChangeEvent) {
	if s.lmcacheMod == nil {
		return
	}
	model := strings.TrimSpace(change.ProcessName)
	if model == "" {
		return
	}
	switch process.ProcessState(change.NewState) {
	case process.StateStopped, process.StateShutdown:
		if s.lmcacheMod.users.release(model) {
			s.lmcacheMod.maybeAutoStop()
		}
	}
}
