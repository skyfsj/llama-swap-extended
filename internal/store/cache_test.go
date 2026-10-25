package store

import (
	"context"
	"testing"
	"time"
)

func TestStore_CacheEntryRoundTrip(t *testing.T) {
	ctx := context.Background()
	st, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer st.Close()

	written := time.Now().Add(-time.Minute).Truncate(time.Second)
	if err := st.SetCacheEntry(ctx, CacheEntry{
		Key:       "k",
		Data:      []byte("value"),
		TTL:       time.Hour,
		Timestamp: written,
	}); err != nil {
		t.Fatalf("SetCacheEntry: %v", err)
	}

	entry, found, err := st.GetCacheEntry(ctx, "k")
	if err != nil || !found {
		t.Fatalf("GetCacheEntry: found=%v err=%v", found, err)
	}
	if string(entry.Data) != "value" {
		t.Errorf("data = %q", entry.Data)
	}
	if !entry.Timestamp.Equal(written) {
		t.Errorf("timestamp = %v, want %v (the writer's clock, not the reader's)", entry.Timestamp, written)
	}
	if entry.TTL != time.Hour {
		t.Errorf("TTL = %v, want 1h", entry.TTL)
	}
}

func TestStore_CacheEntryOverwrites(t *testing.T) {
	ctx := context.Background()
	st, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer st.Close()

	for _, value := range []string{"first", "second"} {
		if err := st.SetCacheEntry(ctx, CacheEntry{Key: "k", Data: []byte(value)}); err != nil {
			t.Fatalf("SetCacheEntry(%s): %v", value, err)
		}
	}
	entry, found, err := st.GetCacheEntry(ctx, "k")
	if err != nil || !found {
		t.Fatalf("GetCacheEntry: found=%v err=%v", found, err)
	}
	if string(entry.Data) != "second" {
		t.Errorf("data = %q, want the last write", entry.Data)
	}
}

func TestStore_CacheEntryExpiry(t *testing.T) {
	ctx := context.Background()
	st, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer st.Close()

	// A one-second TTL written in the past is already expired.
	if err := st.SetCacheEntry(ctx, CacheEntry{
		Key:       "stale",
		Data:      []byte("v"),
		TTL:       time.Second,
		Timestamp: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("SetCacheEntry: %v", err)
	}
	if _, found, err := st.GetCacheEntry(ctx, "stale"); err != nil || found {
		t.Fatalf("expired entry found=%v err=%v, want a miss", found, err)
	}
	// The read removes it, so a later prune has nothing to do.
	if err := st.PruneCacheEntries(ctx); err != nil {
		t.Fatalf("PruneCacheEntries: %v", err)
	}
	if _, found, _ := st.GetCacheEntry(ctx, "stale"); found {
		t.Error("expired entry survived the read and the prune")
	}
}

func TestStore_CacheEntryWithoutTTLNeverExpires(t *testing.T) {
	ctx := context.Background()
	st, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer st.Close()

	if err := st.SetCacheEntry(ctx, CacheEntry{
		Key:       "forever",
		Data:      []byte("v"),
		Timestamp: time.Now().Add(-100 * time.Hour),
	}); err != nil {
		t.Fatalf("SetCacheEntry: %v", err)
	}
	if err := st.PruneCacheEntries(ctx); err != nil {
		t.Fatalf("PruneCacheEntries: %v", err)
	}
	if _, found, err := st.GetCacheEntry(ctx, "forever"); err != nil || !found {
		t.Fatalf("found=%v err=%v, want a hit (no TTL means no expiry)", found, err)
	}
}

func TestStore_CacheSubSecondTTLStillExpires(t *testing.T) {
	ctx := context.Background()
	st, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer st.Close()

	// Rounded down to whole seconds this would be "never"; it must round up
	// instead, or a short expiry would outlive the process that wrote it.
	if err := st.SetCacheEntry(ctx, CacheEntry{
		Key:       "brief",
		Data:      []byte("v"),
		TTL:       250 * time.Millisecond,
		Timestamp: time.Now().Add(-2 * time.Second),
	}); err != nil {
		t.Fatalf("SetCacheEntry: %v", err)
	}
	if _, found, _ := st.GetCacheEntry(ctx, "brief"); found {
		t.Error("a sub-second TTL was stored as never-expiring")
	}
}

func TestStore_CacheEntryDelete(t *testing.T) {
	ctx := context.Background()
	st, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer st.Close()

	if err := st.SetCacheEntry(ctx, CacheEntry{Key: "k", Data: []byte("v")}); err != nil {
		t.Fatalf("SetCacheEntry: %v", err)
	}
	if err := st.DeleteCacheEntry(ctx, "k"); err != nil {
		t.Fatalf("DeleteCacheEntry: %v", err)
	}
	if _, found, _ := st.GetCacheEntry(ctx, "k"); found {
		t.Error("entry survived the delete")
	}
	// Deleting a key that is not there is not an error.
	if err := st.DeleteCacheEntry(ctx, "k"); err != nil {
		t.Fatalf("DeleteCacheEntry on a missing key: %v", err)
	}
}

func TestStore_CacheEntryRequiresKey(t *testing.T) {
	st, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer st.Close()

	if err := st.SetCacheEntry(context.Background(), CacheEntry{Data: []byte("v")}); err == nil {
		t.Fatal("a cache entry without a key must be rejected")
	}
}
