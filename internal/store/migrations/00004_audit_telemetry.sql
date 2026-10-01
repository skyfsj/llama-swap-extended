-- +goose Up
ALTER TABLE audit_conversations ADD COLUMN reasoning_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE audit_conversations ADD COLUMN cache_hit_ratio REAL NOT NULL DEFAULT 0;
ALTER TABLE audit_conversations ADD COLUMN repair_applied INTEGER NOT NULL DEFAULT 0;
ALTER TABLE audit_conversations ADD COLUMN prefix_hash TEXT NOT NULL DEFAULT '';

-- +goose Down
-- SQLite versions supported by llama-swap do not provide a portable DROP
-- COLUMN. The telemetry columns are intentionally retained on downgrade.
