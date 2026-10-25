package gateway

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// skipSlowPolicy decides whether a provider's observed first-token latency is
// bad enough to prefer a sibling instead.
type skipSlowPolicy struct {
	enabled bool
	// maxFirstTokenMs is the absolute ceiling; 0 means the rule is off.
	maxFirstTokenMs float64
	// relativeFactor skips a provider slower than the fastest candidate by
	// this multiple; 0 means the rule is off.
	relativeFactor float64
	minSamples     int
	// probeInterval is how long a skipped provider stays excluded before it is
	// admitted again for one measurement. 0 disables probing, which makes
	// exclusion permanent — see the starvation note on filterSlowRoutes.
	probeInterval time.Duration
}

func resolveSkipSlow(cfg config.GatewaySkipSlowConfig) skipSlowPolicy {
	if !cfg.EnabledValue() {
		return skipSlowPolicy{}
	}
	// 相对倍数是唯一的默认规则：配置里没写时才落默认值。显式给出 0 表示
	// "只要绝对上限这条规则"，此时不能把默认倍数补回去——否则两个规则都关的
	// 配置会被静默变成"倍数 3"，看着像没生效却仍在筛人。
	factor := cfg.RelativeFactor
	if factor == 0 && cfg.MaxFirstTokenMs == 0 {
		factor = cfg.EffectiveRelativeFactor()
	}
	return skipSlowPolicy{
		enabled:         true,
		maxFirstTokenMs: float64(cfg.MaxFirstTokenMs),
		relativeFactor:  factor,
		minSamples:      cfg.EffectiveMinSamples(),
		probeInterval:   cfg.EffectiveProbeInterval(),
	}
}

// latencySample is one provider's observed first-token latency.
type latencySample struct {
	// ewma is the smoothed latency in milliseconds. A smoothed value rather
	// than the last one: a single slow request (a cold model, a burst) must
	// not change routing, while a provider that is consistently slow must.
	ewma float64
	// samples counts observations, and decides when the value is trusted.
	samples int
	// lastAt is the most recent observation, for the status API.
	lastAt time.Time
	// skipped counts how many times this provider was passed over because of
	// this policy, so the effect is visible instead of looking like a mystery.
	skipped int
	// excludedSince is when this provider was first judged too slow in the
	// current exclusion streak. Zero while the provider is not excluded. The
	// probe interval is measured from here, so a provider is skipped normally
	// and only admitted again after it has been out of the pool that long.
	excludedSince time.Time
	// probeUntil is the end of the current admission window that lets an
	// excluded provider be measured again. Zero means no window is open. The
	// window (rather than a one-shot admission) is what makes probing work at
	// all: one request consults the policy several times as the route is
	// resolved and planned, and a one-shot flag would be consumed by the first
	// lookup and leave the actual dispatch filtered out.
	probeUntil time.Time
}

// latencyTable tracks per-provider first-token latency.
//
// Keyed by provider, not by route: the rule exists to prefer a faster upstream
// account, and one provider's adapters share the same upstream capacity, so
// splitting the measurement per adapter would add noise without adding signal.
type latencyTable struct {
	mu sync.Mutex
	// ewmaAlpha weights each new sample. 0.3 gives a half-life of roughly two
	// requests, which tracks a provider degrading over minutes without
	// letting one outlier dominate.
	observations map[string]*latencySample
}

const latencyEWMAAlpha = 0.3

func newLatencyTable() *latencyTable {
	return &latencyTable{observations: make(map[string]*latencySample)}
}

