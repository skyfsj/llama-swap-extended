-- +goose Up
-- Per-request phase telemetry: the decode window (last minus first visible
-- token) and a bounded (time, cumulative tokens) timeline sampled from the
-- streamed response, powering the per-request speed curve and phase share
-- shown in the activity detail view. -1/'' mean "not measured".
ALTER TABLE activity ADD COLUMN decode_ms INTEGER NOT NULL DEFAULT -1;
ALTER TABLE activity ADD COLUMN speed_timeline TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE activity DROP COLUMN decode_ms;
ALTER TABLE activity DROP COLUMN speed_timeline;
