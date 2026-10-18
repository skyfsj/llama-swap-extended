-- +goose Up
-- Activity rows recorded which model served a request but not which upstream
-- provider did. One model name can be exposed by several providers at once (a
-- bare-name pool, or several adapters on one provider), so a row reading
-- "deepseek-flash" could not answer where the request actually went — the
-- question an operator asks when a single provider starts failing or when a
-- bill has to be attributed.
--
-- provider_id holds the provider that produced the answer; provider_adapter
-- names its API surface when the provider has several (an Anthropic client and
-- an OpenAI client reach different adapters). Both are empty for a locally
-- hosted model, and for rows that predate the column.
ALTER TABLE activity ADD COLUMN provider_id TEXT NOT NULL DEFAULT '';
ALTER TABLE activity ADD COLUMN provider_adapter TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_activity_provider_created_id
    ON activity (provider_id, ts_created DESC, id DESC);

-- +goose Down
DROP INDEX idx_activity_provider_created_id;
ALTER TABLE activity DROP COLUMN provider_adapter;
ALTER TABLE activity DROP COLUMN provider_id;
