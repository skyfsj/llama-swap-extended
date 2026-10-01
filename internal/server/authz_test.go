package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

func TestServer_ScopedAuthLegacyKeyAndAnonymousCompatibility(t *testing.T) {
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if identityFromContext(r.Context()).ID == "" {
			t.Error("missing identity")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	open := chain.New(CreateScopedAuthMiddleware(config.Config{}, nil, nil), RequireScope("runtime-admin")).Then(final)
	w := httptest.NewRecorder()
	open.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/runtimes", nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("anonymous status=%d", w.Code)
	}
	protected := chain.New(CreateScopedAuthMiddleware(config.Config{RequiredAPIKeys: []string{"old"}}, nil, nil), RequireScope("runtime-admin")).Then(final)
	w = httptest.NewRecorder()
	protected.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/runtimes", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing key status=%d", w.Code)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/runtimes", nil)
	r.Header.Set("Authorization", "Bearer old")
	w = httptest.NewRecorder()
	protected.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("legacy admin status=%d", w.Code)
	}
}

func TestServer_ScopedAuthManagedKeyScope(t *testing.T) {
	s, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.UpsertAPIKey(t.Context(), store.APIKeyRecord{ID: "k", Name: "client", KeyHash: store.KeyFingerprint("secret"), Scopes: []string{"inference"}}); err != nil {
		t.Fatal(err)
	}
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := chain.New(CreateScopedAuthMiddleware(config.Config{}, s, nil), RequireScope("runtime-admin")).Then(final)
	r := httptest.NewRequest(http.MethodGet, "/api/runtimes", nil)
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("managed scope status=%d", w.Code)
	}
}

func TestServer_ScopedAuthStoreFailureFailsClosed(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	h := chain.New(CreateScopedAuthMiddleware(config.Config{}, st, nil), RequireScope(auth.ScopeInference)).Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`)))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("store failure status=%d body=%s, want fail-closed unauthorized", w.Code, w.Body.String())
	}
}

func TestServer_ScopedAuthManagedSaltedKey(t *testing.T) {
	s, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	salt, digest, err := auth.GenerateSalted("secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAPIKey(t.Context(), store.APIKeyRecord{ID: "salted", KeyHash: digest, KeySalt: salt, Scopes: []string{"inference"}}); err != nil {
		t.Fatal(err)
	}
	h := chain.New(CreateScopedAuthMiddleware(config.Config{}, s, nil), RequireScope("inference")).Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("salted key status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestServer_ScopedAuthModelRestrictionAllowsConfiguredAlias(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertAPIKey(t.Context(), store.APIKeyRecord{ID: "alias-key", KeyHash: store.KeyFingerprint("secret"), Scopes: []string{auth.ScopeInference}, Models: []string{"canonical"}}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Models: map[string]config.ModelConfig{"canonical": {Aliases: []string{"alias"}}}}
	h := chain.New(CreateScopedAuthMiddleware(cfg, st, nil), RequireScope(auth.ScopeInference)).Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","messages":[]}`))
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("alias status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestServer_ScopedAuthModelRestrictionAliasAlsoAllowsCanonical(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertAPIKey(t.Context(), store.APIKeyRecord{ID: "alias-key", KeyHash: store.KeyFingerprint("secret"), Scopes: []string{auth.ScopeInference}, Models: []string{"alias"}}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Models: map[string]config.ModelConfig{"canonical": {Aliases: []string{"alias"}}}}
	h := chain.New(CreateScopedAuthMiddleware(cfg, st, nil), RequireScope(auth.ScopeInference)).Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"canonical","messages":[]}`))
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("canonical status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestServer_ScopedAuthOptionalBatchModelIsAuthorized(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertAPIKey(t.Context(), store.APIKeyRecord{
		ID:      "batch-key",
		KeyHash: store.KeyFingerprint("secret"),
		Scopes:  []string{auth.ScopeInference},
		Models:  []string{"served"},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Models: map[string]config.ModelConfig{"served": {}}}
	h := chain.New(CreateScopedAuthMiddleware(cfg, st, nil), RequireScope(auth.ScopeInference)).Then(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := swaputil.ReadContext(r.Context())
		if !ok || data.ModelID != "served" {
			t.Fatalf("missing inferred request context: %+v, %v", data, ok)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions/batch", strings.NewReader(`{"messages":[]}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("allowed optional batch status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestServer_ScopedAuthOptionalBatchModelRejectsDeniedModel(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertAPIKey(t.Context(), store.APIKeyRecord{
		ID:      "batch-key",
		KeyHash: store.KeyFingerprint("secret"),
		Scopes:  []string{auth.ScopeInference},
		Models:  []string{"denied"},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Models: map[string]config.ModelConfig{"served": {}}}
	h := chain.New(CreateScopedAuthMiddleware(cfg, st, nil), RequireScope(auth.ScopeInference)).Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions/batch", strings.NewReader(`{"messages":[]}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("denied optional batch status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestServer_ScopedAuthModelRestrictionAllowsFullyAuthorizedSelector(t *testing.T) {
	cfg := config.Config{
		Models: map[string]config.ModelConfig{"a": {}, "b": {}, "c": {}},
		Selectors: map[string]config.SelectorConfig{
			"pool":    {Strategy: config.SelectorStrategyFailover, Targets: []string{"a", "b"}},
			"partial": {Strategy: config.SelectorStrategyFailover, Targets: []string{"a", "c"}},
		},
	}
	expanded := expandModelAliases(cfg, []string{"a", "b"})
	identity := auth.Identity{Models: expanded}
	if !identity.HasModel("pool") {
		t.Fatalf("fully authorized selector was not added to identity: %v", expanded)
	}
	if identity.HasModel("partial") {
		t.Fatalf("selector with denied target was added to identity: %v", expanded)
	}
}

