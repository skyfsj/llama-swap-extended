package server

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestServer_ControlActivityMiddlewareTracksAuthorizedRequestLifetime(t *testing.T) {
	var active atomic.Int64
	started := make(chan struct{})
	release := make(chan struct{})
	handler := CreateControlActivityMiddleware(&active)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	}))

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/config", nil))
		close(done)
	}()
	<-started
	if got := active.Load(); got != 1 {
		t.Fatalf("active requests while handler is blocked = %d, want 1", got)
	}
	close(release)
	<-done
	if got := active.Load(); got != 0 {
		t.Fatalf("active requests after handler returns = %d, want 0", got)
	}
}

func TestServer_ControlActivityMiddlewareAllowsNilCounter(t *testing.T) {
	called := false
	handler := CreateControlActivityMiddleware(nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if !called {
		t.Fatal("nil activity counter prevented downstream handler")
	}
}
