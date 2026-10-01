package backend

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type registryTestAdapter struct{}

func (registryTestAdapter) Name() string                                        { return "test" }
func (registryTestAdapter) Capabilities(context.Context) (CapabilitySet, error) { return nil, nil }
func (registryTestAdapter) TransformRequest(context.Context, string, RequestTransform) (RequestTransform, error) {
	return RequestTransform{}, nil
}
func (registryTestAdapter) TransformResponse(context.Context, string, []byte, http.Header) ([]byte, error) {
	return nil, nil
}
func (registryTestAdapter) CacheState(context.Context) (CacheState, error) { return CacheState{}, nil }
func (registryTestAdapter) ResetCache(context.Context) error               { return nil }
func (registryTestAdapter) Sleep(context.Context, int) error               { return nil }
func (registryTestAdapter) Wake(context.Context) error                     { return nil }
func (registryTestAdapter) Progress(context.Context) (Progress, error)     { return Progress{}, nil }

func TestBackend_HTTPAdapterSleepWakeAllowlist(t *testing.T) {
	var sleepPath string
	var resetPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sleep":
			sleepPath = r.URL.RawQuery
			w.WriteHeader(http.StatusOK)
		case "/wake_up":
			w.WriteHeader(http.StatusNoContent)
		case "/is_sleeping":
			_, _ = w.Write([]byte(`{"is_sleeping":true,"cached_tokens":123}`))
		case "/reset_prefix_cache":
			resetPath = r.URL.Path
			_, _ = w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	adapter, err := NewHTTPAdapter("vllm", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Sleep(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	if sleepPath != "level=2" {
		t.Fatalf("query=%q", sleepPath)
	}
	if err := adapter.Wake(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := adapter.CacheState(context.Background())
	if err != nil || !state.Supported || !state.Sleeping || state.CachedTokens != 123 {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	if err := adapter.ResetCache(context.Background()); err != nil {
		t.Fatalf("reset error=%v", err)
	}
	if resetPath != "/reset_prefix_cache" {
		t.Fatalf("reset path=%q", resetPath)
	}
	state, err = adapter.CacheState(context.Background())
	if err != nil || state.LastReset == "" {
		t.Fatalf("reset timestamp state=%+v err=%v", state, err)
	}
}

func TestBackend_HTTPAdapterRejectsUnsupportedSleepLevel(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	adapter, err := NewHTTPAdapter("vllm", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Sleep(context.Background(), 0); err == nil || !strings.Contains(err.Error(), "1 or 2") {
		t.Fatalf("level 0 error = %v, want unsupported-level error", err)
	}
	if called {
		t.Fatal("unsupported sleep level reached backend")
	}
}

func TestBackend_HTTPAdapterCacheStateAcceptsNestedAndStringCounters(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/is_sleeping" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"sleeping":false,"cache":{"n_cache_tokens":"77"},"last_reset":"2026-08-29T00:00:00Z"}`))
	}))
	defer server.Close()
	adapter, err := NewHTTPAdapter("llamacpp", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	state, err := adapter.CacheState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !state.Supported || state.Sleeping || state.CachedTokens != 77 || state.LastReset != "2026-08-29T00:00:00Z" {
		t.Fatalf("state=%+v", state)
	}
}

func TestBackend_HTTPAdapterCacheStateAcceptsCamelCaseCacheState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/is_sleeping" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"cacheState":{"isSleeping":true,"cachedTokens":"88"}}`))
	}))
	defer server.Close()
	adapter, err := NewHTTPAdapter("vllm", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	state, err := adapter.CacheState(context.Background())
	if err != nil || !state.Supported || !state.Sleeping || state.CachedTokens != 88 {
		t.Fatalf("camel-case cache state=%+v err=%v", state, err)
	}
}

func TestBackend_HTTPAdapterCacheStatePrefersExplicitTopLevelCounter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/is_sleeping" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"is_sleeping":false,"cached_tokens":0,"cache":{"n_cache_tokens":77}}`))
	}))
	defer server.Close()
	adapter, err := NewHTTPAdapter("llamacpp", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	state, err := adapter.CacheState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.CachedTokens != 0 {
		t.Fatalf("nested cache counter overrode explicit top-level zero: %+v", state)
	}
}

