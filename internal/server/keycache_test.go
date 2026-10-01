package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/store"
)

func newLoadedKeyCache(t *testing.T, st *store.Store) *keyCache {
	t.Helper()
	kc := newKeyCache(st, nil)
	if err := kc.load(); err != nil {
		t.Fatalf("load key cache: %v", err)
	}
	return kc
}

// A keyless deployment whose store goes away mid-flight must stay open. The
// cache path never touches the store per request, so a dead database cannot
// turn the server into a wall of 401s.
func TestServer_ScopedAuthKeyCacheStoreDownStaysAnonymous(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	kc := newLoadedKeyCache(t, st)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	final := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := chain.New(CreateScopedAuthMiddleware(config.Config{}, st, kc), RequireScope(auth.ScopeInference)).Then(final)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`)))
	if w.Code != http.StatusNoContent {
		t.Fatalf("store down status=%d body=%s, want anonymous pass-through", w.Code, w.Body.String())
	}
}

// Keys loaded into the cache keep working after the store dies, and the
// configured state (from the snapshot) still rejects missing credentials.
func TestServer_ScopedAuthKeyCacheServesKeysAfterStoreDown(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAPIKey(t.Context(), store.APIKeyRecord{
		ID: "k1", Name: "client", KeyHash: store.KeyFingerprint("secret"),
		Scopes: []string{auth.ScopeInference},
	}); err != nil {
		t.Fatal(err)
	}
	kc := newLoadedKeyCache(t, st)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	final := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := chain.New(CreateScopedAuthMiddleware(config.Config{}, st, kc), RequireScope(auth.ScopeInference)).Then(final)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("cached key status=%d body=%s, want pass-through", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`)))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing key status=%d, want unauthorized from cached configured state", w.Code)
	}
}

// Revoking the last key must keep authentication configured (the snapshot
// counts revoked rows) and the revoked secret must stop validating after a
// refresh, which is what the management handlers trigger synchronously.
func TestServer_KeyCacheRefreshDropsRevokedKey(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertAPIKey(t.Context(), store.APIKeyRecord{
		ID: "k1", KeyHash: store.KeyFingerprint("secret"), Scopes: []string{auth.ScopeInference},
	}); err != nil {
		t.Fatal(err)
	}
	kc := newLoadedKeyCache(t, st)
	if _, valid := kc.validate("secret", config.Config{}); !valid {
		t.Fatal("key invalid before revocation")
	}
	if err := st.RevokeAPIKey(t.Context(), "k1", time.Now()); err != nil {
		t.Fatal(err)
	}
	kc.refresh()
	if _, valid := kc.validate("secret", config.Config{}); valid {
		t.Fatal("revoked key still valid after refresh")
	}
	if !kc.configured() {
		t.Fatal("revoked key must keep authentication configured")
	}
}

// A credential presented while the snapshot holds no key triggers a
// rate-limited refresh, so a key written directly to the store (bypassing the
// management API) is visible on the next credential-carrying request.
func TestServer_ScopedAuthKeyCacheProbeSeesDirectStoreWrite(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	kc := newLoadedKeyCache(t, st)
	if err := st.UpsertAPIKey(t.Context(), store.APIKeyRecord{
		ID: "direct", KeyHash: store.KeyFingerprint("direct-secret"),
		Scopes: []string{auth.ScopeInference},
	}); err != nil {
		t.Fatal(err)
	}
	final := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := chain.New(CreateScopedAuthMiddleware(config.Config{}, st, kc), RequireScope(auth.ScopeInference)).Then(final)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	r.Header.Set("Authorization", "Bearer direct-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("direct store key status=%d body=%s, want pass-through after probe", w.Code, w.Body.String())
	}
}

