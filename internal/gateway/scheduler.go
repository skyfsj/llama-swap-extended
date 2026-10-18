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
	// authFailureAt is when the last 401/402 was recorded. A single 401 is
	// often a transient upstream hiccup, so the first one only benches the
	// credential briefly; repeated failures escalate to the configured auth
	// cooldown (see applyFailure).
	authFailureAt time.Time

	// LastError/LastUsed surface in the status API.
	LastError   string
	LastErrorAt time.Time
	LastUsed    time.Time

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
	if now.Before(c.authUntil) || now.Before(c.rateLimitUntil) || now.Before(c.transientUntil) || now.Before(c.quotaUntil) {
		return false
	}
	if c.MaxConcurrent > 0 && c.inflight >= c.MaxConcurrent {
		return false
	}
	return true
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
		c.authFailureAt = now
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

// RecentAuthFailure reports whether an auth failure was recorded within the
// escalation window, i.e. whether the upstream keeps rejecting the credential.
func (c *Credential) RecentAuthFailure(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.authFailureAt.IsZero() && now.Sub(c.authFailureAt) < authEscalationWindow
}

// ClearCooldowns removes any active cooldowns (operator action from the UI).
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

// stickyTable maps session hashes to credential IDs with a sliding TTL.
type stickyTable struct {
	mu      sync.Mutex
	entries map[string]stickyEntry
	ttl     time.Duration
}

type stickyEntry struct {
	credentialID string
	expires      time.Time
}

func newStickyTable(ttl time.Duration) *stickyTable {
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &stickyTable{entries: make(map[string]stickyEntry), ttl: ttl}
}

// get returns the bound credential ID, sliding the TTL forward on a hit.
func (t *stickyTable) get(sessionHash string, now time.Time) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, ok := t.entries[sessionHash]
	if !ok {
		return "", false
	}
	if now.After(entry.expires) {
		delete(t.entries, sessionHash)
		return "", false
	}
	entry.expires = now.Add(t.ttl)
	t.entries[sessionHash] = entry
	return entry.credentialID, true
}

// bind records a session->credential binding. An existing, unexpired binding
// for a different credential is never overwritten (sub2api semantics: the
// sticky target stays bound and resumes serving when healthy again, unless
// the caller explicitly clears it after observing it is permanently gone).
func (t *stickyTable) bind(sessionHash, credentialID string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if existing, ok := t.entries[sessionHash]; ok && now.Before(existing.expires) {
		if existing.credentialID != credentialID {
			return
		}
	}
	t.entries[sessionHash] = stickyEntry{credentialID: credentialID, expires: now.Add(t.ttl)}
}

// unbind removes a binding (called when the bound credential is gone from
// configuration or permanently failed).
func (t *stickyTable) unbind(sessionHash string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, sessionHash)
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
