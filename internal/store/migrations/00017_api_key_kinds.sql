-- +goose Up
-- The API key surface is split into two kinds: management keys that sign in
-- to the control plane, and access keys nested under a management key that
-- carry the model-access restrictions (IP allowlist, concurrency ceiling,
-- expiry and an optional usage group). Legacy rows predate the split and are
-- full-management keys, which keeps every existing credential valid.
ALTER TABLE api_keys ADD COLUMN kind TEXT NOT NULL DEFAULT 'management';
ALTER TABLE api_keys ADD COLUMN parent_id TEXT NOT NULL DEFAULT '';
ALTER TABLE api_keys ADD COLUMN access_ips_json TEXT NOT NULL DEFAULT '[]';
ALTER TABLE api_keys ADD COLUMN max_concurrency INTEGER NOT NULL DEFAULT 0;
ALTER TABLE api_keys ADD COLUMN group_name TEXT NOT NULL DEFAULT '';

-- Activity rows keep the originating client address so the usage records page
-- can show and filter by caller IP, which is also the value the per-key IP
-- allowlist is checked against.
ALTER TABLE activity ADD COLUMN client_ip TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_api_keys_parent ON api_keys (parent_id, revoked_at);
CREATE INDEX idx_activity_req_path_created ON activity (req_path, ts_created DESC);

-- +goose Down
-- SQLite versions supported by llama-swap do not provide a portable DROP
-- COLUMN for the added fields. The migration is intentionally forward-only in
-- practice; the down section keeps goose's migration graph valid and leaves
-- the columns in place.
DROP INDEX idx_activity_req_path_created;
DROP INDEX idx_api_keys_parent;
