package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var ErrExtensionPendingNotFound = errors.New("extension pending call not found")
var ErrExtensionPendingClaimed = errors.New("extension pending call is already in use")

type ExtensionPending struct {
	ID         string
	IdentityID string
	ModelID    string
	Profile    string
	Endpoint   string
	Payload    []byte
	CallIDs    []string
	ExpiresAt  time.Time
}

func (s *Store) SaveExtensionPending(ctx context.Context, record ExtensionPending) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	ctx = normalizeContext(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM extension_pending_calls WHERE pending_id IN (SELECT id FROM extension_pending WHERE expires_at < ?)`, time.Now().Unix()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM extension_pending WHERE expires_at < ?`, time.Now().Unix()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO extension_pending (id,identity_id,model_id,profile,endpoint,payload,expires_at) VALUES (?,?,?,?,?,?,?)`, record.ID, record.IdentityID, record.ModelID, record.Profile, record.Endpoint, record.Payload, record.ExpiresAt.Unix()); err != nil {
		return err
	}
	for _, id := range record.CallIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO extension_pending_calls (call_id,pending_id) VALUES (?,?)`, id, record.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) LookupExtensionPending(ctx context.Context, callID string) (ExtensionPending, error) {
	if err := s.ensureDB(); err != nil {
		return ExtensionPending{}, err
	}
	ctx = normalizeContext(ctx)
	var record ExtensionPending
	var expires int64
	err := s.db.QueryRowContext(ctx, `SELECT p.id,p.identity_id,p.model_id,p.profile,p.endpoint,p.payload,p.expires_at FROM extension_pending_calls c JOIN extension_pending p ON p.id=c.pending_id WHERE c.call_id=? AND p.expires_at>=?`, callID, time.Now().Unix()).Scan(&record.ID, &record.IdentityID, &record.ModelID, &record.Profile, &record.Endpoint, &record.Payload, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return ExtensionPending{}, ErrExtensionPendingNotFound
	}
	if err != nil {
		return ExtensionPending{}, err
	}
	record.ExpiresAt = time.Unix(expires, 0)
	rows, err := s.db.QueryContext(ctx, `SELECT call_id FROM extension_pending_calls WHERE pending_id=? ORDER BY call_id`, record.ID)
	if err != nil {
		return ExtensionPending{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return ExtensionPending{}, err
		}
		record.CallIDs = append(record.CallIDs, id)
	}
	return record, rows.Err()
}

func (s *Store) ClaimExtensionPending(ctx context.Context, id string) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	result, err := s.db.ExecContext(normalizeContext(ctx), `UPDATE extension_pending SET claimed_at=? WHERE id=? AND claimed_at=0 AND expires_at>=?`, time.Now().Unix(), id, time.Now().Unix())
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrExtensionPendingClaimed
	}
	return nil
}

func (s *Store) ReleaseExtensionPending(ctx context.Context, id string) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(normalizeContext(ctx), `UPDATE extension_pending SET claimed_at=0 WHERE id=?`, id)
	return err
}

func (s *Store) DeleteExtensionPending(ctx context.Context, id string) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	ctx = normalizeContext(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM extension_pending_calls WHERE pending_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM extension_pending WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) PruneExtensionPending(ctx context.Context, now time.Time) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	ctx = normalizeContext(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM extension_pending_calls WHERE pending_id IN (SELECT id FROM extension_pending WHERE expires_at < ?)`, now.Unix()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM extension_pending WHERE expires_at < ?`, now.Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ResetExtensionPendingClaims(ctx context.Context) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(normalizeContext(ctx), `UPDATE extension_pending SET claimed_at=0 WHERE claimed_at<>0`)
	return err
}
