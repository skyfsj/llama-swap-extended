package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/gateway"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

const (
	// quotaMinRefreshInterval throttles upstream quota calls per credential.
	// A manual refresh inside the window returns the cached snapshot instead
	// of hammering the subscription.
	quotaMinRefreshInterval = 30 * time.Second
	// quotaResponseLimit bounds the quota response body.
	quotaResponseLimit = 1 << 20
	// quotaRefresherTick is how often the background refresher checks whether
	// any provider is due for a refresh.
	quotaRefresherTick = 30 * time.Second
	// quotaMissingStatus marks a window whose path was absent from the
	// response, so a wrong mapping is visible instead of silently reading 0%.
	quotaMissingStatus = "missing"
)

// quotaWindowSnapshot is one usage window of a credential.
type quotaWindowSnapshot struct {
	ID string `json:"id"`
	// Label is the display fallback when the UI has no translation for ID.
	Label string `json:"label"`
	// Percent is the USED share of the window (0-100) after normalization.
	Percent          int    `json:"percent"`
	RemainingPercent int    `json:"remainingPercent"`
	Status           string `json:"status,omitempty"`
	ResetsAt         string `json:"resetsAt,omitempty"`
}

// quotaExtraSnapshot is one extra scalar row (credit balance, counters).
type quotaExtraSnapshot struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Value any    `json:"value"`
}

// credentialQuota is the display snapshot for one credential.
type credentialQuota struct {
	ProviderID string                `json:"providerID"`
	KeyID      string                `json:"keyID"`
	Windows    []quotaWindowSnapshot `json:"windows,omitempty"`
	Extras     []quotaExtraSnapshot  `json:"extras,omitempty"`
	FetchedAt  time.Time             `json:"fetchedAt,omitempty"`
	Error      string                `json:"error,omitempty"`
	// Enforced reports that the credential is currently parked by quota
	// enforcement, with QuotaCooldownUntil naming the release time.
	Enforced           bool      `json:"enforced,omitempty"`
	QuotaCooldownUntil time.Time `json:"quotaCooldownUntil,omitempty"`
}

// quotaStore caches the latest snapshot per credential. Values are never
// persisted; the cache is dropped on restart.
type quotaStore struct {
	mu       sync.Mutex
	entries  map[string]*quotaEntry
	inflight map[string]bool
}

type quotaEntry struct {
	snapshot    credentialQuota
	lastAttempt time.Time
}

func newQuotaStore() *quotaStore {
	return &quotaStore{entries: make(map[string]*quotaEntry), inflight: make(map[string]bool)}
}

func (q *quotaStore) get(keyID string) (credentialQuota, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	entry, ok := q.entries[keyID]
	if !ok {
		return credentialQuota{}, false
	}
	return entry.snapshot, true
}

func (q *quotaStore) put(snapshot credentialQuota) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.entries[snapshot.KeyID] == nil {
		q.entries[snapshot.KeyID] = &quotaEntry{}
	}
	entry := q.entries[snapshot.KeyID]
	entry.snapshot = snapshot
	entry.lastAttempt = time.Now()
}

// throttled reports whether an upstream attempt for this credential happened
// recently enough to reuse the cached value.
func (q *quotaStore) throttled(keyID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	entry, ok := q.entries[keyID]
	return ok && time.Since(entry.lastAttempt) < quotaMinRefreshInterval
}

// drop removes credentials that are no longer configured.
func (q *quotaStore) prune(valid map[string]struct{}) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for id := range q.entries {
		if _, ok := valid[id]; !ok {
			delete(q.entries, id)
		}
	}
}

// credentialTarget is one credential to query: the live gateway credential
// plus the provider entry it belongs to.
type credentialTarget struct {
	providerID string
	provider   config.ProviderConfig
	cred       *gateway.Credential
}

