package store

import (
	"context"
	"testing"
)

// TestStore_APIKeySourceRoundTrips pins the config-ownership marker: the
// configuration reconcile revokes exactly the rows carrying it, so it has to
// survive every read path and be clearable when a row stops being config-owned.
func TestStore_APIKeySourceRoundTrips(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	if err := s.UpsertAPIKey(ctx, APIKeyRecord{
		ID: "seeded", Name: "from-config", KeyHash: KeyFingerprint("seeded-secret"),
		KeySecret: "seeded-secret", Scopes: []string{"inference"}, Source: KeySourceConfig,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAPIKey(ctx, APIKeyRecord{
		ID: "operator", Name: "from-ui", KeyHash: KeyFingerprint("operator-secret"),
		KeySecret: "operator-secret", Scopes: []string{"inference"}, Source: "  ",
	}); err != nil {
		t.Fatal(err)
	}

	records, err := s.ListAPIKeys(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]APIKeyRecord{}
	for _, record := range records {
		byID[record.ID] = record
	}
	if got := byID["seeded"].Source; got != KeySourceConfig {
		t.Fatalf("seeded source=%q, want %q", got, KeySourceConfig)
	}
	if got := byID["operator"].Source; got != "" {
		t.Fatalf("operator source=%q, want empty (a whitespace marker is not config ownership)", got)
	}

	// The single-row lookup shares the column list, so it must agree.
	byHash, found, err := s.FindAPIKeyByHash(ctx, KeyFingerprint("seeded-secret"))
	if err != nil || !found || byHash.Source != KeySourceConfig {
		t.Fatalf("FindAPIKeyByHash source=%q found=%v err=%v", byHash.Source, found, err)
	}

	// A row that is no longer declared by the file drops the marker, so a later
	// reconcile cannot revoke a key the operator has taken over.
	if err := s.UpsertAPIKey(ctx, APIKeyRecord{
		ID: "seeded", Name: "from-config", KeyHash: KeyFingerprint("seeded-secret"),
		KeySecret: "seeded-secret", Scopes: []string{"inference"},
	}); err != nil {
		t.Fatal(err)
	}
	cleared, found, err := s.FindAPIKeyByHash(ctx, KeyFingerprint("seeded-secret"))
	if err != nil || !found || cleared.Source != "" {
		t.Fatalf("source after a control-plane write=%q found=%v err=%v, want empty", cleared.Source, found, err)
	}
}
