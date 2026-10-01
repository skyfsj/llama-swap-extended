package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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
