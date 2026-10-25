-- +goose Up
ALTER TABLE mcp_approvals ADD COLUMN resume_json BLOB;
ALTER TABLE mcp_approvals ADD COLUMN resume_claimed INTEGER NOT NULL DEFAULT 0;
-- +goose Down
ALTER TABLE mcp_approvals DROP COLUMN resume_claimed;
ALTER TABLE mcp_approvals DROP COLUMN resume_json;
