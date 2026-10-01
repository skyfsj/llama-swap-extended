package server

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

const (
	// keyRefreshInterval is how often the key cache re-reads the durable key
	// table. Writes that go through this server's management API update the
	// snapshot synchronously, so this only needs to be conservative enough
	// to pick up out-of-band changes to the same database file.
	keyRefreshInterval = 30 * time.Second

	// keyReadTimeout bounds durable reads that the cache performs. It is
	// deliberately above the store's busy_timeout so a contended connection
	// can finish waiting for the lock before the read gives up.
	keyReadTimeout = 5 * time.Second
)

// keySnapshot is an immutable view of the durable API-key table.
type keySnapshot struct {
	// configured records whether the table holds any row at all, including
	// revoked ones. Revoked rows still count: dropping them would let an
	// operator reopen every endpoint to anonymous callers by revoking the
	// last key.
	configured bool
	keys       []store.APIKeyRecord
	// byID indexes the same rows by key id. Access keys are nested under a
	// management key, so resolving one credential needs its parent row; the
	// index keeps that off the per-request path.
	byID     map[string]store.APIKeyRecord
	loadedAt time.Time
}

// parent returns the management key an access key is nested under. A nil result
// means the parent is unknown (deleted out of band), which fails closed for the
// access key.
func (s *keySnapshot) parent(id string) *store.APIKeyRecord {
	if s == nil || strings.TrimSpace(id) == "" {
		return nil
	}
	record, ok := s.byID[id]
	if !ok {
		return nil
	}
	return &record
}

func newKeySnapshot(keys []store.APIKeyRecord) *keySnapshot {
	byID := make(map[string]store.APIKeyRecord, len(keys))
	for _, key := range keys {
		byID[key.ID] = key
	}
	return &keySnapshot{configured: len(keys) > 0, keys: keys, byID: byID, loadedAt: time.Now()}
}

// keyCache keeps API-key authentication off the request path's SQLite
// traffic. The middleware historically probed the key table on every request
// and failed closed on any storage error, which turned a transient SQLite
// busy state into a wall of 401s on keyless deployments. The hot path now
// decides authentication state from this in-memory snapshot; the durable
// table is only consulted as a fallback for credentials that are not in the
// snapshot yet.
type keyCache struct {
	store *store.Store
	log   *logmon.Monitor
	snap  atomic.Pointer[keySnapshot]
	mu    sync.Mutex // single-flights refresh
	// lastProbe rate-limits on-demand refreshes (probeForKey) and the
	// validate-time durable fallback so a client cannot hammer the key table
	// with repeated credential-carrying requests.
	lastProbe atomic.Int64

	// touchMu guards the buffered last-used timestamps. The hot path records
	// touches in memory; flushTouches persists them in batches so authentication
	// stays store-free.
	touchMu sync.Mutex
	touches map[string]time.Time
}

// recordTouch buffers a last-used timestamp for the key. Keeping the maximum
// is all that matters: last_used is display data, not an audit record.
func (k *keyCache) recordTouch(id string, at time.Time) {
	if k == nil || id == "" {
		return
	}
	k.touchMu.Lock()
	defer k.touchMu.Unlock()
	if k.touches == nil {
		k.touches = make(map[string]time.Time)
	}
	if prev, ok := k.touches[id]; ok && prev.After(at) {
		return
	}
	k.touches[id] = at
}

// flushTouches persists the buffered last-used timestamps and returns how
// many keys were written. Failed writes are logged and dropped: last_used is
// cosmetic and the next touch of the same key re-buffers it.
func (k *keyCache) flushTouches(ctx context.Context) int {
	if k == nil || k.store == nil {
		return 0
	}
	k.touchMu.Lock()
	pending := k.touches
	k.touches = nil
	k.touchMu.Unlock()
	flushed := 0
	for id, at := range pending {
		tctx, cancel := context.WithTimeout(ctx, keyReadTimeout)
		err := k.store.TouchAPIKey(tctx, id, at)
		cancel()
		if err != nil {
			if k.log != nil {
				k.log.Warnf("persist api key last-used failed: %v", err)
			}
			continue
		}
		flushed++
	}
	return flushed
}

func newKeyCache(st *store.Store, log *logmon.Monitor) *keyCache {
	return &keyCache{store: st, log: log}
}

