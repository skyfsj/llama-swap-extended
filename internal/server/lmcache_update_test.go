package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	runtimeManager "github.com/mostlygeek/llama-swap/internal/runtime"
)

const lmcacheUpdateCandidate = "0.5.5"

// writeLMCacheVersionScript installs the fake console script into a staged
// version's venv. The script records its own resolved path in marker before
// idling, so a test can prove which version's venv actually ran.
func writeLMCacheVersionScript(t *testing.T, root, version, marker string) {
	t.Helper()
	binDir := filepath.Join(root, config.LMCacheRuntimeName, "versions", version, ".venv", "bin")
	if err := os.MkdirAll(binDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "lmcache"), []byte(fakeLMCacheServerScript(marker)), 0o755); err != nil {
		t.Fatal(err)
	}
}

// lmcacheUpdateFixture builds the manager-driven update scenario on top of
// the service fixture: the derived server runtime is pre-staged and current
// at 0.5.4, a no-op provider stands in for the real LMCache provider, and
// the activation hooks are wired exactly like production. It returns the
// marker files the per-version scripts write to when they run.
func lmcacheUpdateFixture(t *testing.T) (*Server, config.Config, *runtimeManager.Manager, string, string, string) {
	t.Helper()
	s, cfg, _, root := lmcacheServiceFixture(t)
	// Marker files live directly under the runtime root: the same pattern the
	// process-supervision tests use, which stays reliable for spawned scripts.
	oldMarker := filepath.Join(root, "launched-"+lmcacheFixtureServerVersion)
	writeLMCacheVersionScript(t, root, lmcacheFixtureServerVersion, oldMarker)
	manager, err := runtimeManager.NewManager(root, map[string]runtimeManager.Provider{
		config.LMCacheRuntimeName: runtimeSwitchLifecycleProvider{},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.SetIdleProbe(func() bool { return true })
	manager.SetActivationHooks(s.switchRuntimeBefore, s.switchRuntimeProcesses)
	return s, cfg, manager, root, oldMarker, filepath.Join(root, "launched-"+lmcacheUpdateCandidate)
}

func lmcacheUpdateConfig(t *testing.T, s *Server, cfg config.Config) config.Config {
	t.Helper()
	port := healthyLMCacheEndpoint(t)
	cfg.LMCache.Server = config.LMCacheServerConfig{Enabled: true, HTTPHost: "127.0.0.1", HTTPPort: port}
	s.setConfig(cfg)
	return cfg
}

func stageLMCacheUpdateCandidate(t *testing.T, manager *runtimeManager.Manager, root string) {
	t.Helper()
	if _, err := manager.Stage(context.Background(), runtimeManager.Spec{
		Name: config.LMCacheRuntimeName, Kind: "lmcache", Mode: runtimeManager.RuntimeModeNative,
		Version: lmcacheUpdateCandidate, SourceType: "pypi", Source: "pypi",
	}); err != nil {
		t.Fatalf("stage %s: %v", lmcacheUpdateCandidate, err)
	}
	marker := filepath.Join(root, "launched-"+lmcacheUpdateCandidate)
	writeLMCacheVersionScript(t, root, lmcacheUpdateCandidate, marker)
}

func readLMCacheCurrent(t *testing.T, root string) string {
	t.Helper()
	target, err := os.Readlink(filepath.Join(root, config.LMCacheRuntimeName, "current"))
	if err != nil {
		t.Fatalf("read current pointer: %v", err)
	}
	return target
}

// requireMarkerRan polls for the version marker until it appears. The marker
// is written by the first line of the version's fake server script, which
// proves that exact venv executed. Polling (rather than a single check)
// absorbs environment launch latency: the supervised process can take a
// while to begin executing after it is spawned, while the state machine
// already reports it RUNNING.
func requireMarkerRan(t *testing.T, marker, version string) {
	t.Helper()
	var (
		data []byte
		err  error
	)
	deadline := time.Now().Add(30 * time.Second)
	for {
		data, err = os.ReadFile(marker)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server from version %s never ran (marker missing): %v", version, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if want := filepath.Join("versions", version, ".venv", "bin", "lmcache"); !strings.Contains(string(data), want) {
		t.Fatalf("marker = %q, want an execution from %s", strings.TrimSpace(string(data)), want)
	}
}

// TestLMCache_UpdateRefusedWhileModelsUseIt is scenario T8: with a model
// reference held, the Runtime Manager's activation of a new version is
// refused by the shared pre-activation guard, the pointer stays on the old
// version, and the serving process is untouched.
func TestLMCache_UpdateRefusedWhileModelsUseIt(t *testing.T) {
	s, cfg, manager, root, _, _ := lmcacheUpdateFixture(t)
	cfg = lmcacheUpdateConfig(t, s, cfg)
	if err := s.lmcacheMod.startServer(cfg); err != nil {
		t.Fatal(err)
	}
	defer s.lmcacheMod.stopServer() //nolint: errcheck
	if st := waitForLMCacheState(t, &s.lmcacheMod.proc, LMCacheStateRunning, 5*time.Second); st.State != LMCacheStateRunning {
		t.Fatalf("server = %+v, want running before the update attempt", st)
	}
	firstPID := s.lmcacheMod.proc.Status().PID
	stageLMCacheUpdateCandidate(t, manager, root)

	s.lmcacheMod.users.acquire("model")
	err := manager.Activate(context.Background(), config.LMCacheRuntimeName, lmcacheUpdateCandidate)
	if err == nil {
		t.Fatal("activation with an in-use model succeeded, want refusal")
	}
	if !errors.Is(err, errLMCacheInUse) {
		t.Fatalf("activation error = %v, want the in-use sentinel", err)
	}
	if !strings.Contains(err.Error(), "model") {
		t.Fatalf("activation error = %v, want the in-use model listed", err)
	}
	if got := readLMCacheCurrent(t, root); got != filepath.Join("versions", lmcacheFixtureServerVersion) {
		t.Fatalf("current pointer = %q, want the old version after the refused update", got)
	}
	if st := s.lmcacheMod.proc.Status(); st.PID != firstPID || st.State != LMCacheStateRunning {
		t.Fatalf("process after refused update = %+v, want the original server untouched", st)
	}
}

// TestLMCache_UpgradeSwitchesServerToNewVersion then rolls back: the update
// stops the server, swaps the pointer, and restarts it from the new venv;
// the rollback restores the old version, which must still start (scenario
// T10 for the derived server runtime).
func TestLMCache_UpgradeSwitchesServerToNewVersion(t *testing.T) {
	s, cfg, manager, root, oldMarker, newMarker := lmcacheUpdateFixture(t)
	cfg = lmcacheUpdateConfig(t, s, cfg)
	if err := s.lmcacheMod.startServer(cfg); err != nil {
		t.Fatal(err)
	}
	defer s.lmcacheMod.stopServer() //nolint: errcheck
	if st := waitForLMCacheState(t, &s.lmcacheMod.proc, LMCacheStateRunning, 5*time.Second); st.State != LMCacheStateRunning {
		t.Fatalf("server = %+v, want running before the upgrade", st)
	}
	firstPID := s.lmcacheMod.proc.Status().PID
	stageLMCacheUpdateCandidate(t, manager, root)

	if err := manager.Activate(context.Background(), config.LMCacheRuntimeName, lmcacheUpdateCandidate); err != nil {
		t.Fatalf("upgrade activation: %v", err)
	}
	if got := readLMCacheCurrent(t, root); got != filepath.Join("versions", lmcacheUpdateCandidate) {
		t.Fatalf("current pointer = %q, want the upgraded version", got)
	}
	st := waitForLMCacheState(t, &s.lmcacheMod.proc, LMCacheStateRunning, 5*time.Second)
	if st.State != LMCacheStateRunning || st.PID == firstPID {
		t.Fatalf("process after upgrade = %+v, want a new process serving the new version", st)
	}
	requireMarkerRan(t, newMarker, lmcacheUpdateCandidate)

	// Rollback must restore the old version and its venv must still be able
	// to serve (no half upgrade, no dead old runtime).
	if err := manager.Rollback(context.Background(), config.LMCacheRuntimeName); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := readLMCacheCurrent(t, root); got != filepath.Join("versions", lmcacheFixtureServerVersion) {
		t.Fatalf("current pointer after rollback = %q, want the old version", got)
	}
	st = waitForLMCacheState(t, &s.lmcacheMod.proc, LMCacheStateRunning, 5*time.Second)
	if st.State != LMCacheStateRunning || st.PID == firstPID {
		t.Fatalf("process after rollback = %+v, want the server restarted on the old version", st)
	}
	requireMarkerRan(t, oldMarker, lmcacheFixtureServerVersion)
}

// TestLMCache_UpgradeHealthFailureRestoresOldServer covers the fail-closed
// boundary: a candidate that exits before the real health wait must restore
// both pointers and the old serving process, while leaving the candidate
// staged for diagnosis/retry.
func TestLMCache_UpgradeHealthFailureRestoresOldServer(t *testing.T) {
	s, cfg, manager, root, _, _ := lmcacheUpdateFixture(t)
	healthVersion := filepath.Join(root, "health-version")
	healthServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthcheck" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		version, _ := os.ReadFile(healthVersion)
		if strings.TrimSpace(string(version)) != lmcacheFixtureServerVersion {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"unhealthy"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"healthy"}`))
	}))
	t.Cleanup(healthServer.Close)
	port := healthServer.Listener.Addr().(*net.TCPAddr).Port
	cfg.LMCache.Server = config.LMCacheServerConfig{Enabled: true, HTTPHost: "127.0.0.1", HTTPPort: port}
	s.setConfig(cfg)
	s.lmcacheMod.proc.readyTimeout = 300 * time.Millisecond
	oldScript := filepath.Join(root, config.LMCacheRuntimeName, "versions", lmcacheFixtureServerVersion, ".venv", "bin", "lmcache")
	if err := os.WriteFile(oldScript, []byte("#!/bin/sh\necho '"+lmcacheFixtureServerVersion+"' > '"+healthVersion+"'\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.lmcacheMod.startServer(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.lmcacheMod.stopServer() })
	if st := waitForLMCacheState(t, &s.lmcacheMod.proc, LMCacheStateRunning, 5*time.Second); st.State != LMCacheStateRunning {
		t.Fatalf("server = %+v, want running before the failed upgrade", st)
	}
	oldPID := s.lmcacheMod.proc.Status().PID
	stageLMCacheUpdateCandidate(t, manager, root)
	candidateScript := filepath.Join(root, config.LMCacheRuntimeName, "versions", lmcacheUpdateCandidate, ".venv", "bin", "lmcache")
	if err := os.WriteFile(candidateScript, []byte("#!/bin/sh\necho '"+lmcacheUpdateCandidate+"' > '"+healthVersion+"'\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Make the endpoint reject the candidate before its process is spawned;
	// otherwise the first health probe could observe the old marker during the
	// tiny process-start race and falsely accept the new server.
	if err := os.WriteFile(healthVersion, []byte(lmcacheUpdateCandidate), 0o600); err != nil {
		t.Fatal(err)
	}

	err := manager.Activate(context.Background(), config.LMCacheRuntimeName, lmcacheUpdateCandidate)
	if err == nil || !strings.Contains(err.Error(), "lmcache server") {
		t.Fatalf("failed upgrade error = %v, want a real server health/start failure", err)
	}
	if got := readLMCacheCurrent(t, root); got != filepath.Join("versions", lmcacheFixtureServerVersion) {
		t.Fatalf("current pointer after failed upgrade = %q, want the old version", got)
	}
	if _, err := os.Stat(filepath.Join(root, config.LMCacheRuntimeName, "versions", lmcacheUpdateCandidate, "manifest.json")); err != nil {
		t.Fatalf("failed candidate manifest disappeared: %v", err)
	}
	st := waitForLMCacheState(t, &s.lmcacheMod.proc, LMCacheStateRunning, 5*time.Second)
	if st.State != LMCacheStateRunning || st.PID == oldPID {
		t.Fatalf("server after failed upgrade = %+v, want old version restarted", st)
	}
}

// TestLMCache_UpdateWhileStoppedRestartsOnNewVersion covers the spec update
// flow ending condition: even a stopped server ends the update serving the
// new version.
func TestLMCache_UpdateWhileStoppedRestartsOnNewVersion(t *testing.T) {
	s, cfg, manager, root, _, newMarker := lmcacheUpdateFixture(t)
	lmcacheUpdateConfig(t, s, cfg)
	if st := s.lmcacheMod.proc.Status(); st.Running {
		t.Fatalf("fixture server = %+v, want stopped", st)
	}
	stageLMCacheUpdateCandidate(t, manager, root)
	if err := manager.Activate(context.Background(), config.LMCacheRuntimeName, lmcacheUpdateCandidate); err != nil {
		t.Fatalf("update activation: %v", err)
	}
	if got := readLMCacheCurrent(t, root); got != filepath.Join("versions", lmcacheUpdateCandidate) {
		t.Fatalf("current pointer = %q, want the upgraded version", got)
	}
	st := waitForLMCacheState(t, &s.lmcacheMod.proc, LMCacheStateRunning, 5*time.Second)
	if st.State != LMCacheStateRunning {
		t.Fatalf("process after update = %+v, want the server restarted on the new version", st)
	}
	requireMarkerRan(t, newMarker, lmcacheUpdateCandidate)
}

// TestLMCache_IdempotentActivationLeavesServerRunning is the regression test
// for the manager skipping the pre-activation hook on a re-activation of the
// current version: the serving process must not be restarted by it.
func TestLMCache_IdempotentActivationLeavesServerRunning(t *testing.T) {
	s, cfg, manager, _, _, _ := lmcacheUpdateFixture(t)
	cfg = lmcacheUpdateConfig(t, s, cfg)
	if err := s.lmcacheMod.startServer(cfg); err != nil {
		t.Fatal(err)
	}
	defer s.lmcacheMod.stopServer() //nolint: errcheck
	if st := waitForLMCacheState(t, &s.lmcacheMod.proc, LMCacheStateRunning, 5*time.Second); st.State != LMCacheStateRunning {
		t.Fatalf("server = %+v, want running before the re-activation", st)
	}
	firstPID := s.lmcacheMod.proc.Status().PID

	if err := manager.Activate(context.Background(), config.LMCacheRuntimeName, lmcacheFixtureServerVersion); err != nil {
		t.Fatalf("idempotent activation: %v", err)
	}
	if st := s.lmcacheMod.proc.Status(); st.PID != firstPID || st.State != LMCacheStateRunning {
		t.Fatalf("process after idempotent activation = %+v, want the same server still running", st)
	}
}

// TestServer_ProtectedRuntimeVersionProbe covers the deletion-protection
// probe answers: an in-progress lmcache update and in-use models protect
// every lmcache version; a per-model runtimeVersion pin protects the pinned
// version of the model runtime.
func TestServer_ProtectedRuntimeVersionProbe(t *testing.T) {
	s, cfg, _, _ := lmcacheServiceFixture(t)
	if ok, _ := s.protectedRuntimeVersion(config.LMCacheRuntimeName, lmcacheFixtureServerVersion); ok {
		t.Fatal("idle lmcache reported protected, want unprotected")
	}

	s.lmcacheMod.proc.setExternalState(LMCacheStateUpdating)
	if ok, reason := s.protectedRuntimeVersion(config.LMCacheRuntimeName, lmcacheFixtureServerVersion); !ok || !strings.Contains(reason, "update in progress") {
		t.Fatalf("updating lmcache = (%v, %q), want protected with the update reason", ok, reason)
	}
	s.lmcacheMod.proc.setExternalState(LMCacheStateStopped)

	s.lmcacheMod.users.acquire("model-a")
	if ok, reason := s.protectedRuntimeVersion(config.LMCacheRuntimeName, lmcacheFixtureServerVersion); !ok || !strings.Contains(reason, "model-a") {
		t.Fatalf("in-use lmcache = (%v, %q), want protected naming model-a", ok, reason)
	}
	s.lmcacheMod.users.release("model-a")

	model := cfg.Models["model"]
	model.Backend.RuntimeVersion = "1"
	cfg.Models["model"] = model
	s.setConfig(cfg)
	if ok, reason := s.protectedRuntimeVersion("vllm-a", "1"); !ok || !strings.Contains(reason, "model") {
		t.Fatalf("pinned vllm-a version = (%v, %q), want protected naming the model", ok, reason)
	}
	if ok, _ := s.protectedRuntimeVersion("vllm-a", "2"); ok {
		t.Fatal("unpinned vllm-a version reported protected, want unprotected")
	}
}

// TestServer_PinnedModelLaunchBindingKeepsStagedVersion is scenario T10 for
// the model runtimes: after an upgrade moves current to a new version, a
// model pinned to the old version still binds the old staged venv, the pin
// wins over a runtime-level switch target, and a pin to an uninstalled
// version fails closed.
func TestServer_PinnedModelLaunchBindingKeepsStagedVersion(t *testing.T) {
	root := t.TempDir()
	for _, v := range []string{"1.0.0", "2.0.0"} {
		writeRuntimeVersion(t, root, "vllm", runtimeManager.Manifest{
			Name: "vllm", Version: v, Kind: "vllm", Source: "pypi",
			Metadata: map[string]string{"mode": runtimeManager.RuntimeModeNative, "sourceType": "pypi"},
		})
	}
	// The upgrade has happened: current points at the new version.
	if err := os.Symlink(filepath.Join("versions", "2.0.0"), filepath.Join(root, "vllm", "current")); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		RuntimeManager: config.RuntimeManagerConfig{Root: root},
		Runtimes:       map[string]config.RuntimeConfig{"vllm": {Kind: "vllm"}},
		Models: map[string]config.ModelConfig{
			"pinned": {Backend: config.BackendConfig{
				Type: "vllm", Runtime: "vllm", RuntimeVersion: "1.0.0",
				Arguments: []string{"vllm", "serve", "pinned"},
			}},
			"latest": {Backend: config.BackendConfig{
				Type: "vllm", Runtime: "vllm",
				Arguments: []string{"vllm", "serve", "latest"},
			}},
		},
	}
	bound, err := bindManagedRuntimeLaunchConfig(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := bound.Models["pinned"].Backend.Arguments[0]; got != filepath.Join(root, "vllm", "versions", "1.0.0", ".venv", "bin", "vllm") {
		t.Fatalf("pinned executable = %q, want the old staged venv", got)
	}
	if got := bound.Models["latest"].Backend.Arguments[0]; got != filepath.Join(root, "vllm", "versions", "2.0.0", ".venv", "bin", "vllm") {
		t.Fatalf("unpinned executable = %q, want the current version's venv", got)
	}

	// A runtime-level switch target must not move the pinned model off its
	// version.
	bound, err = bindManagedRuntimeLaunchConfigForVersions(cfg, map[string]string{"vllm": "2.0.0"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := bound.Models["pinned"].Backend.Arguments[0]; got != filepath.Join(root, "vllm", "versions", "1.0.0", ".venv", "bin", "vllm") {
		t.Fatalf("pinned executable under a switch target = %q, want the pin to win", got)
	}

	pinned := cfg.Models["pinned"]
	pinned.Backend.RuntimeVersion = "9.9.9"
	cfg.Models["pinned"] = pinned
	if _, err := bindManagedRuntimeLaunchConfig(cfg, nil); err == nil || !strings.Contains(err.Error(), "9.9.9") {
		t.Fatalf("pin to an uninstalled version: err = %v, want a fail-closed error naming it", err)
	}
}
