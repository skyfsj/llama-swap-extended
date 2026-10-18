package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestStore_ExtensionPendingPersistsAndClaims(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	record := ExtensionPending{ID: "pending-1", IdentityID: "key-1", ModelID: "m", Endpoint: "chat.completions", Payload: []byte(`{"ok":true}`), CallIDs: []string{"call_ls_one", "call_ls_two"}, ExpiresAt: time.Now().Add(time.Hour)}
	if err := st.SaveExtensionPending(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, err := st.LookupExtensionPending(context.Background(), "call_ls_two")
	if err != nil || got.ID != record.ID || len(got.CallIDs) != 2 {
		t.Fatalf("lookup = %+v, %v", got, err)
	}
	if err := st.ClaimExtensionPending(context.Background(), record.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.ClaimExtensionPending(context.Background(), record.ID); !errors.Is(err, ErrExtensionPendingClaimed) {
		t.Fatalf("second claim = %v", err)
	}
	if err := st.ReleaseExtensionPending(context.Background(), record.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.ClaimExtensionPending(context.Background(), record.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteExtensionPending(context.Background(), record.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LookupExtensionPending(context.Background(), "call_ls_one"); !errors.Is(err, ErrExtensionPendingNotFound) {
		t.Fatalf("deleted lookup = %v", err)
	}
}

func TestStore_ExtensionPendingClaimRecoveryAndPrune(t *testing.T) {
	st, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	record := ExtensionPending{ID: "recover", IdentityID: "key", ModelID: "m", Endpoint: "chat.completions", Payload: []byte(`{}`), CallIDs: []string{"call_ls_recover"}, ExpiresAt: time.Now().Add(time.Hour)}
	if err := st.SaveExtensionPending(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := st.ClaimExtensionPending(ctx, record.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.ResetExtensionPendingClaims(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.ClaimExtensionPending(ctx, record.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.PruneExtensionPending(ctx, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LookupExtensionPending(ctx, "call_ls_recover"); !errors.Is(err, ErrExtensionPendingNotFound) {
		t.Fatalf("pruned lookup=%v", err)
	}
}

// A second round keyed by the same call id supersedes the older, unconsumed one
// instead of failing the save: Gemini pairs calls by tool name, so two mixed
// rounds can legitimately carry the same key.
func TestStore_ExtensionPendingCallIDReuseSupersedes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	base := time.Now().Add(time.Hour)
	first := ExtensionPending{ID: "round-1", IdentityID: "key-1", ModelID: "m", Endpoint: "gemini.generateContent", Payload: []byte(`{"calls":[]}`), CallIDs: []string{"Read"}, ExpiresAt: base}
	if err := st.SaveExtensionPending(context.Background(), first); err != nil {
		t.Fatalf("first save: %v", err)
	}
	second := ExtensionPending{ID: "round-2", IdentityID: "key-1", ModelID: "m", Endpoint: "gemini.generateContent", Payload: []byte(`{"calls":[{"name":"Read","kind":"client"}]}`), CallIDs: []string{"Read"}, ExpiresAt: base}
	if err := st.SaveExtensionPending(context.Background(), second); err != nil {
		t.Fatalf("second save with the same call id: %v", err)
	}
	record, err := st.LookupExtensionPending(context.Background(), "Read")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if record.ID != "round-2" {
		t.Fatalf("lookup returned %q, want the newest round", record.ID)
	}
	// The superseded record is gone, not merely unreachable through its call id.
	if err := st.DeleteExtensionPending(context.Background(), "round-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LookupExtensionPending(context.Background(), "Read"); !errors.Is(err, ErrExtensionPendingNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}
