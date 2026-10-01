-- +goose Up
-- Existing managed keys were able to authenticate the WebUI before this flag
-- existed, so keep them usable while allowing newly created keys to opt out.
ALTER TABLE api_keys ADD COLUMN allow_management_login INTEGER NOT NULL DEFAULT 1;

-- +goose Down
-- SQLite versions supported by llama-swap do not provide a portable DROP
-- COLUMN. The management-login column is intentionally retained on downgrade.
