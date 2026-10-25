package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

const MaxProtocolFileBytes = 16 << 20

type ProtocolFile struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	Owner     string `json:"-"`
	Digest    string `json:"sha256"`
	Filename  string `json:"filename"`
	MIME      string `json:"mime_type"`
	Bytes     int    `json:"bytes"`
	CreatedAt int64  `json:"created_at"`
	Content   []byte `json:"-"`
}

func (s *Store) PutProtocolFile(ctx context.Context, f ProtocolFile) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	if len(f.Content) > MaxProtocolFileBytes || len(f.Content) == 0 || f.ID == "" || f.Filename == "" {
		return errors.New("invalid file or file exceeds 16 MiB")
	}
	sum := sha256.Sum256(f.Content)
	f.Digest = hex.EncodeToString(sum[:])
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var used int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(bytes),0) FROM protocol_files WHERE owner_id=?`, f.Owner).Scan(&used); err != nil {
		return err
	}
	if used+int64(len(f.Content)) > 256<<20 {
		return errors.New("file storage quota exceeded")
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO protocol_file_blobs(digest,content) VALUES(?,?)`, f.Digest, f.Content); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO protocol_files(id,owner_id,digest,filename,mime_type,bytes,created_at) VALUES(?,?,?,?,?,?,?)`, f.ID, f.Owner, f.Digest, f.Filename, f.MIME, len(f.Content), time.Now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) GetProtocolFile(ctx context.Context, id, owner string) (ProtocolFile, error) {
	var f ProtocolFile
	if err := s.ensureDB(); err != nil {
		return f, err
	}
	err := s.db.QueryRowContext(ctx, `SELECT f.id,f.owner_id,f.digest,f.filename,f.mime_type,f.bytes,f.created_at,b.content FROM protocol_files f JOIN protocol_file_blobs b ON b.digest=f.digest WHERE f.id=? AND f.owner_id=?`, id, owner).Scan(&f.ID, &f.Owner, &f.Digest, &f.Filename, &f.MIME, &f.Bytes, &f.CreatedAt, &f.Content)
	f.Object = "file"
	return f, err
}
func (s *Store) FindProtocolFile(ctx context.Context, digest, owner string) (ProtocolFile, error) {
	if err := s.ensureDB(); err != nil {
		return ProtocolFile{}, err
	}
	var id string
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM protocol_files WHERE digest=? AND owner_id=? ORDER BY created_at DESC LIMIT 1`, digest, owner).Scan(&id); err != nil {
		return ProtocolFile{}, err
	}
	return s.GetProtocolFile(ctx, id, owner)
}
func (s *Store) ListProtocolFiles(ctx context.Context, owner string) ([]ProtocolFile, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,digest,filename,mime_type,bytes,created_at FROM protocol_files WHERE owner_id=? ORDER BY created_at DESC,id LIMIT 1000`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	files := []ProtocolFile{}
	for rows.Next() {
		f := ProtocolFile{Owner: owner, Object: "file"}
		if err := rows.Scan(&f.ID, &f.Digest, &f.Filename, &f.MIME, &f.Bytes, &f.CreatedAt); err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, rows.Err()
}
func (s *Store) DeleteProtocolFile(ctx context.Context, id, owner string) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM protocol_file_references WHERE file_id IN (SELECT id FROM protocol_files WHERE id=? AND owner_id=?)`, id, owner); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM protocol_files WHERE id=? AND owner_id=?`, id, owner)
	if err := conversationMutation(result, err); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM protocol_file_blobs WHERE digest NOT IN (SELECT digest FROM protocol_files)`); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) ProtocolFileReference(ctx context.Context, id, owner, scope string) (string, error) {
	if err := s.ensureDB(); err != nil {
		return "", err
	}
	var ref string
	err := s.db.QueryRowContext(ctx, `SELECT r.reference FROM protocol_file_references r JOIN protocol_files f ON f.id=r.file_id WHERE f.id=? AND f.owner_id=? AND r.scope=? AND r.expires_at>?`, id, owner, scope, time.Now().Add(time.Minute).UnixMilli()).Scan(&ref)
	return ref, err
}
func (s *Store) PutProtocolFileReference(ctx context.Context, id, owner, scope, reference string, expires time.Time) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	r, err := s.db.ExecContext(ctx, `INSERT INTO protocol_file_references(file_id,scope,reference,expires_at) SELECT id,?,?,? FROM protocol_files WHERE id=? AND owner_id=? ON CONFLICT(file_id,scope) DO UPDATE SET reference=excluded.reference,expires_at=excluded.expires_at`, scope, reference, expires.UnixMilli(), id, owner)
	return conversationMutation(r, err)
}
