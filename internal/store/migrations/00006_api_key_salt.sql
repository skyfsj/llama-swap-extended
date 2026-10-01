-- +goose Up
ALTER TABLE api_keys ADD COLUMN key_salt TEXT NOT NULL DEFAULT '';

-- +goose Down
-- SQLite versions supported by llama-swap do not provide a portable DROP
-- COLUMN. The salt column is intentionally retained on downgrade.