func TestBackend_HTTPAdapterCacheStateFallsBackToProps(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/is_sleeping":
			w.WriteHeader(http.StatusNotFound)
		case "/props":
			_, _ = w.Write([]byte(`{"cache_tokens":42}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	adapter, err := NewHTTPAdapter("llamacpp", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	state, err := adapter.CacheState(context.Background())
	if err != nil || !state.Supported || state.CachedTokens != 42 {
		t.Fatalf("state=%+v err=%v", state, err)
	}
}

func TestBackend_HTTPAdapterCacheStatePropsWithoutCacheIsUnsupported(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/is_sleeping" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		_, _ = w.Write([]byte(`{"model":"llama"}`))
	}))
	defer server.Close()
	adapter, err := NewHTTPAdapter("llamacpp", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.CacheState(context.Background()); err != ErrUnsupported {
		t.Fatalf("props without cache error=%v", err)
	}
}

func TestBackend_HTTPAdapterResetCacheRejectsRemoteBackend(t *testing.T) {
	adapter, err := NewHTTPAdapter("vllm", "https://backend.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.ResetCache(context.Background()); err != ErrUnsupported {
		t.Fatalf("remote reset error=%v", err)
	}
}

func TestBackend_HTTPAdapterLifecycleRejectsRemoteBackend(t *testing.T) {
	adapter, err := NewHTTPAdapter("vllm", "https://backend.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Sleep(context.Background(), 1); err != ErrUnsupported {
		t.Fatalf("remote sleep error=%v", err)
	}
	if err := adapter.Wake(context.Background()); err != ErrUnsupported {
		t.Fatalf("remote wake error=%v", err)
	}
}

func TestBackend_HTTPAdapterRejectsBaseURLQueryFragmentAndControl(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1:8000?token=secret",
		"http://127.0.0.1:8000#fragment",
		"http://127.0.0.1:8000/%0A",
		"http://127.0.0.1:8000/%250A",
		"http://127.0.0.1:8000/%E2%80%8B",
		"http://127.0.0.1:8000/\nadmin",
	} {
		if _, err := NewHTTPAdapter("vllm", raw); err == nil {
			t.Fatalf("base URL %q unexpectedly accepted", raw)
		}
	}
}

func TestBackend_HTTPAdapterDoesNotFollowControlRedirects(t *testing.T) {
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sleep" {
			t.Fatalf("unexpected source path %q", r.URL.Path)
		}
		http.Redirect(w, r, target.URL+"/sleep", http.StatusFound)
	}))
	defer source.Close()
	adapter, err := NewHTTPAdapter("vllm", source.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Sleep(context.Background(), 1); err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("redirecting sleep error = %v, want HTTP 302", err)
	}
	if redirected {
		t.Fatal("backend control request followed a redirect to another host")
	}
}

func TestBackend_HTTPAdapterResetCacheReportsBusy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/is_sleeping" {
			_, _ = w.Write([]byte(`{"is_sleeping":false}`))
			return
		}
		if r.URL.Path != "/reset_prefix_cache" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"success":false}`))
	}))
	defer server.Close()
	adapter, err := NewHTTPAdapter("vllm", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.ResetCache(context.Background()); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("busy reset error=%v", err)
	}
}

func TestBackend_HTTPAdapterResetCacheRecordsEmptySuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/is_sleeping" {
			_, _ = w.Write([]byte(`{"is_sleeping":false}`))
			return
		}
		if r.URL.Path != "/reset_prefix_cache" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	adapter, err := NewHTTPAdapter("vllm", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.ResetCache(context.Background()); err != nil {
		t.Fatalf("empty reset response error=%v", err)
	}
	state, err := adapter.CacheState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.LastReset == "" {
		t.Fatalf("successful empty reset did not update timestamp: %+v", state)
	}
}

func TestBackend_HTTPAdapterOptionalLifecycleEndpointsReportUnsupported(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	adapter, err := NewHTTPAdapter("llamacpp", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.CacheState(context.Background()); err != ErrUnsupported {
		t.Fatalf("cache state error = %v", err)
	}
	if err := adapter.Sleep(context.Background(), 1); err != ErrUnsupported {
		t.Fatalf("sleep error = %v", err)
	}
	if err := adapter.Wake(context.Background()); err != ErrUnsupported {
		t.Fatalf("wake error = %v", err)
	}
	if err := adapter.ResetCache(context.Background()); err != ErrUnsupported {
		t.Fatalf("reset error = %v", err)
	}
}

func TestBackend_HTTPAdapterCacheStateRejectsMalformedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/is_sleeping" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"is_sleeping":`))
	}))
	defer server.Close()
	adapter, err := NewHTTPAdapter("vllm", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.CacheState(context.Background()); err == nil || !strings.Contains(err.Error(), "decode backend is_sleeping response") {
		t.Fatalf("malformed cache state error = %v", err)
	}
}

