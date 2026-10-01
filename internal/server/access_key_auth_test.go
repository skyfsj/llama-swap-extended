package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/store"
)

// newAccessKeyStore seeds a management key with one nested access key and
// returns the store plus both records.
func newAccessKeyStore(t *testing.T) (*store.Store, store.APIKeyRecord, store.APIKeyRecord) {
	t.Helper()
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := t.Context()
	management := store.APIKeyRecord{
		ID: "mgmt", Name: "manager", KeyHash: store.KeyFingerprint("mgmt-secret"),
		Kind: auth.KeyKindManagement, Scopes: auth.ManagementScopes(), AllowManagementLogin: true,
	}
	access := store.APIKeyRecord{
		ID: "access", Name: "client", KeyHash: store.KeyFingerprint("access-secret"),
		Kind: auth.KeyKindAccess, ParentID: "mgmt", Scopes: []string{auth.ScopeInference},
		AllowedIPs: []string{"203.0.113.0/24"}, MaxConcurrency: 2, Group: "team-a",
	}
	for _, record := range []store.APIKeyRecord{management, access} {
		if err := st.UpsertAPIKey(ctx, record); err != nil {
			t.Fatalf("seed key %q: %v", record.ID, err)
		}
	}
	return st, management, access
}

func accessKeyHandler(t *testing.T, st *store.Store) (http.Handler, *identityRecorder) {
	t.Helper()
	recorder := &identityRecorder{}
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder.capture(identityFromContext(r.Context()))
		w.WriteHeader(http.StatusNoContent)
	})
	return chain.New(CreateScopedAuthMiddleware(config.Config{}, st, nil), RequireScope(auth.ScopeInference)).Then(final), recorder
}

type identityRecorder struct {
	mu         sync.Mutex
	identities []auth.Identity
}

func (r *identityRecorder) capture(identity auth.Identity) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.identities = append(r.identities, identity)
}

func (r *identityRecorder) last() auth.Identity {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.identities) == 0 {
		return auth.Identity{}
	}
	return r.identities[len(r.identities)-1]
}

