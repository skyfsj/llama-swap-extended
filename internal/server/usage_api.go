package server

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// parseUsageQuery reads the shared filter selection used by every usage
// endpoint. List-valued parameters may repeat, which lets the dashboard send
// several keys or models in one request.
func parseUsageQuery(r *http.Request) (store.UsageQuery, string, error) {
	query := r.URL.Query()
	usage := store.UsageQuery{
		Keys:      repeatValues(query["key"]),
		Models:    repeatValues(query["model"]),
		Groups:    repeatValues(query["group"]),
		Endpoints: repeatValues(query["endpoint"]),
		Sort:      strings.TrimSpace(query.Get("sort")),
		Order:     strings.TrimSpace(query.Get("order")),
	}
	granularity := strings.TrimSpace(query.Get("granularity"))
	if granularity == "" {
		granularity = store.UsageGranularityDay
	}
	// Reject an unknown bucket size here rather than letting it become a 500
	// from the store.
	switch granularity {
	case store.UsageGranularityMinute, store.UsageGranularityHour, store.UsageGranularityDay:
	default:
		return store.UsageQuery{}, "", fmt.Errorf("unsupported usage granularity: %s", granularity)
	}
	if raw := strings.TrimSpace(query.Get("start")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return store.UsageQuery{}, "", fmt.Errorf("invalid start timestamp, use RFC3339 format")
		}
		usage.Start = parsed
	}
	if raw := strings.TrimSpace(query.Get("end")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return store.UsageQuery{}, "", fmt.Errorf("invalid end timestamp, use RFC3339 format")
		}
		usage.End = parsed
	}
	if !usage.Start.IsZero() && !usage.End.IsZero() && usage.Start.After(usage.End) {
		return store.UsageQuery{}, "", fmt.Errorf("start timestamp must not be after end timestamp")
	}
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit <= 0 {
			return store.UsageQuery{}, "", fmt.Errorf("invalid limit")
		}
		usage.Limit = limit
	}
	if raw := strings.TrimSpace(query.Get("offset")); raw != "" {
		offset, err := strconv.Atoi(raw)
		if err != nil || offset < 0 {
			return store.UsageQuery{}, "", fmt.Errorf("invalid offset")
		}
		usage.Offset = offset
	}
	return usage, granularity, nil
}

func repeatValues(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				out = append(out, trimmed)
			}
		}
	}
	return out
}

// authorizeUsageQuery applies the same tenancy rules as the activity and audit
// endpoints: a key-scoped credential may only read its own traffic, and a
// model-scoped credential may only read its own models. It returns the query
// with the model filter narrowed to what the identity may see, reports whether
// the answer must be empty, and returns an error when the request itself is a
// scope violation.
func authorizeUsageQuery(cfg config.Config, identity auth.Identity, query store.UsageQuery) (store.UsageQuery, bool, error) {
	for _, keyID := range query.Keys {
		if !keyUsageAllowedForIdentity(identity, keyID) {
			return query, true, nil
		}
	}
	if identity.Legacy || len(identity.Models) == 0 {
		return query, false, nil
	}
	allowed, restricted := allowedCanonicalModels(cfg, identity)
	if !restricted {
		return query, false, nil
	}
	if len(query.Models) == 0 {
		query.Models = allowed
		return query, len(allowed) == 0, nil
	}
	// Every requested model must be inside the identity's allowlist; otherwise
	// the request is a scope violation, not an empty selection.
	for _, model := range query.Models {
		if !modelAllowedForIdentity(cfg, identity, model) {
			return query, false, fmt.Errorf("forbidden: API key is not permitted for model %s", model)
		}
	}
	return query, false, nil
}

