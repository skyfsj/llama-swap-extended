package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	runtimeManager "github.com/mostlygeek/llama-swap/internal/runtime"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// lmcacheFixtureServerVersion is the version pre-staged in the dedicated
// server runtime of the service fixture.
const lmcacheFixtureServerVersion = "0.5.4"

// fakeVenvPython is a stand-in venv interpreter. It reports the lmcache
// version stored in a marker file next to the interpreter, so tests can set
// whatever installed version they need without a real package.
const fakeVenvPython = `#!/bin/sh
bin=$(dirname "$0")
if [ -f "$bin/.lmcache_version" ]; then
	cat "$bin/.lmcache_version"
	exit 0
fi
exit 1
`

// fakeLMCacheServer is a stand-in for the lmcache console script. It records
// its own resolved path (to prove which venv actually ran) and then idles,
// so the supervised process stays alive for the test.
func fakeLMCacheServerScript(marker string) string {
	return "#!/bin/sh\necho \"$0\" > " + marker + "\nexec sleep 30\n"
}

// lmcacheFakeUV records uv invocations and simulates install/uninstall by
// writing the requested version pin into the venv marker file. slow delays
// the simulated install so concurrent callers can observe the in-flight op.
type lmcacheFakeUV struct {
	calls []string
	fail  bool
	slow  time.Duration
}