// quotaTargets lists the distinct credentials of a provider (optionally just
// one key). A provider's credentials appear on every route; adapters may add
// their own, so the result is de-duplicated by credential id.
func (s *Server) quotaTargets(providerID, keyID string) []credentialTarget {
	gw := s.currentGateway()
	if gw == nil {
		return nil
	}
	cfg := s.currentConfig()
	provider, ok := cfg.Providers[providerID]
	if !ok || provider.Quota == nil {
		return nil
	}
	seen := make(map[string]struct{})
	targets := make([]credentialTarget, 0, len(provider.ApiKeys))
	for _, route := range gw.AllRoutes() {
		if route.ProviderID != providerID {
			continue
		}
		for _, cred := range route.Credentials() {
			if keyID != "" && cred.ID != keyID {
				continue
			}
			if _, duplicate := seen[cred.ID]; duplicate {
				continue
			}
			seen[cred.ID] = struct{}{}
			targets = append(targets, credentialTarget{providerID: providerID, provider: provider, cred: cred})
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].cred.ID < targets[j].cred.ID })
	return targets
}

// handleAPIProvidersQuotas returns the cached snapshots without touching the
// upstream; credentials never refreshed report an empty window list.
func (s *Server) handleAPIProvidersQuotas(w http.ResponseWriter, r *http.Request) {
	if s.quota == nil {
		writeJSON(w, map[string]any{"credentials": []credentialQuota{}})
		return
	}
	cfg := s.currentConfig()
	out := make([]credentialQuota, 0)
	for _, id := range sortedProviderIDs(cfg) {
		for _, target := range s.quotaTargets(id, "") {
			snapshot, ok := s.quota.get(target.cred.ID)
			if !ok {
				snapshot = credentialQuota{ProviderID: id, KeyID: target.cred.ID}
			}
			snapshot.Enforced, snapshot.QuotaCooldownUntil = quotaEnforcementState(target.cred, time.Now())
			out = append(out, snapshot)
		}
	}
	writeJSON(w, map[string]any{"credentials": out})
}