// load performs the startup read. Unlike refresh it surfaces errors so a
// server that cannot read its key table refuses to start rather than
// silently guessing at its authentication state.
func (k *keyCache) load() error {
	if k == nil || k.store == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), keyReadTimeout)
	defer cancel()
	keys, err := k.store.ListAPIKeys(ctx, true)
	if err != nil {
		return err
	}
	k.snap.Store(newKeySnapshot(keys))
	return nil
}

// refresh re-reads the key table, keeping the previous snapshot on error. A
// transient storage failure must never flip the authentication state: the
// authentication material lives in memory after startup, so a busy database
// degrades to the last known keys instead of locking every caller out.
func (k *keyCache) refresh() {
	if k == nil || k.store == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), keyReadTimeout)
	defer cancel()
	keys, err := k.store.ListAPIKeys(ctx, true)
	if err != nil {
		if k.log != nil {
			k.log.Warnf("api key cache refresh failed, keeping previous snapshot: %v", err)
		}
		return
	}
	k.snap.Store(newKeySnapshot(keys))
}

// runRefresh keeps the snapshot current for out-of-band writers. It stops
// with the server context.
func (k *keyCache) runRefresh(ctx context.Context) {
	if k == nil || k.store == nil {
		return
	}
	ticker := time.NewTicker(keyRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			k.refresh()
		}
	}
}

// configured reports whether authentication must be enforced. A nil receiver
// or a cache that never loaded means "no managed keys known", which matches
// the historical behavior for servers without a store.
func (k *keyCache) configured() bool {
	if k == nil {
		return false
	}
	snapshot := k.snap.Load()
	return snapshot != nil && snapshot.configured
}

// keyProbeMinInterval bounds how often on-demand probes may re-read the key
// table. It exists so a stream of credential-carrying requests against a
// keyless deployment cannot turn the cache into a per-request store probe
// again, while still making direct or out-of-band key writers visible
// quickly.
const keyProbeMinInterval = 5 * time.Second

// probeForKey re-reads the key table when the snapshot holds no key and a
// credential was presented. Rate-limited and single-flighted by the lastProbe
// timestamp so concurrent requests only trigger one refresh.
func (k *keyCache) probeForKey() {
	if k == nil || k.store == nil {
		return
	}
	if snapshot := k.snap.Load(); snapshot != nil && len(snapshot.keys) > 0 {
		return
	}
	last := k.lastProbe.Load()
	now := time.Now().UnixNano()
	if now-last < int64(keyProbeMinInterval) {
		return
	}
	if !k.lastProbe.CompareAndSwap(last, now) {
		return
	}
	k.refresh()
}

// validate resolves a presented credential against the legacy YAML keys and
// the in-memory key snapshot. While the snapshot still holds no key, a
// matching lookup falls back to the durable table so direct or out-of-band
// writers to the same database file do not wait for the periodic refresh;
// once the snapshot is populated, validation stays in memory and a storage
// failure can only mark one credential invalid, never the whole deployment.
func (k *keyCache) validate(provided string, cfg config.Config) (auth.Identity, bool) {
	if k == nil {
		return auth.Identity{}, false
	}
	provided = strings.TrimSpace(provided)
	if provided == "" {
		return auth.Identity{}, false
	}
	for _, key := range cfg.RequiredAPIKeys {
		if swaputil.ConstantTimeEquals(provided, key) {
			return auth.Identity{ID: store.KeyFingerprint(key), Legacy: true}, true
		}
	}
	snapshot := k.snap.Load()
	if snapshot != nil {
		if identity, ok := identityFromSnapshot(cfg, snapshot, provided); ok {
			return identity, true
		}
	}
	// While the snapshot has never seen a key, give out-of-band writers and
	// multi-process deployments one rate-limited chance to become visible:
	// refresh the snapshot (single-flighted, at most once per window) and
	// re-match against the refreshed view. A legitimate new key is found on
	// its first attempt and every later request validates from memory; a
	// stream of wrong credentials hits the store at most once per window,
	// failing fast within it. Once the snapshot is populated, the refresh
	// loop is the freshness contract and failed validation stays in memory.
	if k.store != nil && (snapshot == nil || len(snapshot.keys) == 0) {
		last := k.lastProbe.Load()
		now := time.Now().UnixNano()
		if now-last < int64(keyProbeMinInterval) {
			return auth.Identity{}, false
		}
		if !k.lastProbe.CompareAndSwap(last, now) {
			return auth.Identity{}, false
		}
		k.refresh()
		snapshot = k.snap.Load()
		if snapshot != nil {
			if identity, ok := identityFromSnapshot(cfg, snapshot, provided); ok {
				return identity, true
			}
		}
	}
	return auth.Identity{}, false
}

