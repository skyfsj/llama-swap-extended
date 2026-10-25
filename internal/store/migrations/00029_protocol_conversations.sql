-- +goose Up
CREATE TABLE protocol_conversations (
    id TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    model_id TEXT NOT NULL DEFAULT '',
    version INTEGER NOT NULL DEFAULT 0,
    state_json BLOB NOT NULL,
    lease_token TEXT NOT NULL DEFAULT '',
    lease_until INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX idx_protocol_conversations_owner ON protocol_conversations(owner_id, updated_at);
CREATE TABLE protocol_checkpoints (
    id TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL,
    owner_id TEXT NOT NULL,
    version INTEGER NOT NULL,
    state_json BLOB NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX idx_protocol_checkpoints_conversation ON protocol_checkpoints(conversation_id, owner_id, created_at);

-- +goose Down
DROP TABLE protocol_checkpoints;
DROP TABLE protocol_conversations;
