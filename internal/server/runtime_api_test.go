package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/backend"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
	runtimeManager "github.com/mostlygeek/llama-swap/internal/runtime"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

type backendActionAdapter struct {
	sleepLevel int
	woke       bool
	reset      bool
}

type sleepAwareRouter struct {
	*stubRouter
	sleeping  bool
	sleepCall int
	wakeCall  int
}

func (r *sleepAwareRouter) ModelSleeping(string) bool { return r.sleeping }

func (r *sleepAwareRouter) SleepModel(ctx context.Context, _ string, _ int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.sleepCall++
	r.sleeping = true
	return nil
}

func (r *sleepAwareRouter) WakeModel(ctx context.Context, _ string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.wakeCall++
	r.sleeping = false
	return nil
}

type blockingRuntimeProvider struct {
	started chan struct{}
	release chan struct{}
}

type catalogAPIProvider struct{}

func (p *blockingRuntimeProvider) Stage(_ context.Context, spec runtimeManager.Spec, _ string) (runtimeManager.Manifest, error) {
	close(p.started)
	<-p.release
	return runtimeManager.Manifest{Name: spec.Name, Version: spec.Version, Kind: spec.Kind, Source: spec.Source}, nil
}

func (*blockingRuntimeProvider) Verify(context.Context, runtimeManager.Manifest, string) error {
	return nil
}

func (*blockingRuntimeProvider) Health(context.Context, runtimeManager.Manifest, string) error {
	return nil
}

func (catalogAPIProvider) Stage(_ context.Context, spec runtimeManager.Spec, _ string) (runtimeManager.Manifest, error) {
	return runtimeManager.Manifest{Name: spec.Name, Version: spec.Version, Kind: spec.Kind, Source: spec.Source}, nil
}

func (catalogAPIProvider) Verify(context.Context, runtimeManager.Manifest, string) error { return nil }
func (catalogAPIProvider) Health(context.Context, runtimeManager.Manifest, string) error { return nil }
func (catalogAPIProvider) ListVersions(context.Context, runtimeManager.Spec, runtimeManager.UpdatePolicy) ([]runtimeManager.VersionCandidate, error) {
	return []runtimeManager.VersionCandidate{{Version: "2.0.0", Recommended: true}}, nil
}

func (a *backendActionAdapter) Name() string { return "test" }
func (a *backendActionAdapter) Capabilities(context.Context) (backend.CapabilitySet, error) {
	return backend.CapabilitySet{"chat": true}, nil
}
func (a *backendActionAdapter) TransformRequest(context.Context, string, backend.RequestTransform) (backend.RequestTransform, error) {
	return backend.RequestTransform{}, nil
}
func (a *backendActionAdapter) TransformResponse(context.Context, string, []byte, http.Header) ([]byte, error) {
	return nil, nil
}
func (a *backendActionAdapter) CacheState(context.Context) (backend.CacheState, error) {
	return backend.CacheState{Supported: true}, nil
}
func (a *backendActionAdapter) ResetCache(context.Context) error {
	a.reset = true
	return nil
}
func (a *backendActionAdapter) Sleep(_ context.Context, level int) error {
	a.sleepLevel = level
	return nil
}
func (a *backendActionAdapter) Wake(context.Context) error {
	a.woke = true
	return nil
}
func (a *backendActionAdapter) Progress(context.Context) (backend.Progress, error) {
	return backend.Progress{Phase: "idle", Completed: 1, Total: 1}, nil
}

func TestServer_RuntimeOperationContextHonorsConfiguredTimeout(t *testing.T) {
	s := &Server{cfg: config.Config{RuntimeManager: config.RuntimeManagerConfig{OperationTimeout: 10 * time.Millisecond}}}
	r := httptest.NewRequest(http.MethodPost, "/api/runtimes/vllm/check", nil)
	ctx, cancel := s.runtimeOperationContext(r)
	defer cancel()
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("context error = %v, want deadline exceeded", ctx.Err())
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("runtime operation context did not expire")
	}
}