// The probe must not turn back into a per-request store read: once rate
// limited it skips the refresh, and a key written in that window only becomes
// visible after the interval passes.
func TestServer_KeyCacheProbeIsRateLimited(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	kc := newLoadedKeyCache(t, st)

	kc.lastProbe.Store(time.Now().UnixNano())
	kc.probeForKey()
	if kc.configured() {
		t.Fatal("rate-limited probe must not refresh")
	}
	if err := st.UpsertAPIKey(t.Context(), store.APIKeyRecord{
		ID: "later", KeyHash: store.KeyFingerprint("later-secret"), Scopes: []string{auth.ScopeInference},
	}); err != nil {
		t.Fatal(err)
	}
	kc.lastProbe.Store(time.Now().Add(-keyProbeMinInterval).UnixNano())
	kc.probeForKey()
	if !kc.configured() {
		t.Fatal("probe after the interval must pick up the new key")
	}
	if _, valid := kc.validate("later-secret", config.Config{}); !valid {
		t.Fatal("new key must validate after the probing refresh")
	}
}

// A salted key (the WebUI format) validates from the snapshot without a store
// read once loaded.
func TestServer_KeyCacheValidatesSaltedKeyFromSnapshot(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	salt, digest, err := auth.GenerateSalted("salted-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAPIKey(t.Context(), store.APIKeyRecord{
		ID: "salted", KeyHash: digest, KeySalt: salt, Scopes: []string{auth.ScopeInference},
	}); err != nil {
		t.Fatal(err)
	}
	kc := newLoadedKeyCache(t, st)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, valid := kc.validate("salted-secret", config.Config{}); !valid {
		t.Fatal("salted key must validate from the snapshot")
	}
	if _, valid := kc.validate("wrong", config.Config{}); valid {
		t.Fatal("wrong secret must not validate")
	}
}

// The validate-time durable fallback (snapshot still empty) must be
// rate-limited: an out-of-band key succeeds on its first presentation and
// from then on validates from memory, while wrong credentials within the
// window fail fast instead of hitting the store per request.
func TestServer_KeyCacheValidateFallbackIsRateLimited(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	kc := newLoadedKeyCache(t, st)

	// Out-of-band writer: the snapshot has never seen this key.
	if err := st.UpsertAPIKey(t.Context(), store.APIKeyRecord{
		ID: "oob", KeyHash: store.KeyFingerprint("oob-secret"),
		Scopes: []string{auth.ScopeInference},
	}); err != nil {
		t.Fatal(err)
	}
	if _, valid := kc.validate("oob-secret", config.Config{}); !valid {
		t.Fatal("out-of-band key must validate on its first attempt")
	}
	// The successful fallback refreshed the snapshot: later requests stay in
	// memory and must keep validating.
	if _, valid := kc.validate("oob-secret", config.Config{}); !valid {
		t.Fatal("key must keep validating from the refreshed snapshot")
	}
}

// Wrong credentials against an empty snapshot must not query the store per
// request: the first fallback consumes the probe window, and everything
// presented within it fails fast. A key written out-of-band during that
// window becomes visible once the window passes (or via the scheduled
// refresh); keys written through the management API refresh synchronously
// and never depend on this path.
func TestServer_KeyCacheValidateFallbackFailFastWithinWindow(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	kc := newLoadedKeyCache(t, st)

	// A wrong credential triggers the fallback and consumes the window.
	if _, valid := kc.validate("wrong-secret", config.Config{}); valid {
		t.Fatal("wrong credential must not validate")
	}
	lastProbeAfterWrong := kc.lastProbe.Load()
	if lastProbeAfterWrong == 0 {
		t.Fatal("the first fallback must win the probe window")
	}

	// Out-of-band writer: the snapshot is still empty (no refresh ran).
	if err := st.UpsertAPIKey(t.Context(), store.APIKeyRecord{
		ID: "victim", KeyHash: store.KeyFingerprint("victim-secret"),
		Scopes: []string{auth.ScopeInference},
	}); err != nil {
		t.Fatal(err)
	}

	// Inside the window: fail fast without touching the probe timestamp.
	if _, valid := kc.validate("victim-secret", config.Config{}); valid {
		t.Fatal("fallback must fail fast within the probe window")
	}
	if kc.lastProbe.Load() != lastProbeAfterWrong {
		t.Fatal("fail-fast path must not advance the probe timestamp")
	}

	// After the window: the legitimate key becomes visible.
	kc.lastProbe.Store(time.Now().Add(-keyProbeMinInterval).UnixNano())
	if _, valid := kc.validate("victim-secret", config.Config{}); !valid {
		t.Fatal("key must validate once the probe window passes")
	}
}
