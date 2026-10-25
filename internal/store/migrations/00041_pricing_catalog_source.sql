-- +goose Up
-- Rows the operator created or corrected by hand must survive every catalog
-- refresh: locally hosted models never appear in models.dev, so a manual row
-- is their only source of a price, and a daily sync that rebuilt the table
-- wholesale erased such an edit within a day. 'catalog' rows are owned by the
-- syncer and replaced wholesale; 'operator' rows are never touched by a
-- refresh and win over a catalog row carrying the same identity.
ALTER TABLE pricing_catalog ADD COLUMN source TEXT NOT NULL DEFAULT 'catalog';

-- +goose Down
ALTER TABLE pricing_catalog DROP COLUMN source;
