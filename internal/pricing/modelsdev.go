// Package pricing synchronizes the public models.dev catalog for non-billing
// cost estimates. Explicit configured prices always take precedence over this
// snapshot.
package pricing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/mostlygeek/llama-swap/internal/store"
)

const (
	maxPricingCatalogBytes int64 = 64 << 20
	// A catalog entry is copied into SQLite and into a temporary slice before
	// the transaction starts. Keep the number of records bounded independently
	// from the byte limit so a compact but extremely wide JSON object cannot
	// amplify memory or transaction time without bound.
	maxPricingEntries       = 200_000
	maxPricingIdentityBytes = 512
)

type Syncer struct {
	Store  *store.Store
	URL    string
	Client *http.Client
	ETag   string
	mu     sync.Mutex
}

type SyncResult struct {
	Changed  bool
	ETag     string
	Count    int
	SyncedAt time.Time
}

func (s *Syncer) Sync(ctx context.Context) (SyncResult, error) {
	if s == nil {
		return SyncResult{}, fmt.Errorf("pricing syncer is required")
	}
	// ETag is both an input to the request and the in-memory mirror of the
	// persisted metadata. Serialize a refresh so concurrent callers cannot send
	// stale validators or race while replacing the SQLite snapshot. The lock is
	// intentionally held across the network request: a later caller must see
	// the first caller's result before deciding whether a full catalog fetch is
	// necessary.
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	if s.Store == nil {
		return SyncResult{}, fmt.Errorf("pricing store is required")
	}
	endpoint := strings.TrimSpace(s.URL)
	if endpoint == "" {
		endpoint = "https://models.dev/api.json"
	}
	client := s.Client
	if client == nil {
		client = &http.Client{}
	} else {
		copyClient := *client
		client = &copyClient
	}
	// Catalog updates are a server-side fetch. Do not let a public catalog (or
	// a compromised custom mirror) redirect the request to another host or a
	// local metadata endpoint after the configured URL has been accepted.
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	if s.ETag == "" {
		meta, metaErr := s.Store.GetPricingMeta(ctx)
		if metaErr != nil {
			return SyncResult{}, fmt.Errorf("read pricing metadata: %w", metaErr)
		}
		s.ETag = meta.ETag
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return SyncResult{}, err
	}
	if s.ETag != "" {
		req.Header.Set("If-None-Match", s.ETag)
	}
	resp, err := client.Do(req)
	if err != nil {
		return SyncResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		now := time.Now()
		// A 304 is still a successful synchronization. Refresh the persisted
		// timestamp so a restart does not immediately retry a catalog that was
		// just validated by its ETag.
		if err := s.Store.SetPricingMeta(ctx, store.PricingMeta{ETag: s.ETag, SyncedAt: now, Source: endpoint}); err != nil {
			return SyncResult{}, fmt.Errorf("persist pricing metadata: %w", err)
		}
		return SyncResult{Changed: false, ETag: s.ETag, SyncedAt: now}, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return SyncResult{}, fmt.Errorf("models.dev returned %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPricingCatalogBytes+1))
	if err != nil {
		return SyncResult{}, err
	}
	if int64(len(body)) > maxPricingCatalogBytes {
		return SyncResult{}, fmt.Errorf("models.dev catalog exceeds %d bytes", maxPricingCatalogBytes)
	}
	var providers map[string]provider
	if err := json.Unmarshal(body, &providers); err != nil {
		return SyncResult{}, fmt.Errorf("decode models.dev catalog: %w", err)
	}
	if providers == nil {
		// JSON `null` is syntactically valid but is not a catalog. Treating it
		// as an empty map would atomically erase the last known-good prices and
		// make a transient/malformed mirror response look like a successful
		// refresh. Keep the previous snapshot instead.
		return SyncResult{}, fmt.Errorf("decode models.dev catalog: catalog must be an object")
	}
	now := time.Now()
	prices := make([]store.Price, 0, minInt(len(providers), maxPricingEntries))
	for providerID, provider := range providers {
		if !safePricingIdentity(providerID) {
			// A public catalog should never be able to inject invisible/control
			// characters into provider/model identifiers. Skip malformed records
			// instead of failing the whole refresh: the pricing contract already
			// treats an absent/ambiguous match as "unpriced" and the last valid
			// snapshot remains available when persistence fails.
			continue
		}
		for modelID, model := range provider.Models {
			if model.Cost == nil {
				continue
			}
			if !safePricingIdentity(modelID) {
				continue
			}
			if len(prices) >= maxPricingEntries {
				return SyncResult{}, fmt.Errorf("models.dev catalog contains more than %d priced models", maxPricingEntries)
			}
			prices = append(prices, store.Price{Provider: providerID, Model: modelID, Input: model.Cost.Input, Output: model.Cost.Output, CacheRead: model.Cost.CacheRead, CacheWrite: model.Cost.CacheWrite, Reasoning: model.Cost.Reasoning, SyncedAt: now})
		}
	}
	candidateETag := resp.Header.Get("ETag")
	if err := s.Store.ReplacePricingSnapshot(ctx, prices, store.PricingMeta{ETag: candidateETag, SyncedAt: now, Source: endpoint}); err != nil {
		return SyncResult{}, fmt.Errorf("persist pricing snapshot: %w", err)
	}
	s.ETag = candidateETag
	return SyncResult{Changed: true, ETag: candidateETag, Count: len(prices), SyncedAt: now}, nil
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

// safePricingIdentity validates catalog keys before they enter the persistent
// lookup table. ASCII spaces inside an identifier remain valid (some provider
// catalogs use display-like model names), while surrounding padding and
// Unicode whitespace/format/control characters are rejected to avoid visually
// colliding exact matches and contaminated UI/audit output.
func safePricingIdentity(value string) bool {
	if value == "" || strings.TrimSpace(value) != value || len(value) > maxPricingIdentityBytes {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || (unicode.IsSpace(r) && r != ' ') {
			return false
		}
	}
	return true
}

type provider struct {
	Models map[string]model `json:"models"`
}
type model struct {
	Cost *cost `json:"cost"`
}
type cost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	Reasoning  float64 `json:"reasoning"`
}
