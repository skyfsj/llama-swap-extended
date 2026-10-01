package server

import (
	"net/http"
	"sync"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// accessKeyLimits enforces the restrictions attached to a model-access key.
// It is process-wide on purpose: one llama-swap process serves one deployment,
// and in-flight counts are only meaningful per key id within that process.
var accessKeyLimits = newKeyConcurrencyLimiter()

// keyConcurrencyLimiter tracks how many requests each access key currently has
// in flight. The ceiling lives on the key record and is enforced here rather
// than in the store because it is a live admission decision, not durable state.
type keyConcurrencyLimiter struct {
	mu     sync.Mutex
	active map[string]int
}

func newKeyConcurrencyLimiter() *keyConcurrencyLimiter {
	return &keyConcurrencyLimiter{active: make(map[string]int)}
}

// acquire reserves one in-flight slot for a key. It returns false when the key
// is already at its ceiling, in which case the caller must reject the request
// and must not call release.
func (l *keyConcurrencyLimiter) acquire(keyID string, max int) bool {
	if l == nil || keyID == "" || max <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active[keyID] >= max {
		return false
	}
	l.active[keyID]++
	return true
}

// release returns a slot previously acquired by acquire. Releasing below zero
// cannot happen through the middleware, but the guard keeps a direct caller
// from driving the counter negative and reopening a saturated key.
func (l *keyConcurrencyLimiter) release(keyID string) {
	if l == nil || keyID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if current, ok := l.active[keyID]; ok && current > 0 {
		l.active[keyID] = current - 1
		if l.active[keyID] == 0 {
			delete(l.active, keyID)
		}
	}
}

// count reports the in-flight requests for a key. The control plane uses it to
// show the live concurrency of an access key without a second data source.
func (l *keyConcurrencyLimiter) count(keyID string) int {
	if l == nil || keyID == "" {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active[keyID]
}

// enforceAccessKeyLimits decides whether a request may proceed under the
// restrictions of its identity. It returns the HTTP status to answer with and a
// message when the request must be rejected, plus a release func that the
// caller must invoke once the response is complete (nil when no slot was
// taken).
//
// The IP allowlist is checked against the same resolved caller address the
// access log uses, so an operator debugging a 403 sees the address that was
// tested in both places.
func enforceAccessKeyLimits(r *http.Request, identity auth.Identity) (release func(), status int, message string) {
	if identity.Legacy || identity.ID == "" {
		return nil, 0, ""
	}
	if len(identity.AllowedIPs) > 0 && !auth.IPAllowed(identity.AllowedIPs, clientIP(r)) {
		return nil, http.StatusForbidden, "forbidden: client IP is not allowed for this API key"
	}
	if identity.MaxConcurrency > 0 && !accessKeyLimits.acquire(identity.ID, identity.MaxConcurrency) {
		return nil, http.StatusTooManyRequests, "too many concurrent requests for this API key"
	}
	if identity.MaxConcurrency > 0 {
		return func() { accessKeyLimits.release(identity.ID) }, 0, ""
	}
	return nil, 0, ""
}

// writeKeyLimitRejection answers a rejected request. The status is passed
// through so the two rejection reasons keep their own semantics (403 for a
// network boundary, 429 for saturation).
func writeKeyLimitRejection(w http.ResponseWriter, r *http.Request, status int, message string) {
	swaputil.SendResponse(w, r, status, message)
}
