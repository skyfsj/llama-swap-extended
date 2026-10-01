-- +goose Up
-- Existing queue entries were created before multi-provider downloads and are
-- therefore Hugging Face tasks.
ALTER TABLE model_downloads ADD COLUMN provider TEXT NOT NULL DEFAULT 'huggingface';

-- +goose Down
-- SQLite versions supported by llama-swap do not provide a portable DROP
-- COLUMN. The provider column is intentionally retained on downgrade.
