-- +goose Up
CREATE TABLE extension_kv (
    extension_id TEXT NOT NULL,
    scope TEXT NOT NULL,
    key TEXT NOT NULL,
    value BLOB NOT NULL,
    expires_at INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (extension_id, scope, key)
);
CREATE INDEX idx_extension_kv_expires ON extension_kv (expires_at);

-- +goose Down
DROP INDEX idx_extension_kv_expires;
DROP TABLE extension_kv;