// observe records one first-token latency for a provider.
func (t *latencyTable) observe(provider string, latency time.Duration) {
	if t == nil || provider == "" || latency <= 0 {
		return
	}
	ms := float64(latency.Milliseconds())
	if ms <= 0 {
		// Sub-millisecond: not worth recording, and rounding to 0 would make
		// the provider look infinitely fast in the relative rule.
		ms = 1
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, ok := t.observations[provider]
	if !ok {
		t.observations[provider] = &latencySample{ewma: ms, samples: 1, lastAt: time.Now()}
		return
	}
	entry.ewma = latencyEWMAAlpha*ms + (1-latencyEWMAAlpha)*entry.ewma
	entry.samples++
	entry.lastAt = time.Now()
	// A measurement ends the probe: the provider has just been observed, so the
	// next exclusion streak (if it is still slow) starts from this moment. The
	// window must not stay open, or a provider that is still slow would keep
	// receiving traffic for the rest of it.
	entry.probeUntil = time.Time{}
	entry.excludedSince = time.Time{}
}

// ObserveFirstToken records one measured first-token latency for a provider.
//
// Exported because the measurement happens in the metrics layer, which already
// parses the response and knows the true first content delta; the gateway only
// consumes it. A non-positive latency is ignored: it means the backend did not
// expose a trustworthy first-token boundary, and averaging that in would make
// a silent provider look fast.
func (g *Gateway) ObserveFirstToken(provider string, firstTokenMs int) {
	if g == nil || firstTokenMs <= 0 {
		return
	}
	g.latency.observe(provider, time.Duration(firstTokenMs)*time.Millisecond)
}

// sampleCount reports how many observations a provider has accumulated.
func (t *latencyTable) sampleCount(provider string) int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if entry, ok := t.observations[provider]; ok {
		return entry.samples
	}
	return 0
}

// noteSkipped records that a provider was passed over by the policy.
func (t *latencyTable) noteSkipped(provider string) {
	if t == nil || provider == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if entry, ok := t.observations[provider]; ok {
		entry.skipped++
	}
}

