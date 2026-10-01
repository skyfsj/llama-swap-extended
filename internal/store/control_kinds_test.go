package store

import (
	"context"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/auth"
)

// usageTestKey writes an API key row and returns its id.
func usageTestKey(t *testing.T, s *Store, id string, parent string, group string, models []string) {
	t.Helper()
	kind := auth.KeyKindManagement
	if parent != "" {
		kind = auth.KeyKindAccess
	}
	if err := s.UpsertAPIKey(context.Background(), APIKeyRecord{
		ID: id, Name: id + "-name", KeyHash: KeyFingerprint(id + "-secret"),
		Kind: kind, ParentID: parent, Group: group, Models: models, Scopes: []string{"inference"},
	}); err != nil {
		t.Fatalf("upsert api key %q: %v", id, err)
	}
}

func TestStore_APIKeyKindsAndNesting(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	usageTestKey(t, s, "mgmt", "", "platform", nil)
	usageTestKey(t, s, "child", "mgmt", "team-a", []string{"gpt-4"})
	// A legacy row without a kind must read back as a management key so
	// credentials created before the split keep working.
	legacy, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if err := legacy.UpsertAPIKey(ctx, APIKeyRecord{ID: "legacy", KeyHash: KeyFingerprint("legacy-secret"), Scopes: []string{"inference"}}); err != nil {
		t.Fatal(err)
	}
	legacyKeys, err := legacy.ListAPIKeys(ctx, false)
	if err != nil || len(legacyKeys) != 1 || legacyKeys[0].Kind != auth.KeyKindManagement {
		t.Fatalf("kindless key read back as %#v, err=%v", legacyKeys, err)
	}

	accessKey, found, err := s.FindAPIKeyByID(ctx, "child")
	if err != nil || !found {
		t.Fatalf("find access key found=%v err=%v", found, err)
	}
	if accessKey.Kind != auth.KeyKindAccess || accessKey.ParentID != "mgmt" || accessKey.Group != "team-a" {
		t.Fatalf("access key = %#v", accessKey)
	}
	if len(accessKey.Models) != 1 || accessKey.Models[0] != "gpt-4" {
		t.Fatalf("access key models = %#v", accessKey.Models)
	}
	if accessKey.KeyHash == "" {
		t.Fatal("access key hash was not persisted")
	}

	counts, err := s.ChildAPIKeyCounts(ctx)
	if err != nil {
		t.Fatalf("child counts: %v", err)
	}
	if counts["mgmt"] != 1 || len(counts) != 1 {
		t.Fatalf("child counts = %#v", counts)
	}

	if err := s.UpsertAPIKey(ctx, APIKeyRecord{
		ID: "restricted", KeyHash: KeyFingerprint("restricted-secret"),
		Kind: auth.KeyKindAccess, ParentID: "mgmt",
		AllowedIPs: []string{"203.0.113.0/24", "10.0.0.1"}, MaxConcurrency: 4, Group: "team-b",
	}); err != nil {
		t.Fatalf("upsert restricted key: %v", err)
	}
	stored, found, err := s.FindAPIKeyByID(ctx, "restricted")
	if err != nil || !found {
		t.Fatalf("find restricted key found=%v err=%v", found, err)
	}
	if len(stored.AllowedIPs) != 2 || stored.MaxConcurrency != 4 || stored.Group != "team-b" {
		t.Fatalf("restricted key = %#v", stored)
	}
}

func TestStore_RevokeAPIKeyCascade(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	usageTestKey(t, s, "mgmt", "", "platform", nil)
	usageTestKey(t, s, "child-a", "mgmt", "team-a", nil)
	usageTestKey(t, s, "child-b", "mgmt", "team-a", nil)
	usageTestKey(t, s, "other", "", "other", nil)

	if err := s.RevokeAPIKeyCascade(ctx, "mgmt", time.Now()); err != nil {
		t.Fatalf("cascade revoke: %v", err)
	}
	active, err := s.ListAPIKeys(ctx, false)
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	if len(active) != 1 || active[0].ID != "other" {
		t.Fatalf("active keys after cascade = %#v", active)
	}
	child, found, err := s.FindAPIKeyByID(ctx, "child-a")
	if err != nil || !found || child.RevokedAt == nil {
		t.Fatalf("nested access key not revoked: found=%v err=%v", found, err)
	}
	counts, err := s.ChildAPIKeyCounts(ctx)
	if err != nil {
		t.Fatalf("child counts: %v", err)
	}
	if counts["mgmt"] != 0 {
		t.Fatalf("child counts after cascade = %#v", counts)
	}
	if err := s.RevokeAPIKeyCascade(ctx, "missing-key", time.Now()); err == nil {
		t.Fatal("cascade revoke of unknown key unexpectedly succeeded")
	}
}

func TestStore_APIKeyNormalizationRejectsBadKindsAndIPs(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for name, record := range map[string]APIKeyRecord{
		"unknown kind": {ID: "k1", KeyHash: KeyFingerprint("s1"), Kind: "service"},
		"bad ip":       {ID: "k2", KeyHash: KeyFingerprint("s2"), AllowedIPs: []string{"10.0.0.0/99"}},
		"bad group":    {ID: "k3", KeyHash: KeyFingerprint("s3"), Group: "team\nname"},
	} {
		if err := s.UpsertAPIKey(ctx, record); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}
