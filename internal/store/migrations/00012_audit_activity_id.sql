-- +goose Up
ALTER TABLE audit_conversations ADD COLUMN activity_id INTEGER NOT NULL DEFAULT 0;

-- Existing audit rows were written before the activity row id was part of the
-- audit record. Resolve them once while the old immutable request metrics are
-- still in the same database; runtime code never has to guess this relation.
UPDATE audit_conversations
SET activity_id = COALESCE((
    SELECT activity.id
    FROM activity
    WHERE activity.model_id = audit_conversations.model_id
      AND activity.key_id = audit_conversations.key_id
      AND activity.session_id = audit_conversations.session_id
      AND activity.req_path = audit_conversations.req_path
      AND activity.resp_status_code = audit_conversations.resp_status_code
      AND activity.input_tokens = audit_conversations.input_tokens
      AND activity.output_tokens = audit_conversations.output_tokens
      AND activity.cache_tokens = audit_conversations.cached_tokens
      AND activity.cache_creation_tokens = audit_conversations.cache_creation_tokens
    ORDER BY ABS(activity.ts_created - audit_conversations.ts_created), activity.id DESC
    LIMIT 1
), 0)
WHERE activity_id = 0;

CREATE INDEX idx_audit_activity_id ON audit_conversations (activity_id);

-- +goose Down
-- SQLite versions supported by llama-swap do not provide a portable DROP
-- COLUMN. The activity linkage is intentionally retained on downgrade.