func TestServer_RuntimeOperationContextKeepsRequestContextWhenUnset(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	s := &Server{}
	r := httptest.NewRequest(http.MethodPost, "/api/runtimes/vllm/check", nil).WithContext(parent)
	ctx, cancel := s.runtimeOperationContext(r)
	defer cancel()
	cancelParent()
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("context error = %v, want cancellation", ctx.Err())
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("request context cancellation was not propagated")
	}
}

func TestServer_StageOperationContextHonorsConfiguredTimeout(t *testing.T) {
	s := &Server{cfg: config.Config{RuntimeManager: config.RuntimeManagerConfig{OperationTimeout: 10 * time.Millisecond}}, shutdownCtx: context.Background()}
	ctx, cancel := s.stageOperationContext()
	defer cancel()
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("context error = %v, want deadline exceeded", ctx.Err())
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("stage operation context did not expire")
	}
}

// TestServer_StageOperationContextCancelledByShutdown verifies a stage
// started through the API does not outlive daemon shutdown.
func TestServer_StageOperationContextCancelledByShutdown(t *testing.T) {
	shutdownCtx, shutdown := context.WithCancel(context.Background())
	s := &Server{cfg: config.Config{RuntimeManager: config.RuntimeManagerConfig{OperationTimeout: time.Hour}}, shutdownCtx: shutdownCtx}
	ctx, cancel := s.stageOperationContext()
	defer cancel()
	shutdown()
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("context error = %v, want cancellation", ctx.Err())
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("shutdown cancellation was not propagated to the stage context")
	}
}

func TestServer_StageOperationContextIsNotBoundToARequestLifetime(t *testing.T) {
	s := &Server{cfg: config.Config{RuntimeManager: config.RuntimeManagerConfig{OperationTimeout: time.Hour}}, shutdownCtx: context.Background()}
	ctx, cancel := s.stageOperationContext()
	defer cancel()
	select {
	case <-ctx.Done():
		t.Fatalf("stage context was cancelled without any request in scope: %v", ctx.Err())
	case <-time.After(100 * time.Millisecond):
	}
}

func TestServer_RuntimeStageContinuesAfterClientDisconnect(t *testing.T) {
	provider := &blockingRuntimeProvider{started: make(chan struct{}), release: make(chan struct{})}
	manager, err := runtimeManager.NewManager(t.TempDir(), map[string]runtimeManager.Provider{"blocking": provider})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: config.Config{RuntimeManager: config.RuntimeManagerConfig{OperationTimeout: time.Hour}}, runtime: manager, shutdownCtx: context.Background()}

	body := `{"kind":"blocking","version":"1","source":"fixture"}`
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodPost, "/api/runtimes/vllm/stage", strings.NewReader(body)).WithContext(requestCtx)
	r.SetPathValue("name", "vllm")
	w := httptest.NewRecorder()

	done := make(chan int, 1)
	go func() {
		s.handleAPIRuntimeStage(w, r)
		done <- w.Code
	}()
	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("handler never reached the provider stage")
	}
	cancelRequest() // the client stops waiting; the stage must keep running
	close(provider.release)

	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("stage status=%d after client disconnect, want 200", code)
		}
	case <-time.After(time.Second):
		t.Fatal("stage did not complete after client disconnect")
	}
}

func TestServer_RuntimeStageRequestSupportsRepositoryAndContainerAliases(t *testing.T) {
	request := runtimeStageRequest{
		Kind: "vllm", Repository: "https://github.com/1CatAI/1Cat-vLLM.git", TrackRef: "v1.2.0",
		BuildEnv: map[string]string{"CUDA_HOME": "/usr/local/cuda"},
	}
	if request.Repository == "" || request.TrackRef == "" || request.BuildEnv["CUDA_HOME"] == "" {
		t.Fatalf("runtime stage aliases not represented: %+v", request)
	}
}

