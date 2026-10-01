-- +goose Up
ALTER TABLE activity ADD COLUMN first_token_ms INTEGER NOT NULL DEFAULT -1;

-- +goose Down
-- SQLite cannot drop a column on every supported version. Keeping this
-- migration irreversible preserves the activity table and its numeric IDs.
