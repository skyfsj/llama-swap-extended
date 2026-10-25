-- +goose Up
-- The pricing cache looks a model up on its own, while the primary key starts
-- with the provider, so that lookup scans the whole catalog (thousands of rows
-- once models.dev has been synced) on every price-cache miss.
CREATE INDEX idx_pricing_catalog_model ON pricing_catalog (model);

-- +goose Down
DROP INDEX idx_pricing_catalog_model;
