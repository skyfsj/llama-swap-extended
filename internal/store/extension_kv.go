package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// The extension KV store backs ctx.kv for extensions with the "persistent"
// storage permission. Scope isolates data per extension (column) and per
// caller ("global", "session:<id>", "key:<id>"); expiry is unix seconds, 0 for
// no expiry.

type ExtensionKV struct {
	Value     []byte
	ExpiresAt time.Time // zero means no expiry
}

func (s *Store) SaveExtensionKV(ctx context.Context, extensionID, scope, key string, value []byte, expiresAt *time.Time) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	ctx = normalizeContext(ctx)
	// The write path is the reaper for persistent entries: every TTL write
	// opportunistically sweeps rows the indexed expiry has passed.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM extension_kv WHERE expires_at != 0 AND expires_at < ?`, time.Now().Unix()); err != nil {
		return err
	}
	var expires int64
	if expiresAt != nil {
		expires = expiresAt.Unix()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO extension_kv (extension_id,scope,key,value,expires_at,updated_at) VALUES (?,?,?,?,?,?)
ON CONFLICT(extension_id,scope,key) DO UPDATE SET value=excluded.value,expires_at=excluded.expires_at,updated_at=excluded.updated_at`,
		extensionID, scope, key, value, expires, time.Now().Unix())
	return err
}

// GetExtensionKV returns the stored value. An expired entry reads as not
// found and is deleted in the same call.
func (s *Store) GetExtensionKV(ctx context.Context, extensionID, scope, key string) (ExtensionKV, bool, error) {
	if err := s.ensureDB(); err != nil {
		return ExtensionKV{}, false, err
	}
	ctx = normalizeContext(ctx)
	var value []byte
	var expires int64
	err := s.db.QueryRowContext(ctx, `SELECT value,expires_at FROM extension_kv WHERE extension_id=? AND scope=? AND key=?`, extensionID, scope, key).Scan(&value, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return ExtensionKV{}, false, nil
	}
	if err != nil {
		return ExtensionKV{}, false, err
	}
	record := ExtensionKV{Value: value}
	if expires != 0 {
		moment := time.Unix(expires, 0)
		if !moment.After(time.Now()) {
			_, _ = s.db.ExecContext(ctx, `DELETE FROM extension_kv WHERE extension_id=? AND scope=? AND key=?`, extensionID, scope, key)
			return ExtensionKV{}, false, nil
		}
		record.ExpiresAt = moment
	}
	return record, true, nil
}

func (s *Store) DeleteExtensionKV(ctx context.Context, extensionID, scope, key string) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	ctx = normalizeContext(ctx)
	_, err := s.db.ExecContext(ctx, `DELETE FROM extension_kv WHERE extension_id=? AND scope=? AND key=?`, extensionID, scope, key)
	return err
}

// ListExtensionKVPrefix lists live keys in a scope whose key starts with the
// prefix. Expired rows are skipped here and removed by the reaper.
func (s *Store) ListExtensionKVPrefix(ctx context.Context, extensionID, scope, prefix string) ([]string, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	ctx = normalizeContext(ctx)
	rows, err := s.db.QueryContext(ctx, `SELECT key FROM extension_kv WHERE extension_id=? AND scope=? AND key LIKE ? ORDER BY key`, extensionID, scope, prefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	now := time.Now().Unix()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	filtered := make([]string, 0, len(keys))
	for _, key := range keys {
		var expires int64
		if err := s.db.QueryRowContext(ctx, `SELECT expires_at FROM extension_kv WHERE extension_id=? AND scope=? AND key=?`, extensionID, scope, key).Scan(&expires); err == nil && expires != 0 && expires < now {
			continue
		}
		filtered = append(filtered, key)
	}
	return filtered, nil
}

// CountExtensionKVs counts an extension's live keys across every scope, which
// is what the quota check uses.
func (s *Store) CountExtensionKVs(ctx context.Context, extensionID string) (int, error) {
	if err := s.ensureDB(); err != nil {
		return 0, err
	}
	ctx = normalizeContext(ctx)
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM extension_kv WHERE extension_id=?`, extensionID).Scan(&count)
	return count, err
}

// PruneExpiredExtensionKVs removes expired rows; the value column only checks
// the indexed expiry, so this is a cheap sweep.
func (s *Store) PruneExpiredExtensionKVs(ctx context.Context) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	ctx = normalizeContext(ctx)
	_, err := s.db.ExecContext(ctx, `DELETE FROM extension_kv WHERE expires_at != 0 AND expires_at < ?`, time.Now().Unix())
	return err
}

// ExtensionSessionUsage aggregates a caller's recent activity, which is what
// ctx.session.usage reports so extensions can enforce their own quotas.
func (s *Store) ExtensionSessionUsage(ctx context.Context, keyID, sessionID string, sinceUnix int64) (requests, inputTokens, outputTokens int64, err error) {
	if err := s.ensureDB(); err != nil {
		return 0, 0, 0, err
	}
	ctx = normalizeContext(ctx)
	var query string
	var args []any
	switch {
	case sessionID != "":
		query = `SELECT COUNT(*), COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0) FROM activity WHERE session_id=? AND ts_created>=?`
		args = []any{sessionID, sinceUnix}
	case keyID != "" && keyID != "anonymous":
		query = `SELECT COUNT(*), COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0) FROM activity WHERE key_id=? AND ts_created>=?`
		args = []any{keyID, sinceUnix}
	default:
		return 0, 0, 0, nil
	}
	err = s.db.QueryRowContext(ctx, query, args...).Scan(&requests, &inputTokens, &outputTokens)
	return requests, inputTokens, outputTokens, err
}
