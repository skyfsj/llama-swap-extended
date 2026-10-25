-- +goose Up
-- The statistics endpoints used to read every activity row into Go and fold it
-- there: /api/metrics/stats, /api/usage and /api/usage/analytics each walked the
-- whole table (analytics walked it six times, twice with a correlated subquery
-- per row). That is linear in the number of stored requests, so the pages got
-- slower the longer the server ran.
--
-- This table pre-aggregates the same numbers by hour and by the dimensions the
-- endpoints group on, so a read folds buckets instead of rows.
--
-- The primary key is deliberately narrow. session_id is NOT part of it: a session
-- holds a handful of requests, so keying on it produces roughly one bucket per
-- row and the "aggregate" costs more than the scan it replaces. Session-scoped
-- statistics are answered by the activity scan instead, which is the right trade
-- because they are rare and always bounded by one conversation's history.
CREATE TABLE activity_rollup (
    -- Hour bucket start, unix seconds.
    bucket_ts INTEGER NOT NULL,
    model_id TEXT NOT NULL,
    key_id TEXT NOT NULL,
    req_path TEXT NOT NULL DEFAULT '',
    requests INTEGER NOT NULL,
    input_tokens INTEGER NOT NULL,
    output_tokens INTEGER NOT NULL,
    cache_tokens INTEGER NOT NULL,
    cache_creation_tokens INTEGER NOT NULL,
    reasoning_tokens INTEGER NOT NULL,
    estimated_cost REAL NOT NULL DEFAULT 0,
    cost_estimated INTEGER NOT NULL DEFAULT 0,
    duration_ms_sum INTEGER NOT NULL DEFAULT 0,
    duration_count INTEGER NOT NULL DEFAULT 0,
    first_token_ms_sum INTEGER NOT NULL DEFAULT 0,
    first_token_count INTEGER NOT NULL DEFAULT 0,
    prompt_speed_sum REAL NOT NULL DEFAULT 0,
    prompt_speed_count INTEGER NOT NULL DEFAULT 0,
    gen_speed_sum REAL NOT NULL DEFAULT 0,
    gen_speed_count INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (bucket_ts, model_id, key_id, req_path)
);

-- The endpoints filter and group by one dimension at a time, so each gets a
-- leading-column index; the bucket keeps each provider's rows contiguous.
CREATE INDEX idx_activity_rollup_model ON activity_rollup (model_id, bucket_ts);
CREATE INDEX idx_activity_rollup_key ON activity_rollup (key_id, bucket_ts);
CREATE INDEX idx_activity_rollup_bucket ON activity_rollup (bucket_ts);

-- +goose Down
DROP INDEX idx_activity_rollup_bucket;
DROP INDEX idx_activity_rollup_key;
DROP INDEX idx_activity_rollup_model;
DROP TABLE activity_rollup;
