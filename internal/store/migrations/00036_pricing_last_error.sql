-- +goose Up
ALTER TABLE pricing_meta ADD COLUMN last_error TEXT NOT NULL DEFAULT '';
-- +goose Down
ALTER TABLE pricing_meta DROP COLUMN last_error;
