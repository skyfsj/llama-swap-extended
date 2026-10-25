-- +goose Up
CREATE TABLE conversation_generations (
    id TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    conversation_id TEXT NOT NULL,
    model_id TEXT NOT NULL,
    epoch TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'queued',
    format TEXT NOT NULL,
    http_status INTEGER NOT NULL DEFAULT 0,
    result BLOB,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX idx_conversation_generations_owner ON conversation_generations(owner_id, created_at);
-- +goose Down
DROP TABLE conversation_generations;