func accessKeyRequest(path string, secret string, remoteAddr string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"m","messages":[]}`))
	request.Header.Set("Authorization", "Bearer "+secret)
	if remoteAddr != "" {
		request.RemoteAddr = remoteAddr
	}
	return request
}

func TestServer_ScopedAuthAccessKeyInheritsParentModelsAndKind(t *testing.T) {
	st, _, _ := newAccessKeyStore(t)
	handler, recorder := accessKeyHandler(t, st)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, accessKeyRequest("/v1/chat/completions", "access-secret", "203.0.113.9:5000"))
	if w.Code != http.StatusNoContent {
		t.Fatalf("access key status=%d body=%s", w.Code, w.Body.String())
	}
	identity := recorder.last()
	if identity.ID != "access" || identity.Kind != auth.KeyKindAccess || identity.ParentID != "mgmt" {
		t.Fatalf("identity = %#v", identity)
	}
	if identity.AllowManagementLogin {
		t.Fatal("access key was allowed to sign in to the management panel")
	}
	if !identity.Has(auth.ScopeInference) {
		t.Fatal("access key lost the inference scope")
	}
	if identity.Has(auth.ScopeKeysAdmin) {
		t.Fatal("access key inherited a control-plane scope")
	}
	if identity.MaxConcurrency != 2 || identity.Group != "team-a" {
		t.Fatalf("identity restrictions = %#v", identity)
	}
}

func TestServer_ScopedAuthAccessKeyIPAllowlist(t *testing.T) {
	st, _, _ := newAccessKeyStore(t)
	handler, _ := accessKeyHandler(t, st)

	allowed := httptest.NewRecorder()
	handler.ServeHTTP(allowed, accessKeyRequest("/v1/chat/completions", "access-secret", "203.0.113.9:5000"))
	if allowed.Code != http.StatusNoContent {
		t.Fatalf("allowed caller status=%d body=%s", allowed.Code, allowed.Body.String())
	}

	rejected := httptest.NewRecorder()
	handler.ServeHTTP(rejected, accessKeyRequest("/v1/chat/completions", "access-secret", "198.51.100.4:5000"))
	if rejected.Code != http.StatusForbidden {
		t.Fatalf("rejected caller status=%d body=%s", rejected.Code, rejected.Body.String())
	}
	if !strings.Contains(rejected.Body.String(), "client IP") {
		t.Fatalf("rejection body = %q", rejected.Body.String())
	}

	// A proxy header is honoured, matching the access log's resolution.
	proxied := accessKeyRequest("/v1/chat/completions", "access-secret", "10.1.1.1:5000")
	proxied.Header.Set("X-Forwarded-For", "203.0.113.8")
	forwarded := httptest.NewRecorder()
	handler.ServeHTTP(forwarded, proxied)
	if forwarded.Code != http.StatusNoContent {
		t.Fatalf("forwarded caller status=%d body=%s", forwarded.Code, forwarded.Body.String())
	}
}

func TestServer_ScopedAuthAccessKeyConcurrencyCeiling(t *testing.T) {
	st, _, _ := newAccessKeyStore(t)
	release := make(chan struct{})
	entered := make(chan struct{}, 8)
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusNoContent)
	})
	handler := chain.New(CreateScopedAuthMiddleware(config.Config{}, st, nil), RequireScope(auth.ScopeInference)).Then(final)

	var wg sync.WaitGroup
	codes := make([]int, 5)
	for index := range codes {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, accessKeyRequest("/v1/chat/completions", "access-secret", "203.0.113.9:5000"))
			codes[index] = recorder.Code
		}(index)
	}
	// Wait until two requests hold a slot, then confirm the third is rejected.
	<-entered
	<-entered
	rejected := httptest.NewRecorder()
	handler.ServeHTTP(rejected, accessKeyRequest("/v1/chat/completions", "access-secret", "203.0.113.9:5000"))
	if rejected.Code != http.StatusTooManyRequests {
		t.Fatalf("saturated key status=%d body=%s", rejected.Code, rejected.Body.String())
	}
	close(release)
	wg.Wait()

	admitted := 0
	for _, code := range codes {
		if code == http.StatusNoContent {
			admitted++
		} else if code != http.StatusTooManyRequests {
			t.Fatalf("unexpected status %d for concurrent request", code)
		}
	}
	if admitted < 2 {
		t.Fatalf("admitted %d requests, want at least the ceiling of 2", admitted)
	}
	// A slot is released once the response completes, so the next request is
	// admitted again.
	after := httptest.NewRecorder()
	handler.ServeHTTP(after, accessKeyRequest("/v1/chat/completions", "access-secret", "203.0.113.9:5000"))
	if after.Code != http.StatusNoContent {
		t.Fatalf("post-release status=%d body=%s", after.Code, after.Body.String())
	}
}

func TestServer_ScopedAuthAccessKeyRequiresValidParent(t *testing.T) {
	st, _, _ := newAccessKeyStore(t)
	handler, _ := accessKeyHandler(t, st)

	// An unknown parent must fail closed rather than grant inference access.
	if err := st.UpsertAPIKey(t.Context(), store.APIKeyRecord{
		ID: "orphan", KeyHash: store.KeyFingerprint("orphan-secret"),
		Kind: auth.KeyKindAccess, ParentID: "missing-parent", Scopes: []string{auth.ScopeInference},
	}); err != nil {
		t.Fatal(err)
	}
	request := accessKeyRequest("/v1/chat/completions", "orphan-secret", "203.0.113.9:5000")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, request)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("orphan access key status=%d body=%s", w.Code, w.Body.String())
	}

	// Revoking the management parent revokes its access keys with it.
	if err := st.RevokeAPIKeyCascade(t.Context(), "mgmt", time.Now()); err != nil {
		t.Fatal(err)
	}
	revoked := httptest.NewRecorder()
	handler.ServeHTTP(revoked, accessKeyRequest("/v1/chat/completions", "access-secret", "203.0.113.9:5000"))
	if revoked.Code != http.StatusUnauthorized {
		t.Fatalf("child of revoked parent status=%d body=%s", revoked.Code, revoked.Body.String())
	}
}

func TestServer_ScopedAuthAccessKeyCannotReachControlPlane(t *testing.T) {
	st, _, _ := newAccessKeyStore(t)
	final := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	handler := chain.New(CreateScopedAuthMiddleware(config.Config{}, st, nil), RequireScope("runtime-admin")).Then(final)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, accessKeyRequest("/api/runtimes", "access-secret", "203.0.113.9:5000"))
	if w.Code != http.StatusForbidden {
		t.Fatalf("access key on control plane status=%d body=%s", w.Code, w.Body.String())
	}
	// The management key that owns it still can.
	manager := httptest.NewRecorder()
	handler.ServeHTTP(manager, accessKeyRequest("/api/runtimes", "mgmt-secret", "203.0.113.9:5000"))
	if manager.Code != http.StatusNoContent {
		t.Fatalf("management key on control plane status=%d body=%s", manager.Code, manager.Body.String())
	}
}

func TestServer_ScopedAuthAccessKeyManagementLoginDenied(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })
	if err := s.store.UpsertAPIKey(t.Context(), store.APIKeyRecord{
		ID: "mgmt", Name: "manager", KeyHash: store.KeyFingerprint("mgmt-secret"),
		Kind: auth.KeyKindManagement, Scopes: auth.ManagementScopes(), AllowManagementLogin: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpsertAPIKey(t.Context(), store.APIKeyRecord{
		ID: "access", Name: "client", KeyHash: store.KeyFingerprint("access-secret"),
		Kind: auth.KeyKindAccess, ParentID: "mgmt", Scopes: []string{auth.ScopeInference},
	}); err != nil {
		t.Fatal(err)
	}
	login := func(secret string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/api/auth/session", strings.NewReader(`{"key":"`+secret+`"}`))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		s.handleAPIAuthSession(response, request)
		return response
	}
	if response := login("access-secret"); response.Code != http.StatusForbidden {
		t.Fatalf("access key login status=%d body=%s", response.Code, response.Body.String())
	}
	if response := login("mgmt-secret"); response.Code != http.StatusOK {
		t.Fatalf("management key login status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestServer_AccessKeyModelIntersectionFailsClosed(t *testing.T) {
	// A row whose restriction does not overlap the parent's models must never
	// become unrestricted: an empty pattern list means "all models" throughout
	// the authorization model.
	if got := intersectModels([]string{"claude"}, []string{"gpt-4"}); len(got) != 1 || got[0] != "gpt-4" {
		t.Fatalf("non-overlapping intersection = %#v, want the parent's restriction", got)
	}
	if err := intersectModelsError([]string{"claude"}, []string{"gpt-4"}); err == nil {
		t.Fatal("non-overlapping model restriction was accepted")
	}
	if err := intersectModelsError([]string{"gpt-4"}, []string{"gpt-*"}); err != nil {
		t.Fatalf("overlapping model restriction rejected: %v", err)
	}
	if err := intersectModelsError(nil, []string{"gpt-4"}); err != nil {
		t.Fatalf("unrestricted access key rejected: %v", err)
	}
	// An unrestricted parent concedes nothing, so the access key keeps its list.
	if got := intersectModels([]string{"gpt-4"}, nil); len(got) != 1 || got[0] != "gpt-4" {
		t.Fatalf("intersection with unrestricted parent = %#v", got)
	}
}
