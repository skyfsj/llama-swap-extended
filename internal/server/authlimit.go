package server

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// authFailureLimiter throttles failed credential checks per caller address.
//
// Counting is driven by an explicit outcome the authentication layer records,
// not by the final status code. A 401 can also come from an upstream provider
// or a model backend that requires its own key; treating those as failed
// logins would lock a caller out of a perfectly valid credential the moment
// the upstream started rejecting it.
type authFailureLimiter struct {
	cfg config.AuthRateLimitConfig

	mu        sync.Mutex
	entries   map[string]*authFailureEntry
	lastPrune time.Time
}

type authFailureEntry struct {
	failures    int
	windowStart time.Time
	blockedTill time.Time
}

func newAuthFailureLimiter(cfg config.AuthRateLimitConfig) *authFailureLimiter {
	return &authFailureLimiter{cfg: cfg.Effective(), entries: make(map[string]*authFailureEntry)}
}

// blocked reports whether the caller must be rejected right now, without
// consuming an attempt.
func (l *authFailureLimiter) blocked(when time.Time, key string) bool {
	if l == nil || l.cfg.Disabled || key == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.entries[key]
	if !ok {
		return false
	}
	if when.After(entry.blockedTill) {
		if !entry.blockedTill.IsZero() {
			// The block expired; start a fresh window so the caller is not
			// instantly re-blocked by the previous failures.
			delete(l.entries, key)
		}
		return false
	}
	return true
}

// recordFailure counts one failed attempt and returns whether the caller is
// now blocked.
func (l *authFailureLimiter) recordFailure(when time.Time, key string) bool {
	if l == nil || l.cfg.Disabled || key == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(when)
	entry, ok := l.entries[key]
	if !ok || when.Sub(entry.windowStart) > l.cfg.Window {
		entry = &authFailureEntry{windowStart: when}
		l.entries[key] = entry
	}
	entry.failures++
	if entry.failures < l.cfg.MaxFailures {
		return false
	}
	entry.blockedTill = when.Add(l.cfg.BlockFor)
	entry.failures = 0
	entry.windowStart = when
	return true
}

// recordSuccess clears the caller's history.
func (l *authFailureLimiter) recordSuccess(key string) {
	if l == nil || l.cfg.Disabled || key == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

// pruneLocked drops entries whose block and window have both expired so an
// attacker rotating source addresses cannot grow the map without bound.
func (l *authFailureLimiter) pruneLocked(now time.Time) {
	if now.Sub(l.lastPrune) < time.Minute {
		return
	}
	l.lastPrune = now
	for key, entry := range l.entries {
		expired := now.After(entry.blockedTill) && now.Sub(entry.windowStart) > l.cfg.Window
		if expired {
			delete(l.entries, key)
		}
	}
}

// RetryAfter returns the remaining block duration, or zero.
func (l *authFailureLimiter) RetryAfter(when time.Time, key string) time.Duration {
	if l == nil || l.cfg.Disabled {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if entry, ok := l.entries[key]; ok && when.Before(entry.blockedTill) {
		return entry.blockedTill.Sub(when)
	}
	return 0
}

// authOutcomeKey carries the result the authentication layer reports to the
// rate-limit middleware. Recording the outcome explicitly keeps upstream 401s
// (a provider whose own key expired, a backend protected by --api-key) out of
// the caller's failure budget.
type authOutcomeKey struct{}

type authOutcome struct {
	failed        bool
	authenticated bool
}

func withAuthOutcome(ctx context.Context) (context.Context, *authOutcome) {
	outcome := &authOutcome{}
	return context.WithValue(ctx, authOutcomeKey{}, outcome), outcome
}

func authOutcomeFrom(r *http.Request) *authOutcome {
	if r == nil {
		return nil
	}
	outcome, _ := r.Context().Value(authOutcomeKey{}).(*authOutcome)
	return outcome
}

// markAuthFailure records that the presented credential failed validation.
// It is a no-op when the rate limiter is not in the chain.
func markAuthFailure(r *http.Request) {
	if outcome := authOutcomeFrom(r); outcome != nil {
		outcome.failed = true
	}
}

// markAuthSuccess records that a credential was accepted, so the caller's
// failure window is cleared even if the request later fails for another
// reason (a scope boundary, a missing model, an upstream error).
func markAuthSuccess(r *http.Request) {
	if outcome := authOutcomeFrom(r); outcome != nil {
		outcome.authenticated = true
	}
}

// streamingPreserver passes the response through unchanged while keeping the
// optional interfaces (Flusher, Hijacker) that a reverse-proxy response writer
// wrapper would otherwise hide. net/http's ResponseController relies on
// Unwrap, so a wrapper that only embeds the interface silently breaks SSE
// flushing and WebSocket upgrades.
type streamingPreserver struct {
	http.ResponseWriter
}

func (s *streamingPreserver) Flush() {
	if flusher, ok := s.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Hijack forwards to the underlying writer so the realtime WebSocket endpoint
// can take over the connection. Without it the upgrade fails with 400.
func (s *streamingPreserver) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("underlying ResponseWriter does not support hijacking")
	}
	return hijacker.Hijack()
}

func (s *streamingPreserver) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// MarkStatus forwards the sentinel status (499 for a client that hung up) to
// the recorder further out, so the access log and the incident archive report
// the same code as the metrics layer.
func (s *streamingPreserver) MarkStatus(code int) {
	if marker, ok := s.ResponseWriter.(swaputil.StatusMarker); ok {
		marker.MarkStatus(code)
	}
}

// WroteHeader reports whether a response status already reached the client.
func (s *streamingPreserver) WroteHeader() bool {
	if marker, ok := s.ResponseWriter.(swaputil.StatusMarker); ok {
		return marker.WroteHeader()
	}
	return false
}

// CreateAuthRateLimitMiddleware rejects a caller that already exceeded its
// failed-credential budget and records the outcome the wrapped authentication
// layer reports.
//
// The decision comes from markAuthFailure/markAuthSuccess rather than the
// response status: downstream 401s belong to the model backend or provider,
// not to this caller's credential. A handler that never marks an outcome (a
// route without authentication) leaves the counter untouched.
//
// The limiter owns the resolved policy; the configuration is passed in only
// through it so the two cannot disagree.
func CreateAuthRateLimitMiddleware(limiter *authFailureLimiter) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if limiter == nil || limiter.cfg.Disabled {
				next.ServeHTTP(w, r)
				return
			}
			key := clientIP(r)
			now := time.Now()
			if limiter.blocked(now, key) {
				retry := limiter.RetryAfter(now, key)
				seconds := int(retry.Seconds() + 0.5)
				if seconds < 1 {
					seconds = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(seconds))
				swaputil.SendResponse(w, r, http.StatusTooManyRequests,
					"too many failed authentication attempts; retry later")
				return
			}
			ctx, outcome := withAuthOutcome(r.Context())
			r = r.WithContext(ctx)
			next.ServeHTTP(&streamingPreserver{ResponseWriter: w}, r)
			switch {
			case outcome.failed:
				limiter.recordFailure(now, key)
			case outcome.authenticated:
				limiter.recordSuccess(key)
			}
		})
	}
}

// composeMiddlewares folds several middlewares into one chain middlewares
// slot, preserving left-to-right execution order.
func composeMiddlewares(mws ...chain.Middleware) chain.Middleware {
	switch len(mws) {
	case 0:
		return func(next http.Handler) http.Handler { return next }
	case 1:
		return mws[0]
	default:
		return func(next http.Handler) http.Handler {
			return chain.New(mws...).Then(next)
		}
	}
}
