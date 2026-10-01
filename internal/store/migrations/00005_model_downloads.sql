-- +goose Up
CREATE TABLE model_downloads (
    id TEXT PRIMARY KEY,
    repo_id TEXT NOT NULL,
    revision TEXT NOT NULL DEFAULT 'main',
    source_id TEXT NOT NULL,
    include_json TEXT NOT NULL DEFAULT '[]',
    exclude_json TEXT NOT NULL DEFAULT '[]',
    status TEXT NOT NULL,
    current_file TEXT NOT NULL DEFAULT '',
    total_files INTEGER NOT NULL DEFAULT 0,
    completed_files INTEGER NOT NULL DEFAULT 0,
    total_bytes INTEGER NOT NULL DEFAULT 0,
    downloaded_bytes INTEGER NOT NULL DEFAULT 0,
    attempts INTEGER NOT NULL DEFAULT 0,
    error TEXT NOT NULL DEFAULT '',
    worker_id TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    started_at INTEGER,
    finished_at INTEGER
);

CREATE INDEX idx_model_downloads_queue
    ON model_downloads (status, created_at, id);

CREATE INDEX idx_model_downloads_updated
    ON model_downloads (status, updated_at);

-- +goose Down
DROP INDEX idx_model_downloads_updated;
DROP INDEX idx_model_downloads_queue;
DROP TABLE model_downloads;
