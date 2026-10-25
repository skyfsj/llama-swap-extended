package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrConversationConflict = errors.New("conversation version or lease conflicts with another request")
var ErrConversationNotFound = errors.New("conversation not found")

const MaxConversationBytes = 16 << 20

type Conversation struct {
	ID        string    `json:"id"`
	Owner     string    `json:"-"`
	Model     string    `json:"model"`
	Version   int64     `json:"version"`
	State     []byte    `json:"-"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ConversationCheckpoint struct {
	ID             string    `json:"id"`
	ConversationID string    `json:"conversation_id"`
	Owner          string    `json:"-"`
	Version        int64     `json:"version"`
	State          []byte    `json:"-"`
	CreatedAt      time.Time `json:"created_at"`
}

func (s *Store) CreateConversation(ctx context.Context, c Conversation) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	if c.ID == "" || len(c.State) > MaxConversationBytes {
		return errors.New("invalid conversation")
	}
	now := time.Now().UnixMilli()
	_, err := s.db.ExecContext(normalizeContext(ctx), `INSERT INTO protocol_conversations(id,owner_id,model_id,version,state_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, c.ID, c.Owner, c.Model, c.Version, nonNilBytes(c.State), now, now)
	return err
}

func (s *Store) GetConversation(ctx context.Context, id, owner string) (Conversation, error) {
	if err := s.ensureDB(); err != nil {
		return Conversation{}, err
	}
	var c Conversation
	var created, updated int64
	err := s.db.QueryRowContext(normalizeContext(ctx), `SELECT id,owner_id,model_id,version,state_json,created_at,updated_at FROM protocol_conversations WHERE id=? AND owner_id=?`, id, owner).Scan(&c.ID, &c.Owner, &c.Model, &c.Version, &c.State, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrConversationNotFound
	}
	c.CreatedAt = time.UnixMilli(created)
	c.UpdatedAt = time.UnixMilli(updated)
	return c, err
}

func (s *Store) ListConversations(ctx context.Context, owner string) ([]Conversation, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(normalizeContext(ctx), `SELECT id,model_id,version,created_at,updated_at FROM protocol_conversations WHERE owner_id=? ORDER BY updated_at DESC LIMIT 100`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Conversation{}
	for rows.Next() {
		var c Conversation
		var created, updated int64
		if err := rows.Scan(&c.ID, &c.Model, &c.Version, &created, &updated); err != nil {
			return nil, err
		}
		c.Owner = owner
		c.CreatedAt = time.UnixMilli(created)
		c.UpdatedAt = time.UnixMilli(updated)
		items = append(items, c)
	}
	return items, rows.Err()
}

func (s *Store) LeaseConversation(ctx context.Context, id, owner string, version int64, token string, until time.Time) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	if token == "" {
		return errors.New("empty lease token")
	}
	r, err := s.db.ExecContext(normalizeContext(ctx), `UPDATE protocol_conversations SET lease_token=?,lease_until=? WHERE id=? AND owner_id=? AND version=? AND (lease_token='' OR lease_until<?)`, token, until.UnixMilli(), id, owner, version, time.Now().UnixMilli())
	return conversationMutation(r, err)
}

func (s *Store) RenewConversation(ctx context.Context, id, owner, token string, until time.Time) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	r, err := s.db.ExecContext(normalizeContext(ctx), `UPDATE protocol_conversations SET lease_until=? WHERE id=? AND owner_id=? AND lease_token=?`, until.UnixMilli(), id, owner, token)
	return conversationMutation(r, err)
}

func (s *Store) CommitConversation(ctx context.Context, c Conversation, token string) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	if len(c.State) > MaxConversationBytes {
		return fmt.Errorf("conversation exceeds %d bytes", MaxConversationBytes)
	}
	r, err := s.db.ExecContext(normalizeContext(ctx), `UPDATE protocol_conversations SET model_id=?,version=version+1,state_json=?,lease_token='',lease_until=0,updated_at=? WHERE id=? AND owner_id=? AND version=? AND lease_token=? AND lease_until>=?`, c.Model, nonNilBytes(c.State), time.Now().UnixMilli(), c.ID, c.Owner, c.Version, token, time.Now().UnixMilli())
	return conversationMutation(r, err)
}

func (s *Store) ReleaseConversation(ctx context.Context, id, owner, token string) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(normalizeContext(ctx), `UPDATE protocol_conversations SET lease_token='',lease_until=0 WHERE id=? AND owner_id=? AND lease_token=?`, id, owner, token)
	return err
}

func (s *Store) DeleteConversation(ctx context.Context, id, owner string) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(normalizeContext(ctx), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(normalizeContext(ctx), `DELETE FROM protocol_conversations WHERE id=? AND owner_id=? AND (lease_token='' OR lease_until<?)`, id, owner, time.Now().UnixMilli())
	if err := conversationMutation(r, err); err != nil {
		return err
	}
	if _, err := tx.ExecContext(normalizeContext(ctx), `DELETE FROM protocol_checkpoints WHERE conversation_id=? AND owner_id=?`, id, owner); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) PutConversationCheckpoint(ctx context.Context, c ConversationCheckpoint) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	if c.ID == "" || len(c.State) > MaxConversationBytes {
		return errors.New("invalid checkpoint")
	}
	r, err := s.db.ExecContext(normalizeContext(ctx), `INSERT INTO protocol_checkpoints(id,conversation_id,owner_id,version,state_json,created_at) SELECT ?,id,owner_id,version,state_json,? FROM protocol_conversations WHERE id=? AND owner_id=? AND version=? AND (lease_token='' OR lease_until<?)`, c.ID, time.Now().UnixMilli(), c.ConversationID, c.Owner, c.Version, time.Now().UnixMilli())
	return conversationMutation(r, err)
}

func (s *Store) GetConversationCheckpoint(ctx context.Context, id, owner string) (ConversationCheckpoint, error) {
	if err := s.ensureDB(); err != nil {
		return ConversationCheckpoint{}, err
	}
	var c ConversationCheckpoint
	var created int64
	err := s.db.QueryRowContext(normalizeContext(ctx), `SELECT id,conversation_id,owner_id,version,state_json,created_at FROM protocol_checkpoints WHERE id=? AND owner_id=?`, id, owner).Scan(&c.ID, &c.ConversationID, &c.Owner, &c.Version, &c.State, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrConversationNotFound
	}
	c.CreatedAt = time.UnixMilli(created)
	return c, err
}

func conversationMutation(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrConversationConflict
	}
	return nil
}
