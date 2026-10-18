package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// quotaUsageBody is the OpenCode Go shaped usage response.
func quotaUsageBody(rollingPercent int, resetsAt string) string {
	return `{"usage":{
		"rolling":{"status":"ok","percent":` + strconv.Itoa(rollingPercent) + `,"resetsAt":"` + resetsAt + `"},
		"weekly":{"status":"ok","percent":9,"resetsAt":"2026-10-19T00:00:00Z"},
		"monthly":{"status":"rate-limited","percent":4,"resetsAt":"2026-11-14T00:46:10Z"}
	}}`
}

// quotaTestYAML builds a provider whose quota block goes through the real
// config loader, so defaults (percent field, threshold, labels) apply exactly
// like they do in production.
func quotaTestYAML(endpoint string, enforce bool, threshold int) string {
	return `
models:
  local-model:
    cmd: echo hi
    proxy: http://127.0.0.1:1
providers:
  sub:
    type: openai
    endpoint: ` + endpoint + `
    apiKeys: [sub-key-1, sub-key-2]
    models: [m1]
    quota:
      endpoint: ` + endpoint + `/usage
      auth: {style: bearer}
      windows:
        - {id: rolling, path: usage.rolling, status: status, resetsAt: resetsAt}
        - {id: weekly, path: usage.weekly, status: status, resetsAt: resetsAt}
        - {id: monthly, path: usage.monthly, status: status, resetsAt: resetsAt}
      extras:
        - {id: credits, path: usage.credits}
      enforce: {enabled: ` + boolText(enforce) + `, threshold: ` + strconv.Itoa(threshold) + `}
`
}

// refreshQuotaForTest performs one refresh through the same code path the
// control API uses.
func (s *Server) refreshQuotaForTest(providerID string) {
	req := httptest.NewRequest("POST", "/api/providers/quotas/refresh", strings.NewReader(`{"providerID":"`+providerID+`"}`))
	s.handleAPIProvidersQuotasRefresh(httptest.NewRecorder(), req)
}

// resetQuotaThrottleForTest clears the per-credential refresh throttle so a
// test can observe two consecutive refreshes.
func (s *Server) resetQuotaThrottleForTest() {
	s.quota.mu.Lock()
	defer s.quota.mu.Unlock()
	for _, entry := range s.quota.entries {
		entry.lastAttempt = time.Time{}
	}
}

