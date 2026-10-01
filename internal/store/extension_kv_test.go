package store

import (
	"context"
	"testing"
	"time"
)

func TestStore_ExtensionKVRoundTrip(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	if err := s.SaveExtensionKV(ctx, "ext", "global", "a", []byte(`{"count":1}`), nil); err != nil {
		t.Fatal(err)
	}
	record, found, err := s.GetExtensionKV(ctx, "ext", "global", "a")
	if err != nil || !found || string(record.Value) != `{"count":1}` {
		t.Fatalf("found=%v value=%s err=%v", found, string(record.Value), err)
	}
	if !record.ExpiresAt.IsZero() {
		t.Fatalf("entry without TTL must not expire: %v", record.ExpiresAt)
	}

	// A second scope with the same key is a different row.
	if err := s.SaveExtensionKV(ctx, "ext", "session:one", "a", []byte(`{"count":2}`), nil); err != nil {
		t.Fatal(err)
	}
	scoped, _, _ := s.GetExtensionKV(ctx, "ext", "session:one", "a")
	if string(scoped.Value) != `{"count":2}` {
		t.Fatalf("scope leak: %s", string(scoped.Value))
	}

	keys, err := s.ListExtensionKVPrefix(ctx, "ext", "global", "")
	if err != nil || len(keys) != 1 || keys[0] != "a" {
		t.Fatalf("keys = %v err=%v", keys, err)
	}
	count, err := s.CountExtensionKVs(ctx, "ext")
	if err != nil || count != 2 {
		t.Fatalf("count = %d err=%v", count, err)
	}

	if err := s.DeleteExtensionKV(ctx, "ext", "session:one", "a"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ = s.GetExtensionKV(ctx, "ext", "session:one", "a"); found {
		t.Fatal("delete failed")
	}
}

func TestStore_ExtensionKVExpiry(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	expired := time.Now().Add(-time.Minute)
	if err := s.SaveExtensionKV(ctx, "ext", "global", "old", []byte("1"), &expired); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.GetExtensionKV(ctx, "ext", "global", "old"); found {
		t.Fatal("expired entry read as live")
	}
	live := time.Now().Add(time.Minute)
	if err := s.SaveExtensionKV(ctx, "ext", "global", "new", []byte("2"), &live); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.GetExtensionKV(ctx, "ext", "global", "new"); !found {
		t.Fatal("live entry missing")
	}
	// The save path is the reaper.
	if err := s.SaveExtensionKV(ctx, "ext", "global", "another", []byte("3"), &live); err != nil {
		t.Fatal(err)
	}
	keys, _ := s.ListExtensionKVPrefix(ctx, "ext", "global", "")
	if len(keys) != 2 {
		t.Fatalf("keys after reap = %v", keys)
	}
}
