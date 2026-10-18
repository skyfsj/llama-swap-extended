-- +goose Up
-- An activity row recorded which provider served a request, but an empty
-- provider_id had two meanings: the request ran on a locally hosted model, or
-- no upstream ever produced an answer (a provider request that failed before a
-- route answered, e.g. every credential cooling down). The UI read both as
-- "local", which is wrong for the second — an operator debugging a provider
-- outage saw their failing rows labelled as if they had run on this host.
--
-- provider_local marks the first case explicitly, so a reader can tell the
-- three states apart: local, provider-attributed, and unattributed.
ALTER TABLE activity ADD COLUMN provider_local INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE activity DROP COLUMN provider_local;
