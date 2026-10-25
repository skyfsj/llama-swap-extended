package store

import (
	"context"
	"errors"
	"time"
)

func (s *Store) SaveMCPTaskRoute(ctx context.Context, id, owner string, route []byte, expires time.Time) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	if id == "" || len(route) > 1<<20 {
		return errors.New("MCP task route exceeds capacity")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM mcp_task_routes WHERE expires_at<?`, time.Now().UnixMilli()); err != nil {
		return err
	}
	var count int
	var size int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(length(route_json)),0) FROM mcp_task_routes WHERE owner_id=?`, owner).Scan(&count, &size); err != nil {
		return err
	}
	if count >= 1024 || size+int64(len(route)) > 64<<20 {
		return errors.New("MCP task route quota exceeded")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO mcp_task_routes(id,owner_id,route_json,expires_at) VALUES(?,?,?,?)`, id, owner, route, expires.UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) LoadMCPTaskRoute(ctx context.Context, id, owner string) ([]byte, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	var route []byte
	err := s.db.QueryRowContext(ctx, `SELECT route_json FROM mcp_task_routes WHERE id=? AND owner_id=? AND expires_at>?`, id, owner, time.Now().UnixMilli()).Scan(&route)
	return route, err
}
