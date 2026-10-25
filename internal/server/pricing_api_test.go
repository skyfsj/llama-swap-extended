package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/store"
)

func TestServer_PricingCatalogAutocomplete(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })
	for _, price := range []store.Price{
		{Provider: "openai", Model: "gpt-5"},
		{Provider: "openai", Model: "gpt-5-mini"},
		{Provider: "anthropic", Model: "claude-sonnet"},
	} {
		if err := s.store.UpsertPrice(context.Background(), price); err != nil {
			t.Fatal(err)
		}
	}

	request := httptest.NewRequest(http.MethodGet, "/api/pricing/catalog?q=ai", nil)
	response := httptest.NewRecorder()
	s.handleAPIPricingCatalog(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var providers struct {
		Providers []string `json:"providers"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &providers); err != nil {
		t.Fatal(err)
	}
	if len(providers.Providers) != 1 || providers.Providers[0] != "openai" {
		t.Fatalf("providers=%v", providers.Providers)
	}

	request = httptest.NewRequest(http.MethodGet, "/api/pricing/catalog?provider=openai&q=mini", nil)
	response = httptest.NewRecorder()
	s.handleAPIPricingCatalog(response, request)
	var models struct {
		Models []string `json:"models"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &models); err != nil {
		t.Fatal(err)
	}
	if len(models.Models) != 1 || models.Models[0] != "gpt-5-mini" {
		t.Fatalf("models=%v", models.Models)
	}
}

func TestServer_PricingRowsCRUD(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })

	body, err := json.Marshal(map[string]any{
		"provider": "openai", "model": "gpt-5", "input": 1.25, "output": 5,
		"cacheRead": 0.1, "cacheWrite": 0.2, "reasoning": 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPut, "/api/pricing/prices", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	s.handleAPIPricingPrices(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("put status=%d body=%s", response.Code, response.Body.String())
	}
	var saved store.Price
	if err := json.Unmarshal(response.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Provider != "openai" || saved.Model != "gpt-5" || saved.Input != 1.25 {
		t.Fatalf("saved=%+v", saved)
	}

	request = httptest.NewRequest(http.MethodGet, "/api/pricing/prices?q=gpt&limit=10", nil)
	response = httptest.NewRecorder()
	s.handleAPIPricingPrices(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", response.Code, response.Body.String())
	}
	var page struct {
		Data  []store.Price
		Total int
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Data) != 1 || page.Data[0].Model != "gpt-5" {
		t.Fatalf("page=%+v", page)
	}

	request = httptest.NewRequest(http.MethodDelete, "/api/pricing/prices?provider=openai&model=gpt-5", nil)
	response = httptest.NewRecorder()
	s.handleAPIPricingPrices(response, request)
	var deleted map[string]bool
	if err := json.Unmarshal(response.Body.Bytes(), &deleted); err != nil || !deleted["deleted"] || response.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", response.Code, response.Body.String())
	}
}

// TestServer_PricingReconfigureOnConfigChange pins that a models.dev edit takes
// effect on the running server. The syncer and its loop used to be created once
// at startup and never touched again, so enabling the section, changing the
// interval, or pointing at another mirror all required a daemon restart — and
// toggling the switch on in the UI left the periodic loop absent entirely.
func TestServer_PricingReconfigureOnConfigChange(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })

	disabled := config.ModelsDevPricingConfig{}
	enabled := false
	disabled.Enabled = &enabled
	if syncer, _ := s.currentPricingSyncer(); syncer != nil {
		t.Fatal("a fresh server must not have a syncer")
	}

	on := config.ModelsDevPricingConfig{URL: "https://models.dev/api.json", RefreshEvery: time.Hour}
	s.applyPricingConfig(disabled, on)
	syncer, url := s.currentPricingSyncer()
	if syncer == nil {
		t.Fatal("enabling modelsDev did not install a syncer")
	}
	if url != on.URL {
		t.Errorf("syncer URL=%q want %q", url, on.URL)
	}

	// The same source with the same interval must keep the syncer: replacing it
	// would throw away the ETag and force a full refetch.
	s.applyPricingConfig(on, on)
	same, _ := s.currentPricingSyncer()
	if same != syncer {
		t.Error("an unchanged configuration replaced the syncer")
	}

	// A new interval restarts the loop without dropping the syncer.
	faster := on
	faster.RefreshEvery = time.Minute
	s.applyPricingConfig(on, faster)
	kept, _ := s.currentPricingSyncer()
	if kept != syncer {
		t.Error("changing refreshEvery replaced the syncer instead of its loop")
	}

	// A new source is a new syncer.
	mirror := on
	mirror.URL = "https://mirror.example/api.json"
	s.applyPricingConfig(faster, mirror)
	moved, movedURL := s.currentPricingSyncer()
	if moved == nil || moved == syncer {
		t.Fatal("changing the source did not install a new syncer")
	}
	if movedURL != mirror.URL {
		t.Errorf("syncer URL=%q want %q", movedURL, mirror.URL)
	}

	// Disabling stops the loop and drops the syncer.
	off := mirror
	off.Enabled = &enabled
	s.applyPricingConfig(mirror, off)
	if syncer, _ := s.currentPricingSyncer(); syncer != nil {
		t.Error("disabling modelsDev left a syncer behind")
	}
}

// TestServer_PricingManualSyncDoesNotNeedAWritableConfig pins that overriding a
// price works when the YAML is read-only. Prices live in SQLite; the old UI
// gated every catalog control on snapshot.writable, which locked operators out
// of the only place they could correct a rate.
func TestServer_PricingManualSyncDoesNotNeedAWritableConfig(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })

	request := httptest.NewRequest(http.MethodPut, "/api/pricing/prices", bytes.NewReader([]byte(
		`{"provider":"openai","model":"gpt-5","input":3.5,"output":12}`)))
	response := httptest.NewRecorder()
	s.handleAPIPricingPrices(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}

	rows, err := s.store.FindPrices(context.Background(), "openai", "gpt-5")
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	if rows[0].Input != 3.5 || rows[0].Output != 12 {
		t.Fatalf("stored rates=%+v want the operator's values", rows[0])
	}

	// Deleting works the same way, and the ETag has to go with the row so the
	// next refresh does not answer 304 and restore it.
	if err := s.store.ReplacePricingSnapshot(context.Background(), []store.Price{
		{Provider: "openai", Model: "gpt-5", Input: 1.25, Output: 10},
	}, store.PricingMeta{ETag: `"v1"`}); err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodDelete, "/api/pricing/prices?provider=openai&model=gpt-5", nil)
	response = httptest.NewRecorder()
	s.handleAPIPricingPrices(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", response.Code, response.Body.String())
	}
	meta, err := s.store.GetPricingMeta(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if meta.ETag != "" {
		t.Errorf("ETag=%q want empty after a local delete", meta.ETag)
	}
}

// TestServer_PricingListSurfacesLastError pins that a failed refresh is visible
// in the listing the panel already reads, rather than only in the daemon log.
func TestServer_PricingListSurfacesLastError(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })

	if err := s.store.SetPricingMeta(context.Background(), store.PricingMeta{
		ETag: `"v1"`, Source: "https://models.dev/api.json", LastError: "models.dev returned 502",
	}); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/pricing/prices", nil)
	response := httptest.NewRecorder()
	s.handleAPIPricingPrices(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		LastError string `json:"lastError"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.LastError != "models.dev returned 502" {
		t.Fatalf("lastError=%q want the recorded failure", payload.LastError)
	}
}
