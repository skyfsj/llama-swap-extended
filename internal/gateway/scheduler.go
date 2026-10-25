// Package gateway implements the upstream provider forwarding path: a
// scheduler that picks one of a provider's API keys per request, and an
// httputil.ReverseProxy based forwarder.
//
// Scheduling follows the "filter at selection time" model (no enable/disable
// state machines): every credential carries cooldown timestamps and a
// concurrency semaphore, and each pick filters out credentials that are
// cooling down or saturated. Candidates are grouped into priority tiers; the
// highest tier with an available credential is chosen, weighted round-robin
// within the tier. Session affinity overrides the rotation when the session's
// bound credential still passes all filters.
package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"math/rand"
	"sort"
	"sync"
	"time"
)

// Credential is one schedulable upstream API key. Created per configured key
// at gateway construction; state (cooldowns, in-flight count) lives here.
type Credential struct {
	// ID is a stable identifier (provider + key hash) used for stickiness
	// and logging. Never contains the key material.
	ID string

	// Value resolved from config value or valueEnv. Empty when the env var
	// is unset; such credentials are unschedulable.
	Value string

	Priority      int
	Weight        int
	MaxConcurrent int // 0 = unlimited

	// Proxy override resolved for this credential: "" inherits environment,
	// "direct" forces no proxy, otherwise a proxy URL.
	Proxy string

	Disabled bool // manual disable from config

	// Cooldown timestamps; a credential is skipped while now < until.
	// Separate classes allow distinct logging/observability.
	authUntil      time.Time // 401/403
	rateLimitUntil time.Time // 429
	transientUntil time.Time // 5xx/network
	// quotaUntil parks the credential when quota enforcement decides a usage
	// window is full. It is set by the quota refresher, not by upstream
	// errors, and cleared when a window frees up or enforcement is disabled.
	quotaUntil time.Time
	// authFailures counts consecutive auth failures. Escalation must not
	// decay while the credential is already benched: a key that is cooling
	// down for ten minutes is not retried until that window ends, by which
	// time a wall-clock recency window has long expired and the next 401
	// would restart the short probe — leaving the credential alternating
	// between probe and full cooldown forever instead of staying benched.
	authFailures int

	// LastError/LastUsed surface in the status API.
	LastError   string
	LastErrorAt time.Time
	LastUsed    time.Time

	// lastUpstream is the most recent upstream answer this credential
	// received (status plus a bounded body). While the credential is cooling
	// down it is what the client is told, so a pool whose every key is
	// benched for 401 keeps answering 401 instead of a synthetic 502: the
	// upstream did refuse the request, and that is the useful answer.
	lastUpstreamStatus int
	lastUpstreamBody   string

	inflight int

	mu sync.Mutex
}

// keyID hashes the secret material into a stable, non-reversible ID.
func keyID(provider, secret string) string {
	sum := sha256.Sum256([]byte(provider + "\x00" + secret))
	return provider + "#" + hex.EncodeToString(sum[:8])
}

// available reports whether the credential may serve a request right now.
func (c *Credential) available(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Disabled || c.Value == "" {
		return false
	}
	if c.concurrencyFullLocked() {
		return false
	}
	return !c.coolingDownLocked(now)
}

// concurrencyFullLocked reports whether the credential is at its concurrency
// cap. The caller must hold c.mu.
func (c *Credential) concurrencyFullLocked() bool {
	return c.MaxConcurrent > 0 && c.inflight >= c.MaxConcurrent
}

// coolingDownLocked reports whether a cooldown window is still active. The
// caller must hold c.mu. A disabled credential or one with no key material is
// not "cooling down": it is permanently unusable and no amount of waiting
// helps.
func (c *Credential) coolingDownLocked(now time.Time) bool {
	return now.Before(c.authUntil) || now.Before(c.rateLimitUntil) ||
		now.Before(c.transientUntil) || now.Before(c.quotaUntil)
}

