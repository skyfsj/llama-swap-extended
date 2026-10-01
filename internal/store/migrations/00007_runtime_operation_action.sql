-- +goose Up
-- Keep the operation action in the SQLite projection as well as the runtime
-- JSONL journal. Existing rows are valid with an empty action and are filled
-- by the next state transition.
ALTER TABLE runtime_operations ADD COLUMN action TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_runtime_operations_updated ON runtime_operations (runtime_name, updated_at DESC, id DESC);

-- +goose Down
-- SQLite versions supported by llama-swap do not provide a portable DROP
-- COLUMN. The action column is intentionally retained on downgrade.