// handleAPIProvidersQuotasRefresh queries the upstream for one credential or
// every credential of a provider (sequentially, so a scope-less key cannot
// stampede the subscription).
func (s *Server) handleAPIProvidersQuotasRefresh(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "could not read request body")
		return
	}
	var req struct {
		ProviderID string `json:"providerID"`
		KeyID      string `json:"keyID,omitempty"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}
	providerID := strings.TrimSpace(req.ProviderID)
	if providerID == "" {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "providerID is required")
		return
	}
	targets := s.quotaTargets(providerID, strings.TrimSpace(req.KeyID))
	if len(targets) == 0 {
		swaputil.SendResponse(w, r, http.StatusNotFound, fmt.Sprintf("provider %s has quota refresh disabled or no matching credential", providerID))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	out := make([]credentialQuota, 0, len(targets))
	for _, target := range targets {
		// An operator action bypasses the throttle: a manual refresh must
		// return fresh data. The background refresher keeps it.
		out = append(out, s.refreshCredentialQuota(ctx, target, true))
	}
	writeJSON(w, map[string]any{"credentials": out})
}

// refreshCredentialQuota fetches one credential's quota and caches it. When
// force is false an attempt inside the throttle window returns the cached
// value instead.
func (s *Server) refreshCredentialQuota(ctx context.Context, target credentialTarget, force bool) credentialQuota {
	keyID := target.cred.ID
	if !force && s.quota.throttled(keyID) {
		if cached, ok := s.quota.get(keyID); ok {
			cached.Enforced, cached.QuotaCooldownUntil = quotaEnforcementState(target.cred, time.Now())
			return cached
		}
	}

	snapshot, err := s.fetchCredentialQuota(ctx, target)
	if err != nil {
		// A failed refresh keeps the last good windows but reports the error.
		previous, ok := s.quota.get(keyID)
		if !ok {
			previous = credentialQuota{ProviderID: target.providerID, KeyID: keyID}
		}
		previous.Error = err.Error()
		previous.Enforced, previous.QuotaCooldownUntil = quotaEnforcementState(target.cred, time.Now())
		s.quota.put(previous)
		return previous
	}
	s.applyQuotaEnforcement(target, snapshot)
	s.quota.put(snapshot)
	snapshot.Enforced, snapshot.QuotaCooldownUntil = quotaEnforcementState(target.cred, time.Now())
	return snapshot
}

// fetchCredentialQuota performs one upstream request with the credential's own
// secret, proxy and the provider timeouts.
func (s *Server) fetchCredentialQuota(ctx context.Context, target credentialTarget) (credentialQuota, error) {
	snapshot := credentialQuota{ProviderID: target.providerID, KeyID: target.cred.ID}
	quota := target.provider.Quota
	if quota == nil {
		return snapshot, fmt.Errorf("provider %s has no quota configuration", target.providerID)
	}
	if strings.TrimSpace(target.cred.Value) == "" {
		return snapshot, fmt.Errorf("credential has no usable value (unset environment variable)")
	}

	ctx, cancel := context.WithTimeout(ctx, quota.Timeout())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, quota.Endpoint, nil)
	if err != nil {
		return snapshot, fmt.Errorf("invalid quota endpoint: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	for name, value := range quota.Headers {
		req.Header.Set(name, value)
	}
	switch quota.Auth.Style {
	case config.QuotaAuthNone:
	case config.QuotaAuthHeader:
		req.Header.Set(quota.Auth.Header, target.cred.Value)
	default:
		req.Header.Set("Authorization", "Bearer "+target.cred.Value)
	}

	transport, err := gateway.BuildTransport(
		target.cred.Proxy,
		target.provider.Timeouts.Connect,
		target.provider.Timeouts.KeepAlive,
		target.provider.Timeouts.ResponseHeader,
		target.provider.Timeouts.TLSHandshake,
		target.provider.Timeouts.IdleConn,
	)
	if err != nil {
		return snapshot, err
	}
	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport}
	resp, err := client.Do(req)
	if err != nil {
		return snapshot, fmt.Errorf("quota request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, quotaResponseLimit))
	if err != nil {
		return snapshot, fmt.Errorf("reading quota response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return snapshot, fmt.Errorf("quota endpoint returned status %d: %s", resp.StatusCode, snippet)
	}

	windows, err := parseQuotaWindows(quota, body)
	if err != nil {
		return snapshot, err
	}
	snapshot.Windows = windows
	snapshot.Extras = parseQuotaExtras(quota, body)
	snapshot.FetchedAt = time.Now()
	return snapshot, nil
}

// parseQuotaWindows extracts the configured windows from a quota response.
func parseQuotaWindows(quota *config.QuotaConfig, body []byte) ([]quotaWindowSnapshot, error) {
	windows := make([]quotaWindowSnapshot, 0, len(quota.Windows))
	matched := 0
	for _, spec := range quota.Windows {
		window := quotaWindowSnapshot{ID: spec.ID, Label: spec.Label}
		result := gjson.GetBytes(body, spec.Path)
		if !result.Exists() {
			window.Status = quotaMissingStatus
			windows = append(windows, window)
			continue
		}
		matched++
		percent := int(result.Get(spec.Percent).Int())
		if spec.PercentMode == "remaining" {
			percent = 100 - percent
		}
		switch {
		case percent < 0:
			percent = 0
		case percent > 100:
			percent = 100
		}
		window.Percent = percent
		window.RemainingPercent = 100 - percent
		if spec.Status != "" {
			window.Status = result.Get(spec.Status).String()
		}
		if spec.ResetsAt != "" {
			window.ResetsAt = result.Get(spec.ResetsAt).String()
		}
		windows = append(windows, window)
	}
	if matched == 0 {
		// A balance-style mapping declares extras only: there is no window to
		// match, and that is a success rather than a mapping error.
		if len(quota.Windows) == 0 {
			return windows, nil
		}
		return nil, fmt.Errorf("quota response contained none of the configured windows (%s)", strings.Join(windowPaths(quota), ", "))
	}
	return windows, nil
}

func windowPaths(quota *config.QuotaConfig) []string {
	paths := make([]string, 0, len(quota.Windows))
	for _, spec := range quota.Windows {
		paths = append(paths, spec.Path)
	}
	return paths
}

func parseQuotaExtras(quota *config.QuotaConfig, body []byte) []quotaExtraSnapshot {
	if len(quota.Extras) == 0 {
		return nil
	}
	extras := make([]quotaExtraSnapshot, 0, len(quota.Extras))
	for _, spec := range quota.Extras {
		result := gjson.GetBytes(body, spec.Path)
		if !result.Exists() {
			continue
		}
		extras = append(extras, quotaExtraSnapshot{ID: spec.ID, Label: spec.Label, Value: result.Value()})
	}
	return extras
}

// applyQuotaEnforcement ties the snapshot to credential scheduling when the
// provider opts in: a window at/above the threshold parks the credential until
// its reset (or a fallback cooldown). Disabled enforcement always releases.
func (s *Server) applyQuotaEnforcement(target credentialTarget, snapshot credentialQuota) {
	quota := target.provider.Quota
	if quota == nil || !quota.Enforce.Enabled {
		target.cred.ClearQuotaCooldown()
		return
	}
	now := time.Now()
	threshold := quota.EffectiveThreshold()
	var until time.Time
	for _, window := range snapshot.Windows {
		if window.Status == quotaMissingStatus || window.Percent < threshold {
			continue
		}
		reset := time.Time{}
		if window.ResetsAt != "" {
			if parsed, err := time.Parse(time.RFC3339, window.ResetsAt); err == nil {
				reset = parsed
			}
		}
		if reset.IsZero() {
			reset = now.Add(quota.FallbackCooldownDuration())
		}
		if reset.After(until) {
			until = reset
		}
	}
	if until.IsZero() {
		target.cred.ClearQuotaCooldown()
		return
	}
	target.cred.SetQuotaCooldown(until)
	if s.proxylog != nil {
		s.proxylog.Warnf("provider %s credential %s parked until %s: quota window at or above %d%%",
			target.providerID, target.cred.ID, until.Format(time.RFC3339), threshold)
	}
}

// quotaEnforcementState reports whether a credential is currently parked by
// quota enforcement.
func quotaEnforcementState(cred *gateway.Credential, now time.Time) (bool, time.Time) {
	until := cred.QuotaCooldownUntil()
	if until.IsZero() || !until.After(now) {
		return false, time.Time{}
	}
	return true, until
}

// runQuotaRefresher keeps snapshots warm for providers that set
// quota.refreshInterval. It also prunes cached credentials that disappeared.
func (s *Server) runQuotaRefresher() {
	if s == nil || s.quota == nil {
		return
	}
	ticker := time.NewTicker(quotaRefresherTick)
	defer ticker.Stop()
	for {
		select {
		case <-s.shutdownCtx.Done():
			return
		case <-ticker.C:
			s.refreshDueQuotas()
		}
	}
}

func (s *Server) refreshDueQuotas() {
	cfg := s.currentConfig()
	valid := make(map[string]struct{})
	for _, id := range sortedProviderIDs(cfg) {
		provider := cfg.Providers[id]
		if provider.Quota == nil {
			continue
		}
		targets := s.quotaTargets(id, "")
		for _, target := range targets {
			valid[target.cred.ID] = struct{}{}
		}
		interval := provider.Quota.RefreshIntervalDuration()
		if interval <= 0 {
			continue
		}
		for _, target := range targets {
			if cached, ok := s.quota.get(target.cred.ID); ok {
				if time.Since(cached.FetchedAt) < interval {
					continue
				}
			}
			ctx, cancel := context.WithTimeout(s.shutdownCtx, interval+provider.Quota.Timeout())
			s.refreshCredentialQuota(ctx, target, true)
			cancel()
		}
	}
	s.quota.prune(valid)
}
