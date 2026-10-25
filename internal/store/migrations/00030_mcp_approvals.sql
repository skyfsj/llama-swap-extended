-- +goose Up
CREATE TABLE mcp_approvals (
    id TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    model_id TEXT NOT NULL,
    binding_json BLOB NOT NULL,
    arguments_json BLOB NOT NULL,
    state TEXT NOT NULL DEFAULT 'pending',
    result_json BLOB,
    expires_at INTEGER NOT NULL
);
CREATE INDEX idx_mcp_approvals_owner ON mcp_approvals(owner_id, expires_at);
-- +goose Down
DROP TABLE mcp_approvals;
