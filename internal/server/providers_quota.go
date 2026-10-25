package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
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
	// quotaUnlimitedStatus marks an amount window whose limit is zero: the key
	// has no allowance cap, so a used share does not exist.
	quotaUnlimitedStatus = "unlimited"
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
	// Used and Limit carry the absolute amounts of an amount window, so the
	// card can show "1.25 / 5 USD" next to the bar instead of a bare
	// percentage. Both are zero for percentage windows.
	Used  float64 `json:"used,omitempty"`
	Limit float64 `json:"limit,omitempty"`
}

// quotaExtraSnapshot is one extra scalar row (credit balance, counters).
type quotaExtraSnapshot struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Value any    `json:"value"`
	// Suffix is appended to the rendered value (" USD", "%", ...).
	Suffix string `json:"suffix,omitempty"`
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
// one key). It reads the gateway's per-provider credential set rather than the
// routes: a key disabled in configuration never becomes a route member, yet
// its subscription still exists and its balance is exactly what an operator
// wants to watch before re-enabling it. The same holds for a provider that is
// switched off entirely.
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
	credentials := gw.ProviderCredentials(providerID)
	targets := make([]credentialTarget, 0, len(credentials))
	for _, cred := range credentials {
		if keyID != "" && cred.ID != keyID {
			continue
		}
		targets = append(targets, credentialTarget{providerID: providerID, provider: provider, cred: cred})
	}
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
		// The three ways to get here are worth distinguishing: an operator
		// staring at "no matching credential" cannot tell a missing quota
		// block from a typo in the key id.
		provider, exists := s.currentConfig().Providers[providerID]
		switch {
		case !exists:
			swaputil.SendResponse(w, r, http.StatusNotFound, fmt.Sprintf("unknown provider %s", providerID))
		case provider.Quota == nil:
			swaputil.SendResponse(w, r, http.StatusNotFound, fmt.Sprintf("provider %s has no quota mapping configured", providerID))
		default:
			swaputil.SendResponse(w, r, http.StatusNotFound, fmt.Sprintf("provider %s has no configured credential with id %q", providerID, strings.TrimSpace(req.KeyID)))
		}
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

	if quota.HasScript() {
		outcome, err := s.runQuotaScript(ctx, target, quota.Script)
		if err != nil {
			return snapshot, err
		}
		snapshot.Windows = outcome.windows
		snapshot.Extras = outcome.extras
		snapshot.Error = outcome.invalidMessage
		snapshot.FetchedAt = time.Now()
		return snapshot, nil
	}

	ctx, cancel := context.WithTimeout(ctx, quota.Timeout())
	defer cancel()

	endpoint := quota.EndpointFor(quotaBaseURL(target.provider))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
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

	windows, matchedWindows, err := parseQuotaWindows(quota, body)
	if err != nil {
		return snapshot, err
	}
	extras := parseQuotaExtras(quota, body)
	if matchedWindows == 0 && len(extras) == 0 && (len(quota.Windows) > 0 || len(quota.Extras) > 0) {
		// A response that matched nothing at all means the mapping and the
		// endpoint disagree (or the endpoint answered with an error envelope
		// under HTTP 200). Reporting it beats rendering an empty card.
		return snapshot, fmt.Errorf("quota response contained none of the configured fields (%s)", describeQuotaPaths(quota))
	}
	snapshot.Windows = windows
	snapshot.Extras = extras
	snapshot.FetchedAt = time.Now()
	return snapshot, nil
}

// quotaBaseURL is the provider's own base URL: the first adapter endpoint, or
// the single-adapter shorthand. A provider has at least one of the two, and
// configuration validation already proved it is absolute.
func quotaBaseURL(provider config.ProviderConfig) string {
	adapters := provider.EffectiveAdapters()
	if len(adapters) > 0 {
		return adapters[0].Endpoint
	}
	return provider.Endpoint
}

