-- +goose Up
-- Activity rows predate managed API keys and cache/cost observability. Keep
-- every column compatible with legacy inserts through explicit defaults.
ALTER TABLE activity ADD COLUMN key_id TEXT NOT NULL DEFAULT '';
ALTER TABLE activity ADD COLUMN session_id TEXT NOT NULL DEFAULT '';
ALTER TABLE activity ADD COLUMN cache_creation_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE activity ADD COLUMN reasoning_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE activity ADD COLUMN estimated_cost REAL NOT NULL DEFAULT 0;
ALTER TABLE activity ADD COLUMN cost_estimated INTEGER NOT NULL DEFAULT 0;
ALTER TABLE activity ADD COLUMN cache_hit_ratio REAL NOT NULL DEFAULT 0;
ALTER TABLE activity ADD COLUMN repair_applied INTEGER NOT NULL DEFAULT 0;
ALTER TABLE activity ADD COLUMN prefix_hash TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_activity_key_created_id
    ON activity (key_id, ts_created DESC, id DESC);

CREATE INDEX idx_activity_session_created_id
    ON activity (session_id, ts_created DESC, id DESC);

-- +goose Down
DROP INDEX idx_activity_session_created_id;
DROP INDEX idx_activity_key_created_id;
-- SQLite cannot drop columns on all supported versions. The migration is
-- intentionally forward-only in practice; down is retained for goose's
-- migration graph and leaves the added columns in place.
