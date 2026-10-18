package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/store"
)

func keyRecordForTest(t *testing.T, s *Server, id string) store.APIKeyRecord {
	t.Helper()
	records, err := s.store.ListAPIKeys(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.ID == id {
			return record
		}
	}
	t.Fatalf("api key %q not found in the store", id)
	return store.APIKeyRecord{}
}

func activeKeyForTest(t *testing.T, s *Server, id string) bool {
	t.Helper()
	return keyRecordForTest(t, s, id).RevokedAt == nil
}

// TestServer_ConfigEditRevokesRemovedStartupKeys covers the whole point of the
// configuration reconcile: a key deleted from the apiKeys section stops working
// immediately, while the keys an operator created through the control plane are
// never touched by an edit to the file.
func TestServer_ConfigEditRevokesRemovedStartupKeys(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })

	kept := "kept-secret"
	removed := "removed-secret"
	s.cfg = config.Config{
		RequiredAPIKeys: []string{kept},
		StartupAPIKeys:  []config.StartupAPIKey{{ID: "retired", Name: "retired", Key: removed}},
	}
	if err := s.seedStartupAPIKeys(s.cfg); err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpsertAPIKey(context.Background(), store.APIKeyRecord{
		ID: "operator-key", Name: "operator", KeyHash: store.KeyFingerprint("operator-secret"),
		KeySecret: "operator-secret", Kind: auth.KeyKindManagement, Scopes: []string{auth.ScopeInference},
		AllowManagementLogin: false,
	}); err != nil {
		t.Fatal(err)
	}

	keptID := store.KeyFingerprint(kept)
	for _, id := range []string{keptID, "retired", "operator-key"} {
		if !activeKeyForTest(t, s, id) {
			t.Fatalf("api key %q is not active after the initial seed", id)
		}
	}
	if source := keyRecordForTest(t, s, keptID).Source; source != store.KeySourceConfig {
		t.Fatalf("legacy scalar source=%q, want %q", source, store.KeySourceConfig)
	}

	// The operator deletes the structured entry from the file.
	next := config.Config{RequiredAPIKeys: []string{kept}}
	if err := s.seedStartupAPIKeys(next); err != nil {
		t.Fatal(err)
	}
	if activeKeyForTest(t, s, "retired") {
		t.Fatal("a key removed from the configuration is still active")
	}
	if !activeKeyForTest(t, s, keptID) {
		t.Fatal("a key still declared by the configuration was revoked")
	}
	if !activeKeyForTest(t, s, "operator-key") {
		t.Fatal("an operator-created key was revoked by a configuration edit")
	}

	// Re-adding the key in the file brings it back rather than staying revoked.
	restored := config.Config{
		RequiredAPIKeys: []string{kept},
		StartupAPIKeys:  []config.StartupAPIKey{{ID: "retired", Name: "retired", Key: removed}},
	}
	if err := s.seedStartupAPIKeys(restored); err != nil {
		t.Fatal(err)
	}
	if !activeKeyForTest(t, s, "retired") {
		t.Fatal("a re-added configuration key stayed revoked")
	}
}

// TestServer_ConfigRemovalCascadesToAccessKeys pins the nesting rule for config
// keys: dropping the management key a client's access key hangs from must not
// leave that access key able to call models.
func TestServer_ConfigRemovalCascadesToAccessKeys(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })

	secret := "parent-secret"
	s.cfg = config.Config{
		RequiredAPIKeys: []string{secret, "other-secret"},
	}
	if err := s.seedStartupAPIKeys(s.cfg); err != nil {
		t.Fatal(err)
	}
	parentID := store.KeyFingerprint(secret)
	if err := s.store.UpsertAPIKey(context.Background(), store.APIKeyRecord{
		ID: "child-access-key", Name: "laptop", KeyHash: store.KeyFingerprint("child-secret"),
		KeySecret: "child-secret", Kind: auth.KeyKindAccess, ParentID: parentID,
		Scopes: []string{auth.ScopeInference},
	}); err != nil {
		t.Fatal(err)
	}

	next := config.Config{RequiredAPIKeys: []string{"other-secret"}}
	if err := s.seedStartupAPIKeys(next); err != nil {
		t.Fatal(err)
	}
	if activeKeyForTest(t, s, parentID) {
		t.Fatal("the removed management key is still active")
	}
	if activeKeyForTest(t, s, "child-access-key") {
		t.Fatal("an access key survived its removed management parent")
	}
}

// TestServer_ConfigEditRefusesToLockEveryoneOut pins the guard: a file edit
// must not be able to leave the deployment requiring a key nobody can present.
// Nothing is revoked when it trips, so the operator keeps a way in and the
// reload caller reports the rejected config instead of applying it.
func TestServer_ConfigEditRefusesToLockEveryoneOut(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })

	secret := "only-login-secret"
	s.cfg = config.Config{RequiredAPIKeys: []string{secret}}
	if err := s.seedStartupAPIKeys(s.cfg); err != nil {
		t.Fatal(err)
	}
	// A management key that can administer nothing in the panel: it keeps
	// authentication on without being able to sign in.
	if err := s.store.UpsertAPIKey(context.Background(), store.APIKeyRecord{
		ID: "api-only", Name: "api-only", KeyHash: store.KeyFingerprint("api-only-secret"),
		KeySecret: "api-only-secret", Kind: auth.KeyKindManagement, Scopes: auth.ManagementScopes(),
		AllowManagementLogin: false, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	err := s.seedStartupAPIKeys(config.Config{})
	if !errors.Is(err, errConfigKeysWouldLockOut) {
		t.Fatalf("error=%v, want errConfigKeysWouldLockOut", err)
	}
	if !activeKeyForTest(t, s, store.KeyFingerprint(secret)) {
		t.Fatal("the guard tripped but the key was revoked anyway")
	}

	// A stored key table always requires authentication, so the way out is
	// another key that can sign in: once the operator has one, the same edit
	// applies and the removed key is revoked.
	if err := s.store.UpsertAPIKey(context.Background(), store.APIKeyRecord{
		ID: "replacement", Name: "replacement", KeyHash: store.KeyFingerprint("replacement-secret"),
		KeySecret: "replacement-secret", Kind: auth.KeyKindManagement, Scopes: auth.ManagementScopes(),
		AllowManagementLogin: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.seedStartupAPIKeys(config.Config{}); err != nil {
		t.Fatal(err)
	}
	if activeKeyForTest(t, s, store.KeyFingerprint(secret)) {
		t.Fatal("the removed key is still active after a replacement signed in")
	}
	if !activeKeyForTest(t, s, "replacement") {
		t.Fatal("the operator's replacement key was revoked by the configuration edit")
	}
}