// cooldownRemaining reports how long until this credential becomes usable
// again, ignoring concurrency (which frees on its own as requests finish).
//
// It is what lets a pool with no available credential still answer a request:
// when every key is benched, the one whose bench ends soonest is the best guess
// available, and trying it beats failing the request outright. A credential
// that will never recover (disabled, or no key material) reports false so the
// caller never picks it as a fallback.
//
// Auth and quota benches are deliberately excluded. A 401/402 means the key
// itself is rejected, and a quota park means the account has no allowance left:
// retrying either one cannot succeed, and hammering the upstream with a key it
// just refused is how an account gets rate-limited or blocked. Only the purely
// time-based benches (transient failures, rate limiting) are worth a retry,
// because those are predictions about the upstream rather than verdicts about
// the credential.
func (c *Credential) cooldownRemaining(now time.Time) (time.Duration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Disabled || c.Value == "" {
		return 0, false
	}
	if c.concurrencyFullLocked() {
		// Saturated, not benched: it would be usable the moment a slot frees,
		// so it is a legitimate fallback with no wait.
		return 0, true
	}
	if now.Before(c.authUntil) || now.Before(c.quotaUntil) {
		// A verdict, not a prediction: never retried by the fallback.
		return 0, false
	}
	var latest time.Time
	for _, until := range []time.Time{c.rateLimitUntil, c.transientUntil} {
		if until.After(latest) {
			latest = until
		}
	}
	if latest.IsZero() || !now.Before(latest) {
		return 0, true
	}
	return latest.Sub(now), true
}

// acquire increments the in-flight count. The caller must invoke the
// returned release function exactly once when the request completes.
func (c *Credential) acquire() (release func(), ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Disabled || c.Value == "" {
		return nil, false
	}
	if c.MaxConcurrent > 0 && c.inflight >= c.MaxConcurrent {
		return nil, false
	}
	c.inflight++
	c.LastUsed = time.Now()
	released := false
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if released {
			return
		}
		released = true
		if c.inflight > 0 {
			c.inflight--
		}
	}, true
}

// cooldown applies a cooldown of the given class starting now.
func (c *Credential) cooldown(now time.Time, class CooldownClass, until time.Time, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch class {
	case CooldownAuth:
		if until.After(c.authUntil) {
			c.authUntil = until
		}
		c.authFailures++
	case CooldownRateLimit:
		if until.After(c.rateLimitUntil) {
			c.rateLimitUntil = until
		}
	case CooldownTransient:
		if until.After(c.transientUntil) {
			c.transientUntil = until
		}
	}
	c.LastError = reason
	c.LastErrorAt = now
}

// RecentAuthFailure reports whether this credential is already known to be
// failing auth. It is true once an auth failure has been recorded and stays
// true until an operator clears the cooldowns, so escalation does not depend
// on how long the credential was benched for.
func (c *Credential) RecentAuthFailure(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.authFailures > 0
}

// ClearCooldowns removes any active cooldowns (operator action from the UI).
// The recorded auth failures stay on purpose: clearing the cooldowns lifts the
// ben, it does not mint a new credential. An operator retrying right after a
// 401 must not get the short first-failure probe again, otherwise every clear
// resets the escalation and a dead key keeps being retried every 30 seconds.
func (c *Credential) ClearCooldowns() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.authUntil = time.Time{}
	c.rateLimitUntil = time.Time{}
	c.transientUntil = time.Time{}
	c.quotaUntil = time.Time{}
}

// SetQuotaCooldown parks the credential until the given time (quota
// enforcement). It never shortens an existing park.
func (c *Credential) SetQuotaCooldown(until time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if until.After(c.quotaUntil) {
		c.quotaUntil = until
	}
}

// ClearQuotaCooldown releases a quota park (window freed or enforcement off).
func (c *Credential) ClearQuotaCooldown() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.quotaUntil = time.Time{}
}

// QuotaCooldownUntil reports the current quota park expiry (zero when none).
func (c *Credential) QuotaCooldownUntil() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.quotaUntil
}

// CooldownForTest applies a cooldown from another package's tests.
func CooldownForTest(c *Credential, class CooldownClass, until time.Time, reason string) {
	c.cooldown(time.Now(), class, until, reason)
}

// noteError records a non-cooldown error for observability (request-scoped
// upstream failures).
func (c *Credential) noteError(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.LastError = reason
	c.LastErrorAt = time.Now()
}

// noteUpstream records the upstream answer this credential received, so a
// request that arrives while it is cooling down can still report the real
// status and body instead of a synthetic one.
func (c *Credential) noteUpstream(status int, snippet string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastUpstreamStatus = status
	c.lastUpstreamBody = snippet
}

