-- +goose Up
CREATE TABLE api_keys (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL DEFAULT '',
    key_hash TEXT NOT NULL UNIQUE,
    scopes_json TEXT NOT NULL DEFAULT '[]',
    models_json TEXT NOT NULL DEFAULT '[]',
    expires_at INTEGER,
    created_at INTEGER NOT NULL,
    last_used_at INTEGER,
    revoked_at INTEGER
);

CREATE INDEX idx_api_keys_active ON api_keys (revoked_at, expires_at);

CREATE TABLE audit_conversations (
    id TEXT PRIMARY KEY,
    request_id TEXT NOT NULL DEFAULT '',
    key_id TEXT NOT NULL DEFAULT '',
    model_id TEXT NOT NULL DEFAULT '',
    session_id TEXT NOT NULL DEFAULT '',
    req_path TEXT NOT NULL DEFAULT '',
    ts_created INTEGER NOT NULL,
    req_headers_json TEXT NOT NULL DEFAULT '{}',
    req_body BLOB NOT NULL DEFAULT X'',
    resp_headers_json TEXT NOT NULL DEFAULT '{}',
    resp_body BLOB NOT NULL DEFAULT X'',
    resp_status_code INTEGER NOT NULL DEFAULT 0,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cached_tokens INTEGER NOT NULL DEFAULT 0,
    cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
    estimated_cost REAL NOT NULL DEFAULT 0,
    complete INTEGER NOT NULL DEFAULT 0,
    size_bytes INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_audit_created ON audit_conversations (ts_created DESC, id DESC);
CREATE INDEX idx_audit_key_created ON audit_conversations (key_id, ts_created DESC, id DESC);
CREATE INDEX idx_audit_model_created ON audit_conversations (model_id, ts_created DESC, id DESC);

CREATE TABLE response_affinity (
    response_id TEXT PRIMARY KEY,
    model_id TEXT NOT NULL,
    backend TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT '',
    response_json BLOB NOT NULL DEFAULT X'',
    expires_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE INDEX idx_response_affinity_expiry ON response_affinity (expires_at);

CREATE TABLE runtime_operations (
    id TEXT PRIMARY KEY,
    runtime_name TEXT NOT NULL,
    state TEXT NOT NULL,
    version TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    started_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE pricing_catalog (
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    input_per_million REAL,
    output_per_million REAL,
    cache_read_per_million REAL,
    cache_write_per_million REAL,
    reasoning_per_million REAL,
    synced_at INTEGER NOT NULL,
    PRIMARY KEY (provider, model)
);

CREATE TABLE pricing_meta (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    etag TEXT NOT NULL DEFAULT '',
    synced_at INTEGER NOT NULL DEFAULT 0,
    source TEXT NOT NULL DEFAULT ''
);

-- +goose Down
DROP TABLE pricing_meta;
DROP TABLE pricing_catalog;
DROP TABLE runtime_operations;
DROP TABLE response_affinity;
DROP TABLE audit_conversations;
DROP TABLE api_keys;
