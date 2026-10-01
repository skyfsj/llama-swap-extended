package server

import (
	"net/http"
	"sync/atomic"
)

// CreateControlActivityMiddleware tracks authorized control-plane handlers
// while they are executing. Runtime auto-update uses this counter as part of
// its idle gate, so a config/key/unload/cache/runtime operation cannot race an
// activation merely because it is not represented by inference inflight state.
// The middleware is intentionally independent from ConfigReconciler or model
// restart logic; it only observes request lifetime.
func CreateControlActivityMiddleware(active *atomic.Int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if active == nil {
				next.ServeHTTP(w, r)
				return
			}
			active.Add(1)
			defer active.Add(-1)
			next.ServeHTTP(w, r)
		})
	}
}
