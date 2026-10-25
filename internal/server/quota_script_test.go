package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/gateway"
)

// quotaScriptYAML builds a provider whose quota block is driven by a script.
func quotaScriptYAML(endpoint, script string) string {
	return `
models:
  local-model:
    cmd: echo hi
    proxy: http://127.0.0.1:1
providers:
  p:
    type: openai
    endpoint: ` + endpoint + `
    apiKeys: [sk-script]
    models: [m1]
    quota:
      timeoutSeconds: 5
      script: |
` + indentYAML(script, "        ") + `
`
}

func indentYAML(text, prefix string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

func quotaScriptConfig(t *testing.T, endpoint, script string) config.Config {
	t.Helper()
	cfg, err := config.LoadConfigFromReader(strings.NewReader(quotaScriptYAML(endpoint, script)))
	if err != nil {
		t.Fatalf("loading quota script config: %v", err)
	}
	return cfg
}

func quotaScriptSnapshotFor(t *testing.T, s *Server, providerID string) credentialQuota {
	t.Helper()
	credentials := s.quotaTargets(providerID, "")
	if len(credentials) == 0 {
		t.Fatal("no quota targets")
	}
	snapshot, ok := s.quota.get(credentials[0].cred.ID)
	if !ok {
		t.Fatal("snapshot missing")
	}
	return snapshot
}

// CC Switch 形态：脚本声明请求，extractor 把响应映射成用量字段。
func TestServer_QuotaScriptCCSwitchShape(t *testing.T) {
	var gotAuth, gotPath atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		gotPath.Store(r.URL.Path)
		w.Write([]byte(`{"is_active":true,"plan":"Pro","total":20,"used":5,"remaining":15}`))
	}))
	defer upstream.Close()

	script := `({
  request: {
    url: "{{baseUrl}}/v1/usage",
    method: "GET",
    headers: { "Authorization": "Bearer {{apiKey}}" }
  },
  extractor: function (response) {
    return {
      isValid: response.is_active !== false,
      planName: response.plan,
      total: response.total,
      used: response.used,
      remaining: response.remaining,
      unit: "USD"
    };
  }
})`
	s := gatewayTestServer(t, quotaScriptConfig(t, upstream.URL, script), upstream)
	s.refreshQuotaForTest("p")

	if gotPath.Load() != "/v1/usage" {
		t.Fatalf("path = %v, want the script's request path", gotPath.Load())
	}
	if gotAuth.Load() != "Bearer sk-script" {
		t.Fatalf("auth = %v, want the key substituted into {{apiKey}}", gotAuth.Load())
	}
	snapshot := quotaScriptSnapshotFor(t, s, "p")
	if snapshot.Error != "" {
		t.Fatalf("snapshot error = %q", snapshot.Error)
	}
	if len(snapshot.Windows) != 1 || snapshot.Windows[0].Percent != 25 || snapshot.Windows[0].Limit != 20 {
		t.Fatalf("windows = %+v, want one 25%% window", snapshot.Windows)
	}
	values := map[string]any{}
	for _, extra := range snapshot.Extras {
		values[extra.ID] = extra.Value
	}
	if values["remaining"] != 15.0 || values["plan"] != "Pro" {
		t.Fatalf("extras = %+v", snapshot.Extras)
	}
}

// 函数形态：注入同步 fetch，可连续请求多次。
func TestServer_QuotaScriptFunctionForm(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/usage":
			w.Write([]byte(`{"used": 6}`))
		case "/plan":
			w.Write([]byte(`{"total": 24}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	script := `async (ctx) => {
  const usage = await ctx.fetch(ctx.baseUrl + "/usage");
  const plan = await ctx.fetch(ctx.baseUrl + "/plan");
  return {
    windows: [{ id: "quota", label: "quota", used: usage.json.used, limit: plan.json.total }],
    extras: [{ id: "remaining", label: "remaining", value: plan.json.total - usage.json.used, suffix: " USD" }]
  };
}`
	s := gatewayTestServer(t, quotaScriptConfig(t, upstream.URL, script), upstream)
	s.refreshQuotaForTest("p")

	snapshot := quotaScriptSnapshotFor(t, s, "p")
	if snapshot.Error != "" {
		t.Fatalf("snapshot error = %q", snapshot.Error)
	}
	if len(snapshot.Windows) != 1 || snapshot.Windows[0].Percent != 25 || snapshot.Windows[0].Used != 6 {
		t.Fatalf("windows = %+v, want 25%% used from two requests", snapshot.Windows)
	}
	if len(snapshot.Extras) != 1 || snapshot.Extras[0].Value != 18.0 {
		t.Fatalf("extras = %+v", snapshot.Extras)
	}
}

// isValid=false 必须让卡片显示错误，而不是静默显示 0%。
func TestServer_QuotaScriptInvalidCredential(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":false}`))
	}))
	defer upstream.Close()

	script := `(async (ctx) => {
  const r = await ctx.fetch(ctx.baseUrl + "/usage");
  return { isValid: r.json.ok, invalidMessage: "key revoked", remaining: 0 };
})`
	s := gatewayTestServer(t, quotaScriptConfig(t, upstream.URL, script), upstream)
	s.refreshQuotaForTest("p")

	snapshot := quotaScriptSnapshotFor(t, s, "p")
	if snapshot.Error != "key revoked" {
		t.Fatalf("error = %q, want the script's invalidMessage", snapshot.Error)
	}
}

