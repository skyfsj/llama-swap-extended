package server

import (
	"context"
	"encoding/json"
	"errors"
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

type lmcacheMetadataCheckProvider struct {
	runtimeSwitchLifecycleProvider
}

func (lmcacheMetadataCheckProvider) CheckForUpdate(_ context.Context, _ runtimeManager.Manifest, desired runtimeManager.Spec, _ runtimeManager.UpdatePolicy) (runtimeManager.Spec, bool, error) {
	desired.Version = "0.5.6"
	return desired, true, nil
}

// postJSONUpdate sends one /api/lmcache/update body to the handler and
// returns the recorded response, so each API step is exercised exactly the
// way the UI drives it — a direct call, no router in between.
func postLMCacheUpdate(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/lmcache/update", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.handleAPILMCacheUpdate(rec, req)
	return rec
}

// postLMCacheRestart calls the restart endpoint the same direct way.
func postLMCacheRestart(t *testing.T, s *Server) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/lmcache/server/restart", nil)
	s.handleAPILMCacheServerRestart(rec, req)
	return rec
}

func postLMCacheCheck(t *testing.T, s *Server) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/lmcache/check", nil)
	s.handleAPILMCacheCheck(rec, req)
	return rec
}

// TestLMCache_CheckAPIReportsArtifactHealthAndRemoteVersion exercises the
// explicit check contract: it verifies the current artifact, probes the real
// server endpoint, and persists the configured provider's candidate without
// staging or activating it.
func TestLMCache_CheckAPIReportsArtifactHealthAndRemoteVersion(t *testing.T) {
	s, cfg, manager, root, _, _ := lmcacheUpdateFixture(t)
	cfg = lmcacheUpdateConfig(t, s, cfg)
	s.runtime = manager
	if err := manager.SetProvider(config.LMCacheRuntimeName, lmcacheMetadataCheckProvider{}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Configure(config.LMCacheRuntimeName, runtimeManager.Spec{
		Name: config.LMCacheRuntimeName, Kind: "lmcache", SourceType: "pypi", Source: "pypi",
	}, runtimeManager.UpdatePolicy{Policy: "manual", Channel: "stable"}); err != nil {
		t.Fatal(err)
	}
	startRunningLMCacheServer(t, s, cfg)

	rec := postLMCacheCheck(t, s)
	if rec.Code != http.StatusOK {
		t.Fatalf("check: status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
	var payload struct {
		Server struct {
			Healthy         bool   `json:"healthy"`
			HealthCheckedAt string `json:"healthCheckedAt"`
		} `json:"server"`
		Update struct {
			Current   string `json:"current"`
			Available string `json:"available"`
			LastCheck string `json:"lastCheck"`
			LastError string `json:"lastError"`
			Channel   string `json:"channel"`
			Policy    string `json:"policy"`
		} `json:"update"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode check response: %v", err)
	}
	if !payload.Server.Healthy || payload.Server.HealthCheckedAt == "" {
		t.Fatalf("server health = %+v, want a real healthy probe timestamp", payload.Server)
	}
	if payload.Update.Current != lmcacheFixtureServerVersion || payload.Update.Available != "0.5.6" || payload.Update.LastCheck == "" {
		t.Fatalf("update = %+v, want current=%s available=0.5.6 and lastCheck", payload.Update, lmcacheFixtureServerVersion)
	}
	if payload.Update.Channel != "stable" || payload.Update.Policy != "manual" || payload.Update.LastError != "" {
		t.Fatalf("update policy/channel/error = %s/%s/%q, want manual/stable/empty", payload.Update.Policy, payload.Update.Channel, payload.Update.LastError)
	}
	if got := readLMCacheCurrent(t, root); got != filepath.Join("versions", lmcacheFixtureServerVersion) {
		t.Fatalf("current pointer after check = %q, want unchanged", got)
	}
}

// TestLMCache_UpgradeAPIOneClickRequiresHealthyCandidate proves that the
// one-click action returns the complete status only after activation and the
// new supervised server are healthy.
func TestLMCache_UpgradeAPIOneClickRequiresHealthyCandidate(t *testing.T) {
	s, cfg, manager, root, _, _ := lmcacheUpdateFixture(t)
	cfg = lmcacheUpdateConfig(t, s, cfg)
	s.runtime = manager
	startRunningLMCacheServer(t, s, cfg)
	stageLMCacheUpdateCandidate(t, manager, root)

	rec := postLMCacheUpdate(t, s, `{"action":"upgrade","version":"`+lmcacheUpdateCandidate+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("upgrade: status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
	var payload struct {
		Server struct {
			Running bool `json:"running"`
			Healthy bool `json:"healthy"`
		} `json:"server"`
		Update struct {
			Current string `json:"current"`
		} `json:"update"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode upgrade response: %v", err)
	}
	if payload.Update.Current != lmcacheUpdateCandidate || !payload.Server.Running || !payload.Server.Healthy {
		t.Fatalf("upgrade result = %+v, want candidate active and healthy", payload)
	}
}

// startRunningLMCacheServer starts the fixture server and waits for RUNNING.
// Tests that mutate the process afterwards keep the returned PID to prove
// the process was (or was not) replaced.
func startRunningLMCacheServer(t *testing.T, s *Server, cfg config.Config) int {
	t.Helper()
	if err := s.lmcacheMod.startServer(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.lmcacheMod.stopServer() })
	if st := waitForLMCacheState(t, &s.lmcacheMod.proc, LMCacheStateRunning, 15*time.Second); st.State != LMCacheStateRunning {
		t.Fatalf("server = %+v, want running", st)
	}
	return s.lmcacheMod.proc.Status().PID
}

// TestLMCache_StatusContractCarriesServerIdentity pins the GET /api/lmcache
// contract the Runtimes card renders: the seven-state enum, the version and
// venv of the dedicated server runtime, the log path, the live using-models
// list from the tracker, and the process identity fields. The fields are
// checked through the JSON wire format, not the Go struct, so a renamed tag
// fails here instead of surfacing as a silent UI gap.
func TestLMCache_StatusContractCarriesServerIdentity(t *testing.T) {
	s, cfg, _, root := lmcacheServiceFixture(t)
	cfg = lmcacheUpdateConfig(t, s, cfg)
	startRunningLMCacheServer(t, s, cfg)

	rec := httptest.NewRecorder()
	s.handleAPILMCacheStatus(rec, httptest.NewRequest(http.MethodGet, "/api/lmcache", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
	var payload struct {
		UsingModels    []string `json:"usingModels"`
		PendingRestart bool     `json:"pendingRestart"`
		Server         struct {
			State     string `json:"state"`
			Version   string `json:"version"`
			VenvPath  string `json:"venvPath"`
			LogPath   string `json:"logPath"`
			PID       int    `json:"pid"`
			StartedAt string `json:"startedAt"`
			LastError string `json:"lastError"`
		} `json:"server"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if payload.Server.State != string(LMCacheStateRunning) {
		t.Fatalf("state = %q, want %q", payload.Server.State, LMCacheStateRunning)
	}
	if payload.Server.Version != lmcacheFixtureServerVersion {
		t.Fatalf("version = %q, want %q", payload.Server.Version, lmcacheFixtureServerVersion)
	}
	wantVenv := filepath.Join(root, config.LMCacheRuntimeName, "versions", lmcacheFixtureServerVersion, ".venv")
	if payload.Server.VenvPath != wantVenv {
		t.Fatalf("venvPath = %q, want the dedicated server venv %q", payload.Server.VenvPath, wantVenv)
	}
	wantLog := filepath.Join(root, config.LMCacheRuntimeName, "server.log")
	if payload.Server.LogPath != wantLog {
		t.Fatalf("logPath = %q, want %q", payload.Server.LogPath, wantLog)
	}
	if payload.Server.PID <= 0 || payload.Server.StartedAt == "" {
		t.Fatalf("pid/startedAt = %d/%q, want a live process identity", payload.Server.PID, payload.Server.StartedAt)
	}
	if len(payload.UsingModels) != 0 || payload.PendingRestart {
		t.Fatalf("usingModels/pendingRestart = %v/%v, want none while idle", payload.UsingModels, payload.PendingRestart)
	}

	// The using list is the live tracker, not the config: acquiring a
	// reference must show up in the next status without any config change.
	s.lmcacheMod.users.acquire("model")
	t.Cleanup(func() { s.lmcacheMod.users.release("model") })
	rec = httptest.NewRecorder()
	s.handleAPILMCacheStatus(rec, httptest.NewRequest(http.MethodGet, "/api/lmcache", nil))
	var second struct {
		UsingModels []string `json:"usingModels"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode second status: %v", err)
	}
	if len(second.UsingModels) != 1 || second.UsingModels[0] != "model" {
		t.Fatalf("usingModels = %v, want [model] after the tracker acquire", second.UsingModels)
	}
}

// TestLMCache_RestartAPIRefusedWhileModelUsesIt is the API-side half of the
// dependency guard: the UI disables the restart button while a model runs,
// but the authority is the backend — a direct call that skips the disabled
// button must still get 409 with the model list, and the process untouched.
func TestLMCache_RestartAPIRefusedWhileModelUsesIt(t *testing.T) {
	s, cfg, _, _ := lmcacheServiceFixture(t)
	cfg = lmcacheUpdateConfig(t, s, cfg)
	firstPID := startRunningLMCacheServer(t, s, cfg)

	s.lmcacheMod.users.acquire("model")
	t.Cleanup(func() { s.lmcacheMod.users.release("model") })

	rec := postLMCacheRestart(t, s)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body = %s, want 409", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model") {
		t.Fatalf("body = %s, want the in-use model listed", rec.Body.String())
	}
	if st := s.lmcacheMod.proc.Status(); st.PID != firstPID || st.State != LMCacheStateRunning {
		t.Fatalf("process after refused restart = %+v, want the original server untouched", st)
	}
}

// TestLMCache_RestartAPIReplacesProcess applies a pending restart-class
// change: the endpoint stops and starts the server, the new process serves
// the same version's venv, and the pending flag clears on success.
func TestLMCache_RestartAPIReplacesProcess(t *testing.T) {
	s, cfg, _, root := lmcacheServiceFixture(t)
	marker := filepath.Join(root, "launched-"+lmcacheFixtureServerVersion)
	_ = writeActiveLMCacheRuntime(t, root, lmcacheFixtureServerVersion, fakeLMCacheServerScript(marker))
	cfg = lmcacheUpdateConfig(t, s, cfg)
	firstPID := startRunningLMCacheServer(t, s, cfg)
	s.lmcacheMod.pendingRestart.Store(true)

	rec := postLMCacheRestart(t, s)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body = %s, want 204", rec.Code, rec.Body.String())
	}
	st := waitForLMCacheState(t, &s.lmcacheMod.proc, LMCacheStateRunning, 5*time.Second)
	if st.State != LMCacheStateRunning || st.PID == firstPID {
		t.Fatalf("process after restart = %+v, want a fresh process", st)
	}
	if s.lmcacheMod.pendingRestart.Load() {
		t.Fatal("pendingRestart still set after a successful restart")
	}
	requireMarkerRan(t, marker, lmcacheFixtureServerVersion)
}

// TestLMCache_RestartAPIColdStartIsAllowed covers the crash-recovery path:
// with no server running, restart must behave as a start instead of
// refusing.
func TestLMCache_RestartAPIColdStartIsAllowed(t *testing.T) {
	s, cfg, _, root := lmcacheServiceFixture(t)
	_ = writeActiveLMCacheRuntime(t, root, lmcacheFixtureServerVersion, "#!/bin/sh\nexec sleep 30\n")
	lmcacheUpdateConfig(t, s, cfg)
	if st := s.lmcacheMod.proc.Status(); st.Running {
		t.Fatalf("fixture server = %+v, want stopped", st)
	}

	rec := postLMCacheRestart(t, s)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body = %s, want 204", rec.Code, rec.Body.String())
	}
	if st := waitForLMCacheState(t, &s.lmcacheMod.proc, LMCacheStateRunning, 5*time.Second); st.State != LMCacheStateRunning {
		t.Fatalf("process after cold restart = %+v, want running", st)
	}
}

// TestLMCache_RestartErrorIncludesLogTail pins the spec-25 error contract:
// a failed restart returns the log path plus the most recent log lines, so
// the failure is diagnosable without leaving the UI.
func TestLMCache_RestartErrorIncludesLogTail(t *testing.T) {
	s, cfg, _, root := lmcacheServiceFixture(t)
	lmcacheUpdateConfig(t, s, cfg)
	logPath := filepath.Join(root, config.LMCacheRuntimeName, "server.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("boom: lmcache failed to bind 0.0.0.0:5555\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	// Remove the console script so the restart fails at resolution, before
	// any process could clear the log.
	executable := filepath.Join(root, config.LMCacheRuntimeName, "versions", lmcacheFixtureServerVersion, ".venv", "bin", "lmcache")
	if err := os.Remove(executable); err != nil {
		t.Fatal(err)
	}

	rec := postLMCacheRestart(t, s)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body = %s, want 502", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "log: "+logPath) {
		t.Fatalf("body = %s, want the server log path", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "boom: lmcache failed to bind") {
		t.Fatalf("body = %s, want the recent log lines", rec.Body.String())
	}
}

// TestLMCache_UpdateAPIRefusedWhileModelsUseIt is the T8 API-side proof:
// while a model references the server, every update step — stage, activate,
// rollback — is refused with 409 and the serving process is untouched. The
// UI's disabled buttons are a convenience; this endpoint is the authority.
func TestLMCache_UpdateAPIRefusedWhileModelsUseIt(t *testing.T) {
	s, cfg, manager, root, _, _ := lmcacheUpdateFixture(t)
	cfg = lmcacheUpdateConfig(t, s, cfg)
	s.runtime = manager
	firstPID := startRunningLMCacheServer(t, s, cfg)
	stageLMCacheUpdateCandidate(t, manager, root)

	s.lmcacheMod.users.acquire("model")
	t.Cleanup(func() { s.lmcacheMod.users.release("model") })

	for _, body := range []string{
		`{"action":"stage","version":"` + lmcacheUpdateCandidate + `"}`,
		`{"action":"activate","version":"` + lmcacheUpdateCandidate + `"}`,
		`{"action":"upgrade","version":"` + lmcacheUpdateCandidate + `"}`,
		`{"action":"rollback"}`,
	} {
		rec := postLMCacheUpdate(t, s, body)
		if rec.Code != http.StatusConflict {
			t.Fatalf("%s: status = %d body = %s, want 409", body, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "model") {
			t.Fatalf("%s: body = %s, want the in-use model listed", body, rec.Body.String())
		}
	}
	if got := readLMCacheCurrent(t, root); got != filepath.Join("versions", lmcacheFixtureServerVersion) {
		t.Fatalf("current pointer = %q, want the old version after the refused update", got)
	}
	if st := s.lmcacheMod.proc.Status(); st.PID != firstPID || st.State != LMCacheStateRunning {
		t.Fatalf("process after refused update = %+v, want the original server untouched", st)
	}
}

// TestLMCache_UpgradeAPIRefusedImmediatelyWhenControlPlaneBusy keeps the
// one-click path from staging or resolving a remote candidate while another
// control-plane operation is active. Manual upgrade is an immediate 409; the
// automatic loop is the path that waits and retries later.
func TestLMCache_UpgradeAPIRefusedImmediatelyWhenControlPlaneBusy(t *testing.T) {
	s, cfg, manager, root, _, _ := lmcacheUpdateFixture(t)
	lmcacheUpdateConfig(t, s, cfg)
	s.runtime = manager
	s.controlPlaneActive.Store(1)
	t.Cleanup(func() { s.controlPlaneActive.Store(0) })

	rec := postLMCacheUpdate(t, s, `{"action":"upgrade","version":"`+lmcacheUpdateCandidate+`"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body = %s, want 409", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "busy") {
		t.Fatalf("body = %s, want a busy diagnostic", rec.Body.String())
	}
	if got := readLMCacheCurrent(t, root); got != filepath.Join("versions", lmcacheFixtureServerVersion) {
		t.Fatalf("current pointer = %q, want unchanged old version", got)
	}
}

// TestLMCache_UpdateAPIStageThenActivateThenRollback drives the whole
// versioned update flow through the API the UI will: stage downloads the
// candidate without touching the running server, a second stage is a cheap
// no-op, activate swaps the pointer and restarts the server on the new
// venv, and rollback restores the old version, which must still serve.
func TestLMCache_UpdateAPIStageThenActivateThenRollback(t *testing.T) {
	s, cfg, manager, root, oldMarker, newMarker := lmcacheUpdateFixture(t)
	cfg = lmcacheUpdateConfig(t, s, cfg)
	s.runtime = manager
	firstPID := startRunningLMCacheServer(t, s, cfg)

	rec := postLMCacheUpdate(t, s, `{"action":"stage","version":"`+lmcacheUpdateCandidate+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("stage: status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
	var staged struct {
		Version string `json:"staged"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &staged); err != nil || staged.Version != lmcacheUpdateCandidate {
		t.Fatalf("stage body = %s, want %s reported as staged", rec.Body.String(), lmcacheUpdateCandidate)
	}
	if got := readLMCacheCurrent(t, root); got != filepath.Join("versions", lmcacheFixtureServerVersion) {
		t.Fatalf("current pointer after stage = %q, want the old version (stage must not apply)", got)
	}
	if st := s.lmcacheMod.proc.Status(); st.PID != firstPID || st.State != LMCacheStateRunning {
		t.Fatalf("process after stage = %+v, want the original server untouched", st)
	}
	// The no-op provider leaves an empty version directory; install the
	// console script the way a real stage would before the re-stage no-op.
	writeLMCacheVersionScript(t, root, lmcacheUpdateCandidate, newMarker)

	rec = postLMCacheUpdate(t, s, `{"action":"stage","version":"`+lmcacheUpdateCandidate+`"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), lmcacheUpdateCandidate) {
		t.Fatalf("re-stage: status = %d body = %s, want the installed version reported as-is", rec.Code, rec.Body.String())
	}

	rec = postLMCacheUpdate(t, s, `{"action":"activate","version":"`+lmcacheUpdateCandidate+`"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("activate: status = %d body = %s, want 204", rec.Code, rec.Body.String())
	}
	if got := readLMCacheCurrent(t, root); got != filepath.Join("versions", lmcacheUpdateCandidate) {
		t.Fatalf("current pointer after activate = %q, want the new version", got)
	}
	st := waitForLMCacheState(t, &s.lmcacheMod.proc, LMCacheStateRunning, 5*time.Second)
	if st.State != LMCacheStateRunning || st.PID == firstPID {
		t.Fatalf("process after activate = %+v, want a new process on the new version", st)
	}
	requireMarkerRan(t, newMarker, lmcacheUpdateCandidate)

	rec = postLMCacheUpdate(t, s, `{"action":"rollback"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("rollback: status = %d body = %s, want 204", rec.Code, rec.Body.String())
	}
	if got := readLMCacheCurrent(t, root); got != filepath.Join("versions", lmcacheFixtureServerVersion) {
		t.Fatalf("current pointer after rollback = %q, want the old version", got)
	}
	st = waitForLMCacheState(t, &s.lmcacheMod.proc, LMCacheStateRunning, 5*time.Second)
	if st.State != LMCacheStateRunning {
		t.Fatalf("process after rollback = %+v, want the server restarted on the old version", st)
	}
	requireMarkerRan(t, oldMarker, lmcacheFixtureServerVersion)
}

// TestLMCache_UpdateAPIRejectsMalformedRequests pins the 400 contract:
// activate requires a version, unknown actions are rejected, and a missing
// or broken body is a client error — never a silent default.
func TestLMCache_UpdateAPIRejectsMalformedRequests(t *testing.T) {
	s, cfg, manager, _, _, _ := lmcacheUpdateFixture(t)
	lmcacheUpdateConfig(t, s, cfg)
	s.runtime = manager

	cases := []string{
		`{"action":"activate"}`,
		`{"action":"wipe","version":"` + lmcacheUpdateCandidate + `"}`,
		`not json`,
	}
	for _, body := range cases {
		rec := postLMCacheUpdate(t, s, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d body = %s, want 400", body, rec.Code, rec.Body.String())
		}
	}
}

// TestLMCache_UpdateAPIStageResolvesConfiguredPin is the version-resolution
// contract: an omitted stage version falls back to the configured pin
// (lmcache.version) instead of silently re-resolving the newest release.
func TestLMCache_UpdateAPIStageResolvesConfiguredPin(t *testing.T) {
	s, cfg, manager, root, _, _ := lmcacheUpdateFixture(t)
	cfg = lmcacheUpdateConfig(t, s, cfg)
	cfg.LMCache.Version = lmcacheUpdateCandidate
	s.setConfig(cfg)
	s.runtime = manager
	startRunningLMCacheServer(t, s, cfg)

	rec := postLMCacheUpdate(t, s, `{"action":"stage"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("stage: status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"staged":"`+lmcacheUpdateCandidate+`"`) {
		t.Fatalf("stage body = %s, want the configured pin %s reported as staged", rec.Body.String(), lmcacheUpdateCandidate)
	}
	if _, err := os.Stat(filepath.Join(root, config.LMCacheRuntimeName, "versions", lmcacheUpdateCandidate, "manifest.json")); err != nil {
		t.Fatalf("staged version missing on disk: %v", err)
	}
}

// TestLMCache_DiagnoseStartErrorFormatsLogTail is the unit contract behind
// the API error body: with a log file the message carries the path and the
// recent lines; without one it degrades to the plain error.
func TestLMCache_DiagnoseStartErrorFormatsLogTail(t *testing.T) {
	s, cfg, _, root := lmcacheServiceFixture(t)
	err := errors.New("start lmcache server: spawn failed")

	if got := s.lmcacheMod.diagnoseStartError(err, cfg); got != err.Error() {
		t.Fatalf("diagnose without log = %q, want the plain error", got)
	}

	logPath := filepath.Join(root, config.LMCacheRuntimeName, "server.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("last-line-marker-xyz\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	got := s.lmcacheMod.diagnoseStartError(err, cfg)
	if !strings.Contains(got, err.Error()) || !strings.Contains(got, "log: "+logPath) || !strings.Contains(got, "last-line-marker-xyz") {
		t.Fatalf("diagnose with log = %q, want error + log path + recent lines", got)
	}
}