// handleAPIUsageAnalytics serves the whole usage-records dashboard payload:
// summary cards, every distribution and the bucketed trend series.
func (s *Server) handleAPIUsageAnalytics(w http.ResponseWriter, r *http.Request) {
	query, granularity, err := parseUsageQuery(r)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	identity := identityFromContext(r.Context())
	query, empty, authErr := authorizeUsageQuery(s.currentConfig(), identity, query)
	if authErr != nil {
		swaputil.SendResponse(w, r, http.StatusForbidden, authErr.Error())
		return
	}
	if empty {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(emptyUsageAnalytics())
		return
	}
	analytics, err := s.store.UsageAnalytics(r.Context(), query, granularity)
	if err != nil {
		s.proxylog.Errorf("usage analytics: %v", err)
		swaputil.SendResponse(w, r, http.StatusInternalServerError, "failed to get usage analytics")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(analytics)
}

// handleAPIUsageOptions lists the distinct filter values available to the
// dashboard, so its selects never offer a value that matches no row.
func (s *Server) handleAPIUsageOptions(w http.ResponseWriter, r *http.Request) {
	query, _, err := parseUsageQuery(r)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	identity := identityFromContext(r.Context())
	query, empty, authErr := authorizeUsageQuery(s.currentConfig(), identity, query)
	if authErr != nil {
		swaputil.SendResponse(w, r, http.StatusForbidden, authErr.Error())
		return
	}
	if empty {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(store.UsageFilterOptions{})
		return
	}
	options, err := s.store.UsageFilterOptions(r.Context(), query)
	if err != nil {
		s.proxylog.Errorf("usage filter options: %v", err)
		swaputil.SendResponse(w, r, http.StatusInternalServerError, "failed to get usage filter options")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(options)
}

// handleAPIUsageRecords serves one page of the usage detail table.
func (s *Server) handleAPIUsageRecords(w http.ResponseWriter, r *http.Request) {
	query, _, err := parseUsageQuery(r)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	identity := identityFromContext(r.Context())
	query, empty, authErr := authorizeUsageQuery(s.currentConfig(), identity, query)
	if authErr != nil {
		swaputil.SendResponse(w, r, http.StatusForbidden, authErr.Error())
		return
	}
	if empty {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(store.UsageRecordPage{Data: []store.UsageRecord{}, Limit: clampUsageLimit(query.Limit)})
		return
	}
	page, err := s.store.UsageRecords(r.Context(), query)
	if err != nil {
		s.proxylog.Errorf("usage records: %v", err)
		swaputil.SendResponse(w, r, http.StatusInternalServerError, "failed to get usage records")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(page)
}

// handleAPIUsageExport streams the same filtered selection as CSV. The export
// is deliberately unbounded by the table's page size: it exists precisely to
// move a full selection out of the product, and the write is streamed so the
// payload is never held in memory twice.
func (s *Server) handleAPIUsageExport(w http.ResponseWriter, r *http.Request) {
	query, _, err := parseUsageQuery(r)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	identity := identityFromContext(r.Context())
	query, empty, authErr := authorizeUsageQuery(s.currentConfig(), identity, query)
	if authErr != nil {
		swaputil.SendResponse(w, r, http.StatusForbidden, authErr.Error())
		return
	}
	// The CSV headers go out before any row, including the empty selection, so
	// the browser always treats the answer as a download.
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="usage-records.csv"`)
	if empty {
		writeUsageCSVHeader(w)
		return
	}
	const exportBatch = 500
	total := 0
	firstBatch := true
	for {
		// The store caps a single page, so walk the selection in batches and
		// write each one before fetching the next.
		batch := query
		batch.Limit = exportBatch
		batch.Offset = total
		batch.Sort, batch.Order = "time", "asc"
		page, err := s.store.UsageRecords(r.Context(), batch)
		if err != nil {
			s.proxylog.Errorf("usage export: %v", err)
			return
		}
		if firstBatch {
			writeUsageCSVHeader(w)
			firstBatch = false
		}
		for _, record := range page.Data {
			writeUsageCSVRow(w, record)
		}
		total += len(page.Data)
		if len(page.Data) < exportBatch || total >= page.Total {
			break
		}
	}
}

func writeUsageCSVHeader(w http.ResponseWriter) {
	writer := csv.NewWriter(w)
	_ = writer.Write([]string{
		"id", "time", "model", "key", "key_id", "key_kind", "group", "endpoint",
		"client_ip", "status", "input_tokens", "output_tokens", "cached_tokens",
		"cache_creation_tokens", "reasoning_tokens", "estimated_cost", "duration_ms",
	})
	writer.Flush()
}

func writeUsageCSVRow(w http.ResponseWriter, record store.UsageRecord) {
	writer := csv.NewWriter(w)
	_ = writer.Write([]string{
		strconv.Itoa(record.ID),
		record.Timestamp.Format(time.RFC3339),
		record.Model,
		record.KeyName,
		record.KeyID,
		record.KeyKind,
		record.KeyGroup,
		record.Endpoint,
		record.ClientIP,
		strconv.Itoa(record.RespStatusCode),
		strconv.Itoa(record.InputTokens),
		strconv.Itoa(record.OutputTokens),
		strconv.Itoa(record.CachedTokens),
		strconv.Itoa(record.CacheCreationTokens),
		strconv.Itoa(record.ReasoningTokens),
		strconv.FormatFloat(record.EstimatedCost, 'f', -1, 64),
		strconv.Itoa(record.DurationMs),
	})
	writer.Flush()
}

func clampUsageLimit(limit int) int {
	if limit <= 0 {
		return 100
	}
	if limit > 1000 {
		return 1000
	}
	return limit
}

func emptyUsageAnalytics() store.UsageAnalytics {
	return store.UsageAnalytics{
		Totals:     store.UsageTotals{},
		ByModel:    []store.UsageDimensionRow{},
		ByGroup:    []store.UsageDimensionRow{},
		ByEndpoint: []store.UsageDimensionRow{},
		ByKey:      []store.UsageDimensionRow{},
		Trend:      []store.UsageTrendBucket{},
	}
}