// LastUpstream reports the most recent upstream answer recorded for this
// credential (status 0 when none was).
func (c *Credential) LastUpstream() (int, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastUpstreamStatus, c.lastUpstreamBody
}

// CooldownClass classifies why a credential is cooling down.
type CooldownClass int

const (
	// CooldownUnclassified is the zero value on purpose: an attempt that did
	// not implicate the credential (a read error or a truncated response) must
	// never be mistaken for an auth failure when it lands in applyFailure.
	CooldownUnclassified CooldownClass = iota
	CooldownAuth
	CooldownRateLimit
	CooldownTransient
)

// CooldownStatus describes one credential's runtime state for the status API.
type CooldownStatus struct {
	ID             string    `json:"id"`
	Disabled       bool      `json:"disabled"`
	Unresolved     bool      `json:"unresolvedEnv"`
	Inflight       int       `json:"inflight"`
	MaxConcurrent  int       `json:"maxConcurrent"`
	AuthUntil      time.Time `json:"authCooldownUntil,omitempty"`
	RateLimitUntil time.Time `json:"rateLimitCooldownUntil,omitempty"`
	TransientUntil time.Time `json:"transientCooldownUntil,omitempty"`
	// QuotaUntil is set by quota enforcement, not by upstream errors.
	QuotaUntil  time.Time `json:"quotaCooldownUntil,omitempty"`
	LastError   string    `json:"lastError,omitempty"`
	LastErrorAt time.Time `json:"lastErrorAt,omitempty"`
	LastUsed    time.Time `json:"lastUsed,omitempty"`
}

// Status snapshots the credential state.
func (c *Credential) Status() CooldownStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return CooldownStatus{
		ID:             c.ID,
		Disabled:       c.Disabled,
		Unresolved:     c.Value == "",
		Inflight:       c.inflight,
		MaxConcurrent:  c.MaxConcurrent,
		AuthUntil:      c.authUntil,
		RateLimitUntil: c.rateLimitUntil,
		TransientUntil: c.transientUntil,
		QuotaUntil:     c.quotaUntil,
		LastError:      c.LastError,
		LastErrorAt:    c.LastErrorAt,
		LastUsed:       c.LastUsed,
	}
}

// scheduler picks among a set of credentials. One instance exists per
// (provider, model) group; bare-name pools create one scheduler spanning
// providers. The zero value is not usable; use newScheduler.
type scheduler struct {
	mu      sync.Mutex
	members []*Credential
	lastID  string // last selected credential ID for rotation continuity
}

func newScheduler(members []*Credential) *scheduler {
	sort.Slice(members, func(i, j int) bool {
		if members[i].Priority != members[j].Priority {
			return members[i].Priority > members[j].Priority
		}
		return members[i].ID < members[j].ID
	})
	return &scheduler{members: members}
}