// latency returns the trusted smoothed latency, and whether the provider has
// enough samples for the value to be used in a routing decision.
func (t *latencyTable) latency(provider string, minSamples int) (float64, bool, int) {
	if t == nil || provider == "" {
		return 0, false, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, ok := t.observations[provider]
	if !ok {
		return 0, false, 0
	}
	return entry.ewma, entry.samples >= minSamples, entry.skipped
}

// admitForProbe decides whether a provider the rule wants to skip should be
// admitted anyway so its recovery can be measured.
//
// The first time a provider is judged too slow it is skipped normally and the
// exclusion streak starts. Once it has been excluded for a full interval, a
// probe window opens and the provider is admitted again. Without this, skipping
// is self-reinforcing: a provider that is never picked never produces another
// measurement, so its smoothed latency stays frozen at the value that excluded
// it and it can never prove it recovered.
//
// The window stays open for one interval (several lookups happen per request,
// and a one-shot admission would be spent before dispatch) and is closed by the
// next observation, which also ends the exclusion streak.
func (t *latencyTable) admitForProbe(provider string, now time.Time, interval time.Duration) bool {
	if t == nil || provider == "" || interval <= 0 {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, ok := t.observations[provider]
	if !ok {
		return false
	}
	if entry.probeUntil.After(now) {
		// A probe window is already open: keep admitting until it is measured
		// or the window expires.
		return true
	}
	if entry.excludedSince.IsZero() {
		// First time this provider is judged too slow: start the streak and
		// skip it as the policy says.
		entry.excludedSince = now
		return false
	}
	if now.Sub(entry.excludedSince) < interval {
		return false
	}
	entry.probeUntil = now.Add(interval)
	return true
}

// ProviderLatency is one provider's latency as reported to the status API.
type ProviderLatency struct {
	FirstTokenMs float64 `json:"firstTokenMs,omitempty"`
	Samples      int     `json:"samples"`
	// Skipped is how many times the policy passed over this provider.
	Skipped int `json:"skipped,omitempty"`
	// Skipping is true while the provider is currently judged too slow.
	Skipping bool `json:"skipping,omitempty"`
}

// Latencies reports the observed first-token latency of every provider that
// has been measured, so the skip-slow policy is observable rather than a
// silent routing change.
func (g *Gateway) Latencies() map[string]ProviderLatency {
	out := make(map[string]ProviderLatency)
	g.mu.RLock()
	latency := g.latency
	g.mu.RUnlock()
	if latency == nil {
		return out
	}
	latency.mu.Lock()
	defer latency.mu.Unlock()
	for provider, entry := range latency.observations {
		out[provider] = ProviderLatency{
			FirstTokenMs: entry.ewma,
			Samples:      entry.samples,
			Skipped:      entry.skipped,
		}
	}
	return out
}

// fastRoutes drops candidates whose provider is known to answer much more
// slowly than the alternatives.
//
// Two properties matter more than the filtering itself:
//
//   - Fail-open. A provider with too few samples is always kept, so a cold
//     start, a reload (which resets the measurements) or a brand-new provider
//     never loses traffic to an unmeasured sibling.
//   - Never starve. When every candidate looks too slow, all of them are
//     returned: skipping all of them would turn "this provider is slow" into
//     "this model is unavailable", which is strictly worse. This is also why
//     a single candidate is never filtered.
func (g *Gateway) fastRoutes(routes []*Route) []*Route {
	g.mu.RLock()
	policy := g.skipSlow
	latency := g.latency
	g.mu.RUnlock()
	return filterSlowRoutes(routes, policy, latency, g.logger)
}

func filterSlowRoutes(routes []*Route, policy skipSlowPolicy, latency *latencyTable, logger *logmon.Monitor) []*Route {
	if len(routes) <= 1 {
		// 单候选（需求点名的那条）：没有可比对象，也绝不能因为"它慢"就
		// 把它去掉——那等于把"慢"升级成"不可用"。
		//
		// 这条早退与下面"可信测量不足两个"、"全部过慢则全保留"三条互为
		// 备份，任意一条都能防住单候选被清空；保留它也让常见路径少走一遍
		// 测量与比较。
		return routes
	}
	if !policy.enabled || latency == nil {
		return routes
	}

	type measured struct {
		route *Route
		ms    float64
	}
	measurements := make([]measured, 0, len(routes))
	fastest := 0.0
	for _, route := range routes {
		ms, trusted, _ := latency.latency(route.ProviderID, policy.minSamples)
		if !trusted {
			continue
		}
		measurements = append(measurements, measured{route: route, ms: ms})
		if fastest == 0 || ms < fastest {
			fastest = ms
		}
	}
	if len(measurements) < 2 {
		// Fewer than two trusted measurements: there is nothing to compare
		// against, so no decision can be made. Note this is a real gap for the
		// caller: the providers that would be filtered are exactly the ones
		// with no measurement, so they are kept by the fail-open rule below
		// anyway. Kept as an early exit so the common unevaluated case is cheap.
		return routes
	}

	kept := make([]*Route, 0, len(routes))
	skipped := make([]string, 0, len(routes))
	now := time.Now()
	for _, route := range routes {
		ms, trusted, _ := latency.latency(route.ProviderID, policy.minSamples)
		if !trusted {
			kept = append(kept, route)
			continue
		}
		if slow := tooSlow(ms, fastest, policy); slow {
			// A provider the rule would drop is admitted while its probe
			// window is open. Skipping is self-reinforcing — an excluded
			// provider produces no new measurement, so its smoothed latency
			// stays frozen at the value that excluded it and a recovery could
			// never be observed. The probe is the only way back into the pool.
			if latency.admitForProbe(route.ProviderID, now, policy.probeInterval) {
				if logger != nil {
					logger.Debugf("gateway: probing previously slow provider %s (last measured first-token %.0fms, fastest %.0fms)",
						route.ProviderID, ms, fastest)
				}
				kept = append(kept, route)
				continue
			}
			skipped = append(skipped, route.ProviderID)
			continue
		}
		kept = append(kept, route)
	}
	if len(kept) == 0 {
		// Never starve: every candidate is slow, so the fastest of them still
		// beats refusing to route.
		return routes
	}
	if len(skipped) > 0 && logger != nil {
		sort.Strings(skipped)
		logger.Debugf("gateway: skipping slow provider(s) %v for this request (fastest observed first-token %.0fms)",
			skipped, fastest)
	}
	for _, provider := range skipped {
		latency.noteSkipped(provider)
	}
	return kept
}

// tooSlow reports whether an observed latency is bad enough to skip, using the
// absolute ceiling and the relative factor. Either rule may be off.
func tooSlow(ms, fastest float64, policy skipSlowPolicy) bool {
	if policy.maxFirstTokenMs > 0 && ms > policy.maxFirstTokenMs {
		return true
	}
	if policy.relativeFactor > 0 && fastest > 0 && ms > fastest*policy.relativeFactor {
		return true
	}
	return false
}

// describeSkipSlow renders the active policy for logs.
func (p skipSlowPolicy) describe() string {
	if !p.enabled {
		return "disabled"
	}
	if p.maxFirstTokenMs > 0 {
		return fmt.Sprintf("maxFirstTokenMs=%.0f relativeFactor=%.2f minSamples=%d",
			p.maxFirstTokenMs, p.relativeFactor, p.minSamples)
	}
	return fmt.Sprintf("relativeFactor=%.2f minSamples=%d", p.relativeFactor, p.minSamples)
}
