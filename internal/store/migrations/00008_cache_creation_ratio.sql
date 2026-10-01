-- +goose Up
-- Cache-read and cache-write percentages are kept separately so operators can
-- distinguish warm-prefix reuse from prompt-cache construction. Existing rows
-- remain valid with a zero default and are still aggregated from token counts.
ALTER TABLE activity ADD COLUMN cache_creation_ratio REAL NOT NULL DEFAULT 0;
ALTER TABLE audit_conversations ADD COLUMN cache_creation_ratio REAL NOT NULL DEFAULT 0;

-- +goose Down
-- SQLite versions supported by llama-swap do not provide a portable DROP
-- COLUMN. The ratio columns are intentionally retained on downgrade.