func (f *lmcacheFakeUV) run(ctx context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if f.slow > 0 {
		select {
		case <-time.After(f.slow):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	uninstall := false
	version := ""
	for _, arg := range args {
		if arg == "uninstall" {
			uninstall = true
		}
		if v, ok := strings.CutPrefix(arg, "lmcache=="); ok && v != "" {
			version = v
		}
	}
	for i, arg := range args {
		if arg == "--python" && i+1 < len(args) && !f.fail {
			marker := filepath.Join(filepath.Dir(args[i+1]), ".lmcache_version")
			if uninstall {
				_ = os.Remove(marker)
			} else {
				_ = os.WriteFile(marker, []byte(version), 0o600)
			}
		}
	}
	if f.fail {
		return "simulated uv failure", errors.New("simulated uv failure")
	}
	return "ok", nil
}

// writeActiveLMCacheRuntime stages the derived server runtime at the given
// version: manifest, current pointer, and the optional fake console script.
// It returns the console script path.
func writeActiveLMCacheRuntime(t *testing.T, root, version, script string) string {
	t.Helper()
	writeRuntimeVersion(t, root, config.LMCacheRuntimeName, runtimeManager.Manifest{
		Name: config.LMCacheRuntimeName, Version: version, Kind: "lmcache", Source: "pypi",
		Metadata: map[string]string{"mode": runtimeManager.RuntimeModeNative, "sourceType": "pypi"},
	})
	runtimeDir := filepath.Join(root, config.LMCacheRuntimeName)
	currentPath := filepath.Join(runtimeDir, "current")
	// Re-staging the same version (fixture plus test override) replaces the
	// pointer instead of failing on the existing symlink.
	_ = os.Remove(currentPath)
	if err := os.Symlink(filepath.Join("versions", version), currentPath); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(runtimeDir, "versions", version, ".venv", "bin")
	if err := os.MkdirAll(binDir, 0o750); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(binDir, "lmcache")
	if script != "" {
		if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return scriptPath
}

// lmcacheServiceFixture stages two active vLLM runtimes with fake venv
// interpreters, a pre-staged dedicated lmcache server runtime, and wires a
// fresh LMCache service onto a bare server.
func lmcacheServiceFixture(t *testing.T) (*Server, config.Config, *lmcacheFakeUV, string) {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"vllm-a", "vllm-b"} {
		writeActiveVLLMRuntime(t, root, name, "1", "0.19.0")
		binDir := filepath.Join(root, name, "versions", "1", ".venv", "bin")
		if err := os.MkdirAll(binDir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(binDir, "python"), []byte(fakeVenvPython), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeActiveLMCacheRuntime(t, root, lmcacheFixtureServerVersion, "#!/bin/sh\nexec sleep 30\n")
	cfg := config.Config{
		RuntimeManager: config.RuntimeManagerConfig{Root: root},
		Runtimes: map[string]config.RuntimeConfig{
			"vllm-a": {Kind: "vllm"},
			"vllm-b": {Kind: "vllm"},
		},
		Models: map[string]config.ModelConfig{
			"model": {Backend: config.BackendConfig{
				Type: "vllm", Runtime: "vllm-a",
				Arguments: []string{"vllm", "serve", "model"},
				LMCache:   &config.LMCacheModelConfig{Enabled: true},
			}},
		},
	}
	runner := &lmcacheFakeUV{}
	s := &Server{}
	s.setConfig(cfg)
	svc := newLMCacheService(s)
	svc.runCommand = runner.run
	s.lmcacheMod = svc
	return s, cfg, runner, root
}

// lmcacheVenvPython resolves the fake interpreter path of one fixture runtime.
func lmcacheVenvPython(root, name string) string {
	return filepath.Join(root, name, "versions", "1", ".venv", "bin", "python")
}

func writeVenvVersion(t *testing.T, root, name, version string) {
	t.Helper()
	marker := filepath.Join(root, name, "versions", "1", ".venv", "bin", ".lmcache_version")
	if err := os.WriteFile(marker, []byte(version), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestLMCache_EnableInstallsServerRuntimeOnly pins the decoupled enable:
// the module install only stages the dedicated server runtime and never
// touches a vLLM venv. The connector is the model's pre-start gate's job.
func TestLMCache_EnableInstallsServerRuntimeOnly(t *testing.T) {
	s, cfg, runner, _ := lmcacheServiceFixture(t)

	if err := s.lmcacheMod.enable(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("uv calls = %v, want none (enable must not touch vLLM venvs)", runner.calls)
	}

	st := s.lmcacheMod.status(cfg)
	// installed/version describe the dedicated server runtime, not the
	// per-venv connector probes.
	if !st.Installed || st.Version != lmcacheFixtureServerVersion {
		t.Fatalf("status installed/version = %v/%q, want the server runtime", st.Installed, st.Version)
	}
	if len(st.EnabledModels) != 1 || st.EnabledModels[0] != "model" {
		t.Fatalf("enabledModels = %v", st.EnabledModels)
	}
}

func TestLMCache_EnableFailsWithoutServerRuntime(t *testing.T) {
	s, cfg, runner, root := lmcacheServiceFixture(t)
	// No current pointer: the dedicated server venv is missing.
	if err := os.Remove(filepath.Join(root, config.LMCacheRuntimeName, "current")); err != nil {
		t.Fatal(err)
	}

	err := s.lmcacheMod.enable(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "lmcache server runtime") {
		t.Fatalf("enable() = %v, want fail-closed server runtime error", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("uv ran despite missing server runtime: %v", runner.calls)
	}
}

func TestLMCache_EnableFailureClearsBusy(t *testing.T) {
	s, cfg, runner, root := lmcacheServiceFixture(t)
	// A failed enable (missing server runtime, no manager to stage into)
	// must release the busy flag so a retry is possible.
	if err := os.Remove(filepath.Join(root, config.LMCacheRuntimeName, "current")); err != nil {
		t.Fatal(err)
	}

	err := s.lmcacheMod.enable(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "lmcache server runtime") {
		t.Fatalf("enable() = %v, want fail-closed server runtime error", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("uv ran despite missing server runtime: %v", runner.calls)
	}
	if st := s.lmcacheMod.status(cfg); st.Installing {
		t.Fatalf("status = %+v, want installing cleared after failure", st)
	}
	// The busy flag is released, so a retry is possible once the runtime
	// exists again.
	writeActiveLMCacheRuntime(t, root, lmcacheFixtureServerVersion, "#!/bin/sh\nexec sleep 30\n")
	if err := s.lmcacheMod.enable(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
}

func TestLMCache_EnableWithoutActiveVenvInstallsServerOnly(t *testing.T) {
	// No vLLM runtime at all: enable must still succeed, installing only the
	// dedicated server runtime.
	root := t.TempDir()
	writeActiveLMCacheRuntime(t, root, lmcacheFixtureServerVersion, "")
	cfg := config.Config{
		RuntimeManager: config.RuntimeManagerConfig{Root: root},
		Runtimes:       map[string]config.RuntimeConfig{"llama": {Kind: "llamacpp"}},
	}
	s := &Server{}
	s.setConfig(cfg)
	runner := &lmcacheFakeUV{}
	svc := newLMCacheService(s)
	svc.runCommand = runner.run
	s.lmcacheMod = svc

	if err := s.lmcacheMod.enable(context.Background(), cfg); err != nil {
		t.Fatalf("enable() = %v, want success without any vLLM runtime", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("uv ran despite no vLLM runtime: %v", runner.calls)
	}
	st := s.lmcacheMod.status(cfg)
	if !st.Installed || st.Version != lmcacheFixtureServerVersion {
		t.Fatalf("status = %+v, want the server runtime installed", st)
	}
	if len(st.Runtimes) != 0 {
		t.Fatalf("status runtimes = %+v, want none without vLLM runtimes", st.Runtimes)
	}
}

func TestLMCache_EnableBusyReturnsConflict(t *testing.T) {
	s, cfg, _, _ := lmcacheServiceFixture(t)
	s.lmcacheMod.installBusy.Store(true)
	defer s.lmcacheMod.installBusy.Store(false)

	if err := s.lmcacheMod.enable(context.Background(), cfg); !errors.Is(err, errLMCacheBusy) {
		t.Fatalf("enable() = %v, want %v", err, errLMCacheBusy)
	}
	if err := s.lmcacheMod.disable(context.Background(), cfg); !errors.Is(err, errLMCacheBusy) {
		t.Fatalf("disable() = %v, want %v", err, errLMCacheBusy)
	}
}

func TestLMCache_APIEnableReturns409WhenBusy(t *testing.T) {
	s, _, _, _ := lmcacheServiceFixture(t)
	s.lmcacheMod.installBusy.Store(true)
	defer s.lmcacheMod.installBusy.Store(false)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/lmcache/enable", nil)
	s.handleAPILMCacheEnable(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body = %s, want 409", rec.Code, rec.Body.String())
	}
}

func TestLMCache_DisableUninstallsFromAllVenvs(t *testing.T) {
	s, cfg, runner, root := lmcacheServiceFixture(t)
	writeVenvVersion(t, root, "vllm-a", lmcacheFixtureServerVersion)
	writeVenvVersion(t, root, "vllm-b", lmcacheFixtureServerVersion)

	if err := s.lmcacheMod.disable(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"uv pip uninstall --python " + lmcacheVenvPython(root, "vllm-a") + " -y lmcache",
		"uv pip uninstall --python " + lmcacheVenvPython(root, "vllm-b") + " -y lmcache",
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("uv calls = %v, want %v", runner.calls, want)
	}
	// disable uninstalls the connector from the venvs but keeps the dedicated
	// server runtime, so installed stays true while the venv probes go clean.
	st := s.lmcacheMod.status(cfg)
	if !st.Installed || st.AllInstalled || len(st.Runtimes) != 2 {
		t.Fatalf("status = %+v, want the server runtime installed and the venvs clean", st)
	}
	for _, entry := range st.Runtimes {
		if entry.Installed || entry.Version != "" {
			t.Fatalf("runtime %s = %+v, want the connector uninstalled", entry.Name, entry)
		}
	}
}

func TestLMCache_DisableRefusedWhileModelsUseIt(t *testing.T) {
	s, cfg, runner, _ := lmcacheServiceFixture(t)
	s.local = &runtimeSwitchRouter{running: map[string]process.ProcessState{
		"model": process.StateReady,
	}}

	err := s.lmcacheMod.disable(context.Background(), cfg)
	if !errors.Is(err, errLMCacheInUse) {
		t.Fatalf("disable() = %v, want %v", err, errLMCacheInUse)
	}
	if !strings.Contains(err.Error(), "model") {
		t.Fatalf("disable() error = %q, want the in-use model listed", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("uv ran despite in-use guard: %v", runner.calls)
	}
}

func TestLMCache_ServerStopAPIRefusedWhileModelUsesIt(t *testing.T) {
	s, cfg, _, root := lmcacheServiceFixture(t)
	_ = writeActiveLMCacheRuntime(t, root, lmcacheFixtureServerVersion, "#!/bin/sh\nexec sleep 30\n")
	cfg.LMCache.Server = config.LMCacheServerConfig{Enabled: true}
	s.local = &runtimeSwitchRouter{running: map[string]process.ProcessState{
		"model": process.StateReady,
	}}

	if err := s.lmcacheMod.startServer(cfg); err != nil {
		t.Fatal(err)
	}
	defer s.lmcacheMod.stopServer()
	if got := s.lmcacheMod.proc.Status(); !got.Running {
		t.Fatalf("process status = %+v, want running", got)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/lmcache/server/stop", nil)
	s.handleAPILMCacheServerStop(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body = %s, want 409", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model") {
		t.Fatalf("body = %s, want the in-use model listed", rec.Body.String())
	}
	if got := s.lmcacheMod.proc.Status(); !got.Running {
		t.Fatalf("process status after refused stop = %+v, want still running", got)
	}
}

func TestLMCache_ServerStartStopSupervisesProcess(t *testing.T) {
	s, cfg, _, root := lmcacheServiceFixture(t)
	marker := filepath.Join(root, "launched-by")
	_ = writeActiveLMCacheRuntime(t, root, lmcacheFixtureServerVersion, fakeLMCacheServerScript(marker))
	cfg.LMCache.Server = config.LMCacheServerConfig{Enabled: true, L1SizeGB: 4}

	if err := s.lmcacheMod.startServer(cfg); err != nil {
		t.Fatal(err)
	}
	defer s.lmcacheMod.stopServer()
	st := s.lmcacheMod.proc.Status()
	if !st.Running || st.PID <= 0 {
		t.Fatalf("process status = %+v, want running", st)
	}
	wantPath := filepath.Join("lmcache", "versions", lmcacheFixtureServerVersion, ".venv", "bin", "lmcache")
	var got []byte
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ = os.ReadFile(marker); strings.Contains(string(got), wantPath) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(string(got), wantPath) {
		t.Fatalf("launched from %q, want the dedicated server venv", got)
	}
	if _, err := os.Stat(filepath.Join(root, "lmcache", "server.log")); err != nil {
		t.Fatalf("server log missing: %v", err)
	}

	// Restarting replaces the running process instead of erroring.
	if err := s.lmcacheMod.startServer(cfg); err != nil {
		t.Fatal(err)
	}
	if got := s.lmcacheMod.proc.Status(); !got.Running {
		t.Fatalf("process status after restart = %+v, want running", got)
	}

	if err := s.lmcacheMod.stopServer(); err != nil {
		t.Fatal(err)
	}
	if got := s.lmcacheMod.proc.Status(); got.Running {
		t.Fatalf("process status after stop = %+v, want stopped", got)
	}
}

func TestLMCache_ServerStartFailsWithoutDedicatedVenv(t *testing.T) {
	s, cfg, _, root := lmcacheServiceFixture(t)
	// No dedicated server runtime at all: the server must never fall back to
	// a vLLM venv (the isolation guarantee).
	if err := os.Remove(filepath.Join(root, config.LMCacheRuntimeName, "current")); err != nil {
		t.Fatal(err)
	}
	err := s.lmcacheMod.startServer(cfg)
	if err == nil || !strings.Contains(err.Error(), "not staged") {
		t.Fatalf("startServer() = %v, want not-installed error", err)
	}
}

func TestLMCache_StatusReportsServerEffectiveConfig(t *testing.T) {
	s, cfg, _, _ := lmcacheServiceFixture(t)
	st := s.lmcacheMod.status(cfg)
	if st.Server.Enabled || st.Server.Running {
		t.Fatalf("server status = %+v, want disabled and stopped", st.Server)
	}
	if st.Server.State != LMCacheStateStopped {
		t.Fatalf("server state = %s, want STOPPED", st.Server.State)
	}
	if st.Server.Port != 5555 || st.Server.HTTPPort != 8900 || st.Server.L1SizeGB != 20 ||
		st.Server.EvictionPolicy != "LRU" || st.Server.ChunkSize != 0 {
		t.Fatalf("server effective config = %+v, want documented defaults (chunkSize 0 = auto)", st.Server)
	}
	if st.Server.L2Enabled || st.Server.L2MaxBytes != 20<<30 || st.Server.L2UsedBytes != nil ||
		st.Server.L3Enabled || st.Server.L3Path != "" {
		t.Fatalf("default L2/L3 status = %+v, want disabled tiers with the 20 GiB pool and no usage", st.Server)
	}

	cfg.LMCache.Server = config.LMCacheServerConfig{
		Enabled: true, Port: 6000, HTTPPort: 9000, L1SizeGB: 8, EvictionPolicy: "noop", ChunkSize: 128,
		L2: config.LMCacheL2Config{Enabled: true, MaxBytes: "30GB"},
		L3: config.LMCacheL3Config{Enabled: true, Path: "/var/lib/lmcache"},
	}
	st = s.lmcacheMod.status(cfg)
	if !st.Server.Enabled || st.Server.Port != 6000 || st.Server.HTTPPort != 9000 ||
		st.Server.L1SizeGB != 8 || st.Server.EvictionPolicy != "noop" || st.Server.ChunkSize != 128 {
		t.Fatalf("server status = %+v, want configured values", st.Server)
	}
	if st.Server.Version != lmcacheFixtureServerVersion {
		t.Fatalf("server version = %q, want %q", st.Server.Version, lmcacheFixtureServerVersion)
	}
	if !st.Server.L2Enabled || st.Server.L2MaxBytes != 30*1000*1000*1000 {
		t.Fatalf("L2 status = enabled=%v limit=%d, want enabled with the 30GB limit", st.Server.L2Enabled, st.Server.L2MaxBytes)
	}
	if st.Server.L2UsedBytes != nil {
		t.Fatalf("L2 used = %d, want nil (the server is not running, usage must be unavailable not zero)", *st.Server.L2UsedBytes)
	}
	if !st.Server.L3Enabled || st.Server.L3Path != "/var/lib/lmcache" {
		t.Fatalf("L3 status = enabled=%v path=%q, want enabled with the configured path", st.Server.L3Enabled, st.Server.L3Path)
	}
}

func TestLMCache_StatusNotInstalledWithoutServerRuntime(t *testing.T) {
	s, cfg, _, root := lmcacheServiceFixture(t)
	if err := os.Remove(filepath.Join(root, config.LMCacheRuntimeName, "current")); err != nil {
		t.Fatal(err)
	}
	st := s.lmcacheMod.status(cfg)
	if st.Server.State != LMCacheStateNotInstalled {
		t.Fatalf("server state = %s, want NOT_INSTALLED", st.Server.State)
	}
}

// startFakeServer runs the fixture's fake lmcache script under the process
// supervisor against an arbitrary health endpoint, returning the health URL
// the watcher probes.
func startFakeServer(t *testing.T, svc *lmcacheService, executable string, healthURL string) {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "server.log")
	if err := svc.proc.Start(executable, []string{"30"}, "", logPath, healthURL); err != nil {
		t.Fatal(err)
	}
}

func waitForState(t *testing.T, p *lmcacheProcess, want lmcacheState, timeout time.Duration) lmcacheProcStatus {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if st := p.Status(); st.State == want {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	return p.Status()
}

// waitForStatus polls until cond holds and returns that snapshot, or the last
// one after timeout.
//
// Use this instead of waitForState whenever the assertion spans more than the
// state field. The health-failure path publishes State=ERROR and LastError
// under one lock acquisition and only then calls Stop, which clears Running —
// so a wait on State==ERROR alone can return the intermediate snapshot and fail
// the teardown assertion intermittently.
func waitForStatus(t *testing.T, p *lmcacheProcess, timeout time.Duration, cond func(lmcacheProcStatus) bool) lmcacheProcStatus {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		st := p.Status()
		if cond(st) {
			return st
		}
		if time.Now().After(deadline) {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestLMCache_ProcessRunningAfterHealthCheck(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep unavailable")
	}
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"healthy"}`))
	}))
	t.Cleanup(healthy.Close)

	svc := newLMCacheService(&Server{})
	svc.proc.readyTimeout = 5 * time.Second
	startFakeServer(t, svc, "sleep", healthy.URL+"/healthcheck")
	defer svc.proc.Stop(lmcacheStopTimeout)

	st := waitForState(t, &svc.proc, LMCacheStateRunning, 5*time.Second)
	if !st.Running {
		t.Fatalf("status = %+v, want running process", st)
	}
}

func TestLMCache_HealthProbeRejectsUnhealthySubstring(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{name: "healthy status", body: `{"status":"healthy"}`, want: true},
		{name: "healthy boolean", body: `{"healthy":true}`, want: true},
		{name: "healthy snake case boolean", body: `{"is_healthy":true}`, want: true},
		{name: "unhealthy status", body: `{"status":"unhealthy"}`, want: false},
		{name: "plain false", body: "false", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(server.Close)
			if got := probeLMCacheHealth(server.Client(), server.URL+"/healthcheck"); got != tc.want {
				t.Fatalf("probeLMCacheHealth(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

func TestLMCache_ProcessErrorsWhenHealthTimesOut(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep unavailable")
	}
	// Point the watcher at a closed endpoint: the process stays alive but
	// never answers, so the ready timeout must fail the start.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	svc := newLMCacheService(&Server{})
	svc.proc.readyTimeout = 300 * time.Millisecond
	startFakeServer(t, svc, "sleep", deadURL+"/healthcheck")

	st := waitForState(t, &svc.proc, LMCacheStateError, 3*time.Second)
	if st.State != LMCacheStateError {
		t.Fatalf("status = %+v, want ERROR after health timeout", st)
	}
	if !strings.Contains(st.LastError, "did not become healthy") {
		t.Fatalf("lastError = %q, want timeout cause", st.LastError)
	}
	// The failed start must not leave a zombie: the process is terminated.
	requireServerTerminated(t, &svc.proc, "status")
}

// requireServerTerminated polls until the supervised server process is gone.
// Teardown follows the ERROR state asynchronously; under load it can lag, so
// poll instead of asserting the instant the state flips.
func requireServerTerminated(t *testing.T, p *lmcacheProcess, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := p.Status(); !got.Running {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s = %+v, want the failed start terminated", what, p.Status())
}

func TestLMCache_StartServerAndWaitCancelsAndStopsProcess(t *testing.T) {
	s, cfg, _, _ := lmcacheServiceFixture(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	cfg.LMCache.Server = config.LMCacheServerConfig{Enabled: true, HTTPHost: "127.0.0.1", HTTPPort: port}
	s.setConfig(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- s.lmcacheMod.startServerAndWait(ctx, cfg) }()
	if got := waitForState(t, &s.lmcacheMod.proc, LMCacheStateStarting, time.Second); !got.Running {
		t.Fatalf("process status = %+v, want a running process before cancellation", got)
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("startServerAndWait() error = %v, want context cancellation", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("startServerAndWait did not return after cancellation")
	}
	if got := s.lmcacheMod.proc.Status(); got.Running {
		t.Fatalf("process status after cancellation = %+v, want stopped", got)
	}
}

func TestLMCache_ProcessDegradesWhenHealthFails(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep unavailable")
	}
	healthy := make(chan struct{})
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-healthy:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"unhealthy"}`))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"healthy"}`))
		}
	}))
	t.Cleanup(probe.Close)

	svc := newLMCacheService(&Server{})
	svc.proc.readyTimeout = 5 * time.Second
	svc.proc.healthInterval = 20 * time.Millisecond
	startFakeServer(t, svc, "sleep", probe.URL+"/healthcheck")

	st := waitForState(t, &svc.proc, LMCacheStateRunning, 5*time.Second)
	if st.State != LMCacheStateRunning {
		t.Fatalf("status = %+v, want RUNNING before health loss", st)
	}
	close(healthy)

	st = waitForState(t, &svc.proc, LMCacheStateError, 3*time.Second)
	if st.State != LMCacheStateError {
		t.Fatalf("status = %+v, want ERROR after health loss", st)
	}
	if !strings.Contains(st.LastError, "health check failed") {
		t.Fatalf("lastError = %q, want health failure cause", st.LastError)
	}
}

func TestLMCache_ProcessCrashBecomesError(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	svc := newLMCacheService(&Server{})
	svc.proc.readyTimeout = 5 * time.Second
	logPath := filepath.Join(t.TempDir(), "server.log")
	// The "server" exits immediately with a non-zero code.
	if err := svc.proc.Start("sh", []string{"-c", "exit 7"}, "", logPath, ""); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st := svc.proc.Status(); st.State == LMCacheStateError {
			if !strings.Contains(st.LastError, "exited unexpectedly") {
				t.Fatalf("lastError = %q, want crash cause", st.LastError)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("final status = %+v, want ERROR", svc.proc.Status())
}

func TestLMCache_ProcessStopIsIdempotent(t *testing.T) {
	svc := newLMCacheService(&Server{})
	if err := svc.proc.Stop(lmcacheStopTimeout); err != nil {
		t.Fatalf("stop of a stopped process = %v, want nil", err)
	}
	if st := svc.proc.Status(); st.State != LMCacheStateStopped {
		t.Fatalf("status = %+v, want STOPPED", st)
	}
}

func TestLMCache_SyncDefersRestartWhileModelsUseIt(t *testing.T) {
	s, cfg, _, _ := lmcacheServiceFixture(t)
	marker := filepath.Join(t.TempDir(), "launched-by")
	_ = writeActiveLMCacheRuntime(t, cfg.RuntimeManager.Root, lmcacheFixtureServerVersion, fakeLMCacheServerScript(marker))
	port := healthyLMCacheEndpoint(t)
	cfg.LMCache.Server = config.LMCacheServerConfig{Enabled: true, HTTPHost: "127.0.0.1", HTTPPort: port}
	s.setConfig(cfg)
	s.local = &runtimeSwitchRouter{running: map[string]process.ProcessState{
		"model": process.StateReady,
	}}
	if err := s.lmcacheMod.startServer(cfg); err != nil {
		t.Fatal(err)
	}
	defer s.lmcacheMod.stopServer()

	// A parameter change wants a restart, but the model still uses the
	// server: the change is deferred, not applied.
	desired := cfg
	desired.LMCache.Server.L1SizeGB = 8
	if err := s.syncLMCacheModule(cfg, desired); err != nil {
		t.Fatal(err)
	}
	if !s.lmcacheMod.pendingRestart.Load() {
		t.Fatalf("pendingRestart = false, want the deferred restart flagged")
	}
	if got := s.lmcacheMod.proc.Status(); !got.Running {
		t.Fatalf("process status = %+v, want the old server left running", got)
	}

	// Once the model stops, the next sync applies the pending restart.
	s.local = &runtimeSwitchRouter{running: map[string]process.ProcessState{}}
	if err := s.syncLMCacheModule(cfg, desired); err != nil {
		t.Fatal(err)
	}
	if s.lmcacheMod.pendingRestart.Load() {
		t.Fatalf("pendingRestart = true after restart applied, want cleared")
	}
	if got := s.lmcacheMod.proc.Status(); !got.Running {
		t.Fatalf("process status = %+v, want the restarted server", got)
	}

	// L2/L3 belong to the same restart-class group (D4): a change that only
	// touches the pool size or the disk tier defers while in use and applies
	// once the model stops.
	s.local = &runtimeSwitchRouter{running: map[string]process.ProcessState{
		"model": process.StateReady,
	}}
	desired = cfg
	desired.LMCache.Server.L2 = config.LMCacheL2Config{Enabled: true, MaxBytes: "16GB"}
	desired.LMCache.Server.L3 = config.LMCacheL3Config{Enabled: true, Path: filepath.Join(t.TempDir(), "l3")}
	if err := s.syncLMCacheModule(cfg, desired); err != nil {
		t.Fatal(err)
	}
	if !s.lmcacheMod.pendingRestart.Load() {
		t.Fatalf("pendingRestart = false, want the L2/L3 change deferred while in use")
	}
	if got := s.lmcacheMod.proc.Status(); !got.Running {
		t.Fatalf("process status = %+v, want the old server left running", got)
	}

	s.local = &runtimeSwitchRouter{running: map[string]process.ProcessState{}}
	s.lmcacheMod.diskFree = func(string) (int64, error) { return 10 << 30, nil }
	if err := s.syncLMCacheModule(cfg, desired); err != nil {
		t.Fatal(err)
	}
	if s.lmcacheMod.pendingRestart.Load() {
		t.Fatalf("pendingRestart = true after the L2/L3 restart applied, want cleared")
	}
	if got := s.lmcacheMod.proc.Status(); !got.Running {
		t.Fatalf("process status = %+v, want the restarted server", got)
	}
}

// TestLMCache_StartServerAndWaitPublishesSettledProgress guards the operator
// start path's progress contract: the last event the UI saw was this start's
// "starting" phase, so a settled start must replace it with a terminal event
// instead of leaving the card's progress bar pinned mid-start.
func TestLMCache_StartServerAndWaitPublishesSettledProgress(t *testing.T) {
	s, cfg, _, _ := lmcacheServiceFixture(t)
	marker := filepath.Join(t.TempDir(), "launched-by")
	_ = writeActiveLMCacheRuntime(t, cfg.RuntimeManager.Root, lmcacheFixtureServerVersion, fakeLMCacheServerScript(marker))
	port := healthyLMCacheEndpoint(t)
	cfg.LMCache.Server = config.LMCacheServerConfig{Enabled: true, HTTPHost: "127.0.0.1", HTTPPort: port}
	s.setConfig(cfg)

	events := make(chan swaputil.BackendProgressEvent, 8)
	cancel := event.On(func(e swaputil.BackendProgressEvent) {
		if e.Runtime == lmcacheProgressKey {
			events <- e
		}
	})
	defer cancel()

	if err := s.lmcacheMod.startServerAndWait(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	defer s.lmcacheMod.stopServer()

	want := []struct {
		phase    string
		progress float64
	}{{"starting", 0.5}, {"idle", 1}}
	deadline := time.Now().Add(3 * time.Second)
	for i := 0; i < len(want) && time.Now().Before(deadline); {
		select {
		case e := <-events:
			if e.Phase != want[i].phase || e.Progress != want[i].progress {
				t.Fatalf("progress event %d = phase %q progress %v, want %q %v", i, e.Phase, e.Progress, want[i].phase, want[i].progress)
			}
			i++
		case <-time.After(time.Until(deadline)):
		}
	}
	if got := len(events); got != 0 {
		t.Fatalf("progress stream has %d unexpected trailing events", got)
	}
}

// TestLMCache_SyncDoesNotResurrectStoppedServer pins the operator-stop
// contract: an explicit stop must survive a config reload that touches the
// LMCache section, even with autoStart defaulting to true.
func TestLMCache_SyncDoesNotResurrectStoppedServer(t *testing.T) {
	s, cfg, _, _ := lmcacheServiceFixture(t)
	marker := filepath.Join(t.TempDir(), "launched-by")
	_ = writeActiveLMCacheRuntime(t, cfg.RuntimeManager.Root, lmcacheFixtureServerVersion, fakeLMCacheServerScript(marker))
	port := healthyLMCacheEndpoint(t)
	cfg.LMCache = config.LMCacheModuleConfig{Enabled: true, Server: config.LMCacheServerConfig{Enabled: true, HTTPHost: "127.0.0.1", HTTPPort: port}}
	s.setConfig(cfg)
	if err := s.lmcacheMod.startServer(cfg); err != nil {
		t.Fatal(err)
	}
	if st := waitForState(t, &s.lmcacheMod.proc, LMCacheStateRunning, 3*time.Second); !st.Running {
		t.Fatalf("process status = %+v, want a running server before the stop", st)
	}
	if err := s.lmcacheMod.stopServer(); err != nil {
		t.Fatal(err)
	}

	desired := cfg
	desired.LMCache.Server.L1SizeGB = 8
	if err := s.syncLMCacheModule(cfg, desired); err != nil {
		t.Fatal(err)
	}
	if got := s.lmcacheMod.proc.Status(); got.Running || got.State != LMCacheStateStopped {
		t.Fatalf("process status after sync = %+v, want the stopped server left alone", got)
	}
}

// TestLMCache_SyncStartsNewlyEnabledServer covers the counterpart: a reload
// that itself enables the stopped server is an explicit start and proceeds.
func TestLMCache_SyncStartsNewlyEnabledServer(t *testing.T) {
	s, cfg, _, _ := lmcacheServiceFixture(t)
	marker := filepath.Join(t.TempDir(), "launched-by")
	_ = writeActiveLMCacheRuntime(t, cfg.RuntimeManager.Root, lmcacheFixtureServerVersion, fakeLMCacheServerScript(marker))
	port := healthyLMCacheEndpoint(t)
	cfg.LMCache = config.LMCacheModuleConfig{Enabled: true, Server: config.LMCacheServerConfig{Enabled: false, HTTPHost: "127.0.0.1", HTTPPort: port}}
	s.setConfig(cfg)

	desired := cfg
	desired.LMCache.Server.Enabled = true
	if err := s.syncLMCacheModule(cfg, desired); err != nil {
		t.Fatal(err)
	}
	defer s.lmcacheMod.stopServer()
	if st := waitForState(t, &s.lmcacheMod.proc, LMCacheStateRunning, 3*time.Second); !st.Running {
		t.Fatalf("process status = %+v, want the newly enabled server started", st)
	}
}

// TestLMCache_TrackerReferenceCounting covers the reference tracker itself:
// idempotent acquire/release, sorted listing, the in-use error, and
// concurrent use (run under -race in make test-all).
func TestLMCache_TrackerReferenceCounting(t *testing.T) {
	u := newLMCacheUsers()
	if got := u.users(); len(got) != 0 {
		t.Fatalf("new tracker users = %v, want empty", got)
	}

	u.acquire("model-b")
	u.acquire("model-a")
	u.acquire("model-a") // idempotent
	if got := u.users(); !reflect.DeepEqual(got, []string{"model-a", "model-b"}) {
		t.Fatalf("users = %v, want [model-a model-b]", got)
	}
	err := u.requireZero()
	if !errors.Is(err, errLMCacheInUse) {
		t.Fatalf("requireZero = %v, want the in-use sentinel", err)
	}
	if !strings.Contains(err.Error(), "model-a") || !strings.Contains(err.Error(), "model-b") {
		t.Fatalf("requireZero = %v, want both models listed", err)
	}
	if !u.release("model-a") || u.release("model-a") {
		t.Fatalf("release of model-a = true then false, want the first to report a held reference")
	}
	if u.release("ghost") {
		t.Fatalf("release of unknown model reported a held reference")
	}
	if got := u.users(); !reflect.DeepEqual(got, []string{"model-b"}) {
		t.Fatalf("users after release = %v, want [model-b]", got)
	}

	const workers = 8
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("model-%d", i%4)
			for j := 0; j < 100; j++ {
				u.acquire(id)
				u.users()
				u.requireZero()
				u.release(id)
			}
		}(i)
	}
	wg.Wait()
	if !u.release("model-b") {
		t.Fatalf("final release of model-b reported no held reference")
	}
}

// TestLMCache_StopRefusedWhileTrackerInUse is scenario T4: the reference
// comes from the pre-start tracker alone (no model runs in the router), the
// stop is refused with the model listed, and the server is left untouched.
func TestLMCache_StopRefusedWhileTrackerInUse(t *testing.T) {
	s, cfg, _, _ := lmcacheServiceFixture(t)
	cfg.LMCache.Server = config.LMCacheServerConfig{Enabled: true}
	if err := s.lmcacheMod.startServer(cfg); err != nil {
		t.Fatal(err)
	}
	defer s.lmcacheMod.stopServer() //nolint: errcheck
	if got := s.lmcacheMod.proc.Status(); !got.Running {
		t.Fatalf("process status = %+v, want running", got)
	}

	s.lmcacheMod.users.acquire("model")

	rec := httptest.NewRecorder()
	s.handleAPILMCacheServerStop(rec, httptest.NewRequest(http.MethodPost, "/api/lmcache/server/stop", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body = %s, want 409", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model") {
		t.Fatalf("body = %s, want the in-use model listed", rec.Body.String())
	}
	if got := s.lmcacheMod.proc.Status(); !got.Running {
		t.Fatalf("process after refused stop = %+v, want still running", got)
	}

	if !s.lmcacheMod.users.release("model") {
		t.Fatalf("release reported no held reference")
	}
	rec = httptest.NewRecorder()
	s.handleAPILMCacheServerStop(rec, httptest.NewRequest(http.MethodPost, "/api/lmcache/server/stop", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status after release = %d body = %s, want 204", rec.Code, rec.Body.String())
	}
	if got := s.lmcacheMod.proc.Status(); got.Running {
		t.Fatalf("process after stop = %+v, want stopped", got)
	}
}

// TestLMCache_ReferenceReleaseKeepsServerUntilAllFree is scenario T5: two
// models hold references, releasing one keeps the server protected, and only
// the last release unblocks the stop.
func TestLMCache_ReferenceReleaseKeepsServerUntilAllFree(t *testing.T) {
	s, cfg, _, _ := lmcacheServiceFixture(t)
	cfg.LMCache.Server = config.LMCacheServerConfig{Enabled: true}
	if err := s.lmcacheMod.startServer(cfg); err != nil {
		t.Fatal(err)
	}
	defer s.lmcacheMod.stopServer() //nolint: errcheck

	s.lmcacheMod.users.acquire("model-a")
	s.lmcacheMod.users.acquire("model-b")

	stop := func() int {
		rec := httptest.NewRecorder()
		s.handleAPILMCacheServerStop(rec, httptest.NewRequest(http.MethodPost, "/api/lmcache/server/stop", nil))
		return rec.Code
	}

	if code := stop(); code != http.StatusConflict {
		t.Fatalf("stop with two references = %d, want 409", code)
	}
	s.lmcacheMod.users.release("model-a")
	if got := s.lmcacheMod.users.users(); !reflect.DeepEqual(got, []string{"model-b"}) {
		t.Fatalf("users = %v, want [model-b]", got)
	}
	if code := stop(); code != http.StatusConflict {
		t.Fatalf("stop with one reference = %d, want 409", code)
	}
	if got := s.lmcacheMod.proc.Status(); !got.Running {
		t.Fatalf("process = %+v, want the server left running", got)
	}
	s.lmcacheMod.users.release("model-b")
	if code := stop(); code != http.StatusNoContent {
		t.Fatalf("stop after last release = %d, want 204", code)
	}
}

// TestLMCache_RestartRefusedWhileInUse is scenario T7: starting the server
// while it is running is a restart, and a restart must be refused while a
// model holds a reference; the original process stays up.
func TestLMCache_RestartRefusedWhileInUse(t *testing.T) {
	s, cfg, _, _ := lmcacheServiceFixture(t)
	cfg = lmcacheUpdateConfig(t, s, cfg)
	if err := s.lmcacheMod.startServer(cfg); err != nil {
		t.Fatal(err)
	}
	defer s.lmcacheMod.stopServer() //nolint: errcheck
	firstPID := s.lmcacheMod.proc.Status().PID
	if firstPID <= 0 {
		t.Fatalf("server did not spawn a process")
	}

	s.lmcacheMod.users.acquire("model")
	rec := httptest.NewRecorder()
	s.handleAPILMCacheServerStart(rec, httptest.NewRequest(http.MethodPost, "/api/lmcache/server/start", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("restart status = %d body = %s, want 409 while in use", rec.Code, rec.Body.String())
	}
	if got := s.lmcacheMod.proc.Status(); !got.Running || got.PID != firstPID {
		t.Fatalf("process = %+v, want the original server untouched (pid %d)", got, firstPID)
	}

	s.lmcacheMod.users.release("model")
	rec = httptest.NewRecorder()
	s.handleAPILMCacheServerStart(rec, httptest.NewRequest(http.MethodPost, "/api/lmcache/server/start", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("restart after release = %d body = %s, want 204", rec.Code, rec.Body.String())
	}
	if got := s.lmcacheMod.proc.Status(); !got.Running {
		t.Fatalf("process after restart = %+v, want running", got)
	}
}

// TestLMCache_StatusReportsTrackerUsers checks the live reference view in
// the status endpoint: a tracked reference shows up even without any model
// running in the router.
func TestLMCache_StatusReportsTrackerUsers(t *testing.T) {
	s, cfg, _, _ := lmcacheServiceFixture(t)
	s.lmcacheMod.users.acquire("tracked")
	st := s.lmcacheMod.status(cfg)
	found := false
	for _, id := range st.UsingModels {
		if id == "tracked" {
			found = true
		}
	}
	if !found {
		t.Fatalf("usingModels = %v, want the tracker reference", st.UsingModels)
	}
}

// TestLMCache_RuntimeOperationGuard checks that the generic runtime
// endpoints (stage/activate/rollback) are refused on the reserved lmcache
// runtime name while in use, and never touch other runtimes.
func TestLMCache_RuntimeOperationGuard(t *testing.T) {
	s, cfg, _, _ := lmcacheServiceFixture(t)
	if err := s.lmcacheRuntimeOperationGuard(cfg, "vllm-a"); err != nil {
		t.Fatalf("guard for a foreign runtime = %v, want nil", err)
	}
	if err := s.lmcacheRuntimeOperationGuard(cfg, config.LMCacheRuntimeName); err != nil {
		t.Fatalf("guard with no users = %v, want nil", err)
	}
	s.lmcacheMod.users.acquire("model")
	err := s.lmcacheRuntimeOperationGuard(cfg, config.LMCacheRuntimeName)
	if !errors.Is(err, errLMCacheInUse) {
		t.Fatalf("guard while in use = %v, want the in-use sentinel", err)
	}
	if !strings.Contains(err.Error(), "model") {
		t.Fatalf("guard error = %v, want the model listed", err)
	}
}

// TestLMCache_DecorateModelProcess installs the pre-start dependency gate on
// LMCache-enabled models only: a gated start that cannot bring the server up
// fails the model start and leaves no reference behind (acquired, then
// released), while a model without LMCache has no gate at all.
func TestLMCache_DecorateModelProcess(t *testing.T) {
	s, cfg, _, _ := lmcacheServiceFixture(t)
	// Server enabled but nothing answers the probe: the gate must fail
	// closed instead of letting the model start without its cache.
	cfg.LMCache.Server = config.LMCacheServerConfig{Enabled: true, HTTPHost: "127.0.0.1", HTTPPort: 1}
	cfg.Models["plain"] = config.ModelConfig{Backend: config.BackendConfig{
		Type: "vllm", Runtime: "vllm-a",
	}}
	s.setConfig(cfg)
	s.lmcacheMod.proc.readyTimeout = 300 * time.Millisecond

	logger := logmon.NewWriter(io.Discard)
	lmProc, err := process.New(context.Background(), "model", config.ModelConfig{
		Cmd: "definitely-missing-binary-xyz",
	}, logger, logger)
	if err != nil {
		t.Fatalf("process.New(lm): %v", err)
	}
	plainProc, err := process.New(context.Background(), "plain", config.ModelConfig{
		Cmd: "definitely-missing-binary-xyz",
	}, logger, logger)
	if err != nil {
		t.Fatalf("process.New(plain): %v", err)
	}

	s.decorateModelProcess("model", lmProc)
	s.decorateModelProcess("plain", plainProc)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := lmProc.EnsureReady(ctx, 20*time.Second); err == nil {
		t.Fatal("EnsureReady(lm) succeeded, want the gate to fail closed")
	}
	if got := s.lmcacheMod.users.users(); len(got) != 0 {
		t.Fatalf("users after failed gate = %v, want none (acquired then released)", got)
	}
	requireServerTerminated(t, &s.lmcacheMod.proc, "lmcache server")
	if err := plainProc.EnsureReady(ctx, 3*time.Second); err == nil {
		t.Fatal("EnsureReady(plain) succeeded, want the missing binary to fail")
	}
	if got := s.lmcacheMod.users.users(); len(got) != 0 {
		t.Fatalf("users after plain start = %v, want the plain model untracked", got)
	}
}

// TestLMCache_ModelGateFailsWhenServerDisabled fails the model start with an
// explicit config error when lmcache.server.enabled is false: fail-closed,
// no server spawn, no reference.
func TestLMCache_ModelGateFailsWhenServerDisabled(t *testing.T) {
	s, _, _, _ := lmcacheServiceFixture(t)
	// The fixture leaves lmcache.server.enabled false by default.
	logger := logmon.NewWriter(io.Discard)
	proc, err := process.New(context.Background(), "model", config.ModelConfig{
		Cmd: "definitely-missing-binary-xyz",
	}, logger, logger)
	if err != nil {
		t.Fatalf("process.New: %v", err)
	}
	s.decorateModelProcess("model", proc)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := proc.EnsureReady(ctx, 3*time.Second); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("EnsureReady = %v, want the explicit disabled error", err)
	}
	if got := s.lmcacheMod.users.users(); len(got) != 0 {
		t.Fatalf("users = %v, want none", got)
	}
	if got := s.lmcacheMod.proc.Status(); got.Running {
		t.Fatalf("server = %+v, want nothing started for a disabled config", got)
	}
}

// skipIfNoSimpleResponder skips the test when the prebuilt test binary is
// absent; `make simple-responder` produces build/.
func skipIfNoSimpleResponder(t *testing.T) {
	t.Helper()
	path := filepath.Join("..", "..", "build", fmt.Sprintf("simple-responder_%s_%s", runtime.GOOS, runtime.GOARCH))
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Skipf("simple-responder not found at %s, run `make simple-responder`", path)
	}
}

// simpleResponderCmd resolves the prebuilt binary with a free port, in the
// shell-command form ProcessCommand expects.
func simpleResponderCmd(t *testing.T, args ...string) (string, int) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("get free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	cmdPath := filepath.ToSlash(filepath.Join("..", "..", "build", fmt.Sprintf("simple-responder_%s_%s", runtime.GOOS, runtime.GOARCH)))
	base := []string{cmdPath, fmt.Sprintf("-port %d", port)}
	base = append(base, args...)
	return strings.Join(base, " "), port
}

// healthyLMCacheEndpoint serves /healthcheck healthy and returns its port.
func healthyLMCacheEndpoint(t *testing.T) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthcheck" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"healthy"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().(*net.TCPAddr).Port
}

// waitForLMCacheState polls the supervised server state until it matches or
// the timeout expires.
func waitForLMCacheState(t *testing.T, p *lmcacheProcess, want lmcacheState, timeout time.Duration) lmcacheProcStatus {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if st := p.Status(); st.State == want {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	return p.Status()
}

// TestLMCache_ModelGateAutoStartsServer is scenario T1: with the server
// stopped, a model start finds the gate cold; the gate starts the server,
// waits for it to become healthy, then lets the model proceed.
func TestLMCache_ModelGateAutoStartsServer(t *testing.T) {
	skipIfNoSimpleResponder(t)
	s, cfg, _, _ := lmcacheServiceFixture(t)
	port := healthyLMCacheEndpoint(t)
	cfg.LMCache.Server = config.LMCacheServerConfig{Enabled: true, HTTPHost: "127.0.0.1", HTTPPort: port}
	s.setConfig(cfg)

	cmd, modelPort := simpleResponderCmd(t, "-silent")
	logger := logmon.NewWriter(io.Discard)
	proc, err := process.New(context.Background(), "model", config.ModelConfig{
		Cmd: cmd, Proxy: fmt.Sprintf("http://127.0.0.1:%d", modelPort),
		CheckEndpoint: "/health", HealthCheckTimeout: 10,
	}, logger, logger)
	if err != nil {
		t.Fatalf("process.New: %v", err)
	}
	defer proc.Stop(2 * time.Second) //nolint: errcheck
	s.decorateModelProcess("model", proc)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := proc.EnsureReady(ctx, 60*time.Second); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	if got := s.lmcacheMod.proc.Status(); got.State != LMCacheStateRunning {
		t.Fatalf("lmcache server = %+v, want RUNNING", got)
	}
	if got := s.lmcacheMod.users.users(); !reflect.DeepEqual(got, []string{"model"}) {
		t.Fatalf("users = %v, want [model]", got)
	}
	if got := proc.State(); got != process.StateReady {
		t.Fatalf("model state = %s, want ready", got)
	}
}

// TestLMCache_ModelGateReusesHealthyServer is scenario T2: with the server
// already RUNNING and healthy, a model start reuses it — no second instance
// is spawned (same PID).
func TestLMCache_ModelGateReusesHealthyServer(t *testing.T) {
	skipIfNoSimpleResponder(t)
	s, cfg, _, _ := lmcacheServiceFixture(t)
	port := healthyLMCacheEndpoint(t)
	cfg.LMCache.Server = config.LMCacheServerConfig{Enabled: true, HTTPHost: "127.0.0.1", HTTPPort: port}
	s.setConfig(cfg)

	if err := s.lmcacheMod.startServer(cfg); err != nil {
		t.Fatal(err)
	}
	defer s.lmcacheMod.stopServer() //nolint: errcheck
	st := waitForLMCacheState(t, &s.lmcacheMod.proc, LMCacheStateRunning, 15*time.Second)
	if st.State != LMCacheStateRunning {
		t.Fatalf("server = %+v, want RUNNING before any model start", st)
	}
	firstPID := st.PID

	cmd, modelPort := simpleResponderCmd(t, "-silent")
	logger := logmon.NewWriter(io.Discard)
	proc, err := process.New(context.Background(), "model", config.ModelConfig{
		Cmd: cmd, Proxy: fmt.Sprintf("http://127.0.0.1:%d", modelPort),
		CheckEndpoint: "/health", HealthCheckTimeout: 10,
	}, logger, logger)
	if err != nil {
		t.Fatalf("process.New: %v", err)
	}
	defer proc.Stop(2 * time.Second) //nolint: errcheck
	s.decorateModelProcess("model", proc)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := proc.EnsureReady(ctx, 60*time.Second); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	if got := s.lmcacheMod.proc.Status(); got.PID != firstPID {
		t.Fatalf("server pid = %d, want the original instance %d (no second server)", got.PID, firstPID)
	}
	if got := s.lmcacheMod.users.users(); !reflect.DeepEqual(got, []string{"model"}) {
		t.Fatalf("users = %v, want [model]", got)
	}
}

// TestLMCache_ModelStartFailsClosedWhenServerCannotStart is scenario T3:
// when the server cannot become healthy, the model start fails with the
// server's last error and the log path; no reference is left behind and the
// failed server process is terminated.
func TestLMCache_ModelStartFailsClosedWhenServerCannotStart(t *testing.T) {
	s, cfg, _, _ := lmcacheServiceFixture(t)
	// A closed endpoint: the fake server stays alive but never answers.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	port := dead.Listener.Addr().(*net.TCPAddr).Port
	dead.Close()
	cfg.LMCache.Server = config.LMCacheServerConfig{Enabled: true, HTTPHost: "127.0.0.1", HTTPPort: port}
	s.setConfig(cfg)
	s.lmcacheMod.proc.readyTimeout = 300 * time.Millisecond

	logger := logmon.NewWriter(io.Discard)
	proc, err := process.New(context.Background(), "model", config.ModelConfig{
		Cmd: "definitely-missing-binary-xyz",
	}, logger, logger)
	if err != nil {
		t.Fatalf("process.New: %v", err)
	}
	s.decorateModelProcess("model", proc)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = proc.EnsureReady(ctx, 20*time.Second)
	if err == nil {
		t.Fatal("model start succeeded, want fail-closed")
	}
	if !strings.Contains(err.Error(), "LMCache is not ready") || !strings.Contains(err.Error(), "server.log") {
		t.Fatalf("gate error = %v, want the cause and the log path", err)
	}
	if got := proc.State(); got != process.StateStopped {
		t.Fatalf("model state = %s, want stopped", got)
	}
	if got := s.lmcacheMod.users.users(); len(got) != 0 {
		t.Fatalf("users = %v, want none after the failed gate", got)
	}
	requireServerTerminated(t, &s.lmcacheMod.proc, "server")
}

// TestLMCache_ConcurrentModelStartsShareOneServer is scenario T6: two
// models start concurrently against a cold server; the singleflight gate
// spawns exactly one server and both model starts share its settled result.
func TestLMCache_ConcurrentModelStartsShareOneServer(t *testing.T) {
	s, cfg, _, root := lmcacheServiceFixture(t)
	_ = writeActiveLMCacheRuntime(t, root, lmcacheFixtureServerVersion, "#!/bin/sh\nexec sleep 30\n")
	port := healthyLMCacheEndpoint(t)
	cfg.LMCache.Server = config.LMCacheServerConfig{Enabled: true, HTTPHost: "127.0.0.1", HTTPPort: port}
	// Both starting models must be LMCache-enabled for the decorator to gate
	// them.
	for _, id := range []string{"model-0", "model-1"} {
		cfg.Models[id] = config.ModelConfig{Backend: config.BackendConfig{
			Type: "vllm", Runtime: "vllm-a", LMCache: &config.LMCacheModelConfig{Enabled: true},
		}}
	}
	s.setConfig(cfg)

	logger := logmon.NewWriter(io.Discard)
	procs := make([]*process.ProcessCommand, 2)
	for i := range procs {
		p, err := process.New(context.Background(), fmt.Sprintf("model-%d", i), config.ModelConfig{
			Cmd: "definitely-missing-binary-xyz",
		}, logger, logger)
		if err != nil {
			t.Fatalf("process.New(%d): %v", i, err)
		}
		procs[i] = p
		s.decorateModelProcess(fmt.Sprintf("model-%d", i), p)
	}

	var wg sync.WaitGroup
	errs := make([]error, len(procs))
	for i := range procs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			errs[i] = procs[i].EnsureReady(ctx, 30*time.Second)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err == nil {
			t.Fatalf("model-%d start succeeded, want the missing binary to fail after the gate", i)
		}
		// The failure must be the model's own post-gate validation. Any
		// "LMCache is not ready" error would mean this model did not share
		// the in-flight start, i.e. singleflight was bypassed.
		if !strings.Contains(err.Error(), "upstream proxy missing") {
			t.Fatalf("model-%d error = %v, want the post-gate proxy validation error", i, err)
		}
	}
	if got := s.lmcacheMod.proc.Status(); got.State != LMCacheStateRunning {
		t.Fatalf("server = %+v, want RUNNING after the shared start", got)
	}
	if n := s.lmcacheMod.spawns.Load(); n != 1 {
		t.Fatalf("server spawns = %d, want exactly one (singleflight)", n)
	}
	if got := s.lmcacheMod.users.users(); !reflect.DeepEqual(got, []string{"model-0", "model-1"}) {
		t.Fatalf("users = %v, want both models referencing the shared server", got)
	}
	_ = s.lmcacheMod.stopServer()
}

// lmcacheGateServer enables the fixture server against a healthy endpoint so
// the gate's server half passes and the tests exercise the connector half in
// isolation.
func lmcacheGateServer(t *testing.T, s *Server, cfg config.Config) config.Config {
	t.Helper()
	port := healthyLMCacheEndpoint(t)
	cfg.LMCache.Server = config.LMCacheServerConfig{Enabled: true, HTTPHost: "127.0.0.1", HTTPPort: port}
	s.setConfig(cfg)
	t.Cleanup(func() { _ = s.lmcacheMod.stopServer() })
	return cfg
}

// TestLMCache_ModelGateInstallsConnectorWhenMissing is the decoupled install
// path: a model start guarantees its own vLLM venv gets the connector, pinned
// to the server's current version.
func TestLMCache_ModelGateInstallsConnectorWhenMissing(t *testing.T) {
	s, cfg, runner, root := lmcacheServiceFixture(t)
	lmcacheGateServer(t, s, cfg)

	gate := s.lmcacheMod.ensureReadyForModel("model")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := gate(ctx); err != nil {
		t.Fatalf("gate: %v", err)
	}
	want := "uv pip install --python " + lmcacheVenvPython(root, "vllm-a") + " lmcache==" + lmcacheFixtureServerVersion
	if !reflect.DeepEqual(runner.calls, []string{want}) {
		t.Fatalf("uv calls = %v, want one install into the model's venv", runner.calls)
	}
	if got := s.lmcacheMod.users.users(); !reflect.DeepEqual(got, []string{"model"}) {
		t.Fatalf("users = %v, want the model holding a reference after the gate", got)
	}
}

// TestLMCache_ModelGateReplacesMismatchedConnector keeps the versioned
// protocol guarantee on the model side: a connector from a different release
// than the server is replaced, never left behind.
func TestLMCache_ModelGateReplacesMismatchedConnector(t *testing.T) {
	s, cfg, runner, root := lmcacheServiceFixture(t)
	writeVenvVersion(t, root, "vllm-a", "0.3.0")
	lmcacheGateServer(t, s, cfg)

	gate := s.lmcacheMod.ensureReadyForModel("model")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := gate(ctx); err != nil {
		t.Fatalf("gate: %v", err)
	}
	if len(runner.calls) != 1 || !strings.HasSuffix(runner.calls[0], "lmcache=="+lmcacheFixtureServerVersion) {
		t.Fatalf("uv calls = %v, want a re-pin to the server version", runner.calls)
	}
}

func TestLMCache_ModelGateSkipsMatchingConnector(t *testing.T) {
	s, cfg, runner, root := lmcacheServiceFixture(t)
	writeVenvVersion(t, root, "vllm-a", lmcacheFixtureServerVersion)
	lmcacheGateServer(t, s, cfg)

	gate := s.lmcacheMod.ensureReadyForModel("model")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := gate(ctx); err != nil {
		t.Fatalf("gate: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("uv calls = %v, want none when the connector already matches", runner.calls)
	}
}

// TestLMCache_ModelGateFailsWhenRuntimeNotActivated fails the model start
// (fail-closed) when the model's vLLM runtime has no activated version: there
// is no venv whose connector state could be guaranteed.
func TestLMCache_ModelGateFailsWhenRuntimeNotActivated(t *testing.T) {
	s, cfg, runner, _ := lmcacheServiceFixture(t)
	// A declared vLLM runtime that was never staged.
	cfg.Runtimes["vllm-c"] = config.RuntimeConfig{Kind: "vllm"}
	cfg.Models["lonely"] = config.ModelConfig{Backend: config.BackendConfig{
		Type: "vllm", Runtime: "vllm-c", LMCache: &config.LMCacheModelConfig{Enabled: true},
	}}
	lmcacheGateServer(t, s, cfg)

	gate := s.lmcacheMod.ensureReadyForModel("lonely")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := gate(ctx)
	if err == nil || !strings.Contains(err.Error(), "no activated version") {
		t.Fatalf("gate = %v, want the not-activated error", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("uv ran despite a missing venv: %v", runner.calls)
	}
	if got := s.lmcacheMod.users.users(); len(got) != 0 {
		t.Fatalf("users = %v, want none after the failed gate", got)
	}
}

// TestLMCache_ModelGateHonorsPinnedRuntimeVersion installs the connector into
// the venv of the pinned backend.runtimeVersion, not the current pointer, so
// the guaranteed venv matches the launch binding.
func TestLMCache_ModelGateHonorsPinnedRuntimeVersion(t *testing.T) {
	s, cfg, runner, root := lmcacheServiceFixture(t)
	writeRuntimeVersion(t, root, "vllm-a", runtimeManager.Manifest{
		Name: "vllm-a", Version: "2", Kind: "vllm", Source: "pypi", VLLM: "0.19.0",
		Metadata: map[string]string{"mode": runtimeManager.RuntimeModeNative, "sourceType": "pypi"},
	})
	binDir := filepath.Join(root, "vllm-a", "versions", "2", ".venv", "bin")
	if err := os.MkdirAll(binDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "python"), []byte(fakeVenvPython), 0o755); err != nil {
		t.Fatal(err)
	}
	pinned := cfg.Models["model"]
	pinned.Backend.RuntimeVersion = "2"
	cfg.Models["model"] = pinned
	lmcacheGateServer(t, s, cfg)

	gate := s.lmcacheMod.ensureReadyForModel("model")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := gate(ctx); err != nil {
		t.Fatalf("gate: %v", err)
	}
	want := "uv pip install --python " + filepath.Join(binDir, "python") + " lmcache==" + lmcacheFixtureServerVersion
	if !reflect.DeepEqual(runner.calls, []string{want}) {
		t.Fatalf("uv calls = %v, want the install into the pinned version's venv", runner.calls)
	}
}

// TestLMCache_ConcurrentModelStartsShareConnectorInstall is the connector
// half of scenario T6: two models sharing one venv start concurrently and the
// per-venv singleflight runs exactly one uv install.
func TestLMCache_ConcurrentModelStartsShareConnectorInstall(t *testing.T) {
	s, cfg, runner, _ := lmcacheServiceFixture(t)
	runner.slow = 300 * time.Millisecond
	for _, id := range []string{"model-0", "model-1"} {
		cfg.Models[id] = config.ModelConfig{Backend: config.BackendConfig{
			Type: "vllm", Runtime: "vllm-a", LMCache: &config.LMCacheModelConfig{Enabled: true},
		}}
	}
	lmcacheGateServer(t, s, cfg)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, id := range []string{"model-0", "model-1"} {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			errs[i] = s.lmcacheMod.ensureReadyForModel(id)(ctx)
		}(i, id)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("gate(%d) = %v", i, err)
		}
	}
	if len(runner.calls) != 1 {
		t.Fatalf("uv calls = %v, want exactly one shared install", runner.calls)
	}
	if got := s.lmcacheMod.users.users(); !reflect.DeepEqual(got, []string{"model-0", "model-1"}) {
		t.Fatalf("users = %v, want both models referencing the module", got)
	}
}

// TestLMCache_ConnectorInstallSurvivesShortStartBudget pins the decoupling
// of the install's lifetime from the model start budget: when the shared
// install outlives the caller's start timeout, the gate returns a retryable
// error while uv keeps running in the background (not cancelled with the
// caller), and the retried start joins the in-flight install instead of
// stacking a second uv run.
func TestLMCache_ConnectorInstallSurvivesShortStartBudget(t *testing.T) {
	s, cfg, runner, _ := lmcacheServiceFixture(t)
	runner.slow = 2 * time.Second
	cfg = lmcacheGateServer(t, s, cfg)
	if err := s.lmcacheMod.startServer(cfg); err != nil {
		t.Fatal(err)
	}
	if st := waitForLMCacheState(t, &s.lmcacheMod.proc, LMCacheStateRunning, 15*time.Second); st.State != LMCacheStateRunning {
		t.Fatalf("server = %+v, want RUNNING before the gated start", st)
	}

	gate := s.lmcacheMod.ensureReadyForModel("model")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := gate(ctx)
	if err == nil || !strings.Contains(err.Error(), "still running in the background") {
		t.Fatalf("gate = %v, want the retryable in-progress error", err)
	}
	// The caller's budget expired mid-install: uv must have survived it, so
	// the retried start joins the in-flight op and succeeds without a second
	// uv run.
	retryCtx, retryCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer retryCancel()
	if err := gate(retryCtx); err != nil {
		t.Fatalf("retried gate: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("uv calls = %v, want exactly one install surviving the short budget", runner.calls)
	}
	if got := s.lmcacheMod.users.users(); !reflect.DeepEqual(got, []string{"model"}) {
		t.Fatalf("users = %v, want the model after the retried gate", got)
	}
}

// TestLMCache_ConnectorInstallRejectsDisallowedIndexURL keeps the source
// allowlist enforced on the model-side install: a disallowed index URL fails
// the model start before any uv runs.
func TestLMCache_ConnectorInstallRejectsDisallowedIndexURL(t *testing.T) {
	s, cfg, runner, _ := lmcacheServiceFixture(t)
	cfg.LMCache.IndexURL = "http://pypi.example/simple"
	lmcacheGateServer(t, s, cfg)

	gate := s.lmcacheMod.ensureReadyForModel("model")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := gate(ctx)
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("gate = %v, want sourceAllowlist error", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("uv ran despite invalid indexURL: %v", runner.calls)
	}
	if got := s.lmcacheMod.users.users(); len(got) != 0 {
		t.Fatalf("users = %v, want none after the failed gate", got)
	}
}

// TestLMCache_ConnectorInstallWithAllowedIndexURL passes the configured index
// URL through to the model-side install when the allowlist permits it.
func TestLMCache_ConnectorInstallWithAllowedIndexURL(t *testing.T) {
	s, cfg, runner, _ := lmcacheServiceFixture(t)
	cfg.RuntimeManager.SourceAllowlist = []string{"https://pypi.example"}
	cfg.LMCache.IndexURL = "https://pypi.example/simple"
	lmcacheGateServer(t, s, cfg)

	gate := s.lmcacheMod.ensureReadyForModel("model")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := gate(ctx); err != nil {
		t.Fatalf("gate: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("uv calls = %v, want one install", runner.calls)
	}
	if !strings.Contains(runner.calls[0], "--index-url https://pypi.example/simple") || !strings.HasSuffix(runner.calls[0], "lmcache=="+lmcacheFixtureServerVersion) {
		t.Fatalf("uv call = %q, want indexURL and server-version pin", runner.calls[0])
	}
}

// TestLMCache_InProcessGateSkipsStandaloneServer proves the non-standalone
// (inProcess) mode is independent of the standalone service: the gate neither
// requires lmcache.server.enabled nor starts the server, and the model holds
// no server reference, so stopping the service never has to wait for it.
func TestLMCache_InProcessGateSkipsStandaloneServer(t *testing.T) {
	s, cfg, runner, root := lmcacheServiceFixture(t)
	// The fixture leaves lmcache.server.enabled false and would fail the
	// standalone gate with an explicit "disabled" error.
	lm := cfg.Models["model"]
	lm.Backend.LMCache = &config.LMCacheModelConfig{Enabled: true, Mode: config.LMCacheModeInProcess, ChunkSize: 512}
	cfg.Models["model"] = lm
	s.setConfig(cfg)

	gate := s.lmcacheMod.ensureReadyForModel("model")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := gate(ctx); err != nil {
		t.Fatalf("gate: %v", err)
	}
	if got := s.lmcacheMod.users.users(); len(got) != 0 {
		t.Fatalf("users = %v, want no server reference for an inProcess model", got)
	}
	if got := s.lmcacheMod.proc.Status(); got.Running {
		t.Fatalf("server = %+v, want nothing started for the inProcess mode", got)
	}
	want := "uv pip install --python " + lmcacheVenvPython(root, "vllm-a") + " lmcache==" + lmcacheFixtureServerVersion
	if !reflect.DeepEqual(runner.calls, []string{want}) {
		t.Fatalf("uv calls = %v, want the connector install into the model's venv", runner.calls)
	}
}

// TestLMCache_InProcessGateInstallsWithoutServerRuntime proves the connector
// install does not depend on the standalone server runtime being staged: the
// module's configured version pin is used instead.
func TestLMCache_InProcessGateInstallsWithoutServerRuntime(t *testing.T) {
	s, cfg, runner, root := lmcacheServiceFixture(t)
	lm := cfg.Models["model"]
	lm.Backend.LMCache = &config.LMCacheModelConfig{Enabled: true, Mode: config.LMCacheModeInProcess, ChunkSize: 512}
	cfg.Models["model"] = lm
	cfg.LMCache.Version = "1.2.3"
	// Unstage the server runtime entirely so only the version pin remains.
	if err := os.RemoveAll(filepath.Join(root, config.LMCacheRuntimeName)); err != nil {
		t.Fatalf("unstage server runtime: %v", err)
	}
	s.setConfig(cfg)

	gate := s.lmcacheMod.ensureReadyForModel("model")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := gate(ctx); err != nil {
		t.Fatalf("gate: %v", err)
	}
	want := "uv pip install --python " + lmcacheVenvPython(root, "vllm-a") + " lmcache==1.2.3"
	if !reflect.DeepEqual(runner.calls, []string{want}) {
		t.Fatalf("uv calls = %v, want the pin-styled install", runner.calls)
	}
}

// TestLMCache_InProcessGateSkipsServerReference makes the lifecycle contract
// explicit: an inProcess model is not a user of the standalone server, so the
// "in use" guard that blocks stopping the service must not see it.
func TestLMCache_InProcessGateSkipsServerReference(t *testing.T) {
	s, cfg, _, root := lmcacheServiceFixture(t)
	lm := cfg.Models["model"]
	lm.Backend.LMCache = &config.LMCacheModelConfig{Enabled: true, Mode: config.LMCacheModeInProcess}
	cfg.Models["model"] = lm
	// A second model that really uses the standalone server stays reported.
	cfg.Models["standalone"] = config.ModelConfig{Backend: config.BackendConfig{
		Type: "vllm", Runtime: "vllm-a", LMCache: &config.LMCacheModelConfig{Enabled: true},
	}}
	s.setConfig(cfg)
	writeVenvVersion(t, root, "vllm-a", lmcacheFixtureServerVersion)

	s.local = &runtimeSwitchRouter{running: map[string]process.ProcessState{
		"model":      process.StateReady,
		"standalone": process.StateReady,
	}}
	if got := s.lmcacheMod.lmcacheInUseModels(cfg); !reflect.DeepEqual(got, []string{"standalone"}) {
		t.Fatalf("in-use models = %v, want only the standalone model", got)
	}
}

// TestLMCache_AutoStopStopsServerWhenLastReferenceReleases covers the
// autoStop opt-in: releasing a reference while another model still uses the
// server leaves it running, and the last release stops it.
func TestLMCache_AutoStopStopsServerWhenLastReferenceReleases(t *testing.T) {
	s, cfg, _, _ := lmcacheServiceFixture(t)
	port := healthyLMCacheEndpoint(t)
	autoStop := true
	cfg.LMCache.AutoStop = &autoStop
	cfg.LMCache.Server = config.LMCacheServerConfig{Enabled: true, HTTPHost: "127.0.0.1", HTTPPort: port}
	s.setConfig(cfg)

	if err := s.lmcacheMod.startServer(cfg); err != nil {
		t.Fatal(err)
	}
	if st := waitForLMCacheState(t, &s.lmcacheMod.proc, LMCacheStateRunning, 15*time.Second); st.State != LMCacheStateRunning {
		t.Fatalf("server = %+v, want RUNNING before any release", st)
	}

	s.lmcacheMod.users.acquire("model-a")
	s.lmcacheMod.users.acquire("model-b")

	// The first release cannot stop the server: model-a still references it,
	// and the auto-stop decision re-checks the tracker under the start mutex.
	s.releaseLMCacheReference(swaputil.ProcessStateChangeEvent{ProcessName: "model-b", NewState: string(process.StateStopped)})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := s.lmcacheMod.users.users(); len(got) != 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := s.lmcacheMod.proc.Status(); got.State != LMCacheStateRunning {
		t.Fatalf("server = %+v, want still RUNNING while model-a references it", got)
	}

	// The last release triggers the auto-stop.
	s.releaseLMCacheReference(swaputil.ProcessStateChangeEvent{ProcessName: "model-a", NewState: string(process.StateStopped)})
	if st := waitForLMCacheState(t, &s.lmcacheMod.proc, LMCacheStateStopped, 15*time.Second); st.State != LMCacheStateStopped {
		t.Fatalf("server = %+v, want STOPPED after the last release with autoStop", st)
	}
	if got := s.lmcacheMod.users.users(); len(got) != 0 {
		t.Fatalf("users = %v, want none", got)
	}
}

// TestLMCache_ReleaseLMCacheReference covers the event-stream release path:
// leaving the running states drops the reference, other transitions keep it.
func TestLMCache_ReleaseLMCacheReference(t *testing.T) {
	s, _, _, _ := lmcacheServiceFixture(t)
	s.lmcacheMod.users.acquire("model")

	ev := func(newState string) swaputil.ProcessStateChangeEvent {
		return swaputil.ProcessStateChangeEvent{ProcessName: "model", NewState: newState}
	}
	s.releaseLMCacheReference(ev(string(process.StateStarting)))
	s.releaseLMCacheReference(ev(string(process.StateReady)))
	if got := s.lmcacheMod.users.users(); !reflect.DeepEqual(got, []string{"model"}) {
		t.Fatalf("users = %v, want the reference kept while running", got)
	}
	s.releaseLMCacheReference(ev(string(process.StateStopped)))
	if got := s.lmcacheMod.users.users(); len(got) != 0 {
		t.Fatalf("users after stop = %v, want empty", got)
	}
	s.releaseLMCacheReference(ev(string(process.StateShutdown))) // idempotent
	if got := s.lmcacheMod.users.users(); len(got) != 0 {
		t.Fatalf("users after shutdown = %v, want empty", got)
	}
}

// TestLMCache_ConcurrentStopWhileReleasing hammers the stop endpoint and the
// tracker from many goroutines: only 409/204 may be returned, and the server
// must never end up stopped while a reference is still held.
func TestLMCache_ConcurrentStopWhileReleasing(t *testing.T) {
	s, cfg, _, _ := lmcacheServiceFixture(t)
	cfg.LMCache.Server = config.LMCacheServerConfig{Enabled: true}
	if err := s.lmcacheMod.startServer(cfg); err != nil {
		t.Fatal(err)
	}
	defer s.lmcacheMod.stopServer() //nolint: errcheck

	const workers = 8
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("model-%d", i%3)
			s.lmcacheMod.users.acquire(id)
			for _, attempt := range []struct {
				released bool
			}{
				{false}, // while this worker still holds its reference
				{true},  // right after releasing it
			} {
				if attempt.released {
					s.lmcacheMod.users.release(id)
				}
				rec := httptest.NewRecorder()
				s.handleAPILMCacheServerStop(rec, httptest.NewRequest(http.MethodPost, "/api/lmcache/server/stop", nil))
				if rec.Code != http.StatusConflict && rec.Code != http.StatusNoContent {
					t.Errorf("stop status = %d, want 409 or 204", rec.Code)
				}
			}
		}(i)
	}
	wg.Wait()

	// Every worker has released, so the tracker must be empty.
	if got := s.lmcacheMod.users.users(); len(got) != 0 {
		t.Fatalf("users after all workers = %v, want empty", got)
	}
}

// Test 11: the L2/L3 configuration renders into the real `lmcache server`
// CLI arguments (LMCache 0.5.4 shape, verified in P0). The rendering is
// locked here so a config or CLI change fails loudly instead of silently
// starting the server with a wrong pool size or a missing disk tier.
func TestLMCache_L2L3ArgsRendering(t *testing.T) {
	// baseline is the default effective config: 20 GB pool, LRU, auto chunk
	// size (the --chunk-size flag is omitted), no disk tier.
	baseline := []string{
		"server",
		"--host", "localhost",
		"--port", "5555",
		"--l1-size-gb", "20",
		"--eviction-policy", "LRU",
		"--http-host", "127.0.0.1",
		"--http-port", "8900",
	}
	withL1 := func(gb string) []string {
		args := append([]string{}, baseline...)
		args[argsIndexL1GB(args)] = gb
		return args
	}
	withChunk := func(size string) []string {
		return append(append([]string{}, baseline...), "--chunk-size", size)
	}
	cases := map[string]struct {
		server config.LMCacheServerConfig
		want   []string
	}{
		"defaults render the baseline CLI": {config.LMCacheServerConfig{}, baseline},
		"explicit chunk size renders the flag": {
			config.LMCacheServerConfig{ChunkSize: 128},
			withChunk("128"),
		},
		"l2 maxBytes wins over legacy l1SizeGB": {
			config.LMCacheServerConfig{
				L1SizeGB: 8,
				L2:       config.LMCacheL2Config{Enabled: true, MaxBytes: "30GB"},
			},
			withL1("27.939677238464355"),
		},
		"legacy l1SizeGB renders as whole GiB": {
			config.LMCacheServerConfig{L1SizeGB: 8},
			withL1("8"),
		},
		"l2 GiB suffix renders unchanged": {
			config.LMCacheServerConfig{L2: config.LMCacheL2Config{Enabled: true, MaxBytes: "12GiB"}},
			withL1("12"),
		},
		"hybrid models render separate object groups": {
			config.LMCacheServerConfig{SeparateObjectGroups: true},
			append(append([]string{}, baseline...), "--separate-object-groups"),
		},
		"l3 enabled appends the fs adapter": {
			config.LMCacheServerConfig{L3: config.LMCacheL3Config{Enabled: true, Path: "/var/lib/lmcache"}},
			append(append([]string{}, baseline...),
				"--l2-adapter", `{"base_path":"/var/lib/lmcache","type":"fs"}`),
		},
		"l3 disabled renders no adapter": {
			config.LMCacheServerConfig{L3: config.LMCacheL3Config{Enabled: false}},
			baseline,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := lmcacheServerArgs(tc.server.Effective())
			if err != nil {
				t.Fatalf("lmcacheServerArgs() error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("lmcacheServerArgs() = %v, want %v", got, tc.want)
			}
		})
	}
	t.Run("unparseable l2 size is an explicit error", func(t *testing.T) {
		_, err := lmcacheServerArgs(config.LMCacheServerConfig{
			L2: config.LMCacheL2Config{Enabled: true, MaxBytes: "10XB"},
		}.Effective())
		if err == nil || !strings.Contains(err.Error(), "lmcache.server.l2") {
			t.Fatalf("lmcacheServerArgs() = %v, want an l2 size error", err)
		}
	})
}

// argsIndexL1GB returns the index of the rendered --l1-size-gb value.
func argsIndexL1GB(args []string) int {
	for i, a := range args {
		if a == "--l1-size-gb" {
			return i + 1
		}
	}
	panic("--l1-size-gb not rendered")
}

// Test 12: a broken L3 (disk tier) configuration is fail-closed. The path is
// auto-created when possible; when it cannot be created, is not writable, or
// has insufficient free space the server must not spawn and the error must
// be explicit. The free-space probe goes through the diskFree seam so the
// cases are hermetic (no dependence on the host's real disk).
func TestLMCache_L3SpawnValidation(t *testing.T) {
	t.Run("auto creates the missing path and leaves no probe behind", func(t *testing.T) {
		s, cfg, _, root := lmcacheServiceFixture(t)
		path := filepath.Join(root, "l3", "nested")
		cfg.LMCache.Server.L3 = config.LMCacheL3Config{Enabled: true, Path: path}
		s.setConfig(cfg)
		s.lmcacheMod.diskFree = func(string) (int64, error) { return 10 << 30, nil }
		if err := s.lmcacheMod.prepareL3Runtime(cfg.LMCache.Server.Effective()); err != nil {
			t.Fatalf("prepareL3Runtime() = %v, want the missing path auto-created", err)
		}
		fi, err := os.Stat(path)
		if err != nil || !fi.IsDir() {
			t.Fatalf("L3 path %s not created as a directory: %v", path, err)
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("write probe left behind in %s: %v", path, entries)
		}
	})

	t.Run("blocked path fails closed without spawning", func(t *testing.T) {
		s, cfg, _, root := lmcacheServiceFixture(t)
		path := filepath.Join(root, "l3-blocked")
		if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg.LMCache.Server.L3 = config.LMCacheL3Config{Enabled: true, Path: path}
		s.setConfig(cfg)
		err := s.lmcacheMod.startServer(cfg)
		if err == nil || !strings.Contains(err.Error(), "cannot create") {
			t.Fatalf("startServer() = %v, want an explicit cannot-create error", err)
		}
		if n := s.lmcacheMod.spawns.Load(); n != 0 {
			t.Fatalf("server spawned %d times despite the L3 failure, want 0", n)
		}
		if st := s.lmcacheMod.proc.Status(); st.State != LMCacheStateError {
			t.Fatalf("state = %s, want ERROR", st.State)
		}
	})

	t.Run("not-writable path fails closed without spawning", func(t *testing.T) {
		s, cfg, _, root := lmcacheServiceFixture(t)
		path := filepath.Join(root, "l3-readonly")
		if err := os.MkdirAll(path, 0o750); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o750) })
		if err := os.Chmod(path, 0o555); err != nil {
			t.Fatal(err)
		}
		cfg.LMCache.Server.L3 = config.LMCacheL3Config{Enabled: true, Path: path}
		s.setConfig(cfg)
		err := s.lmcacheMod.startServer(cfg)
		if err == nil || !strings.Contains(err.Error(), "is not writable") {
			t.Fatalf("startServer() = %v, want the explicit not-writable error", err)
		}
		if n := s.lmcacheMod.spawns.Load(); n != 0 {
			t.Fatalf("server spawned %d times despite the L3 failure, want 0", n)
		}
		if st := s.lmcacheMod.proc.Status(); st.State != LMCacheStateError {
			t.Fatalf("state = %s, want ERROR", st.State)
		}
	})

	t.Run("insufficient free space fails closed without spawning", func(t *testing.T) {
		s, cfg, _, root := lmcacheServiceFixture(t)
		path := filepath.Join(root, "l3-full")
		cfg.LMCache.Server.L3 = config.LMCacheL3Config{Enabled: true, Path: path, MaxBytes: "2GiB"}
		s.setConfig(cfg)
		s.lmcacheMod.diskFree = func(string) (int64, error) { return 1 << 30, nil }
		err := s.lmcacheMod.startServer(cfg)
		if err == nil || !strings.Contains(err.Error(), "has only 1073741824 bytes free, need at least 2147483648") {
			t.Fatalf("startServer() = %v, want the explicit free-space error", err)
		}
		if n := s.lmcacheMod.spawns.Load(); n != 0 {
			t.Fatalf("server spawned %d times despite the L3 failure, want 0", n)
		}
		if st := s.lmcacheMod.proc.Status(); st.State != LMCacheStateError {
			t.Fatalf("state = %s, want ERROR", st.State)
		}
	})

	t.Run("free space above the default floor is accepted", func(t *testing.T) {
		s, cfg, _, root := lmcacheServiceFixture(t)
		path := filepath.Join(root, "l3-ok")
		cfg.LMCache.Server.L3 = config.LMCacheL3Config{Enabled: true, Path: path}
		s.setConfig(cfg)
		s.lmcacheMod.diskFree = func(string) (int64, error) { return 1<<30 + 1, nil }
		if err := s.lmcacheMod.prepareL3Runtime(cfg.LMCache.Server.Effective()); err != nil {
			t.Fatalf("prepareL3Runtime() = %v, want success at just above the 1 GiB floor", err)
		}
	})

	t.Run("disk stat failure fails closed without spawning", func(t *testing.T) {
		s, cfg, _, root := lmcacheServiceFixture(t)
		path := filepath.Join(root, "l3-unstatable")
		cfg.LMCache.Server.L3 = config.LMCacheL3Config{Enabled: true, Path: path}
		s.setConfig(cfg)
		s.lmcacheMod.diskFree = func(string) (int64, error) { return 0, errors.New("stat failed") }
		err := s.lmcacheMod.startServer(cfg)
		if err == nil || !strings.Contains(err.Error(), "cannot stat") {
			t.Fatalf("startServer() = %v, want an explicit cannot-stat error", err)
		}
		if n := s.lmcacheMod.spawns.Load(); n != 0 {
			t.Fatalf("server spawned %d times despite the L3 failure, want 0", n)
		}
	})
}

// TestLMCache_SyncRestartsSystemStoppedServer pins the failed-restart
// recovery: when the daemon stops the server for a config apply and the
// start does not come back, the next reload brings it up instead of leaving
// it down behind the "never resurrect an operator stop" rule.
func TestLMCache_SyncRestartsSystemStoppedServer(t *testing.T) {
	s, cfg, _, _ := lmcacheServiceFixture(t)
	marker := filepath.Join(t.TempDir(), "launched-by")
	_ = writeActiveLMCacheRuntime(t, cfg.RuntimeManager.Root, lmcacheFixtureServerVersion, fakeLMCacheServerScript(marker))
	port := healthyLMCacheEndpoint(t)
	cfg.LMCache = config.LMCacheModuleConfig{Enabled: true, Server: config.LMCacheServerConfig{Enabled: true, HTTPHost: "127.0.0.1", HTTPPort: port}}
	s.setConfig(cfg)

	// The state a failed config restart leaves behind: the daemon stopped
	// the server for the apply and the start did not come back.
	s.lmcacheMod.systemStopped.Store(true)

	desired := cfg
	desired.LMCache.Server.L1SizeGB = 8
	if err := s.syncLMCacheModule(cfg, desired); err != nil {
		t.Fatal(err)
	}
	if got := s.lmcacheMod.proc.Status(); !got.Running {
		t.Fatalf("process status after sync = %+v, want the server brought back", got)
	}
	if s.lmcacheMod.systemStopped.Load() {
		t.Fatal("systemStopped = true after the server was brought back")
	}
}

// TestLMCache_OperatorStopSurvivesReload pins the other half: stopping the
// server through the API is an operator decision, so the flag is cleared and
// a later reload must leave the server down.
func TestLMCache_OperatorStopSurvivesReload(t *testing.T) {
	s, cfg, _, _ := lmcacheServiceFixture(t)
	marker := filepath.Join(t.TempDir(), "launched-by")
	_ = writeActiveLMCacheRuntime(t, cfg.RuntimeManager.Root, lmcacheFixtureServerVersion, fakeLMCacheServerScript(marker))
	port := healthyLMCacheEndpoint(t)
	cfg.LMCache = config.LMCacheModuleConfig{Enabled: true, Server: config.LMCacheServerConfig{Enabled: true, HTTPHost: "127.0.0.1", HTTPPort: port}}
	s.setConfig(cfg)
	if err := s.lmcacheMod.startServer(cfg); err != nil {
		t.Fatal(err)
	}
	defer s.lmcacheMod.stopServer()
	if st := waitForState(t, &s.lmcacheMod.proc, LMCacheStateRunning, 3*time.Second); !st.Running {
		t.Fatalf("process status = %+v, want a running server before the stop", st)
	}

	// Even a stale system-stop marker must not survive an operator stop.
	s.lmcacheMod.systemStopped.Store(true)
	w := httptest.NewRecorder()
	s.handleAPILMCacheServerStop(w, httptest.NewRequest(http.MethodPost, "/api/lmcache/server/stop", nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("stop status = %d body = %q", w.Code, w.Body.String())
	}
	if s.lmcacheMod.systemStopped.Load() {
		t.Fatal("operator stop did not clear the system stop flag")
	}

	desired := cfg
	desired.LMCache.Server.L1SizeGB = 8
	if err := s.syncLMCacheModule(cfg, desired); err != nil {
		t.Fatal(err)
	}
	if got := s.lmcacheMod.proc.Status(); got.Running {
		t.Fatalf("process status after reload = %+v, want the stopped server left alone", got)
	}
}

// TestLMCache_TransientProbeMissDoesNotKillServer covers the failure that
// stranded a real model start: the health watcher re-probes a RUNNING server
// every 10s, and a single slow answer used to terminate it immediately. The
// management frontend is one HTTP process, and registering 64 KV cache tensors
// across four TP workers legitimately starves it for seconds; killing it then
// leaves those workers blocked until their own 300s register timeout fails the
// entire model start.
func TestLMCache_TransientProbeMissDoesNotKillServer(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep unavailable")
	}
	var mu sync.Mutex
	healthy := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		up := healthy
		mu.Unlock()
		if !up {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"healthy"}`))
	}))
	t.Cleanup(server.Close)

	svc := newLMCacheService(&Server{})
	svc.proc.readyTimeout = 5 * time.Second
	svc.proc.healthInterval = 20 * time.Millisecond
	startFakeServer(t, svc, "sleep", server.URL+"/healthcheck")
	defer svc.proc.Stop(lmcacheStopTimeout)

	if st := waitForState(t, &svc.proc, LMCacheStateRunning, 5*time.Second); !st.Running {
		t.Fatalf("initial status = %+v, want running", st)
	}

	// Miss a couple of probes (< lmcacheUnhealthyFailures) and recover: the
	// server must survive and keep reporting healthy.
	mu.Lock()
	healthy = false
	mu.Unlock()
	time.Sleep(time.Duration(lmcacheUnhealthyFailures-1) * svc.proc.healthInterval)
	mu.Lock()
	healthy = true
	mu.Unlock()

	time.Sleep(3 * time.Duration(lmcacheUnhealthyFailures) * svc.proc.healthInterval)
	st := svc.proc.Status()
	if st.State != LMCacheStateRunning || !st.Running {
		t.Fatalf("status after transient misses = %+v, want still running", st)
	}
	if !st.Healthy {
		t.Fatalf("status = %+v, want healthy once probes recover", st)
	}
}

// TestLMCache_SustainedProbeFailuresKillServer keeps the other side of the
// contract: a server that stays unreachable must still be torn down so the
// state matches reality instead of advertising a dead frontend.
func TestLMCache_SustainedProbeFailuresKillServer(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep unavailable")
	}
	var mu sync.Mutex
	up := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		healthy := up
		mu.Unlock()
		if !healthy {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"healthy"}`))
	}))
	t.Cleanup(server.Close)

	svc := newLMCacheService(&Server{})
	svc.proc.readyTimeout = 5 * time.Second
	svc.proc.healthInterval = 20 * time.Millisecond
	startFakeServer(t, svc, "sleep", server.URL+"/healthcheck")
	defer svc.proc.Stop(lmcacheStopTimeout)

	if st := waitForState(t, &svc.proc, LMCacheStateRunning, 5*time.Second); !st.Running {
		t.Fatalf("initial status = %+v, want running", st)
	}
	mu.Lock()
	up = false
	mu.Unlock()

	st := waitForStatus(t, &svc.proc, 5*time.Second, func(st lmcacheProcStatus) bool {
		return st.State == LMCacheStateError && !st.Running
	})
	if st.Running {
		t.Fatalf("status = %+v, want the dead server torn down", st)
	}
}

// TestLMCache_StartOpWakesEveryWaiter pins the singleflight contract. Any number
// of models can be swapped in at once, so the shared start result has to reach
// all of them: with a buffered channel only the first receiver got it and every
// other model start blocked until its own deadline and then failed a perfectly
// healthy LMCache server.
func TestLMCache_StartOpWakesEveryWaiter(t *testing.T) {
	op := &lmcacheStartOp{sig: make(chan struct{})}
	const waiters = 8
	errs := make(chan error, waiters)
	for range waiters {
		go func() { errs <- op.wait(context.Background()) }()
	}

	// Let the waiters block on sig before settling, so the test exercises the
	// wake-up path rather than a pre-read.
	time.Sleep(50 * time.Millisecond)
	op.finish(nil)

	for i := range waiters {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("waiter %d got %v, want nil", i, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("waiter %d never woke: the start result reached only one receiver", i)
		}
	}
}

// A waiter whose own budget expires first reports that, not the unfinished
// start's zero value.
func TestLMCache_StartOpWaitHonoursCallerDeadline(t *testing.T) {
	op := &lmcacheStartOp{sig: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if err := op.wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait = %v, want the caller's deadline", err)
	}

	// A start that settles just after the deadline still reports its real
	// outcome to a waiter that got there late.
	op.finish(nil)
	if err := op.wait(context.Background()); err != nil {
		t.Fatalf("late wait = %v, want the settled result", err)
	}
}
