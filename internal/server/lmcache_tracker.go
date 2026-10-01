package server

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// lmcacheUsers is the live reference count of models using the LMCache
// server. A reference is acquired by the model process's pre-start hook
// (before the upstream spawns) and released when the model process leaves
// the running states (stopped, failed start, shutdown). It is the
// authoritative dependency view for destructive operations: the
// config-based view (lmcacheInUseModels) complements it for models that
// were already running before tracking engaged.
type lmcacheUsers struct {
	mu  sync.Mutex
	set map[string]struct{}
}

func newLMCacheUsers() *lmcacheUsers {
	return &lmcacheUsers{set: make(map[string]struct{})}
}

// acquire records modelID as using the server. Idempotent.
func (u *lmcacheUsers) acquire(modelID string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.set[modelID] = struct{}{}
}

// release drops modelID's reference and reports whether one was held.
// Idempotent: releasing a model that holds no reference is a no-op.
func (u *lmcacheUsers) release(modelID string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if _, ok := u.set[modelID]; !ok {
		return false
	}
	delete(u.set, modelID)
	return true
}

// users returns the sorted model IDs currently holding a reference.
func (u *lmcacheUsers) users() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	ids := make([]string, 0, len(u.set))
	for id := range u.set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// requireZero fails while any model still uses the server. The error wraps
// errLMCacheInUse (so the API maps it to 409) and lists the models so the
// operator knows what to stop first.
func (u *lmcacheUsers) requireZero() error {
	ids := u.users()
	if len(ids) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %d model(s) are using LMCache: %s (stop those models first)", errLMCacheInUse, len(ids), strings.Join(ids, ", "))
}
