package backend

import (
	"context"
	"errors"
	"sync"
	"time"
)

type CacheReport struct {
	Hit            bool      `json:"hit"`
	CachedTokens   int64     `json:"cachedTokens"`
	CreationTokens int64     `json:"creationTokens"`
	PrefixHash     string    `json:"prefixHash,omitempty"`
	ObservedAt     time.Time `json:"observedAt"`
}

// CacheController is a narrow allowlisted wrapper. It intentionally has no
// collective-RPC, weight-update, LoRA or profiling methods.
type CacheController struct {
	mu       sync.RWMutex
	adapters map[string]BackendAdapter
	reports  map[string]CacheReport
}

func NewCacheController() *CacheController {
	return &CacheController{adapters: make(map[string]BackendAdapter), reports: make(map[string]CacheReport)}
}
func (c *CacheController) Register(model string, adapter BackendAdapter) {
	if c == nil || isNilAdapter(adapter) || model == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.adapters == nil {
		c.adapters = make(map[string]BackendAdapter)
	}
	c.adapters[model] = adapter
}
func (c *CacheController) Observe(model string, report CacheReport) {
	if c == nil || model == "" {
		return
	}
	report = report.Normalize()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reports == nil {
		c.reports = make(map[string]CacheReport)
	}
	if report.ObservedAt.IsZero() {
		report.ObservedAt = time.Now()
	}
	if previous, exists := c.reports[model]; exists && previous.ObservedAt.After(report.ObservedAt) {
		// Metrics are emitted from concurrent request recorders. A slower
		// recorder can finish after a newer observation and must not make the
		// control plane regress to stale cache counters.
		return
	}
	c.reports[model] = report
}
func (c *CacheController) Report(model string) (CacheReport, bool) {
	if c == nil || model == "" {
		return CacheReport{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	report, ok := c.reports[model]
	return report, ok
}

// Snapshot returns the latest cache observation for every registered model.
// The returned map is detached from the controller and is safe for callers to
// sort, label, or retain while concurrent inference recorders continue to
// publish observations. Prefix hashes remain diagnostic values; callers must
// not expose them as unbounded metric labels.
func (c *CacheController) Snapshot() map[string]CacheReport {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.reports) == 0 {
		return nil
	}
	out := make(map[string]CacheReport, len(c.reports))
	for model, report := range c.reports {
		out[model] = report
	}
	return out
}

func (c *CacheController) State(ctx context.Context, model string) (CacheState, error) {
	if c == nil {
		return CacheState{}, errors.New("backend cache controller is unavailable")
	}
	c.mu.RLock()
	adapter := c.adapters[model]
	c.mu.RUnlock()
	if isNilAdapter(adapter) {
		return CacheState{}, errors.New("backend model is not registered")
	}
	state, err := adapter.CacheState(ctx)
	return state.Normalize(), err
}

func (c *CacheController) Reset(ctx context.Context, model string) error {
	if c == nil {
		return errors.New("backend cache controller is unavailable")
	}
	c.mu.RLock()
	adapter := c.adapters[model]
	c.mu.RUnlock()
	if isNilAdapter(adapter) {
		return errors.New("backend model is not registered")
	}
	if err := adapter.ResetCache(ctx); err != nil {
		return err
	}
	// A successful prefix-cache reset invalidates the previous observation.
	// Drop it instead of reporting stale cached-token counts until the next
	// inference response is observed.
	c.mu.Lock()
	delete(c.reports, model)
	c.mu.Unlock()
	return nil
}
