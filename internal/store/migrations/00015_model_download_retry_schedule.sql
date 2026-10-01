-- +goose Up
-- A retrying task keeps its resumable .part files and waits in SQLite instead
-- of becoming terminal after one exhausted request-level retry budget.
ALTER TABLE model_downloads ADD COLUMN next_retry_at INTEGER NOT NULL DEFAULT 0;
CREATE INDEX idx_model_downloads_retry_schedule
    ON model_downloads (status, next_retry_at, created_at, id);

-- +goose Down
DROP INDEX idx_model_downloads_retry_schedule;
-- SQLite cannot portably drop a column. Retaining next_retry_at keeps
-- existing task records readable when downgrading.