// identityFromSnapshot matches a presented credential against every key in one
// snapshot and resolves the resulting identity. The snapshot's byID index makes
// the management parent of an access key resolvable without a second pass over
// the key list.
func identityFromSnapshot(cfg config.Config, snapshot *keySnapshot, provided string) (auth.Identity, bool) {
	for _, record := range snapshot.keys {
		if !recordMatchesSecret(record, provided) {
			continue
		}
		identity, ok, _ := identityForKeyRecord(cfg, record, snapshot.parent(record.ParentID))
		if ok {
			return identity, true
		}
	}
	return auth.Identity{}, false
}

// recordMatchesSecret reports whether a presented secret authenticates one
// key record, preferring the salted digest when the record carries one.
// Both comparisons are constant-time.
func recordMatchesSecret(record store.APIKeyRecord, provided string) bool {
	if len(record.KeySalt) > 0 {
		return auth.VerifySalted(provided, record.KeySalt, record.KeyHash)
	}
	return auth.Verify(provided, record.KeyHash)
}

// identityForKeyRecord converts an already-authenticated durable record into
// its authorization identity, applying revocation/expiry and model alias
// expansion. The optional parent is the management key an access key is nested
// under; it must be present and still valid, otherwise the access key fails
// closed.
func identityForKeyRecord(cfg config.Config, record store.APIKeyRecord, parent *store.APIKeyRecord) (auth.Identity, bool, error) {
	now := time.Now()
	if record.RevokedAt != nil || (record.ExpiresAt != nil && !record.ExpiresAt.After(now)) {
		return auth.Identity{}, false, nil
	}
	if record.Kind == auth.KeyKindAccess {
		identity, ok, err := accessKeyIdentity(cfg, record, parent)
		return identity, ok, err
	}
	scopes, err := auth.NormalizeScopes(record.Scopes)
	if err != nil {
		return auth.Identity{}, false, err
	}
	return auth.Identity{
		ID:                   record.ID,
		Scopes:               scopes,
		Models:               expandModelAliases(cfg, record.Models),
		AllowManagementLogin: record.AllowManagementLogin,
		Kind:                 record.Kind,
	}, true, nil
}

// accessKeyIdentity derives the identity of a model-access credential. The
// credential intentionally carries the inference scope only: management keys
// own the control plane and are the only credentials that may sign in, so an
// access key can never inherit a scope that would let it administer the
// deployment. The caller restrictions (IP allowlist, concurrency ceiling,
// usage group) travel with the identity so middleware can enforce them, and
// the model allowlist is intersected with the parent's so a management key
// cannot be widened by one of its access keys.
func accessKeyIdentity(cfg config.Config, record store.APIKeyRecord, parent *store.APIKeyRecord) (auth.Identity, bool, error) {
	now := time.Now()
	if parent == nil || parent.RevokedAt != nil || (parent.ExpiresAt != nil && !parent.ExpiresAt.After(now)) {
		return auth.Identity{}, false, nil
	}
	return auth.Identity{
		ID:     record.ID,
		Scopes: map[string]struct{}{auth.ScopeInference: {}},
		Models: expandModelAliases(cfg, intersectModels(record.Models, parent.Models)),
		// Access keys are API credentials by construction.
		AllowManagementLogin: false,
		Kind:                 auth.KeyKindAccess,
		ParentID:             parent.ID,
		AllowedIPs:           append([]string(nil), record.AllowedIPs...),
		MaxConcurrency:       record.MaxConcurrency,
		Group:                record.Group,
	}, true, nil
}

// accessKeyModelIntersection was the duplicate of intersectModels that lived
// here; the canonical version now lives beside the control-plane callers that
// write the restriction (see intersectModels in control_api.go).