// 脚本报错里不能出现凭据明文：用户可能把 apiKey 拼进了 URL。
func TestServer_QuotaScriptErrorRedactsCredential(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"nope"}`))
	}))
	defer upstream.Close()

	script := `({ request: { url: "{{baseUrl}}/usage?key={{apiKey}}", method: "GET" },
  extractor: function (response) { return { remaining: 1 }; } })`
	s := gatewayTestServer(t, quotaScriptConfig(t, upstream.URL, script), upstream)
	s.refreshQuotaForTest("p")

	snapshot := quotaScriptSnapshotFor(t, s, "p")
	if snapshot.Error == "" {
		t.Fatal("expected an error for the 401 response")
	}
	if strings.Contains(snapshot.Error, "sk-script") {
		t.Fatalf("error leaked the credential: %q", snapshot.Error)
	}
}

func TestServer_QuotaScriptMustReturnObject(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	s := gatewayTestServer(t, quotaScriptConfig(t, upstream.URL, `"just a string"`), upstream)
	s.refreshQuotaForTest("p")

	snapshot := quotaScriptSnapshotFor(t, s, "p")
	if !strings.Contains(snapshot.Error, "windows/extras") {
		t.Fatalf("error = %q, want a shape complaint", snapshot.Error)
	}
}

func TestServer_QuotaScriptTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	yaml := strings.Replace(quotaScriptYAML(upstream.URL, `(function(){ while (true) {} })()`), "timeoutSeconds: 5", "timeoutSeconds: 1", 1)
	cfg, err := config.LoadConfigFromReader(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	s := gatewayTestServer(t, cfg, upstream)
	start := time.Now()
	s.refreshQuotaForTest("p")
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("script ran %s, want the 1s budget to interrupt it", elapsed)
	}
	snapshot := quotaScriptSnapshotFor(t, s, "p")
	if !strings.Contains(snapshot.Error, "timed out") && !strings.Contains(snapshot.Error, "interrupted") {
		t.Fatalf("error = %q, want a timeout", snapshot.Error)
	}
}

// 测试端点用草稿脚本跑一次，不写缓存、不要求保存。
func TestServer_QuotaScriptTestEndpointRunsDraft(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"remaining": 7.5, "unit": "USD"}`))
	}))
	defer upstream.Close()

	// The provider has no quota block at all: the editor is drafting one.
	cfg := quotaScriptConfig(t, upstream.URL, `({ request: { url: "{{baseUrl}}/usage" }, extractor: (r) => ({ remaining: r.remaining, unit: r.unit }) })`)
	provider := cfg.Providers["p"]
	provider.Quota = nil
	cfg.Providers["p"] = provider

	s := gatewayTestServer(t, cfg, upstream)
	keyID := gateway.CredentialID("p", config.ProviderKey{Value: "sk-script"})
	body := `{"providerID":"p","keyID":"` + keyID + `","script":"({ request: { url: \"{{baseUrl}}/usage\" }, extractor: (r) => ({ remaining: r.remaining, unit: r.unit }) })"}`
	req := httptest.NewRequest("POST", "/api/providers/quotas/test", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleAPIProvidersQuotasTest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Snapshot credentialQuota     `json:"snapshot"`
		Request  *quotaScriptRequest `json:"request"`
		Error    string              `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Error != "" {
		t.Fatalf("error = %q", resp.Error)
	}
	if len(resp.Snapshot.Extras) != 1 || resp.Snapshot.Extras[0].Value != 7.5 {
		t.Fatalf("snapshot = %+v", resp.Snapshot)
	}
	if resp.Request == nil || !strings.HasSuffix(resp.Request.URL, "/usage") {
		t.Fatalf("request echo = %+v", resp.Request)
	}
	// The test must not populate the cache.
	if _, ok := s.quota.get(keyID); ok {
		t.Fatal("test endpoint must not cache the snapshot")
	}
}

// 预设详情端点把脚本原文交给编辑器，方便“载入预设脚本”再改。
func TestServer_QuotaPresetDetailEndpoint(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest("GET", "/api/providers/quota-presets/newapi", nil)
	req.SetPathValue("id", "newapi")
	w := httptest.NewRecorder()
	s.handleAPIProviderQuotaPreset(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	var detail config.QuotaPresetDetail
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if detail.ID != "newapi" || detail.Name == "" {
		t.Fatalf("detail = %+v", detail)
	}
}

// 内置脚本预设跑通：/api/usage/token/ + /api/status 的 quota_per_unit 换算。
func TestServer_QuotaScriptPresetNewAPIUnits(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/usage/token/":
			w.Write([]byte(`{"code":true,"data":{"total_granted":3000000,"total_used":1000000,"total_available":2000000,"unlimited_quota":false}}`))
		case "/api/status":
			w.Write([]byte(`{"data":{"quota_per_unit":1000000}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	yaml := `
models:
  local-model:
    cmd: echo hi
    proxy: http://127.0.0.1:1
providers:
  p:
    type: openai
    endpoint: ` + upstream.URL + `
    apiKeys: [sk-script]
    models: [m1]
    quota:
      preset: newapi-units
`
	cfg, err := config.LoadConfigFromReader(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	s := gatewayTestServer(t, cfg, upstream)
	s.refreshQuotaForTest("p")

	snapshot := quotaScriptSnapshotFor(t, s, "p")
	if snapshot.Error != "" {
		t.Fatalf("error = %q", snapshot.Error)
	}
	if len(snapshot.Windows) != 1 || snapshot.Windows[0].Percent != 33 || snapshot.Windows[0].Limit != 3 {
		t.Fatalf("windows = %+v, want 33%% of 3 USD", snapshot.Windows)
	}
	values := map[string]any{}
	for _, extra := range snapshot.Extras {
		values[extra.ID] = extra.Value
	}
	if values["available"] != 2.0 || values["unit"] != 1000000.0 {
		t.Fatalf("extras = %+v", snapshot.Extras)
	}
}

// 脚本末尾的行注释不能把包装器的终止符吃掉。
func TestServer_QuotaScriptTrailingLineComment(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"remaining": 3, "unit": "USD"}`))
	}))
	defer upstream.Close()

	script := `({ request: { url: "{{baseUrl}}/usage" }, extractor: (r) => ({ remaining: r.remaining, unit: r.unit }) }) // trailing note`
	s := gatewayTestServer(t, quotaScriptConfig(t, upstream.URL, script), upstream)
	s.refreshQuotaForTest("p")
	snapshot := quotaScriptSnapshotFor(t, s, "p")
	if snapshot.Error != "" {
		t.Fatalf("error = %q, want the trailing comment to be tolerated", snapshot.Error)
	}
	if len(snapshot.Extras) != 1 || snapshot.Extras[0].Value != 3.0 {
		t.Fatalf("extras = %+v", snapshot.Extras)
	}
}

// 传输层错误会把完整 URL 带进错误文本，凭据必须被抹掉。
func TestServer_QuotaScriptTransportErrorRedactsCredential(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	script := `({ request: { url: "http://127.0.0.1:1/usage?key={{apiKey}}" },
  extractor: function (response) { return { remaining: 1 }; } })`
	s := gatewayTestServer(t, quotaScriptConfig(t, upstream.URL, script), upstream)
	s.refreshQuotaForTest("p")

	snapshot := quotaScriptSnapshotFor(t, s, "p")
	if snapshot.Error == "" {
		t.Fatal("expected a transport error")
	}
	if strings.Contains(snapshot.Error, "sk-script") {
		t.Fatalf("transport error leaked the credential: %q", snapshot.Error)
	}
	if !strings.Contains(snapshot.Error, "[redacted]") {
		t.Fatalf("error = %q, want the placeholder to make the redaction visible", snapshot.Error)
	}
}