// describeQuotaPaths lists the configured paths for the "nothing matched"
// error, so a broken mapping is diagnosable from the card.
func describeQuotaPaths(quota *config.QuotaConfig) string {
	paths := make([]string, 0, len(quota.Windows)+len(quota.Extras))
	for _, window := range quota.Windows {
		paths = append(paths, window.Path)
	}
	for _, extra := range quota.Extras {
		paths = append(paths, extra.Path)
	}
	return strings.Join(paths, ", ")
}

// parseQuotaWindows extracts the configured windows from a quota response.
// matched counts the windows whose mapping actually resolved, which is what
// distinguishes "this response has nothing for this mapping" from a response
// whose fields are simply zero.
func parseQuotaWindows(quota *config.QuotaConfig, body []byte) ([]quotaWindowSnapshot, int, error) {
	windows := make([]quotaWindowSnapshot, 0, len(quota.Windows))
	matched := 0
	for _, spec := range quota.Windows {
		window := quotaWindowSnapshot{ID: spec.ID, Label: spec.Label}
		result := gjson.GetBytes(body, spec.Path)
		if !result.Exists() {
			if spec.Optional {
				// The upstream is in another mode (wallet vs subscription);
				// the field is expected to be absent, so showing "missing"
				// would report a fault that is not there.
				continue
			}
			window.Status = quotaMissingStatus
			windows = append(windows, window)
			continue
		}
		matched++

		var percent int
		if spec.Limit != "" {
			percent = applyAmountWindow(quota, spec, result, &window)
		} else {
			percent = int(result.Get(spec.Percent).Int())
			if spec.PercentMode == "remaining" {
				percent = 100 - percent
			}
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
	return windows, matched, nil
}

// applyAmountWindow fills an amount window from its used/limit counters and
// returns the used share. Aggregators (new-api, sub2api) publish absolute
// amounts rather than a percentage, so the ratio has to be computed here.
func applyAmountWindow(quota *config.QuotaConfig, spec config.QuotaWindow, result gjson.Result, window *quotaWindowSnapshot) int {
	limit := result.Get(spec.Limit).Float()
	used := result.Get(spec.Percent).Float()
	if quota.DivideBy > 0 {
		limit /= quota.DivideBy
		used /= quota.DivideBy
	}
	window.Used = used
	window.Limit = limit
	if limit <= 0 {
		// No cap configured: an "unlimited" key is not a 0%-used key, and
		// dividing by zero would report 100% used.
		window.Status = quotaUnlimitedStatus
		return 0
	}
	return int(math.Round(used / limit * 100))
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
		value := result.Value()
		if spec.Format == "unix" || spec.Format == "unixMs" {
			seconds := result.Int()
			if spec.Format == "unixMs" {
				seconds /= 1000
			}
			if seconds <= 0 {
				// Timestamp zero means "never expires", which is not a date;
				// printing 1970 would only confuse the card.
				continue
			}
			value = time.Unix(seconds, 0).UTC().Format(time.RFC3339)
		}
		divideBy := spec.DivideBy
		if divideBy == 0 {
			divideBy = quota.DivideBy
		}
		extras = append(extras, quotaExtraSnapshot{
			ID:     spec.ID,
			Label:  spec.Label,
			Value:  config.ScaleQuotaValue(value, divideBy),
			Suffix: spec.Suffix,
		})
	}
	return extras
}

// applyQuotaEnforcement ties the snapshot to credential scheduling when the
// provider opts in: a window at/above the threshold parks the credential until
// its reset (or a fallback cooldown). Disabled enforcement always releases.
func (s *Server) applyQuotaEnforcement(target credentialTarget, snapshot credentialQuota) {
	quota := target.provider.Quota
	if quota == nil || !quota.Enforce.Enabled || target.cred.Disabled {
		// A disabled key is not schedulable, so a park would only show up as
		// "额度已满" on a card whose key is off anyway.
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

// handleAPIProvidersQuotasTest runs one usage script without caching the
// result or touching enforcement. It backs the editor's "test script" button,
// so a draft can be exercised before it is saved (and before the provider even
// has a quota block).
func (s *Server) handleAPIProvidersQuotasTest(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "could not read request body")
		return
	}
	var req struct {
		ProviderID string `json:"providerID"`
		KeyID      string `json:"keyID,omitempty"`
		Script     string `json:"script,omitempty"`
		Preset     string `json:"preset,omitempty"`
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
	provider, ok := s.currentConfig().Providers[providerID]
	if !ok {
		swaputil.SendResponse(w, r, http.StatusNotFound, fmt.Sprintf("unknown provider %s", providerID))
		return
	}

	script := strings.TrimSpace(req.Script)
	if script == "" && strings.TrimSpace(req.Preset) != "" {
		preset, ok := config.QuotaPresetByID(strings.TrimSpace(req.Preset))
		if !ok {
			swaputil.SendResponse(w, r, http.StatusNotFound, fmt.Sprintf("unknown quota preset %q", req.Preset))
			return
		}
		script = preset.Script
	}
	if script == "" && provider.Quota != nil {
		script = provider.Quota.Script
	}
	if strings.TrimSpace(script) == "" {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "no script to run")
		return
	}

	gw := s.currentGateway()
	if gw == nil {
		swaputil.SendResponse(w, r, http.StatusServiceUnavailable, "no provider gateway active")
		return
	}
	keyID := strings.TrimSpace(req.KeyID)
	var credential *gateway.Credential
	for _, cred := range gw.ProviderCredentials(providerID) {
		if keyID == "" || cred.ID == keyID {
			credential = cred
			break
		}
	}
	if credential == nil {
		swaputil.SendResponse(w, r, http.StatusNotFound, fmt.Sprintf("provider %s has no configured credential with id %q", providerID, keyID))
		return
	}

	// A draft may exist before the quota block does; the script still needs a
	// timeout and the provider's base URL, so run against a copy.
	quota := config.QuotaConfig{Script: script, TimeoutSeconds: config.QuotaDefaultTimeoutSeconds}
	if provider.Quota != nil {
		quota = *provider.Quota
		quota.Script = script
	}
	provider.Quota = &quota
	target := credentialTarget{providerID: providerID, provider: provider, cred: credential}

	outcome, runErr := s.runQuotaScript(r.Context(), target, script)
	snapshot := credentialQuota{
		ProviderID: providerID,
		KeyID:      credential.ID,
		Windows:    outcome.windows,
		Extras:     outcome.extras,
		Error:      outcome.invalidMessage,
		FetchedAt:  time.Now(),
	}
	if runErr != nil {
		snapshot.Error = runErr.Error()
		snapshot.FetchedAt = time.Time{}
	}
	writeJSON(w, struct {
		Snapshot credentialQuota     `json:"snapshot"`
		Request  *quotaScriptRequest `json:"request,omitempty"`
		Error    string              `json:"error,omitempty"`
	}{
		Snapshot: snapshot,
		Request:  maskQuotaScriptRequest(outcome.request),
		Error:    snapshot.Error,
	})
}

// handleAPIProviderQuotaPreset returns one built-in preset including its
// script, so the editor can load a script preset's source for adaptation.
func (s *Server) handleAPIProviderQuotaPreset(w http.ResponseWriter, r *http.Request) {
	detail, ok := config.QuotaPresetDetailByID(r.PathValue("id"))
	if !ok {
		swaputil.SendResponse(w, r, http.StatusNotFound, fmt.Sprintf("unknown quota preset %q", r.PathValue("id")))
		return
	}
	writeJSON(w, detail)
}

// maskQuotaScriptRequest hides credential material from the test panel's
// request echo.
func maskQuotaScriptRequest(request *quotaScriptRequest) *quotaScriptRequest {
	if request == nil {
		return nil
	}
	masked := *request
	masked.Headers = make(map[string]string, len(request.Headers))
	for name, value := range request.Headers {
		switch strings.ToLower(name) {
		case "authorization", "x-api-key", "x-goog-api-key", "api-key", "cookie":
			masked.Headers[name] = "[redacted]"
		default:
			masked.Headers[name] = value
		}
	}
	return &masked
}