// pick selects a credential: filter to available ones, take the highest
// priority tier present, then weighted-random within the tier. lastID
// rotation breaks ties so a shrinking candidate set does not starve members
// (the rotation remembers the last selected ID, not an index).
// hasCandidate reports whether any credential would be returned by pick right
// now: not excluded and not unavailable. It lets a caller decide which routes
// are worth comparing before spending a pick on them.
func (s *scheduler) hasCandidate(exclude map[string]struct{}) bool {
	if s == nil {
		// A route assembled by hand (or one whose members failed to resolve)
		// has no scheduler; reporting "nothing to pick" lets the caller try
		// the next route instead of panicking on it.
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for _, c := range s.members {
		if _, skipped := exclude[c.ID]; skipped {
			continue
		}
		if c.available(now) {
			return true
		}
	}
	return false
}

// pickFallback selects the best credential to try when pick found nothing
// available: the one whose cooldown ends soonest, among those this request has
// not tried yet.
//
// The pool reaching this state means every key is benched — a burst exhausted
// the transient cooldowns, or several keys failed at once. Refusing the request
// outright is the worst answer available: the upstream may well serve it now
// (the cooldown may be conservative), and if it does not, the attempt returns a
// real upstream error instead of a synthetic "all keys are cooling down". The
// cost of being wrong is one failed attempt; the cost of not trying is a
// request that had a chance.
//
// "Soonest remaining" is the right order: a key benched for 5 more seconds is a
// far better guess than one with four minutes left, and it is what makes the
// fallback converge back to normal scheduling instead of fighting it.
func (s *scheduler) pickFallback(now time.Time, exclude map[string]struct{}) (*Credential, time.Duration) {
	if s == nil {
		return nil, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var (
		best     *Credential
		bestWait time.Duration
	)
	for _, c := range s.members {
		if _, skipped := exclude[c.ID]; skipped {
			continue
		}
		wait, usable := c.cooldownRemaining(now)
		if !usable {
			continue
		}
		if best == nil || wait < bestWait {
			best, bestWait = c, wait
		}
	}
	return best, bestWait
}

// pick selects a credential: filter to available ones, take the highest
// priority tier present, then weighted-random within the tier. lastID rotation
// breaks ties so a shrinking candidate set does not starve members (the
// rotation remembers the last selected ID, not an index).
func (s *scheduler) pick(now time.Time, exclude map[string]struct{}) *Credential {
	s.mu.Lock()
	defer s.mu.Unlock()

	available := make([]*Credential, 0, len(s.members))
	for _, c := range s.members {
		if _, skipped := exclude[c.ID]; skipped {
			continue
		}
		if c.available(now) {
			available = append(available, c)
		}
	}
	if len(available) == 0 {
		return nil
	}

	// Highest priority tier.
	top := available[0].Priority
	tier := available[:0]
	for _, c := range available {
		if c.Priority == top {
			tier = append(tier, c)
		}
	}

	chosen := weightedPick(tier, s.lastID, rand.Intn)
	s.lastID = chosen.ID
	return chosen
}

// weightedPick does weighted random within a tier. All-zero weights share
// equally; small weights are amplified (smoothing factor 100) to avoid
// rounding skew. When prev is among the tied candidates the random tiebreak
// prefers its successor, so a shrinking candidate set keeps rotating fairly.
func weightedPick(tier []*Credential, prev string, randn func(int) int) *Credential {
	if len(tier) == 1 {
		return tier[0]
	}

	sum := 0
	for _, c := range tier {
		sum += c.Weight
	}
	if sum == 0 {
		// Equal probability; rotate past prev when it is in the tier.
		start := 0
		for i, c := range tier {
			if c.ID == prev {
				start = i + 1
				break
			}
		}
		return tier[start%len(tier)]
	}

	factor := 1
	if sum < 10 {
		factor = 100
	}
	total := 0
	for _, c := range tier {
		total += c.Weight * factor
	}
	r := randn(total)
	for _, c := range tier {
		r -= c.Weight * factor
		if r < 0 {
			return c
		}
	}
	return tier[len(tier)-1]
}

// stickyTable maps session hashes to the upstream route that served them with
// a sliding TTL.
//
// The binding is route-level on purpose: a provider's API surfaces keep
// separate caches upstream (the same conversation sent to an OpenAI adapter
// and to an Anthropic adapter is cached twice), so sticking to a provider is
// not enough — the session must keep hitting the same provider *and adapter*
// that served it. The credential is recorded too: keys of one account share
// the provider-side cache, but keys of different accounts do not, so a
// session should not hop between them while its first choice is usable.
type stickyTable struct {
	mu      sync.Mutex
	entries map[string]stickyEntry
	ttl     time.Duration
}

type stickyEntry struct {
	// provider and route identify the upstream surface. route is the stable
	// Route.Key() (provider + adapter + model), provider the owning provider
	// so a surface that is momentarily unavailable can fall back to a sibling
	// surface of the same provider before another provider takes the session.
	provider     string
	route        string
	credentialID string
	expires      time.Time
}

func newStickyTable(ttl time.Duration) *stickyTable {
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &stickyTable{entries: make(map[string]stickyEntry), ttl: ttl}
}

// get returns the bound route, sliding the TTL forward on a hit.
func (t *stickyTable) get(sessionHash string, now time.Time) (stickyEntry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, ok := t.entries[sessionHash]
	if !ok {
		return stickyEntry{}, false
	}
	if now.After(entry.expires) {
		delete(t.entries, sessionHash)
		return stickyEntry{}, false
	}
	entry.expires = now.Add(t.ttl)
	t.entries[sessionHash] = entry
	return entry, true
}

// bind records a session->route binding.
//
// A binding for another route is replaced only when that route is no longer
// usable (boundUnavailable). Once a session has moved off a provider it builds
// its prompt cache on the provider that answered, so the original provider
// becoming healthy again must not pull it back: that would discard the cache
// it just accumulated. The session stays where it landed until that provider
// fails too, or until the binding expires.
//
// The bound credential may be refreshed within the same route: a key rotation
// or a cooldown should not cost the session its provider-side cache.
// sweepExpired drops the bindings whose TTL has passed. get removes an entry
// only when that exact session comes back, so a session that never returns
// would otherwise stay in the table for as long as the daemon runs.
func (t *stickyTable) sweepExpired(now time.Time) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	swept := 0
	for hash, entry := range t.entries {
		if now.After(entry.expires) {
			delete(t.entries, hash)
			swept++
		}
	}
	return swept
}

func (t *stickyTable) bind(sessionHash, provider, route, credentialID string, now time.Time, boundUnavailable bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if existing, ok := t.entries[sessionHash]; ok && now.Before(existing.expires) {
		if existing.route != route && !boundUnavailable {
			return
		}
	}
	t.entries[sessionHash] = stickyEntry{
		provider:     provider,
		route:        route,
		credentialID: credentialID,
		expires:      now.Add(t.ttl),
	}
}

// unbind removes a binding (called when the bound route is gone from
// configuration or permanently failed).
func (t *stickyTable) unbind(sessionHash string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, sessionHash)
}

