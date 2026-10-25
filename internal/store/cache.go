package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"
)

// CacheEntry is one cached blob. It is a general purpose key/value record:
// callers own the key namespace and the encoding of Data.
//
// TTL bounds how long the entry stays readable. A zero or negative TTL never
// expires, which suits data that is rewritten on a known event rather than
// aged out. Timestamp is when the entry was written; a zero Timestamp is
// replaced with the current time on Set.
type CacheEntry struct {
	Key       string
	Data      []byte
	TTL       time.Duration
	Timestamp time.Time
}

// Expired reports whether the entry is past its TTL at time at. Entries with a
// zero or negative TTL, or a zero Timestamp, never expire.
func (e CacheEntry) Expired(at time.Time) bool {
	if e.TTL <= 0 || e.Timestamp.IsZero() {
		return false
	}
	return at.After(e.Timestamp.Add(e.TTL))
}

// ttlSeconds converts a TTL to the whole seconds the table stores. A negative
// TTL means "never expires" and is stored as 0.
//
// A positive but sub-second TTL rounds up: storing it as 0 would turn a short
// expiry into "never", and the entry would outlive the process that wrote it.
func ttlSeconds(ttl time.Duration) int64 {
	if ttl <= 0 {
		return 0
	}
	seconds := ttl / time.Second
	if ttl%time.Second != 0 {
		seconds++
	}
	if seconds > math.MaxInt64/1 {
		return math.MaxInt64
	}
	return int64(seconds)
}

// SetCacheEntry inserts entry, replacing any entry stored under the same key.
// A zero Timestamp is replaced with the current time.
func (s *Store) SetCacheEntry(ctx context.Context, entry CacheEntry) error {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return err
	}
	if entry.Key == "" {
		return errors.New("cache entry key is required")
	}
	written := entry.Timestamp
	if written.IsZero() {
		written = time.Now()
	}
	expires := int64(0)
	if ttl := ttlSeconds(entry.TTL); ttl > 0 {
		expires = written.Unix() + ttl
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO cache_entries (key,data,expires_at,updated_at) VALUES (?,?,?,?)
ON CONFLICT(key) DO UPDATE SET data=excluded.data,expires_at=excluded.expires_at,updated_at=excluded.updated_at`,
		entry.Key, entry.Data, expires, written.Unix())
	if err != nil {
		return fmt.Errorf("set cache entry: %w", err)
	}
	return nil
}

// GetCacheEntry returns the entry stored under key. found is false when no
// entry exists or the entry has expired; an expired entry is deleted as a side
// effect of the read.
func (s *Store) GetCacheEntry(ctx context.Context, key string) (CacheEntry, bool, error) {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return CacheEntry{}, false, err
	}
	var (
		data      []byte
		expires   int64
		updatedAt int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT data,expires_at,updated_at FROM cache_entries WHERE key=?`, key).
		Scan(&data, &expires, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return CacheEntry{}, false, nil
	}
	if err != nil {
		return CacheEntry{}, false, fmt.Errorf("get cache entry: %w", err)
	}

	written := time.Unix(updatedAt, 0)
	if expires != 0 && !time.Now().Before(time.Unix(expires, 0)) {
		// Expired rows are removed here rather than left for Prune: a reader
		// that finds a stale value must not be the one that keeps serving it.
		if _, delErr := s.db.ExecContext(ctx, `DELETE FROM cache_entries WHERE key=?`, key); delErr != nil {
			return CacheEntry{}, false, fmt.Errorf("delete expired cache entry: %w", delErr)
		}
		return CacheEntry{}, false, nil
	}

	entry := CacheEntry{Key: key, Data: data, Timestamp: written}
	if expires != 0 {
		entry.TTL = time.Duration(expires-updatedAt) * time.Second
	}
	return entry, true, nil
}

// DeleteCacheEntry removes the entry stored under key. Deleting a key that is
// not present is not an error.
func (s *Store) DeleteCacheEntry(ctx context.Context, key string) error {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM cache_entries WHERE key=?`, key); err != nil {
		return fmt.Errorf("delete cache entry: %w", err)
	}
	return nil
}

// PruneCacheEntries deletes every expired entry. Entries with no expiry are
// kept.
func (s *Store) PruneCacheEntries(ctx context.Context) error {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM cache_entries WHERE expires_at != 0 AND expires_at <= ?`, time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("prune cache entries: %w", err)
	}
	return nil
}
