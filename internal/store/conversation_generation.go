package store

import (
	"context"
	"errors"
	"time"
)

type ConversationGeneration struct {
	ID             string    `json:"id"`
	Owner          string    `json:"-"`
	ConversationID string    `json:"conversation_id"`
	Model          string    `json:"model"`
	Epoch          string    `json:"-"`
	State          string    `json:"status"`
	Format         string    `json:"format"`
	HTTPStatus     int       `json:"http_status,omitempty"`
	Result         []byte    `json:"-"`
	Native         []byte    `json:"-"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func (s *Store) CreateConversationGeneration(ctx context.Context, g ConversationGeneration) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	var size int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(length(result)),0) FROM conversation_generations WHERE owner_id=?`, g.Owner).Scan(&count, &size); err != nil {
		return err
	}
	if count >= 256 || size > 256<<20 {
		return errors.New("background generation storage quota exceeded")
	}
	now := time.Now().UnixMilli()
	_, err = tx.ExecContext(ctx, `INSERT INTO conversation_generations(id,owner_id,conversation_id,model_id,epoch,format,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, g.ID, g.Owner, g.ConversationID, g.Model, g.Epoch, g.Format, now, now)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetConversationGeneration(ctx context.Context, id, owner string) (ConversationGeneration, error) {
	var g ConversationGeneration
	if err := s.ensureDB(); err != nil {
		return g, err
	}
	var created, updated int64
	err := s.db.QueryRowContext(ctx, `SELECT id,owner_id,conversation_id,model_id,epoch,state,format,http_status,result,native_json,created_at,updated_at FROM conversation_generations WHERE id=? AND owner_id=?`, id, owner).Scan(&g.ID, &g.Owner, &g.ConversationID, &g.Model, &g.Epoch, &g.State, &g.Format, &g.HTTPStatus, &g.Result, &g.Native, &created, &updated)
	g.CreatedAt = time.UnixMilli(created)
	g.UpdatedAt = time.UnixMilli(updated)
	return g, err
}

func (s *Store) UpdateConversationGeneration(ctx context.Context, g ConversationGeneration, previous string) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	if len(g.Result) > MaxConversationBytes || len(g.Native) > MaxConversationBytes {
		return errors.New("background result exceeds capacity")
	}
	r, err := s.db.ExecContext(ctx, `UPDATE conversation_generations SET state=?,http_status=?,result=?,native_json=?,updated_at=? WHERE id=? AND owner_id=? AND epoch=? AND state=? AND (SELECT COALESCE(SUM(COALESCE(length(result),0)+COALESCE(length(native_json),0)),0) FROM conversation_generations WHERE owner_id=?) - COALESCE(length(result),0) - COALESCE(length(native_json),0) + ? <= ?`, g.State, g.HTTPStatus, g.Result, g.Native, time.Now().UnixMilli(), g.ID, g.Owner, g.Epoch, previous, g.Owner, len(g.Result)+len(g.Native), 256<<20)
	return conversationMutation(r, err)
}

func (s *Store) ClaimNativeGeneration(ctx context.Context, id, owner, previousEpoch, epoch string) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	r, err := s.db.ExecContext(ctx, `UPDATE conversation_generations SET epoch=? WHERE id=? AND owner_id=? AND epoch=? AND state='in_progress' AND native_json IS NOT NULL`, epoch, id, owner, previousEpoch)
	return conversationMutation(r, err)
}

func (s *Store) CommitNativeGeneration(ctx context.Context, g ConversationGeneration, c Conversation, token string) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	if len(c.State) > MaxConversationBytes || len(g.Result) > MaxConversationBytes {
		return errors.New("native checkpoint exceeds capacity")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, `UPDATE protocol_conversations SET model_id=?,version=version+1,state_json=?,lease_token='',lease_until=0,updated_at=? WHERE id=? AND owner_id=? AND version=? AND lease_token=? AND lease_until>=?`, c.Model, c.State, time.Now().UnixMilli(), c.ID, c.Owner, c.Version, token, time.Now().UnixMilli())
	if err := conversationMutation(r, err); err != nil {
		return err
	}
	r, err = tx.ExecContext(ctx, `UPDATE conversation_generations SET state='completed',http_status=?,result=?,native_json=NULL,updated_at=? WHERE id=? AND owner_id=? AND epoch=? AND state='in_progress' AND (SELECT COALESCE(SUM(COALESCE(length(result),0)+COALESCE(length(native_json),0)),0) FROM conversation_generations WHERE owner_id=?) - COALESCE(length(result),0) - COALESCE(length(native_json),0) + ? <= ?`, g.HTTPStatus, g.Result, time.Now().UnixMilli(), g.ID, g.Owner, g.Epoch, g.Owner, len(g.Result), 256<<20)
	if err := conversationMutation(r, err); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DeleteConversationGeneration(ctx context.Context, id, owner string) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	r, err := s.db.ExecContext(ctx, `DELETE FROM conversation_generations WHERE id=? AND owner_id=? AND state NOT IN ('queued','in_progress')`, id, owner)
	return conversationMutation(r, err)
}