func TestBackend_HTTPAdapterCacheStateRejectsNullResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/is_sleeping" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte("null"))
	}))
	defer server.Close()
	adapter, err := NewHTTPAdapter("vllm", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.CacheState(context.Background()); err == nil || !strings.Contains(err.Error(), "must not be null") {
		t.Fatalf("null cache state error = %v", err)
	}
}

func TestBackend_HTTPAdapterCacheStateRejectsNullNestedCache(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/is_sleeping" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"is_sleeping":false,"cache":null}`))
	}))
	defer server.Close()
	adapter, err := NewHTTPAdapter("llamacpp", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.CacheState(context.Background()); err == nil || !strings.Contains(err.Error(), "cache field must not be null") {
		t.Fatalf("null nested cache error = %v", err)
	}
}

func TestBackend_HTTPAdapterNilContextUsesBackground(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sleep" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	adapter, err := NewHTTPAdapter("vllm", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Sleep(testNilHTTPContext(), 1); err != nil {
		t.Fatalf("nil-context sleep failed: %v", err)
	}
}

func TestBackend_HTTPAdapterProgressParsesEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/progress" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"progress":{"phase":"loading","current":"3","total":10,"detail":"weights","error":"recoverable"}}`))
	}))
	defer server.Close()
	adapter, err := NewHTTPAdapter("vllm", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	progress, err := adapter.Progress(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if progress.Phase != "loading" || progress.Completed != 3 || progress.Total != 10 || progress.Message != "weights" || progress.Error != "recoverable" {
		t.Fatalf("progress=%+v", progress)
	}
}

func TestBackend_HTTPAdapterProgressAcceptsCamelCaseNestedState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/progress" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"progressState":{"state":"prefill","currentTokens":"4","totalTokens":9}}`))
	}))
	defer server.Close()
	adapter, err := NewHTTPAdapter("vllm", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	progress, err := adapter.Progress(context.Background())
	if err != nil || progress.Phase != "prefill" || progress.Completed != 4 || progress.Total != 9 {
		t.Fatalf("camel-case progress=%+v err=%v", progress, err)
	}
}

func TestBackend_ProgressNormalizeBoundsRemoteDiagnostics(t *testing.T) {
	progress := (Progress{
		Phase:     "\t" + strings.Repeat("loading", 20),
		Completed: -4,
		Total:     -9,
		Message:   strings.Repeat("x", maxProgressMessage+100),
		Error:     "bad\u200berror",
	}).Normalize()
	if progress.Phase != "idle" {
		t.Fatalf("invalid phase = %q, want idle fallback", progress.Phase)
	}
	if progress.Completed != 0 || progress.Total != 0 {
		t.Fatalf("negative counters = %+v", progress)
	}
	if len(progress.Message) != maxProgressMessage {
		t.Fatalf("message length = %d, want %d", len(progress.Message), maxProgressMessage)
	}
	if progress.Error != "" {
		t.Fatalf("invisible error was retained: %q", progress.Error)
	}
}

func TestBackend_HTTPAdapterProgressNormalizesRemoteDiagnostics(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/progress" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"phase":"\tloading","current":-2,"total":-1,"message":"` + strings.Repeat("m", maxProgressMessage+10) + `","error":"bad\u200berror"}`))
	}))
	defer server.Close()
	adapter, err := NewHTTPAdapter("vllm", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	progress, err := adapter.Progress(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if progress.Phase != "idle" || progress.Completed != 0 || progress.Total != 0 || len(progress.Message) != maxProgressMessage || progress.Error != "" {
		t.Fatalf("normalized remote progress = %+v", progress)
	}
}

func TestBackend_HTTPAdapterProgressFallsBackWhenUnsupported(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	adapter, err := NewHTTPAdapter("llamacpp", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	progress, err := adapter.Progress(context.Background())
	if err != nil || progress.Phase != "idle" || progress.Completed != 1 || progress.Total != 1 {
		t.Fatalf("progress=%+v err=%v", progress, err)
	}
}

func TestBackend_HTTPAdapterProgressRejectsMalformedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/progress" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"phase":`))
	}))
	defer server.Close()
	adapter, err := NewHTTPAdapter("vllm", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Progress(context.Background()); err == nil || !strings.Contains(err.Error(), "decode backend progress response") {
		t.Fatalf("malformed progress error=%v", err)
	}
}

