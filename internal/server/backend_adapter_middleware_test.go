package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/backend"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

type transformMiddlewareAdapter struct {
	requestCalls  int
	responseCalls int
	requestPath   string
	requestBody   string
	responsePath  string
	requestErr    error
	responseErr   error
}

func (a *transformMiddlewareAdapter) Name() string { return "transform-test" }

func (a *transformMiddlewareAdapter) Capabilities(context.Context) (backend.CapabilitySet, error) {
	return nil, nil
}

func (a *transformMiddlewareAdapter) TransformRequest(_ context.Context, path string, req backend.RequestTransform) (backend.RequestTransform, error) {
	a.requestCalls++
	a.requestPath = path
	a.requestBody = string(req.Body)
	if a.requestErr != nil {
		return backend.RequestTransform{}, a.requestErr
	}
	return backend.RequestTransform{Body: []byte(`{"model":"m","adapted":true}`), Header: req.Header}, nil
}

func (a *transformMiddlewareAdapter) TransformResponse(_ context.Context, path string, body []byte, _ http.Header) ([]byte, error) {
	a.responseCalls++
	a.responsePath = path
	if a.responseErr != nil {
		return nil, a.responseErr
	}
	return []byte(`{"adapted":true,"raw":` + string(body) + `}`), nil
}

func (a *transformMiddlewareAdapter) CacheState(context.Context) (backend.CacheState, error) {
	return backend.CacheState{}, backend.ErrUnsupported
}
func (a *transformMiddlewareAdapter) ResetCache(context.Context) error { return backend.ErrUnsupported }
func (a *transformMiddlewareAdapter) Sleep(context.Context, int) error { return backend.ErrUnsupported }
func (a *transformMiddlewareAdapter) Wake(context.Context) error       { return backend.ErrUnsupported }
func (a *transformMiddlewareAdapter) Progress(context.Context) (backend.Progress, error) {
	return backend.Progress{}, backend.ErrUnsupported
}

func middlewareRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r.WithContext(swaputil.SetContext(r.Context(), swaputil.ReqContextData{ModelID: "m", Model: "m"}))
}

func TestBackendAdapterMiddleware_TransformsJSONRequestAndResponse(t *testing.T) {
	adapter := &transformMiddlewareAdapter{}
	var upstreamBody string
	handler := CreateBackendAdapterMiddleware(config.Config{}, map[string]backend.BackendAdapter{"m": adapter})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read transformed body: %v", err)
		}
		upstreamBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "999")
		_, _ = w.Write([]byte(`{"answer":"raw"}`))
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, middlewareRequest(t, `{"model":"m","prompt":"hello"}`))
	if adapter.requestCalls != 1 || adapter.responseCalls != 1 {
		t.Fatalf("transform calls request=%d response=%d", adapter.requestCalls, adapter.responseCalls)
	}
	if adapter.requestPath != "/v1/chat/completions" || adapter.responsePath != adapter.requestPath {
		t.Fatalf("paths request=%q response=%q", adapter.requestPath, adapter.responsePath)
	}
	if upstreamBody != `{"model":"m","adapted":true}` {
		t.Fatalf("upstream body=%q", upstreamBody)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d", recorder.Code)
	}
	if got := recorder.Body.String(); got != `{"adapted":true,"raw":{"answer":"raw"}}` {
		t.Fatalf("response=%q", got)
	}
	if got := recorder.Header().Get("Content-Length"); got == "999" {
		t.Fatalf("stale content length survived transform: %q", got)
	}
}

func TestBackendAdapterMiddleware_ResolvesModelWhenContextIsUnset(t *testing.T) {
	adapter := &transformMiddlewareAdapter{}
	called := false
	handler := CreateBackendAdapterMiddleware(config.Config{}, map[string]backend.BackendAdapter{"m": adapter})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if data, ok := swaputil.ReadContext(r.Context()); !ok || data.ModelID != "m" {
			t.Fatalf("request context=%+v present=%v", data, ok)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, request)
	if !called || adapter.requestCalls != 1 || adapter.responseCalls != 1 {
		t.Fatalf("called=%v request=%d response=%d", called, adapter.requestCalls, adapter.responseCalls)
	}
}

func TestBackendAdapterMiddleware_SkipsResponseTransformForStreaming(t *testing.T) {
	adapter := &transformMiddlewareAdapter{}
	var upstreamBody string
	handler := CreateBackendAdapterMiddleware(config.Config{}, map[string]backend.BackendAdapter{"m": adapter})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamBody = string(body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: raw\n\n"))
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, middlewareRequest(t, `{"model":"m","stream":true}`))
	if adapter.requestCalls != 1 || adapter.responseCalls != 0 {
		t.Fatalf("transform calls request=%d response=%d", adapter.requestCalls, adapter.responseCalls)
	}
	if upstreamBody != `{"model":"m","adapted":true}` {
		t.Fatalf("upstream body=%q", upstreamBody)
	}
	if got := recorder.Body.String(); got != "data: raw\n\n" {
		t.Fatalf("stream response=%q", got)
	}
}

func TestBackendAdapterMiddleware_FlushCommitsRawResponse(t *testing.T) {
	adapter := &transformMiddlewareAdapter{}
	handler := CreateBackendAdapterMiddleware(config.Config{}, map[string]backend.BackendAdapter{"m": adapter})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		_, _ = w.Write([]byte(`{"answer":"raw"}`))
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, middlewareRequest(t, `{"model":"m"}`))
	if adapter.responseCalls != 0 {
		t.Fatalf("response transform called after flush: %d", adapter.responseCalls)
	}
	if got := recorder.Body.String(); got != `{"answer":"raw"}` {
		t.Fatalf("flushed response=%q", got)
	}
}

func TestBackendAdapterMiddleware_MapsTransformErrors(t *testing.T) {
	adapter := &transformMiddlewareAdapter{requestErr: backend.ErrUnsupported}
	called := false
	handler := CreateBackendAdapterMiddleware(config.Config{}, map[string]backend.BackendAdapter{"m": adapter})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, middlewareRequest(t, `{"model":"m"}`))
	if called {
		t.Fatal("upstream called after unsupported request transform")
	}
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("status=%d, want 501", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "backend request transform") {
		t.Fatalf("body=%q", recorder.Body.String())
	}

	adapter = &transformMiddlewareAdapter{responseErr: errors.New("bad response")}
	handler = CreateBackendAdapterMiddleware(config.Config{}, map[string]backend.BackendAdapter{"m": adapter})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answer":"raw"}`))
	}))
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, middlewareRequest(t, `{"model":"m"}`))
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "backend response transform") {
		t.Fatalf("response error status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}
