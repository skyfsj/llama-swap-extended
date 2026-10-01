package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/pricing"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

const maxPricingPriceJSONBody = 64 << 10

type pricingPricesResponse struct {
	Data       []store.Price `json:"data"`
	Page       int           `json:"page"`
	Limit      int           `json:"limit"`
	Total      int           `json:"total"`
	TotalPages int           `json:"total_pages"`
	ETag       string        `json:"etag,omitempty"`
	SyncedAt   *time.Time    `json:"syncedAt,omitempty"`
	Source     string        `json:"source,omitempty"`
}

type pricingPriceRequest struct {
	Provider   string  `json:"provider"`
	Model      string  `json:"model"`
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	Reasoning  float64 `json:"reasoning"`
}

type pricingSyncResponse struct {
	Changed  bool      `json:"changed"`
	ETag     string    `json:"etag,omitempty"`
	Count    int       `json:"count"`
	SyncedAt time.Time `json:"syncedAt"`
}

// handleAPIPricingCatalog exposes a bounded projection of the locally synced
// models.dev catalog for configuration autocomplete. Rates remain internal to
// usage estimation; this endpoint returns identities only.
func (s *Server) handleAPIPricingCatalog(w http.ResponseWriter, r *http.Request) {
	provider := strings.TrimSpace(r.URL.Query().Get("provider"))
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	providers := []string{}
	models := []string{}
	if s.store != nil {
		var err error
		if provider == "" {
			providers, err = s.store.ListPricingProviders(r.Context(), query, limit)
		} else {
			models, err = s.store.ListPricingModels(r.Context(), provider, query, limit)
		}
		if err != nil {
			swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"providers": providers, "models": models})
}

// handleAPIPricingPrices exposes the editable, local pricing snapshot used by
// the configuration UI. It deliberately lives beside the autocomplete API so
// the latter can remain a small identity-only response for model editors.
func (s *Server) handleAPIPricingPrices(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleAPIListPricingPrices(w, r)
	case http.MethodPut:
		s.handleAPIUpsertPricingPrice(w, r)
	case http.MethodDelete:
		s.handleAPIDeletePricingPrice(w, r)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		swaputil.SendResponse(w, r, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleAPIListPricingPrices(w http.ResponseWriter, r *http.Request) {
	page := parsePositiveInt(r.URL.Query().Get("page"), 1)
	limit := parsePositiveInt(r.URL.Query().Get("limit"), 50)
	provider := strings.TrimSpace(r.URL.Query().Get("provider"))
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	result := store.PricingCatalogPage{Data: []store.Price{}, Page: page, Limit: limit}
	if s.store != nil {
		var err error
		result, err = s.store.ListPricingPrices(r.Context(), provider, query, page, limit)
		if err != nil {
			swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writePricingPricesResponse(w, r, result, s.pricingMeta(r))
}

func (s *Server) handleAPIUpsertPricingPrice(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		swaputil.SendResponse(w, r, http.StatusNotImplemented, "pricing store is not configured")
		return
	}
	var request pricingPriceRequest
	if err := decodeJSONBody(w, r, &request, maxPricingPriceJSONBody); err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "a pricing object is required")
		return
	}
	price := store.Price{
		Provider: strings.TrimSpace(request.Provider), Model: strings.TrimSpace(request.Model),
		Input: request.Input, Output: request.Output, CacheRead: request.CacheRead,
		CacheWrite: request.CacheWrite, Reasoning: request.Reasoning,
	}
	if err := s.store.UpsertPrice(r.Context(), price); err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	prices, err := s.store.FindPrices(r.Context(), price.Provider, price.Model)
	if err != nil || len(prices) == 0 {
		if err == nil {
			err = http.ErrNotSupported
		}
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(prices[0])
}

func (s *Server) handleAPIDeletePricingPrice(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		swaputil.SendResponse(w, r, http.StatusNotImplemented, "pricing store is not configured")
		return
	}
	provider := strings.TrimSpace(r.URL.Query().Get("provider"))
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	deleted, err := s.store.DeletePrice(r.Context(), provider, model)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if !deleted {
		swaputil.SendResponse(w, r, http.StatusNotFound, "pricing row not found")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"deleted": true})
}

// handleAPIPricingSync performs one on-demand refresh. When the periodic
// syncer has not been initialized (for example after toggling the setting at
// runtime), create a one-shot syncer from the effective config.
func (s *Server) handleAPIPricingSync(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		swaputil.SendResponse(w, r, http.StatusNotImplemented, "pricing store is not configured")
		return
	}
	settings := s.currentConfig().EffectivePricing().ModelsDev
	if !settings.Enabled {
		swaputil.SendResponse(w, r, http.StatusConflict, "pricing synchronization is disabled")
		return
	}
	syncer := s.pricing
	if syncer == nil || strings.TrimSpace(syncer.URL) != strings.TrimSpace(settings.URL) {
		syncer = &pricing.Syncer{Store: s.store, URL: settings.URL}
	}
	result, err := syncer.Sync(r.Context())
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(pricingSyncResponse{Changed: result.Changed, ETag: result.ETag, Count: result.Count, SyncedAt: result.SyncedAt})
}

func (s *Server) pricingMeta(r *http.Request) store.PricingMeta {
	if s.store == nil {
		return store.PricingMeta{}
	}
	meta, err := s.store.GetPricingMeta(r.Context())
	if err != nil {
		return store.PricingMeta{}
	}
	return meta
}

func writePricingPricesResponse(w http.ResponseWriter, r *http.Request, page store.PricingCatalogPage, meta store.PricingMeta) {
	var syncedAt *time.Time
	if !meta.SyncedAt.IsZero() {
		value := meta.SyncedAt
		syncedAt = &value
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(pricingPricesResponse{
		Data: page.Data, Page: page.Page, Limit: page.Limit, Total: page.Total, TotalPages: page.TotalPages,
		ETag: meta.ETag, SyncedAt: syncedAt, Source: meta.Source,
	})
}

func parsePositiveInt(value string, fallback int) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}
