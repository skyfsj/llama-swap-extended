-- +goose Up
CREATE TABLE extension_pending (
    id TEXT PRIMARY KEY,
    identity_id TEXT NOT NULL,
    model_id TEXT NOT NULL,
    profile TEXT NOT NULL,
    endpoint TEXT NOT NULL,
    payload BLOB NOT NULL,
    expires_at INTEGER NOT NULL,
    claimed_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE extension_pending_calls (
    call_id TEXT PRIMARY KEY,
    pending_id TEXT NOT NULL
);
CREATE INDEX idx_extension_pending_expires ON extension_pending (expires_at);
CREATE INDEX idx_extension_pending_calls_pending ON extension_pending_calls (pending_id);

-- +goose Down
DROP TABLE extension_pending_calls;
DROP TABLE extension_pending;