func TestServer_RuntimeStageSpecFromRequestUsesConfiguredDefinition(t *testing.T) {
	manager, err := runtimeManager.NewManager(t.TempDir(), map[string]runtimeManager.Provider{"test": runtimeSwitchLifecycleProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Configure("demo", runtimeManager.Spec{
		Name: "demo", Kind: "test", Mode: runtimeManager.RuntimeModeNative,
		Version: "old", SourceType: "git", Source: "https://github.com/example/demo.git", Ref: "main",
		Build: map[string]string{"backend": "cuda"}, BuildEnv: map[string]string{"CUDA_HOME": "/opt/cuda"},
	}, runtimeManager.UpdatePolicy{Policy: "manual"}); err != nil {
		t.Fatal(err)
	}
	server := &Server{runtime: manager}
	commit := strings.Repeat("a", 40)
	spec := server.runtimeStageSpecFromRequest("demo", runtimeStageRequest{Version: "git-" + commit})
	if spec.Name != "demo" || spec.Kind != "test" || spec.Source != "https://github.com/example/demo.git" || spec.Ref != commit || spec.Commit != commit {
		t.Fatalf("selected version did not preserve configured source: %+v", spec)
	}
	if spec.Build["backend"] != "cuda" || spec.BuildEnv["CUDA_HOME"] != "/opt/cuda" {
		t.Fatalf("selected version lost configured build settings: %+v", spec)
	}
	digest := "sha256:" + strings.Repeat("b", 64)
	digestSpec := server.runtimeStageSpecFromRequest("demo", runtimeStageRequest{Version: "image-" + strings.Repeat("b", 64), Digest: digest})
	if digestSpec.Metadata["expectedImageDigest"] != digest || digestSpec.Metadata["resolvedImageDigest"] != digest {
		t.Fatalf("selected image digest was not carried into stage spec: %+v", digestSpec.Metadata)
	}
}

func TestServer_RuntimeVersionCatalogEndpointReturnsProviderCandidates(t *testing.T) {
	manager, err := runtimeManager.NewManager(t.TempDir(), map[string]runtimeManager.Provider{"catalog": catalogAPIProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Configure("demo", runtimeManager.Spec{
		Name: "demo", Kind: "catalog", SourceType: "git", Source: "https://github.com/example/demo.git", Ref: "main",
	}, runtimeManager.UpdatePolicy{Policy: "manual"}); err != nil {
		t.Fatal(err)
	}
	server := &Server{runtime: manager}
	request := httptest.NewRequest(http.MethodGet, "/api/runtimes/demo/versions", nil)
	request.SetPathValue("name", "demo")
	response := httptest.NewRecorder()
	server.handleAPIRuntimeVersions(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("catalog status=%d body=%q", response.Code, response.Body.String())
	}
	var payload struct {
		Data      []runtimeManager.VersionCandidate `json:"data"`
		Source    string                            `json:"sourceType"`
		Supported bool                              `json:"supported"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Supported || payload.Source != "git" || len(payload.Data) != 1 || payload.Data[0].Version != "2.0.0" {
		t.Fatalf("catalog payload=%+v", payload)
	}
}

func TestServer_RuntimeDeleteVersionProtectsCurrentAndRemovesUnreferenced(t *testing.T) {
	manager, err := runtimeManager.NewManager(t.TempDir(), map[string]runtimeManager.Provider{"test": runtimeSwitchLifecycleProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	spec := runtimeManager.Spec{Name: "demo", Kind: "test", Version: "1", Source: "fixture"}
	if _, err := manager.Stage(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	server := &Server{runtime: manager}
	request := httptest.NewRequest(http.MethodDelete, "/api/runtimes/demo/versions/1", nil)
	request.SetPathValue("name", "demo")
	request.SetPathValue("version", "1")
	response := httptest.NewRecorder()
	server.handleAPIRuntimeDeleteVersion(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%q", response.Code, response.Body.String())
	}
	detail, ok, err := manager.Detail("demo")
	if err != nil || !ok || len(detail.Versions) != 0 {
		t.Fatalf("detail after delete = %+v found=%v err=%v", detail, ok, err)
	}

	if _, err := manager.Stage(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if err := manager.Activate(context.Background(), "demo", "1"); err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodDelete, "/api/runtimes/demo/versions/1", nil)
	request.SetPathValue("name", "demo")
	request.SetPathValue("version", "1")
	response = httptest.NewRecorder()
	server.handleAPIRuntimeDeleteVersion(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("current version delete status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestServer_RuntimeAPIIsAvailableWithoutEnableFlag(t *testing.T) {
	manager, err := runtimeManager.NewManager(t.TempDir(), runtimeProvidersForConfig(config.Config{}))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: config.Config{}, runtime: manager}
	w := httptest.NewRecorder()
	s.handleAPIRuntimes(w, httptest.NewRequest(http.MethodGet, "/api/runtimes", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"data":[]`) {
		t.Fatalf("runtime list status=%d body=%q", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"enabled"`) {
		t.Fatalf("runtime list still exposes an enable switch: %q", w.Body.String())
	}
}

func TestServer_RuntimeAPIReportsUnavailableInsteadOfDisabled(t *testing.T) {
	s := &Server{}
	w := httptest.NewRecorder()
	s.handleAPIRuntimes(w, httptest.NewRequest(http.MethodGet, "/api/runtimes", nil))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "unavailable") {
		t.Fatalf("runtime list status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestServer_RuntimeListRemainsResponsiveDuringStage(t *testing.T) {
	provider := &blockingRuntimeProvider{started: make(chan struct{}), release: make(chan struct{})}
	manager, err := runtimeManager.NewManager(t.TempDir(), map[string]runtimeManager.Provider{"blocking": provider})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: config.Config{}, runtime: manager}

	stageDone := make(chan error, 1)
	go func() {
		_, stageErr := manager.Stage(context.Background(), runtimeManager.Spec{Name: "vllm", Kind: "blocking", Version: "1", Source: "fixture"})
		stageDone <- stageErr
	}()
	<-provider.started

	responseDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		s.handleAPIRuntimes(w, httptest.NewRequest(http.MethodGet, "/api/runtimes", nil))
		responseDone <- w
	}()
	select {
	case response := <-responseDone:
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"state":"BUILDING"`) {
			t.Fatalf("runtime list status=%d body=%q", response.Code, response.Body.String())
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("/api/runtimes waited for provider stage to finish")
	}

	close(provider.release)
	if err := <-stageDone; err != nil {
		t.Fatalf("stage failed: %v", err)
	}
}

func TestServer_EffectiveBackendCapabilitiesExplicitAllowlistWins(t *testing.T) {
	discovered := &backend.Discovery{Capabilities: map[string]bool{
		"chat": true, "responses": true, "embeddings": true,
	}}
	model := config.ModelConfig{Backend: config.BackendConfig{APIs: []string{"chat"}}}
	capabilities := effectiveBackendCapabilities(model, discovered)
	if !capabilities["chat"] {
		t.Fatal("explicit chat capability was lost")
	}
	if capabilities["responses"] || capabilities["embeddings"] {
		t.Fatalf("discovery re-enabled omitted capabilities: %#v", capabilities)
	}
}

func TestServer_EffectiveBackendCapabilitiesUsesDiscoveryWhenUnconfigured(t *testing.T) {
	discovered := &backend.Discovery{Capabilities: map[string]bool{"chat": true, "responses": true}}
	capabilities := effectiveBackendCapabilities(config.ModelConfig{}, discovered)
	if !capabilities["chat"] || !capabilities["responses"] {
		t.Fatalf("discovered capabilities missing: %#v", capabilities)
	}
}

func TestServer_EffectiveBackendCapabilitiesExpandsWildcardAllowlist(t *testing.T) {
	discovered := &backend.Discovery{Capabilities: map[string]bool{"chat": true}}
	model := config.ModelConfig{Backend: config.BackendConfig{APIs: []string{"all"}}}
	capabilities := effectiveBackendCapabilities(model, discovered)
	for _, name := range discoveredCapabilityNames {
		if !capabilities[name] {
			t.Fatalf("wildcard omitted capability %q: %#v", name, capabilities)
		}
	}
	if capabilities["all"] || capabilities["*"] {
		t.Fatalf("wildcard marker leaked as a capability: %#v", capabilities)
	}
}

func TestPublicBackendConfigRedactsEnvironmentAndCommandSecrets(t *testing.T) {
	value := config.BackendConfig{
		Type:      "vllm",
		Arguments: []string{"serve", "--api-key=argv-secret", "--token", "separate-secret", "--model", "qwen"},
		Container: config.RuntimeContainerConfig{
			Image:   "ghcr.io/example/vllm:stable",
			Env:     map[string]string{"HF_TOKEN": "env-secret", "PORT": "8000"},
			Command: []string{"serve", "--password", "command-secret"},
		},
	}
	public, err := publicBackendConfig(value)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(public)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"argv-secret", "separate-secret", "env-secret", "command-secret"} {
		if strings.Contains(string(body), secret) {
			t.Fatalf("backend capability payload leaked %q: %s", secret, body)
		}
	}
	for _, marker := range []string{"[REDACTED]", "--api-key=[REDACTED]", "--password", "PORT"} {
		if !strings.Contains(string(body), marker) {
			t.Fatalf("backend capability payload lost expected marker %q: %s", marker, body)
		}
	}
}

func TestServer_BackendCapabilitiesEndpointRedactsBackendSecrets(t *testing.T) {
	s := &Server{cfg: config.Config{Models: map[string]config.ModelConfig{
		"m": {Proxy: "http://127.0.0.1:1", Backend: config.BackendConfig{
			Type:      "vllm",
			Arguments: []string{"serve", "--api-key=endpoint-secret"},
			Container: config.RuntimeContainerConfig{Env: map[string]string{"TOKEN": "endpoint-env-secret"}},
		}},
	}}}
	r := httptest.NewRequest(http.MethodGet, "/api/backends/m/capabilities", nil)
	r.SetPathValue("model", "m")
	w := httptest.NewRecorder()
	s.handleAPIBackendCapabilities(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	for _, secret := range []string{"endpoint-secret", "endpoint-env-secret"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("capabilities endpoint leaked %q: %s", secret, w.Body.String())
		}
	}
}

func TestServer_BackendProgressEndpointReturnsLatestSnapshot(t *testing.T) {
	s := &Server{cfg: config.Config{Models: map[string]config.ModelConfig{"m": {}}}}
	s.recordBackendProgress(swaputil.BackendProgressEvent{Model: "m", Phase: "downloading", Progress: 0.6, Completed: 60, Total: 100, Message: "fetching"})
	r := httptest.NewRequest(http.MethodGet, "/api/backends/m/progress", nil)
	r.SetPathValue("model", "m")
	w := httptest.NewRecorder()
	s.handleAPIBackendProgress(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	var response struct {
		Model    string                        `json:"model"`
		Progress swaputil.BackendProgressEvent `json:"progress"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Model != "m" || response.Progress.Phase != "downloading" || response.Progress.Progress != 0.6 || response.Progress.Completed != 60 || response.Progress.Total != 100 {
		t.Fatalf("response=%+v", response)
	}
}

func TestServer_BackendSleepActionDoesNotRequireCacheController(t *testing.T) {
	adapter := &backendActionAdapter{}
	s := &Server{
		cfg:             config.Config{Models: map[string]config.ModelConfig{"m": {}}},
		backendAdapters: map[string]backend.BackendAdapter{"m": adapter},
	}
	r := httptest.NewRequest(http.MethodPost, "/api/backends/m/sleep?level=2", nil)
	r.SetPathValue("model", "m")
	w := httptest.NewRecorder()
	s.handleAPIBackendAction(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if adapter.sleepLevel != 2 {
		t.Fatalf("sleep level=%d, want 2", adapter.sleepLevel)
	}
}

func TestServer_BackendSleepActionUsesLocalProcessController(t *testing.T) {
	local := &sleepAwareRouter{stubRouter: newStubRouter([]string{"m"}, "")}
	local.running = map[string]process.ProcessState{"m": process.StateReady}
	adapter := &backendActionAdapter{}
	s := &Server{
		cfg: config.Config{Models: map[string]config.ModelConfig{
			"m": {Backend: config.BackendConfig{Type: "vllm"}},
		}},
		local:           local,
		backendAdapters: map[string]backend.BackendAdapter{"m": adapter},
	}

	sleepRequest := httptest.NewRequest(http.MethodPost, "/api/backends/m/sleep?level=2", nil)
	sleepRequest.SetPathValue("model", "m")
	sleepResponse := httptest.NewRecorder()
	s.handleAPIBackendAction(sleepResponse, sleepRequest)
	if sleepResponse.Code != http.StatusOK {
		t.Fatalf("sleep status=%d body=%q", sleepResponse.Code, sleepResponse.Body.String())
	}
	if local.sleepCall != 1 || adapter.sleepLevel != 0 {
		t.Fatalf("sleep local calls=%d adapter level=%d, want local=1 adapter=0", local.sleepCall, adapter.sleepLevel)
	}

	wakeRequest := httptest.NewRequest(http.MethodPost, "/api/backends/m/wake", nil)
	wakeRequest.SetPathValue("model", "m")
	wakeResponse := httptest.NewRecorder()
	s.handleAPIBackendAction(wakeResponse, wakeRequest)
	if wakeResponse.Code != http.StatusOK {
		t.Fatalf("wake status=%d body=%q", wakeResponse.Code, wakeResponse.Body.String())
	}
	if local.wakeCall != 1 || adapter.woke {
		t.Fatalf("wake local calls=%d adapter woke=%v, want local=1 adapter=false", local.wakeCall, adapter.woke)
	}
}

func TestServer_ModelStatusReportsSleepingVLLM(t *testing.T) {
	local := &sleepAwareRouter{stubRouter: newStubRouter([]string{"m"}, ""), sleeping: true}
	local.running = map[string]process.ProcessState{"m": process.StateReady}
	s := newTestServer(local, newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	s.cfg = config.Config{Models: map[string]config.ModelConfig{
		"m": {Backend: config.BackendConfig{Type: "vllm"}},
	}}

	status := s.modelStatus()
	if len(status) != 1 {
		t.Fatalf("model status=%+v, want one model", status)
	}
	if status[0].State != string(process.StateSleeping) {
		t.Fatalf("state=%q, want %q", status[0].State, process.StateSleeping)
	}
	if status[0].BackendType != "vllm" {
		t.Fatalf("backendType=%q, want vllm", status[0].BackendType)
	}
}

func TestServer_BackendSleepActionRejectsUnsupportedLevel(t *testing.T) {
	adapter := &backendActionAdapter{}
	s := &Server{
		cfg:             config.Config{Models: map[string]config.ModelConfig{"m": {}}},
		backendAdapters: map[string]backend.BackendAdapter{"m": adapter},
	}
	r := httptest.NewRequest(http.MethodPost, "/api/backends/m/sleep?level=0", nil)
	r.SetPathValue("model", "m")
	w := httptest.NewRecorder()
	s.handleAPIBackendAction(w, r)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "1 or 2") {
		t.Fatalf("status=%d body=%q, want unsupported-level 400", w.Code, w.Body.String())
	}
	if adapter.sleepLevel != 0 {
		t.Fatalf("unsupported sleep level reached adapter: %d", adapter.sleepLevel)
	}
}

func TestServer_BackendActionRejectsTypedNilAdapter(t *testing.T) {
	var typedNil *backendActionAdapter
	s := &Server{
		cfg:             config.Config{Models: map[string]config.ModelConfig{"m": {}}},
		backendAdapters: map[string]backend.BackendAdapter{"m": typedNil},
	}
	r := httptest.NewRequest(http.MethodPost, "/api/backends/m/sleep", nil)
	r.SetPathValue("model", "m")
	w := httptest.NewRecorder()
	s.handleAPIBackendAction(w, r)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status=%d body=%q, want not implemented", w.Code, w.Body.String())
	}
}

func TestServer_BackendActionEnforcesModelScopedIdentity(t *testing.T) {
	cfg, err := config.LoadConfigFromReader(strings.NewReader(`
models:
  canonical:
    aliases: [friendly]
    proxy: http://127.0.0.1:1
`))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	adapter := &backendActionAdapter{}
	s := &Server{
		cfg:             cfg,
		backendAdapters: map[string]backend.BackendAdapter{"canonical": adapter},
	}
	denied := httptest.NewRequest(http.MethodPost, "/api/backends/canonical/sleep", nil)
	denied.SetPathValue("model", "canonical")
	denied = denied.WithContext(withIdentity(denied.Context(), auth.Identity{ID: "restricted", Models: []string{"other"}}))
	w := httptest.NewRecorder()
	s.handleAPIBackendAction(w, denied)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%q, want forbidden", w.Code, w.Body.String())
	}
	if adapter.sleepLevel != 0 {
		t.Fatalf("restricted backend action reached adapter at level %d", adapter.sleepLevel)
	}

	allowed := httptest.NewRequest(http.MethodPost, "/api/backends/friendly/sleep?level=2", nil)
	allowed.SetPathValue("model", "friendly")
	allowed = allowed.WithContext(withIdentity(allowed.Context(), auth.Identity{ID: "restricted", Models: []string{"canonical"}}))
	w = httptest.NewRecorder()
	s.handleAPIBackendAction(w, allowed)
	if w.Code != http.StatusOK || adapter.sleepLevel != 2 {
		t.Fatalf("allowed status=%d body=%q level=%d, want successful alias action", w.Code, w.Body.String(), adapter.sleepLevel)
	}
}

func TestServer_BackendSleepActionRejectsInflightRequest(t *testing.T) {
	tracker := newInflightTracker()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	request = request.WithContext(swaputil.SetContext(request.Context(), swaputil.ReqContextData{ModelID: "m"}))
	id := tracker.Add(request, func() {})
	defer tracker.Remove(id)

	adapter := &backendActionAdapter{}
	s := &Server{
		cfg:             config.Config{Models: map[string]config.ModelConfig{"m": {}}},
		backendAdapters: map[string]backend.BackendAdapter{"m": adapter},
		inflight:        tracker,
	}
	r := httptest.NewRequest(http.MethodPost, "/api/backends/m/sleep", nil)
	r.SetPathValue("model", "m")
	w := httptest.NewRecorder()
	s.handleAPIBackendAction(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%q, want conflict", w.Code, w.Body.String())
	}
	if adapter.sleepLevel != 0 {
		t.Fatalf("sleep action reached adapter at level %d", adapter.sleepLevel)
	}
}

func TestServer_BackendCacheResetRejectsInflightRequest(t *testing.T) {
	tracker := newInflightTracker()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	request = request.WithContext(swaputil.SetContext(request.Context(), swaputil.ReqContextData{ModelID: "m"}))
	id := tracker.Add(request, func() {})
	defer tracker.Remove(id)

	adapter := &backendActionAdapter{}
	controller := backend.NewCacheController()
	controller.Register("m", adapter)
	s := &Server{
		cfg:             config.Config{Models: map[string]config.ModelConfig{"m": {}}},
		backendAdapters: map[string]backend.BackendAdapter{"m": adapter},
		cacheController: controller,
		inflight:        tracker,
	}
	r := httptest.NewRequest(http.MethodPost, "/api/backends/m/cache/reset", nil)
	r.SetPathValue("model", "m")
	w := httptest.NewRecorder()
	s.handleAPIBackendAction(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%q, want conflict", w.Code, w.Body.String())
	}
	if adapter.reset {
		t.Fatal("cache reset reached adapter while request was in flight")
	}
}

func TestServer_BackendActionResolvesModelAlias(t *testing.T) {
	cfg, err := config.LoadConfigFromReader(strings.NewReader(`
models:
  canonical:
    aliases: [friendly]
    proxy: http://127.0.0.1:1
`))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	adapter := &backendActionAdapter{}
	s := &Server{
		cfg:             cfg,
		backendAdapters: map[string]backend.BackendAdapter{"canonical": adapter},
	}
	r := httptest.NewRequest(http.MethodPost, "/api/backends/friendly/sleep?level=2", nil)
	r.SetPathValue("model", "friendly")
	w := httptest.NewRecorder()
	s.handleAPIBackendAction(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if adapter.sleepLevel != 2 {
		t.Fatalf("sleep level=%d, want 2", adapter.sleepLevel)
	}
	var response struct {
		Model string `json:"model"`
		OK    bool   `json:"ok"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Model != "canonical" || !response.OK {
		t.Fatalf("response=%+v, want canonical successful backend action", response)
	}
}

func TestServer_BackendDetailResolvesModelAlias(t *testing.T) {
	cfg, err := config.LoadConfigFromReader(strings.NewReader(`
models:
  canonical:
    aliases: [friendly]
    backend:
      type: vllm
      protocol: native
      apis: [chat]
      resources:
        vramMiB: 2048
    proxy: http://127.0.0.1:1
`))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	s := &Server{
		cfg:             cfg,
		backendAdapters: map[string]backend.BackendAdapter{"canonical": &backendActionAdapter{}},
	}
	r := httptest.NewRequest(http.MethodGet, "/api/backends/friendly", nil)
	r.SetPathValue("model", "friendly")
	w := httptest.NewRecorder()
	s.handleAPIBackend(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	var response struct {
		Data backendStatus `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.Model != "canonical" || response.Data.Type != "vllm" || response.Data.Resources == nil || response.Data.Resources.VRAMMiB != 2048 {
		t.Fatalf("detail=%+v, want canonical backend projection", response.Data)
	}
}

func TestServer_BackendDetailRejectsUnknownModel(t *testing.T) {
	s := &Server{cfg: config.Config{Models: map[string]config.ModelConfig{}}}
	r := httptest.NewRequest(http.MethodGet, "/api/backends/missing", nil)
	r.SetPathValue("model", "missing")
	w := httptest.NewRecorder()
	s.handleAPIBackend(w, r)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "model not found") {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestServer_BackendListIsSortedAndModelScoped(t *testing.T) {
	s := &Server{cfg: config.Config{Models: map[string]config.ModelConfig{
		"zeta":  {},
		"alpha": {},
		"beta":  {},
	}}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/backends", nil)
	s.handleAPIBackends(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var payload struct {
		Data []backendStatus `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(payload.Data))
	for _, entry := range payload.Data {
		got = append(got, entry.Model)
	}
	want := []string{"alpha", "beta", "zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("backend order=%v, want %v", got, want)
	}
}

func TestServer_HandleAPIRuntimeLogs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	manager, err := runtimeManager.NewManager(root, map[string]runtimeManager.Provider{})
	if err != nil {
		t.Fatal(err)
	}

	// Feed the capture the same way a build feeds it: streamed stdout/stderr
	// chunks belonging to one operation.
	manager.EmitRuntimeLog("vllm-dev", "op-42", "stdout", []byte("running build step 1/3\n"))
	manager.EmitRuntimeLog("vllm-dev", "op-42", "stderr", []byte("warning: deprecated flag\n"))

	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	s.runtime = manager
	s.setConfig(config.Config{Runtimes: map[string]config.RuntimeConfig{
		"vllm-dev": {Kind: "vllm", Source: config.RuntimeSource{Type: "pypi"}},
	}})

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/runtimes/vllm-dev/logs", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %q", w.Code, w.Body.String())
	}
	var payload struct {
		Runtime     string `json:"runtime"`
		OperationID string `json:"operationId"`
		Output      string `json:"output"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Runtime != "vllm-dev" || payload.OperationID != "op-42" {
		t.Fatalf("payload = %+v", payload)
	}
	if !strings.Contains(payload.Output, "running build step 1/3") || !strings.Contains(payload.Output, "warning: deprecated flag") {
		t.Fatalf("output = %q, want the streamed build output", payload.Output)
	}

	// An unknown runtime answers with the empty shape instead of an error so
	// the log panel renders its empty state.
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/runtimes/unknown/logs", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("unknown runtime status = %d", w.Code)
	}
}