// snapshot copies the live bindings. It is how a configuration reload carries
// session affinity across a gateway rebuild: the credentials, cooldowns and
// in-flight counts of the new instance are all fresh, but which provider a
// conversation was talking to is not something the new config should decide.
func (t *stickyTable) snapshot() map[string]stickyEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]stickyEntry, len(t.entries))
	for hash, entry := range t.entries {
		out[hash] = entry
	}
	return out
}

// adopt installs bindings captured from a previous instance, keeping only
// those whose route still exists in the new configuration. A binding whose
// surface was removed cannot serve anything and would only be dropped later
// on its first use; the credential is kept even when it is gone, because the
// route is what carries the provider-side cache and the attempt code already
// falls back to any usable credential of that route.
func (t *stickyTable) adopt(entries map[string]stickyEntry, keep func(route string) bool) int {
	if len(entries) == 0 {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	adopted := 0
	for hash, entry := range entries {
		if !keep(entry.route) {
			continue
		}
		t.entries[hash] = entry
		adopted++
	}
	return adopted
}

// clearCredential removes every session bound to the given credential ID.
func (t *stickyTable) clearCredential(credentialID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for hash, entry := range t.entries {
		if entry.credentialID == credentialID {
			delete(t.entries, hash)
		}
	}
}

// poolCandidate is one provider's bid for a bare model name. A provider with
// several adapters exposing the same model is a single candidate: its surfaces
// are one upstream account with one quota, so it should hold one share of the
// traffic, not one share per adapter.
type poolCandidate struct {
	provider string
	route    *Route
	priority int
	weight   int
}

// poolRotator carries the rotation state of one bare model name across
// requests. Without it every request would build a fresh scheduler, whose
// "last pick" is always the empty string, and a zero-weight pool would always
// send traffic to the same provider (the first in sorted order).
type poolRotator struct {
	mu   sync.Mutex
	last string
}

// pick selects one provider: highest priority tier first, weighted random
// within it, rotating past the previous pick when all weights are zero.
func (r *poolRotator) pick(candidates []poolCandidate, randn func(int) int) (poolCandidate, bool) {
	if len(candidates) == 0 {
		return poolCandidate{}, false
	}
	top := candidates[0].priority
	for _, candidate := range candidates[1:] {
		if candidate.priority > top {
			top = candidate.priority
		}
	}
	tier := make([]poolCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.priority == top {
			tier = append(tier, candidate)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	chosen := weightedCandidatePick(tier, r.last, randn)
	r.last = chosen.provider
	return chosen, true
}

// rotationState copies each bare model's last provider pick. The cursor is
// per bare name, so a reload preserves the distribution instead of resetting it.
func (g *Gateway) rotationState() map[string]string {
	out := map[string]string{}
	g.pools.Range(func(key, value any) bool {
		model, _ := key.(string)
		rotator, _ := value.(*poolRotator)
		if rotator == nil {
			return true
		}
		rotator.mu.Lock()
		last := rotator.last
		rotator.mu.Unlock()
		if last == "" {
			return true
		}
		out[model] = last
		return true
	})
	return out
}

// adoptRotation restores the per-model rotation cursor. Entries for models the
// new configuration does not serve are dropped: they describe a pool that no
// longer exists.
func (g *Gateway) adoptRotation(state map[string]string) {
	for model, last := range state {
		// A cursor is created when it is missing: the carried state describes a
		// pool the new configuration still serves, and dropping it would reset
		// the rotation the old gateway had established. An existing cursor is
		// left alone, because the new gateway may already have served a request.
		rotator := g.rotator(model)
		rotator.mu.Lock()
		if rotator.last == "" {
			rotator.last = last
		}
		rotator.mu.Unlock()
	}
}

// CarryState moves the per-session bindings and rotation cursors of the
// previous gateway into this one. A configuration reload rebuilds the Gateway
// from scratch, and the new instance starts with every credential fresh: that
// is correct for cooldowns and in-flight counts, but it would also drop which
// provider each conversation was talking to, so the next request of every
// live conversation would land on a different upstream and lose its prompt
// cache. State that describes the old topology is dropped instead of carried.
//
// It returns the number of bindings carried. Passing the receiver or nil is a
// no-op.
func (g *Gateway) CarryState(previous *Gateway) int {
	if g == nil || previous == nil || g == previous {
		return 0
	}
	g.mu.RLock()
	routes := make(map[string]struct{}, len(g.routes))
	for _, route := range g.routes {
		routes[route.Key()] = struct{}{}
	}
	sticky := g.sticky
	ttl := g.cooldown.stickyTTL
	g.mu.RUnlock()
	if sticky == nil {
		return 0
	}
	if len(routes) > 0 {
		sticky.ttl = ttl
	}
	adopted := sticky.adopt(previous.stickySnapshot(), func(route string) bool {
		if len(routes) == 0 {
			return false
		}
		_, ok := routes[route]
		return ok
	})
	g.adoptRotation(previous.rotationState())
	return adopted
}

// stickySnapshot copies the live session bindings for CarryState.
func (g *Gateway) stickySnapshot() map[string]stickyEntry {
	if g == nil {
		return nil
	}
	g.mu.RLock()
	sticky := g.sticky
	g.mu.RUnlock()
	if sticky == nil {
		return nil
	}
	return sticky.snapshot()
}

// weightedCandidatePick mirrors weightedPick for provider candidates.
func weightedCandidatePick(tier []poolCandidate, prev string, randn func(int) int) poolCandidate {
	if len(tier) == 1 {
		return tier[0]
	}
	sum := 0
	for _, candidate := range tier {
		sum += candidate.weight
	}
	if sum == 0 {
		start := 0
		for i, candidate := range tier {
			if candidate.provider == prev {
				start = i + 1
				break
			}
		}
		return tier[start%len(tier)]
	}
	factor := 1
	if sum < 10 {
		factor = 100
	}
	total := 0
	for _, candidate := range tier {
		total += candidate.weight * factor
	}
	r := randn(total)
	for _, candidate := range tier {
		r -= candidate.weight * factor
		if r < 0 {
			return candidate
		}
	}
	return tier[len(tier)-1]
}

// SessionHash derives a sticky session key. Explicit headers win; both
// branches mix the authenticated key id in so two callers presenting the
// same session header (or identical prompt prefixes) never share one
// credential binding. The upstream-facing session id (Session.ID) keeps the
// caller's raw value; only the affinity key is scoped per caller.
func SessionHash(headerValue, clientIP, keyID, bodyFingerprint string) string {
	if v := trimSpace(headerValue); v != "" {
		return "h:" + hashString(v+"\x00"+keyID)
	}
	return "c:" + hashString(clientIP+"\x00"+keyID+"\x00"+bodyFingerprint)
}

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}

func trimSpace(s string) string {
	start := 0
	for start < len(s) && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	end := len(s)
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}