func TestServer_ModelAllowedForIdentityPeerTargetUsesQualifiedName(t *testing.T) {
	cfg := config.Config{Peers: config.PeerDictionaryConfig{
		"remote": {Models: []string{"model"}},
	}}
	identity := auth.Identity{ID: "scoped", Models: []string{"remote/model"}}
	if !modelAllowedForIdentity(cfg, identity, "model") {
		t.Fatal("unqualified unique peer target should be authorized by its qualified name")
	}
	if !modelAllowedForIdentity(cfg, identity, "remote/model") {
		t.Fatal("qualified peer target should be authorized")
	}
}

func TestServer_BackendProgressVisibleScopesGlobalAndAliases(t *testing.T) {
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"canonical": {Aliases: []string{"alias"}},
	}}
	identity := auth.Identity{ID: "scoped", Models: []string{"alias"}}
	cases := []struct {
		name     string
		progress swaputil.BackendProgressEvent
		want     bool
	}{
		{name: "global runtime event", progress: swaputil.BackendProgressEvent{Runtime: "vllm", Phase: "checking"}},
		{name: "canonical model", progress: swaputil.BackendProgressEvent{Model: "canonical", Phase: "loading"}, want: true},
		{name: "configured alias", progress: swaputil.BackendProgressEvent{Model: "alias", Phase: "active"}, want: true},
		{name: "other model", progress: swaputil.BackendProgressEvent{Model: "other", Phase: "loading"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := backendProgressVisible(cfg, identity, tt.progress); got != tt.want {
				t.Fatalf("visible=%v, want %v for %+v", got, tt.want, tt.progress)
			}
		})
	}
	if !backendProgressVisible(cfg, auth.Identity{Legacy: true}, cases[0].progress) {
		t.Fatal("legacy identity should receive global progress")
	}
	if !backendProgressVisible(cfg, auth.Identity{}, cases[0].progress) {
		t.Fatal("unrestricted identity should receive global progress")
	}
}

func TestServer_ActivityEventVisibleScopesModelCaptures(t *testing.T) {
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"canonical": {Aliases: []string{"alias"}},
	}}
	identity := auth.Identity{ID: "scoped", Models: []string{"alias"}}
	if !activityEventVisible(cfg, identity, ActivityLogEntry{ID: 1, Model: "canonical"}) {
		t.Fatal("canonical activity should be visible through configured alias")
	}
	if activityEventVisible(cfg, identity, ActivityLogEntry{ID: 2, Model: "other"}) {
		t.Fatal("activity id for another model leaked to scoped identity")
	}
	if activityEventVisible(cfg, identity, ActivityLogEntry{ID: 3}) {
		t.Fatal("model-less activity must not be visible to scoped identity")
	}
}

func TestServer_RuntimeAllowedForIdentityRequiresExclusiveModelOwnership(t *testing.T) {
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"allowed": {Backend: config.BackendConfig{Runtime: "vllm"}},
		"denied":  {Backend: config.BackendConfig{Runtime: "vllm"}},
		"other":   {Backend: config.BackendConfig{Runtime: "llamacpp"}},
	}}
	identity := auth.Identity{ID: "scoped", Models: []string{"allowed"}}
	if runtimeAllowedForIdentity(cfg, identity, "vllm") {
		t.Fatal("runtime shared with a denied model must not be visible to a scoped key")
	}
	if runtimeAllowedForIdentity(cfg, identity, "llamacpp") {
		t.Fatal("runtime with no authorized model reference must not be visible")
	}
	cfg.Models["denied"] = config.ModelConfig{Backend: config.BackendConfig{Runtime: "other-runtime"}}
	if !runtimeAllowedForIdentity(cfg, identity, "vllm") {
		t.Fatal("runtime exclusively referenced by an authorized model should be visible")
	}
	if !runtimeAllowedForIdentity(cfg, auth.Identity{Legacy: true}, "unconfigured") {
		t.Fatal("legacy identity should retain global runtime access")
	}
}
