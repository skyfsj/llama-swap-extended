-- +goose Up
-- Key rows recorded when a key was last used but not from where. An operator
-- auditing a credential needs the caller address: it answers "is this key
-- being used from a machine we recognize", and for an access key it is the
-- address its IP allowlist is checked against. The column is written by the
-- same buffered touch that records last_used_at, and reads as empty for rows
-- that predate it (or were never used).
ALTER TABLE api_keys ADD COLUMN last_ip TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE api_keys DROP COLUMN last_ip;
