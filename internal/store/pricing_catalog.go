package store

import (
	"context"
	"errors"
	"strings"
	"time"
)

const defaultPricingCatalogLimit = 100
const maxPricingCatalogLimit = 200

func normalizePricingCatalogLimit(limit int) int {
	if limit <= 0 {
		return defaultPricingCatalogLimit
	}
	if limit > maxPricingCatalogLimit {
		return maxPricingCatalogLimit
	}
	return limit
}

func pricingCatalogPattern(query string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + replacer.Replace(strings.TrimSpace(query)) + "%"
}

// PricingCatalogPage is the bounded projection consumed by the pricing
// management UI. The catalog can contain thousands of models, so callers
// should use page/limit rather than loading the complete snapshot.
type PricingCatalogPage struct {
	Data       []Price `json:"data"`
	Page       int     `json:"page"`
	Limit      int     `json:"limit"`
	Total      int     `json:"total"`
	TotalPages int     `json:"total_pages"`
}

// ListPricingPrices returns editable pricing rows from the local catalog. A
// query matches either provider or model, while provider narrows the result
// to one exact provider. Rates remain in the response because this endpoint
// is for the authenticated configuration UI, unlike autocomplete below.
func (s *Store) ListPricingPrices(ctx context.Context, provider, query string, page, limit int) (PricingCatalogPage, error) {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return PricingCatalogPage{}, err
	}
	if page <= 0 {
		page = 1
	}
	limit = normalizePricingCatalogLimit(limit)
	provider = strings.TrimSpace(provider)
	query = strings.TrimSpace(query)

	where := "1=1"
	args := make([]any, 0, 3)
	if provider != "" {
		where += " AND provider=?"
		args = append(args, provider)
	}
	if query != "" {
		where += " AND (provider LIKE ? ESCAPE '\\' OR model LIKE ? ESCAPE '\\')"
		pattern := pricingCatalogPattern(query)
		args = append(args, pattern, pattern)
	}

	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM pricing_catalog WHERE "+where, args...).Scan(&total); err != nil {
		return PricingCatalogPage{}, err
	}
	totalPages := calculateTotalPages(total, limit)
	if totalPages > 0 && page > totalPages {
		page = totalPages
	}
	offset := (page - 1) * limit
	rows, err := s.db.QueryContext(ctx, `SELECT provider,model,input_per_million,output_per_million,cache_read_per_million,cache_write_per_million,reasoning_per_million,synced_at
		FROM pricing_catalog WHERE `+where+` ORDER BY model COLLATE NOCASE,provider COLLATE NOCASE LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return PricingCatalogPage{}, err
	}
	defer rows.Close()
	data := make([]Price, 0, limit)
	for rows.Next() {
		var price Price
		var syncedAt int64
		if err := rows.Scan(&price.Provider, &price.Model, &price.Input, &price.Output, &price.CacheRead, &price.CacheWrite, &price.Reasoning, &syncedAt); err != nil {
			return PricingCatalogPage{}, err
		}
		price.SyncedAt = time.Unix(syncedAt, 0)
		data = append(data, price)
	}
	if err := rows.Err(); err != nil {
		return PricingCatalogPage{}, err
	}
	return PricingCatalogPage{Data: data, Page: page, Limit: limit, Total: total, TotalPages: totalPages}, nil
}

// DeletePrice removes one exact provider/model row and reports whether it
// existed. Keeping the operation exact avoids accidentally deleting every
// provider's price when model IDs overlap.
func (s *Store) DeletePrice(ctx context.Context, provider, model string) (bool, error) {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return false, err
	}
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if provider == "" {
		return false, errors.New("price provider is required")
	}
	if err := validatePrice(Price{Provider: provider, Model: model}); err != nil {
		return false, err
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM pricing_catalog WHERE provider=? AND model=?`, provider, model)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count > 0, err
}

// ListPricingProviders returns provider IDs from the locally synchronized
// models.dev snapshot. Results are bounded so control-plane autocomplete never
// needs to load the full catalog into memory.
func (s *Store) ListPricingProviders(ctx context.Context, query string, limit int) ([]string, error) {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT provider FROM pricing_catalog WHERE provider LIKE ? ESCAPE '\' ORDER BY provider LIMIT ?`, pricingCatalogPattern(query), normalizePricingCatalogLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	providers := make([]string, 0)
	for rows.Next() {
		var provider string
		if err := rows.Scan(&provider); err != nil {
			return nil, err
		}
		providers = append(providers, provider)
	}
	return providers, rows.Err()
}

// ListPricingModels returns model IDs for one exact provider in the locally
// synchronized models.dev snapshot.
func (s *Store) ListPricingModels(ctx context.Context, provider, query string, limit int) ([]string, error) {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return []string{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT model FROM pricing_catalog WHERE provider=? AND model LIKE ? ESCAPE '\' ORDER BY model LIMIT ?`, provider, pricingCatalogPattern(query), normalizePricingCatalogLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	models := make([]string, 0)
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			return nil, err
		}
		models = append(models, model)
	}
	return models, rows.Err()
}
