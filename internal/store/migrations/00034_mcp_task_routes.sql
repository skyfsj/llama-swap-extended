-- +goose Up
CREATE TABLE mcp_task_routes (
    id TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    route_json BLOB NOT NULL,
    expires_at INTEGER NOT NULL
);
CREATE INDEX idx_mcp_task_routes_owner ON mcp_task_routes(owner_id, expires_at);
-- +goose Down
DROP TABLE mcp_task_routes;
