-- +goose Up
ALTER TABLE conversation_generations ADD COLUMN native_json BLOB;
-- +goose Down
ALTER TABLE conversation_generations DROP COLUMN native_json;
