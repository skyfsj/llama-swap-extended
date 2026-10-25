-- +goose Up
-- The cost backfill walks activity rows that were recorded before a price was
-- available. A partial index over exactly those rows keeps that scan (and the
-- "is anything still unpriced" probe) cheap while the priced majority of the
-- table stays out of it; once the backfill has caught up the index is nearly
-- empty and costs nothing to maintain.
CREATE INDEX idx_activity_unpriced ON activity (id) WHERE cost_estimated = 0;

-- +goose Down
DROP INDEX idx_activity_unpriced;
