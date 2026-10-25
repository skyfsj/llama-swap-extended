package server

import (
	"net/http"
	"strconv"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"golang.org/x/sync/semaphore"
)

// CreateConcurrencyLimitMiddleware returns middleware that caps the number of
// inference requests served at once across all models, using a weighted
// semaphore. A request that cannot acquire a slot is rejected immediately
// with a 429 rather than queued (see issue #1086). Callers must not add this
// middleware when limit <= 0; there is no "no limit" mode here.
func CreateConcurrencyLimitMiddleware(limit int) chain.Middleware {
	sem := semaphore.NewWeighted(int64(limit))
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !sem.TryAcquire(1) {
				// A slot frees as soon as any in-flight request finishes.
				// There is no way to know which one, so ask the client to
				// retry after a short, uniform delay.
				w.Header().Set("Retry-After", strconv.Itoa(1))
				swaputil.SendResponse(w, r, http.StatusTooManyRequests,
					"too many concurrent inference requests; retry later")
				return
			}
			defer sem.Release(1)
			next.ServeHTTP(w, r)
		})
	}
}
