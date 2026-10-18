package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
)

func authLimitHandler(cfg config.AuthRateLimitConfig) http.Handler {
	limiter := newAuthFailureLimiter(cfg)
	auth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") == "Bearer good" {
				markAuthSuccess(r)
				next.ServeHTTP(w, r)
				return
			}
			markAuthFailure(r)
			w.WriteHeader(http.StatusUnauthorized)
		})
	}
	return chain.New(
		CreateClientIPMiddleware(config.NetworkConfig{}),
		CreateAuthRateLimitMiddleware(limiter),
		auth,
	).Then(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
}

func authLimitRequest(remote string, bearer string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	request.RemoteAddr = remote
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	return request
}

func TestServer_AuthRateLimitBlocksAfterRepeatedFailures(t *testing.T) {
	cfg := config.AuthRateLimitConfig{MaxFailures: 3, Window: time.Minute, BlockFor: time.Minute}
	handler := authLimitHandler(cfg)

	for attempt := 1; attempt <= 3; attempt++ {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, authLimitRequest("198.51.100.4:5000", "wrong"))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status=%d, want 401", attempt, w.Code)
		}
	}

	blocked := httptest.NewRecorder()
	handler.ServeHTTP(blocked, authLimitRequest("198.51.100.4:5000", "wrong"))
	if blocked.Code != http.StatusTooManyRequests {
		t.Fatalf("blocked status=%d body=%s, want 429", blocked.Code, blocked.Body.String())
	}
	if blocked.Header().Get("Retry-After") == "" {
		t.Fatal("blocked response has no Retry-After header")
	}

	// A different caller is unaffected.
	other := httptest.NewRecorder()
	handler.ServeHTTP(other, authLimitRequest("203.0.113.9:5000", "good"))
	if other.Code != http.StatusNoContent {
		t.Fatalf("unrelated caller status=%d, want 204", other.Code)
	}
}

func TestServer_AuthRateLimitSuccessClearsCounter(t *testing.T) {
	cfg := config.AuthRateLimitConfig{MaxFailures: 2, Window: time.Minute, BlockFor: time.Minute}
	handler := authLimitHandler(cfg)

	_ = httptest.NewRecorder()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, authLimitRequest("198.51.100.4:5000", "wrong"))
	success := httptest.NewRecorder()
	handler.ServeHTTP(success, authLimitRequest("198.51.100.4:5000", "good"))
	if success.Code != http.StatusNoContent {
		t.Fatalf("success status=%d", success.Code)
	}
	// The counter was cleared, so another failure does not block.
	next := httptest.NewRecorder()
	handler.ServeHTTP(next, authLimitRequest("198.51.100.4:5000", "wrong"))
	if next.Code != http.StatusUnauthorized {
		t.Fatalf("post-success failure status=%d, want 401", next.Code)
	}
}

func TestServer_AuthRateLimitDisabled(t *testing.T) {
	cfg := config.AuthRateLimitConfig{Disabled: true, MaxFailures: 1, Window: time.Second, BlockFor: time.Minute}
	handler := authLimitHandler(cfg)
	for attempt := 0; attempt < 5; attempt++ {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, authLimitRequest("198.51.100.4:5000", "wrong"))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("disabled limiter attempt %d status=%d, want 401", attempt, w.Code)
		}
	}
}

func TestAuthFailureLimiter_RetryAfterShrinks(t *testing.T) {
	cfg := config.AuthRateLimitConfig{MaxFailures: 1, Window: time.Minute, BlockFor: 30 * time.Second}.Effective()
	limiter := newAuthFailureLimiter(cfg)
	start := time.Now()
	if !limiter.recordFailure(start, "203.0.113.9") {
		t.Fatal("first failure did not block")
	}
	if retry := limiter.RetryAfter(start.Add(10*time.Second), "203.0.113.9"); retry != 20*time.Second {
		t.Fatalf("RetryAfter = %s, want 20s", retry)
	}
	// Once the block expires the caller is allowed again.
	if limiter.blocked(start.Add(31*time.Second), "203.0.113.9") {
		t.Fatal("caller still blocked after the block expired")
	}
}

// TestServer_AuthRateLimitIgnoresDownstream401 is the regression guard for the
// worst failure mode of a status-code-driven limiter: a valid credential whose
// upstream happens to answer 401 (expired provider key, a backend protected by
// --api-key) must not be locked out of its own account.
func TestServer_AuthRateLimitIgnoresDownstream401(t *testing.T) {
	cfg := config.AuthRateLimitConfig{MaxFailures: 2, Window: time.Minute, BlockFor: time.Minute}
	limiter := newAuthFailureLimiter(cfg)
	auth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			markAuthSuccess(r)
			next.ServeHTTP(w, r)
		})
	}
	upstream401 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	handler := chain.New(
		CreateClientIPMiddleware(config.NetworkConfig{}),
		CreateAuthRateLimitMiddleware(limiter),
		auth,
	).Then(upstream401)

	for attempt := 1; attempt <= 5; attempt++ {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, authLimitRequest("198.51.100.4:5000", "good"))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status=%d, want the upstream 401 to pass through", attempt, w.Code)
		}
	}
}

// TestServer_AuthRateLimitForbiddenDoesNotCount pins the other half: a valid
// credential that merely lacks a scope is not a failed login.
func TestServer_AuthRateLimitForbiddenDoesNotCount(t *testing.T) {
	cfg := config.AuthRateLimitConfig{MaxFailures: 2, Window: time.Minute, BlockFor: time.Minute}
	limiter := newAuthFailureLimiter(cfg)
	auth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			markAuthSuccess(r)
			w.WriteHeader(http.StatusForbidden)
		})
	}
	handler := chain.New(
		CreateClientIPMiddleware(config.NetworkConfig{}),
		CreateAuthRateLimitMiddleware(limiter),
		auth,
	).Then(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	for attempt := 1; attempt <= 5; attempt++ {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, authLimitRequest("198.51.100.4:5000", "good"))
		if w.Code != http.StatusForbidden {
			t.Fatalf("attempt %d status=%d, want 403", attempt, w.Code)
		}
	}
}

// TestStreamingPreserverForwardsStatusMarker keeps the 499 client-closed
// sentinel observable to the outermost recorder. Without the forwarding the
// access log reports 200 for a request the client abandoned.
func TestStreamingPreserverForwardsStatusMarker(t *testing.T) {
	outer := &statusRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
	inner := &streamingPreserver{ResponseWriter: outer}

	if inner.WroteHeader() {
		t.Fatal("WroteHeader reported true before anything was written")
	}
	inner.MarkStatus(499)
	// WroteHeader deliberately stays false: MarkStatus records a sentinel for
	// logging without claiming a response reached the client. What matters is
	// that the code reaches the outermost recorder.
	if outer.status != 499 {
		t.Fatalf("outer status = %d, want 499", outer.status)
	}

	// Flush must still reach the real writer.
	flushed := false
	flusher := &flushRecorder{ResponseWriter: httptest.NewRecorder(), onFlush: func() { flushed = true }}
	preserver := &streamingPreserver{ResponseWriter: flusher}
	preserver.Flush()
	if !flushed {
		t.Fatal("Flush did not reach the underlying writer")
	}
}

type flushRecorder struct {
	http.ResponseWriter
	onFlush func()
}

func (f *flushRecorder) Flush() {
	if f.onFlush != nil {
		f.onFlush()
	}
}

func (f *flushRecorder) Unwrap() http.ResponseWriter { return f.ResponseWriter }
