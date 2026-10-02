-- +goose Up
-- The flusher can overlap windows across restarts (the cursor is in-memory),
-- so sample rows must be idempotent per source and window start.
DROP INDEX IF EXISTS idx_power_samples_unique;
CREATE UNIQUE INDEX idx_power_samples_unique ON power_samples (source, source_id, ts_start);

-- One open occupancy interval per (model, card): a crashed process leaves
-- ended_ts=0 rows behind, and the startup sweep closes them, but a duplicate
-- open must never silently double the attribution coverage.
DROP INDEX IF EXISTS idx_model_gpu_intervals_open_unique;
CREATE UNIQUE INDEX idx_model_gpu_intervals_open_unique ON model_gpu_intervals (model_id, gpu_uuid) WHERE ended_ts = 0;

-- +goose Down
DROP INDEX IF EXISTS idx_power_samples_unique;
DROP INDEX IF EXISTS idx_model_gpu_intervals_open_unique;
