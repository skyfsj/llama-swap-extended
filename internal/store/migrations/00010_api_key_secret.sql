-- +goose Up
-- The control plane needs the original secret for explicit integrations such
-- as CC Switch imports. Keep it out of the public JSON projection; the hash
-- and salt remain the authentication lookup fields.
ALTER TABLE api_keys ADD COLUMN key_secret TEXT NOT NULL DEFAULT '';

-- +goose Down
-- SQLite versions supported by llama-swap do not provide a portable DROP
-- COLUMN. The secret column is intentionally retained on downgrade.
