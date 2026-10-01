package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/store"
)

func TestServer_UsageAnalyticsAndRecords(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })
	if err := s.store.UpsertAPIKey(t.Context(), store.APIKeyRecord{
		ID: "mgmt", Name: "manager", KeyHash: store.KeyFingerprint("mgmt-secret"),
		Kind: auth.KeyKindManagement, Scopes: auth.ManagementScopes(), AllowManagementLogin: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpsertAPIKey(t.Context(), store.APIKeyRecord{
		ID: "child", Name: "client", KeyHash: store.KeyFingerprint("child-secret"),
		Kind: auth.KeyKindAccess, ParentID: "mgmt", Scopes: []string{auth.ScopeInference}, Group: "team-a",
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, entry := range []store.ActivityLogEntry{
		{Timestamp: now.Add(-90 * time.Minute), Model: "gpt-4", KeyID: "child", ReqPath: "/v1/chat/completions", ClientIP: "203.0.113.7",
			Tokens: store.TokenMetrics{InputTokens: 100, OutputTokens: 50, CachedTokens: 40}, EstimatedCost: 0.5, CostEstimated: true, DurationMs: 2000},
		{Timestamp: now.Add(-30 * time.Minute), Model: "claude", ReqPath: "/v1/responses", ClientIP: "10.0.0.5",
			Tokens: store.TokenMetrics{InputTokens: 10, OutputTokens: 5}, DurationMs: 1000},
	} {
		if _, err := s.store.InsertActivity(t.Context(), entry); err != nil {
			t.Fatalf("insert activity: %v", err)
		}
	}

	analyticsRequest := httptest.NewRequest(http.MethodGet, "/api/usage/analytics?granularity=hour", nil)
	analyticsResponse := httptest.NewRecorder()
	s.handleAPIUsageAnalytics(analyticsResponse, analyticsRequest)
	if analyticsResponse.Code != http.StatusOK {
		t.Fatalf("analytics status=%d body=%s", analyticsResponse.Code, analyticsResponse.Body.String())
	}
	var analytics store.UsageAnalytics
	if err := json.Unmarshal(analyticsResponse.Body.Bytes(), &analytics); err != nil {
		t.Fatal(err)
	}
	if analytics.Totals.Requests != 2 || analytics.Totals.InputTokens != 110 {
		t.Fatalf("analytics totals = %#v", analytics.Totals)
	}
	if analytics.Granularity != store.UsageGranularityHour {
		t.Fatalf("granularity = %q", analytics.Granularity)
	}
	// The keyed row groups under the key's group; the anonymous row falls into
	// the "ungrouped" bucket rather than disappearing from the dashboard.
	if len(analytics.ByGroup) != 2 {
		t.Fatalf("by group = %#v", analytics.ByGroup)
	}
	if analytics.ByGroup[0].Name != "team-a" || analytics.ByGroup[0].Requests != 1 {
		t.Fatalf("by group = %#v", analytics.ByGroup)
	}
	if analytics.ByGroup[1].Name != "ungrouped" {
		t.Fatalf("by group = %#v", analytics.ByGroup)
	}

	recordsRequest := httptest.NewRequest(http.MethodGet, "/api/usage/records?model=gpt-4", nil)
	recordsResponse := httptest.NewRecorder()
	s.handleAPIUsageRecords(recordsResponse, recordsRequest)
	if recordsResponse.Code != http.StatusOK {
		t.Fatalf("records status=%d body=%s", recordsResponse.Code, recordsResponse.Body.String())
	}
	var page store.UsageRecordPage
	if err := json.Unmarshal(recordsResponse.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Data) != 1 || page.Data[0].Model != "gpt-4" {
		t.Fatalf("filtered records = %#v", page.Data)
	}
	if page.Data[0].ClientIP != "203.0.113.7" || page.Data[0].KeyName != "client" || page.Data[0].KeyGroup != "team-a" {
		t.Fatalf("record join = %#v", page.Data[0])
	}

	// An unsupported granularity is a client error, not a silent default.
	badGranularity := httptest.NewRequest(http.MethodGet, "/api/usage/analytics?granularity=century", nil)
	badResponse := httptest.NewRecorder()
	s.handleAPIUsageAnalytics(badResponse, badGranularity)
	if badResponse.Code != http.StatusBadRequest {
		t.Fatalf("bad granularity status=%d body=%s", badResponse.Code, badResponse.Body.String())
	}
}

func TestServer_UsageOptionsAndExport(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })
	if err := s.store.UpsertAPIKey(t.Context(), store.APIKeyRecord{
		ID: "child", Name: "client", KeyHash: store.KeyFingerprint("child-secret"),
		Kind: auth.KeyKindAccess, Group: "team-a",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.InsertActivity(t.Context(), store.ActivityLogEntry{
		Model: "gpt-4", KeyID: "child", ReqPath: "/v1/chat/completions", ClientIP: "203.0.113.7",
		Tokens: store.TokenMetrics{InputTokens: 10, OutputTokens: 5}, EstimatedCost: 0.25, DurationMs: 100,
	}); err != nil {
		t.Fatal(err)
	}

	optionsRequest := httptest.NewRequest(http.MethodGet, "/api/usage/options", nil)
	optionsResponse := httptest.NewRecorder()
	s.handleAPIUsageOptions(optionsResponse, optionsRequest)
	if optionsResponse.Code != http.StatusOK {
		t.Fatalf("options status=%d body=%s", optionsResponse.Code, optionsResponse.Body.String())
	}
	var options store.UsageFilterOptions
	if err := json.Unmarshal(optionsResponse.Body.Bytes(), &options); err != nil {
		t.Fatal(err)
	}
	if len(options.Models) != 1 || options.Models[0] != "gpt-4" {
		t.Fatalf("filter models = %#v", options.Models)
	}
	if len(options.Endpoints) != 1 || len(options.Groups) != 1 {
		t.Fatalf("filter endpoints=%#v groups=%#v", options.Endpoints, options.Groups)
	}
	if len(options.Keys) != 1 || options.Keys[0].ID != "child" || options.Keys[0].Group != "team-a" {
		t.Fatalf("filter keys = %#v", options.Keys)
	}

	exportRequest := httptest.NewRequest(http.MethodGet, "/api/usage/export.csv", nil)
	exportResponse := httptest.NewRecorder()
	s.handleAPIUsageExport(exportResponse, exportRequest)
	if exportResponse.Code != http.StatusOK {
		t.Fatalf("export status=%d body=%s", exportResponse.Code, exportResponse.Body.String())
	}
	body := exportResponse.Body.String()
	for _, needle := range []string{"client_ip", "203.0.113.7", "gpt-4", "0.25"} {
		if !strings.Contains(body, needle) {
			t.Fatalf("csv body is missing %q: %q", needle, body)
		}
	}
}