func TestBackend_HTTPAdapterProgressRejectsNullResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/progress" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte("null"))
	}))
	defer server.Close()
	adapter, err := NewHTTPAdapter("vllm", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Progress(context.Background()); err == nil || !strings.Contains(err.Error(), "must be an object") {
		t.Fatalf("null progress error = %v", err)
	}
}

func TestBackend_HTTPAdapterProgressRejectsNullNestedProgress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/progress" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"progress":null}`))
	}))
	defer server.Close()
	adapter, err := NewHTTPAdapter("llamacpp", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Progress(context.Background()); err == nil || !strings.Contains(err.Error(), "progress field must not be null") {
		t.Fatalf("null nested progress error = %v", err)
	}
}

func TestBackend_HTTPAdapterControlResponsesRejectOversize(t *testing.T) {
	for _, path := range []string{"/is_sleeping", "/progress"} {
		t.Run(strings.TrimPrefix(path, "/"), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != path {
					t.Fatalf("unexpected path %q", r.URL.Path)
				}
				payload := []byte(`{"is_sleeping":false}`)
				if path == "/progress" {
					payload = []byte(`{"phase":"idle"}`)
				}
				payload = append(payload, []byte(strings.Repeat(" ", int(maxBackendControlResponseBytes)))...)
				_, _ = w.Write(payload)
			}))
			defer server.Close()
			adapter, err := NewHTTPAdapter("vllm", server.URL)
			if err != nil {
				t.Fatal(err)
			}
			if path == "/progress" {
				if _, err := adapter.Progress(context.Background()); err == nil || !strings.Contains(err.Error(), "control response exceeds") {
					t.Fatalf("oversized progress response error = %v", err)
				}
				return
			}
			if _, err := adapter.CacheState(context.Background()); err == nil || !strings.Contains(err.Error(), "control response exceeds") {
				t.Fatalf("oversized cache response error = %v", err)
			}
		})
	}
}

func TestBackend_HTTPAdapterZeroValueIsSafe(t *testing.T) {
	var zero HTTPAdapter
	if got := zero.Name(); got != "" {
		t.Fatalf("zero-value adapter name = %q", got)
	}
	if _, err := zero.Capabilities(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("zero-value capabilities error = %v", err)
	}
	if _, err := zero.CacheState(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("zero-value cache state error = %v", err)
	}
	if _, err := zero.Progress(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("zero-value progress error = %v", err)
	}
	if err := zero.ResetCache(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("zero-value reset error = %v", err)
	}
	if err := zero.Sleep(context.Background(), 1); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("zero-value sleep error = %v", err)
	}
	if err := zero.Wake(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("zero-value wake error = %v", err)
	}

	var nilAdapter *HTTPAdapter
	if got := nilAdapter.Name(); got != "" {
		t.Fatalf("nil adapter name = %q", got)
	}
	if _, err := nilAdapter.Capabilities(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("nil capabilities error = %v", err)
	}
	if _, err := nilAdapter.CacheState(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("nil cache state error = %v", err)
	}
	if _, err := nilAdapter.Progress(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("nil progress error = %v", err)
	}
	if err := nilAdapter.ResetCache(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("nil reset error = %v", err)
	}
	if err := nilAdapter.Sleep(context.Background(), 1); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("nil sleep error = %v", err)
	}
	if err := nilAdapter.Wake(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("nil wake error = %v", err)
	}
}

func testNilHTTPContext() context.Context { return nil }

func TestBackend_CacheControllerResetClearsStaleReport(t *testing.T) {
	controller := NewCacheController()
	controller.Register("m", registryTestAdapter{})
	controller.Observe("m", CacheReport{Hit: true, CachedTokens: 42})
	if err := controller.Reset(context.Background(), "m"); err != nil {
		t.Fatal(err)
	}
	if _, ok := controller.Report("m"); ok {
		t.Fatal("cache report survived a successful reset")
	}
}

