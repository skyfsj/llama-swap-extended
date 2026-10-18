-- Scope stored Responses to the credential that created them. An empty value
-- means a row written before this migration; the API treats those as
-- model-scoped (the previous behaviour) so upgrades do not strand them.
-- +goose Up
ALTER TABLE response_affinity ADD COLUMN identity_id TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_response_affinity_identity ON response_affinity (identity_id);

-- +goose Down
DROP INDEX idx_response_affinity_identity;
ALTER TABLE response_affinity DROP COLUMN identity_id;
