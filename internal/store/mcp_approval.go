package store

import (
	"context"
	"errors"
	"time"
)

type MCPApproval struct {
	ID        string    `json:"id"`
	Owner     string    `json:"-"`
	Model     string    `json:"model"`
	Binding   []byte    `json:"binding"`
	Arguments []byte    `json:"arguments"`
	State     string    `json:"state"`
	Result    []byte    `json:"result,omitempty"`
	Resume    []byte    `json:"-"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Store) CreateMCPApproval(ctx context.Context, a MCPApproval) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	if len(a.Binding)+len(a.Arguments) > 1<<20 {
		return errors.New("MCP approval exceeds capacity")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM mcp_approvals WHERE expires_at<?`, time.Now().UnixMilli()); err != nil {
		return err
	}
	var count int
	var size int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(length(binding_json)+length(arguments_json)+COALESCE(length(result_json),0)+COALESCE(length(resume_json),0)),0) FROM mcp_approvals WHERE owner_id=?`, a.Owner).Scan(&count, &size); err != nil {
		return err
	}
	if count >= 64 || size+int64(len(a.Binding)+len(a.Arguments)) > 64<<20 {
		return errors.New("MCP approval storage quota exceeded")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO mcp_approvals(id,owner_id,model_id,binding_json,arguments_json,expires_at) VALUES(?,?,?,?,?,?)`, a.ID, a.Owner, a.Model, a.Binding, a.Arguments, a.ExpiresAt.UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) GetMCPApproval(ctx context.Context, id, owner string) (MCPApproval, error) {
	var a MCPApproval
	var expires int64
	if err := s.ensureDB(); err != nil {
		return a, err
	}
	err := s.db.QueryRowContext(ctx, `SELECT id,owner_id,model_id,binding_json,arguments_json,state,result_json,resume_json,expires_at FROM mcp_approvals WHERE id=? AND owner_id=? AND expires_at>?`, id, owner, time.Now().UnixMilli()).Scan(&a.ID, &a.Owner, &a.Model, &a.Binding, &a.Arguments, &a.State, &a.Result, &a.Resume, &expires)
	a.ExpiresAt = time.UnixMilli(expires)
	return a, err
}
func (s *Store) SetMCPApprovalResume(ctx context.Context, id, owner string, resume []byte) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	if len(resume) > 16<<20 {
		return errors.New("MCP continuation exceeds capacity")
	}
	r, err := s.db.ExecContext(ctx, `UPDATE mcp_approvals SET resume_json=? WHERE id=? AND owner_id=? AND state IN ('pending','completed') AND resume_json IS NULL AND (SELECT COALESCE(SUM(length(binding_json)+length(arguments_json)+COALESCE(length(result_json),0)+COALESCE(length(resume_json),0)),0) FROM mcp_approvals WHERE owner_id=?) - COALESCE(length(resume_json),0) + ? <= ?`, resume, id, owner, owner, len(resume), 64<<20)
	return conversationMutation(r, err)
}
func (s *Store) ClaimMCPResume(ctx context.Context, id, owner string) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	r, err := s.db.ExecContext(ctx, `UPDATE mcp_approvals SET resume_claimed=1 WHERE id=? AND owner_id=? AND resume_claimed=0 AND expires_at>?`, id, owner, time.Now().UnixMilli())
	return conversationMutation(r, err)
}
func (s *Store) ClaimMCPApproval(ctx context.Context, id, owner string, allow bool) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	state := "running"
	if !allow {
		state = "denied"
	}
	r, err := s.db.ExecContext(ctx, `UPDATE mcp_approvals SET state=? WHERE id=? AND owner_id=? AND state='pending' AND expires_at>?`, state, id, owner, time.Now().UnixMilli())
	return conversationMutation(r, err)
}
func (s *Store) FinishMCPApproval(ctx context.Context, id, owner, state string, result []byte) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	if len(result) > 16<<20 {
		return errors.New("MCP result exceeds capacity")
	}
	r, err := s.db.ExecContext(ctx, `UPDATE mcp_approvals SET state=?,result_json=? WHERE id=? AND owner_id=? AND state='running' AND (SELECT COALESCE(SUM(length(binding_json)+length(arguments_json)+COALESCE(length(result_json),0)+COALESCE(length(resume_json),0)),0) FROM mcp_approvals WHERE owner_id=?) - COALESCE(length(result_json),0) + ? <= ?`, state, result, id, owner, owner, len(result), 64<<20)
	return conversationMutation(r, err)
}

func (s *Store) ClaimMCPInput(ctx context.Context, id, owner string, previous []byte) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	r, err := s.db.ExecContext(ctx, `UPDATE mcp_approvals SET state='running' WHERE id=? AND owner_id=? AND state='completed' AND result_json=? AND resume_claimed=0 AND expires_at>?`, id, owner, previous, time.Now().UnixMilli())
	return conversationMutation(r, err)
}
