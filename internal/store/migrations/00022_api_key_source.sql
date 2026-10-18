-- +goose Up
-- api_keys rows can be seeded from the configuration file's apiKeys section or
-- created through the control plane. The marker is what lets a later seed pass
-- revoke the rows the file no longer defines (removing a key from the file must
-- take effect immediately) without ever touching an operator-created key.
ALTER TABLE api_keys ADD COLUMN source TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_api_keys_source ON api_keys (source);

-- Best-effort backfill for rows seeded before the marker existed. The seeder is
-- the only writer that names a key "Configured API key" for a scalar entry or
-- ids one "startup-<hash>" for a structured entry without an explicit id, and
-- its rows are always unrestricted management keys.
UPDATE api_keys
   SET source = 'config'
 WHERE source = ''
   AND kind = 'management'
   AND allow_management_login = 1
   AND (parent_id IS NULL OR parent_id = '')
   AND (id LIKE 'startup-%' OR name = 'Configured API key');

-- +goose Down
DROP INDEX idx_api_keys_source;
ALTER TABLE api_keys DROP COLUMN source;