func TestBackend_CacheTelemetryNormalizationBoundsRemoteValues(t *testing.T) {
	state := (CacheState{CachedTokens: -4, LastReset: "bad\u200breset"}).Normalize()
	if state.CachedTokens != 0 || state.LastReset != "" {
		t.Fatalf("normalized cache state = %+v", state)
	}
	report := (CacheReport{CachedTokens: -2, CreationTokens: -3, PrefixHash: strings.Repeat("h", 300)}).Normalize()
	if report.CachedTokens != 0 || report.CreationTokens != 0 || len(report.PrefixHash) != 256 || report.ObservedAt.IsZero() {
		t.Fatalf("normalized cache report = %+v", report)
	}
}

func TestBackend_CacheControllerObserveNormalizesReport(t *testing.T) {
	controller := NewCacheController()
	controller.Observe("m", CacheReport{CachedTokens: -1, CreationTokens: -1, PrefixHash: "bad\u202Ehash"})
	report, ok := controller.Report("m")
	if !ok {
		t.Fatal("cache report was not stored")
	}
	if report.CachedTokens != 0 || report.CreationTokens != 0 || report.PrefixHash != "" || report.ObservedAt.IsZero() {
		t.Fatalf("controller report was not normalized: %+v", report)
	}
}

func TestBackend_CacheControllerObserveIgnoresOutOfOrderReport(t *testing.T) {
	controller := NewCacheController()
	now := time.Now()
	controller.Observe("m", CacheReport{CachedTokens: 20, ObservedAt: now.Add(time.Second)})
	controller.Observe("m", CacheReport{CachedTokens: 5, ObservedAt: now})
	report, ok := controller.Report("m")
	if !ok || report.CachedTokens != 20 || !report.ObservedAt.Equal(now.Add(time.Second)) {
		t.Fatalf("out-of-order report replaced newer observation: %+v ok=%v", report, ok)
	}
}

func TestBackend_CacheControllerSnapshotIsDetached(t *testing.T) {
	controller := NewCacheController()
	controller.Observe("m1", CacheReport{Hit: true, CachedTokens: 7})
	controller.Observe("m2", CacheReport{CreationTokens: 3})

	snapshot := controller.Snapshot()
	if len(snapshot) != 2 || snapshot["m1"].CachedTokens != 7 || snapshot["m2"].CreationTokens != 3 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	delete(snapshot, "m1")
	snapshot["m2"] = CacheReport{}
	if report, ok := controller.Report("m1"); !ok || report.CachedTokens != 7 {
		t.Fatalf("snapshot mutation removed controller report: %+v, ok=%v", report, ok)
	}
	if report, ok := controller.Report("m2"); !ok || report.CreationTokens != 3 {
		t.Fatalf("snapshot mutation changed controller report: %+v, ok=%v", report, ok)
	}
}

func TestBackend_CacheControllerZeroValueIsSafe(t *testing.T) {
	var controller CacheController
	controller.Register("m", registryTestAdapter{})
	controller.Observe("m", CacheReport{CachedTokens: 7})
	report, ok := controller.Report("m")
	if !ok || report.CachedTokens != 7 {
		t.Fatalf("zero-value controller report=%+v ok=%v", report, ok)
	}
	if _, err := controller.State(context.Background(), "unknown"); err == nil {
		t.Fatal("zero-value controller accepted an unregistered model")
	}
	if err := controller.Reset(context.Background(), "unknown"); err == nil {
		t.Fatal("zero-value controller reset an unregistered model")
	}
	var nilController *CacheController
	if _, ok := nilController.Report("m"); ok {
		t.Fatal("nil controller returned a cache report")
	}
	if err := nilController.Reset(context.Background(), "m"); err == nil {
		t.Fatal("nil controller reset unexpectedly succeeded")
	}
}

func TestBackend_RegistryConcurrentAccess(t *testing.T) {
	var registry Registry
	adapter := registryTestAdapter{}
	if err := registry.Register("test", adapter); err != nil {
		t.Fatal(err)
	}
	if got, ok := registry.Get("test"); !ok || got == nil {
		t.Fatalf("registry lookup = %v, %v", got, ok)
	}
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				_, _ = registry.Get("test")
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}