func boolText(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func quotaTestConfig(t *testing.T, endpoint string, enforce bool, threshold int) config.Config {
	t.Helper()
	cfg, err := config.LoadConfigFromReader(strings.NewReader(quotaTestYAML(endpoint, enforce, threshold)))
	if err != nil {
		t.Fatalf("loading quota test config: %v", err)
	}
	return cfg
}

func TestServer_QuotaRefreshParsesWindows(t *testing.T) {
	var gotAuth atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/usage":
			gotAuth.Store(r.Header.Get("Authorization"))
			w.Write([]byte(`{"usage":{
				"rolling":{"status":"ok","percent":0,"resetsAt":"2026-10-15T05:59:57.724Z"},
				"weekly":{"status":"ok","percent":12,"resetsAt":"2026-10-19T00:00:00Z"},
				"monthly":{"status":"ok","percent":6,"resetsAt":"2026-11-14T00:46:10Z"},
				"credits": 12.5
			}}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer upstream.Close()

	s := gatewayTestServer(t, quotaTestConfig(t, upstream.URL, false, 100), upstream)
	req := httptest.NewRequest("POST", "/api/providers/quotas/refresh", strings.NewReader(`{"providerID":"sub"}`))
	w := httptest.NewRecorder()
	s.handleAPIProvidersQuotasRefresh(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if auth, _ := gotAuth.Load().(string); auth != "Bearer sub-key-1" && auth != "Bearer sub-key-2" {
		t.Fatalf("quota Authorization = %q, want the subscription key", auth)
	}
	var resp struct {
		Credentials []credentialQuota `json:"credentials"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if len(resp.Credentials) != 2 {
		t.Fatalf("credentials = %d, want one per key", len(resp.Credentials))
	}
	first := resp.Credentials[0]
	if len(first.Windows) != 3 {
		t.Fatalf("windows = %+v", first.Windows)
	}
	if first.Windows[0].ID != "rolling" || first.Windows[0].Percent != 0 || first.Windows[0].RemainingPercent != 100 {
		t.Errorf("window0 = %+v", first.Windows[0])
	}
	if first.Windows[1].Percent != 12 || first.Windows[1].RemainingPercent != 88 || first.Windows[1].ResetsAt != "2026-10-19T00:00:00Z" {
		t.Errorf("window1 = %+v", first.Windows[1])
	}
	if first.Windows[0].Label != "rolling" {
		t.Errorf("label fallback = %q", first.Windows[0].Label)
	}
	if len(first.Extras) != 1 || first.Extras[0].Value != 12.5 {
		t.Errorf("extras = %+v", first.Extras)
	}
	if first.FetchedAt.IsZero() || first.Error != "" {
		t.Errorf("snapshot = %+v", first)
	}
}

func TestServer_QuotaRefreshReportsUpstreamErrors(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/usage" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"bad key"}`))
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	s := gatewayTestServer(t, quotaTestConfig(t, upstream.URL, false, 100), upstream)
	req := httptest.NewRequest("POST", "/api/providers/quotas/refresh", strings.NewReader(`{"providerID":"sub","keyID":"sub#nope"}`))
	w := httptest.NewRecorder()
	s.handleAPIProvidersQuotasRefresh(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown key must 404, got %d body=%s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest("POST", "/api/providers/quotas/refresh", strings.NewReader(`{"providerID":"sub"}`))
	w = httptest.NewRecorder()
	s.handleAPIProvidersQuotasRefresh(w, req)
	var resp struct {
		Credentials []credentialQuota `json:"credentials"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Credentials) == 0 || !strings.Contains(resp.Credentials[0].Error, "401") {
		t.Fatalf("error snapshot = %+v", resp.Credentials)
	}
}

func TestServer_QuotaEnforcementParksCredential(t *testing.T) {
	var rollingPercent atomic.Int32
	rollingPercent.Store(40)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/usage" {
			reset := time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339)
			w.Write([]byte(quotaUsageBody(int(rollingPercent.Load()), reset)))
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	s := gatewayTestServer(t, quotaTestConfig(t, upstream.URL, true, 100), upstream)

	// Below the threshold: both credentials stay schedulable.
	s.refreshQuotaForTest("sub")
	for _, route := range s.currentGateway().AllRoutes() {
		for _, cred := range route.Credentials() {
			if cred.QuotaCooldownUntil().After(time.Now()) {
				t.Fatalf("credential %s parked below threshold", cred.ID)
			}
		}
	}

	// A full rolling window parks the credential until its reset.
	rollingPercent.Store(100)
	s.resetQuotaThrottleForTest()
	s.refreshQuotaForTest("sub")
	parked := 0
	for _, route := range s.currentGateway().AllRoutes() {
		for _, cred := range route.Credentials() {
			if cred.QuotaCooldownUntil().After(time.Now()) {
				parked++
			}
		}
	}
	if parked == 0 {
		t.Fatal("quota enforcement did not park any credential")
	}
	if got := s.currentGateway().RouteFor("sub/m1", false).BestCredential(); got != nil {
		t.Fatalf("a parked credential must not be schedulable, got %s", got.ID)
	}

	// Window frees up: the park is released.
	rollingPercent.Store(10)
	s.resetQuotaThrottleForTest()
	s.refreshQuotaForTest("sub")
	for _, route := range s.currentGateway().AllRoutes() {
		for _, cred := range route.Credentials() {
			if cred.QuotaCooldownUntil().After(time.Now()) {
				t.Fatalf("credential %s still parked after the window freed", cred.ID)
			}
		}
	}
}

func TestServer_ProviderTemplateListAndApply(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			w.Write([]byte(`{"object":"list","data":[
				{"id":"grok-4.7"},{"id":"claude-haiku-5-5"},{"id":"kimi-k3"},{"id":"gemini-4-argon-contributor"}
			]}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer upstream.Close()

	s := gatewayTestServer(t, gatewayTestConfig(), upstream)

	// List: the built-in template registry.
	req := httptest.NewRequest("GET", "/api/providers/templates", nil)
	w := httptest.NewRecorder()
	s.handleAPIProviderTemplates(w, req)
	if w.Code != 200 {
		t.Fatalf("list status = %d body=%s", w.Code, w.Body.String())
	}
	var listed struct {
		Templates []struct {
			ID          string `json:"id"`
			RequiresKey bool   `json:"requiresKey"`
			Adapters    []struct {
				Name string `json:"name"`
				Type string `json:"type"`
			} `json:"adapters"`
		} `json:"templates"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	found := false
	for _, template := range listed.Templates {
		if template.ID == "opencode-go" {
			found = true
			if template.RequiresKey {
				t.Error("opencode-go model list is public; requiresKey must be false")
			}
			if len(template.Adapters) != 4 {
				t.Errorf("adapters = %+v", template.Adapters)
			}
		}
	}
	if !found {
		t.Fatalf("opencode-go missing from %s", w.Body.String())
	}

	// Apply: the model list is pulled and distributed by modelMatch. The
	// template's own modelsEndpoint points at opencode.ai, so the test
	// overrides the registry entry through a local fetch: call the pull helper
	// with a template whose endpoint is the test server.
	template, ok := config.ProviderTemplateByID("opencode-go")
	if !ok {
		t.Fatal("template missing")
	}
	template.ModelsEndpoint = upstream.URL + "/models"
	models, err := s.pullTemplateModels(context.Background(), template, "")
	if err != nil {
		t.Fatalf("pull models: %v", err)
	}
	assigned := template.DistributeModels(models)
	want := map[string][]string{
		"Responses": {"grok-4.7"},
		"Messages":  {"claude-haiku-5-5"},
		"Google":    {"gemini-4-argon-contributor"},
		"Chat":      {"kimi-k3"},
	}
	if !reflect.DeepEqual(assigned, want) {
		t.Fatalf("assigned = %v, want %v", assigned, want)
	}
}

func TestServer_ProviderTemplateApplyUsesStoredCredential(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			if r.Header.Get("Authorization") == "" {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":"missing key"}`))
				return
			}
			w.Write([]byte(`{"data":[{"id":"kimi-k3"}]}`))
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	s := gatewayTestServer(t, gatewayTestConfig(), upstream)
	// The provider key in the fixture is "k1"/"k2".
	if key, ok := s.storedCredential("prov"); !ok || (key != "k1" && key != "k2") {
		t.Fatalf("stored credential = %q ok=%v", key, ok)
	}

	template, _ := config.ProviderTemplateByID("opencode-go")
	template.ModelsEndpoint = upstream.URL + "/models"
	template.ModelsAuth = config.TemplateModelsAuthBearer
	models, err := s.pullTemplateModels(context.Background(), template, "k1")
	if err != nil {
		t.Fatalf("pull with credential failed: %v", err)
	}
	if len(models) != 1 || models[0] != "kimi-k3" {
		t.Fatalf("models = %v", models)
	}
}
