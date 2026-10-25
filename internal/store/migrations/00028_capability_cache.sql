-- +goose Up
-- Discovered model capabilities are only readable while the model's upstream is
-- running, so they are cached: the value a probe learned at model start has to
-- survive an unload and a restart of llama-swap, otherwise /v1/models would
-- stop advertising a context length and modalities the model does have.
--
-- The table is deliberately a general key/value blob store rather than a
-- capability-shaped one: callers own the key namespace and the encoding, so a
-- second consumer can reuse the same table, the same expiry semantics and the
-- same prune path instead of adding another near-identical table.
CREATE TABLE cache_entries (
    key TEXT PRIMARY KEY,
    data BLOB NOT NULL,
    -- expires_at is a unix timestamp; 0 means the entry never expires, which
    -- suits data rewritten on a known event rather than aged out.
    expires_at INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL
);

-- Prune reads by expiry, so the index keeps it off a full table scan.
CREATE INDEX idx_cache_entries_expires_at
    ON cache_entries (expires_at);

-- +goose Down
DROP TABLE cache_entries;
