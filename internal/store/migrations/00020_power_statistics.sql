-- +goose Up
CREATE TABLE power_samples (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    ts_start INTEGER NOT NULL,
    ts_end INTEGER NOT NULL,
    source TEXT NOT NULL,
    source_id TEXT NOT NULL,
    source_name TEXT NOT NULL DEFAULT '',
    avg_watts REAL NOT NULL DEFAULT 0,
    min_watts REAL NOT NULL DEFAULT -1,
    max_watts REAL NOT NULL DEFAULT -1,
    samples INTEGER NOT NULL DEFAULT 0,
    energy_wh REAL NOT NULL DEFAULT 0,
    price_per_kwh REAL NOT NULL DEFAULT 0,
    tariff_name TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_power_samples_source_ts ON power_samples (source, source_id, ts_start);

CREATE TABLE model_gpu_intervals (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    model_id TEXT NOT NULL,
    gpu_uuid TEXT NOT NULL,
    started_ts INTEGER NOT NULL,
    ended_ts INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_model_gpu_intervals_gpu ON model_gpu_intervals (gpu_uuid, started_ts);
CREATE INDEX idx_model_gpu_intervals_model ON model_gpu_intervals (model_id, started_ts);

-- +goose Down
DROP TABLE power_samples;
DROP TABLE model_gpu_intervals;
