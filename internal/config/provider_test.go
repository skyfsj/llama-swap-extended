package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func loadProvidersYAML(t *testing.T, yamlText string) (Config, error) {
	t.Helper()
	return LoadConfigFromReader(strings.NewReader(yamlText))
}

const providersBaseYAML = `
models:
  local-model:
    cmd: echo ${PORT}
providers:
  deepseek:
    type: openai
    endpoint: https://api.deepseek.com
    apiKeys:
      - ${env.PROVIDER_TEST_KEY}
    models:
      - deepseek-chat
`

func TestConfig_ProviderScalarForms(t *testing.T) {
	t.Setenv("PROVIDER_TEST_KEY", "sk-test-1")
	cfg, err := loadProvidersYAML(t, providersBaseYAML)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p, ok := cfg.Providers["deepseek"]
	if !ok {
		t.Fatal("provider deepseek missing")
	}
	if p.Type != ProviderTypeOpenAI {
		t.Errorf("type = %q", p.Type)
	}
	if p.Endpoint != "https://api.deepseek.com" {
		t.Errorf("endpoint = %q", p.Endpoint)
	}
	if len(p.ApiKeys) != 1 || p.ApiKeys[0].Value != "sk-test-1" {
		t.Errorf("apiKeys = %+v", p.ApiKeys)
	}
	if len(p.Models) != 1 || p.Models[0].Local != "deepseek-chat" || p.Models[0].Remote != "deepseek-chat" {
		t.Errorf("models = %+v", p.Models)
	}
	if !p.StickyEnabled() {
		t.Error("sticky should default to enabled")
	}
	if p.BareNamePolicy != "local-first" {
		t.Errorf("bareNamePolicy = %q", p.BareNamePolicy)
	}
	if p.MaxRetries != 2 {
		t.Errorf("maxRetries = %d", p.MaxRetries)
	}
	if p.Timeouts.ResponseHeader != 600 {
		t.Errorf("responseHeader default = %d", p.Timeouts.ResponseHeader)
	}
}

func TestConfig_ProviderStructuredForms(t *testing.T) {
	t.Setenv("PROVIDER_TEST_KEY", "sk-test-1")
	yaml := `
models:
  local-model:
    cmd: echo ${PORT}
providers:
  deepseek:
    type: openai
    endpoint: https://api.deepseek.com
    apiKeys:
      - ${env.PROVIDER_TEST_KEY}
    models:
      - deepseek-chat
  claude:
    type: anthropic
    endpoint: https://api.anthropic.com/
    priority: 5
    weight: 1
    sticky: false
    bareNamePolicy: provider-first
    maxRetries: 4
    apiKeys:
      - value: sk-anthropic
        weight: 3
        maxConcurrent: 2
        proxy: socks5://127.0.0.1:1080
      - valueEnv: PROVIDER_TEST_BACKUP
    models:
      - local: claude-sonnet
        remote: claude-sonnet-4-5
      - claude-haiku: claude-haiku-4-5
`
	t.Setenv("PROVIDER_TEST_BACKUP", "sk-backup")
	cfg, err := loadProvidersYAML(t, yaml)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p, ok := cfg.Providers["claude"]
	if !ok {
		t.Fatal("provider claude missing")
	}
	// trailing slash trimmed
	if p.Endpoint != "https://api.anthropic.com" {
		t.Errorf("endpoint = %q", p.Endpoint)
	}
	if p.StickyEnabled() {
		t.Error("sticky should be disabled")
	}
	if p.BareNamePolicy != "provider-first" {
		t.Errorf("bareNamePolicy = %q", p.BareNamePolicy)
	}
	if len(p.ApiKeys) != 2 {
		t.Fatalf("apiKeys = %+v", p.ApiKeys)
	}
	k0 := p.ApiKeys[0]
	if k0.Value != "sk-anthropic" || k0.Weight != 3 || k0.MaxConcurrent != 2 {
		t.Errorf("key0 = %+v", k0)
	}
	// priority inherits provider default (5)
	if k0.Priority != 5 {
		t.Errorf("key0 priority = %d, want inherited 5", k0.Priority)
	}
	k1 := p.ApiKeys[1]
	if k1.ValueEnv != "PROVIDER_TEST_BACKUP" || k1.Value != "" {
		t.Errorf("key1 valueEnv not preserved: %+v", k1)
	}
	if k1.Weight != 1 || k1.Priority != 5 {
		t.Errorf("key1 inherited defaults wrong: %+v", k1)
	}
	if len(p.Models) != 2 {
		t.Fatalf("models = %+v", p.Models)
	}
	if p.Models[0].Local != "claude-sonnet" || p.Models[0].Remote != "claude-sonnet-4-5" {
		t.Errorf("model0 = %+v", p.Models[0])
	}
	if p.Models[1].Local != "claude-haiku" || p.Models[1].Remote != "claude-haiku-4-5" {
		t.Errorf("model1 shorthand mapping: %+v", p.Models[1])
	}
}

func TestConfig_ProviderEndpointV1SuffixNormalized(t *testing.T) {
	t.Setenv("PROVIDER_TEST_KEY", "sk-test-1")
	yaml := `
models:
  local-model:
    cmd: echo ${PORT}
providers:
  a:
    type: openai
    endpoint: https://api.example.com/v1/
    apiKeys:
      - ${env.PROVIDER_TEST_KEY}
    models:
      - m1
  b:
    type: openai
    endpoint: http://192.168.1.28:7863/v1
    apiKeys:
      - ${env.PROVIDER_TEST_KEY}
    models:
      - m2
  c:
    type: openai
    endpoint: https://api.example.com/v1/x
    apiKeys:
      - ${env.PROVIDER_TEST_KEY}
    models:
      - m3
`
	cfg, err := loadProvidersYAML(t, yaml)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cases := map[string]string{
		"a": "https://api.example.com",
		"b": "http://192.168.1.28:7863",
		// only a bare /v1 suffix is stripped, deeper paths stay intact
		"c": "https://api.example.com/v1/x",
	}
	for id, want := range cases {
		p, ok := cfg.Providers[id]
		if !ok {
			t.Fatalf("provider %s missing", id)
		}
		if p.Endpoint != want {
			t.Errorf("provider %s endpoint = %q, want %q", id, p.Endpoint, want)
		}
	}
}

func TestConfig_ProviderValidationErrors(t *testing.T) {
	t.Setenv("PROVIDER_TEST_KEY", "sk-test-1")
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "bad type",
			yaml: `
models:
  m0:
    cmd: echo ${PORT}
providers:
  p:
    type: bedrock
    endpoint: https://x.com
    apiKeys: [k]
    models: [m]`,
			wantErr: "type must be one of",
		},
		{
			name: "missing endpoint",
			yaml: `
models:
  m0:
    cmd: echo ${PORT}
providers:
  p:
    apiKeys: [k]
    models: [m]`,
			wantErr: "endpoint is required",
		},
		{
			name: "endpoint scheme",
			yaml: `
models:
  m0:
    cmd: echo ${PORT}
providers:
  p:
    endpoint: ftp://x.com
    apiKeys: [k]
    models: [m]`,
			wantErr: "http or https",
		},
		{
			name: "no keys",
			yaml: `
models:
  m0:
    cmd: echo ${PORT}
providers:
  p:
    endpoint: https://x.com
    apiKeys: []
    models: [m]`,
			wantErr: "at least one apiKey",
		},
		{
			name: "empty key",
			yaml: `
models:
  m0:
    cmd: echo ${PORT}
providers:
  p:
    endpoint: https://x.com
    apiKeys:
      - valueEnv: ""
    models: [m]`,
			wantErr: "needs a value or valueEnv",
		},
		{
			name: "bad proxy scheme",
			yaml: `
models:
  m0:
    cmd: echo ${PORT}
providers:
  p:
    endpoint: https://x.com
    proxy: ftp://proxy:1080
    apiKeys: [k]
    models: [m]`,
			wantErr: "proxy must use",
		},
		{
			name: "bad bareNamePolicy",
			yaml: `
models:
  m0:
    cmd: echo ${PORT}
providers:
  p:
    endpoint: https://x.com
    bareNamePolicy: always
    apiKeys: [k]
    models: [m]`,
			wantErr: "bareNamePolicy",
		},
		{
			name: "no models",
			yaml: `
models:
  m0:
    cmd: echo ${PORT}
providers:
  p:
    endpoint: https://x.com
    apiKeys: [k]
    models: []`,
			wantErr: "at least one model",
		},
		{
			// A name declared twice is resolved, not rejected: model names are
			// claimed before aliases, and every surface that serves a name
			// stays reachable through the pool.
			name: "duplicate model is accepted",
			yaml: `
models:
  m0:
    cmd: echo ${PORT}
providers:
  p:
    endpoint: https://x.com
    apiKeys: [k]
    models: [m, m]`,
		},
		{
			name: "direct proxy is accepted",
			yaml: `
models:
  m0:
    cmd: echo ${PORT}
providers:
  p:
    endpoint: https://x.com
    proxy: direct
    apiKeys: [k]
    models: [m]`,
			wantErr: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadProvidersYAML(t, tc.yaml)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestConfig_ProviderNamespaceValidation(t *testing.T) {
	t.Setenv("PROVIDER_TEST_KEY", "sk-test-1")
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "fqn conflicts with local model",
			yaml: `
models:
  p/m0:
    cmd: echo ${PORT}
providers:
  p:
    endpoint: https://x.com
    apiKeys: [k]
    models: [m0]`,
			wantErr: "conflicts with fully qualified provider model name",
		},
		{
			name: "fqn conflicts with model alias",
			yaml: `
models:
  m0:
    cmd: echo ${PORT}
    aliases: [p/other]
providers:
  p:
    endpoint: https://x.com
    apiKeys: [k]
    models: [other]`,
			wantErr: "conflicts with fully qualified provider model name",
		},
		{
			name: "provider fqn conflicts with peer fqn",
			yaml: `
models:
  m0:
    cmd: echo ${PORT}
peers:
  peer1:
    proxy: http://192.0.2.1
    models: [shared]
providers:
  peer1:
    endpoint: https://x.com
    apiKeys: [k]
    models: [shared]`,
			wantErr: "conflicts with peer model name",
		},
		{
			name: "provider bare name conflicts with peer bare name",
			yaml: `
models:
  m0:
    cmd: echo ${PORT}
peers:
  peer1:
    proxy: http://192.0.2.1
    models: [shared]
providers:
  p:
    endpoint: https://x.com
    apiKeys: [k]
    models: [shared]`,
			wantErr: "exposed by providers",
		},
		{
			name: "pool of two providers is allowed",
			yaml: `
models:
  m0:
    cmd: echo ${PORT}
providers:
  a:
    endpoint: https://a.com
    apiKeys: [k]
    models: [shared]
  b:
    endpoint: https://b.com
    apiKeys: [k]
    models: [shared]`,
			wantErr: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadProvidersYAML(t, tc.yaml)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestConfig_ResolveProviderModel(t *testing.T) {
	cfg := Config{
		Providers: ProviderDictionaryConfig{
			"a": {Models: []ProviderModel{{Local: "m1", Remote: "m1"}, {Local: "shared", Remote: "a-shared"}}, Type: ProviderTypeOpenAI, Endpoint: "https://a.com"},
			"b": {Models: []ProviderModel{{Local: "shared", Remote: "b-shared"}}, Type: ProviderTypeOpenAI, Endpoint: "https://b.com"},
			"c": {Models: []ProviderModel{{Local: "solo", Remote: "solo"}}, Disabled: true, Type: ProviderTypeOpenAI, Endpoint: "https://c.com"},
		},
	}

	if id, model, _, found := cfg.ResolveProviderModel("a/m1"); !found || id != "a" || model != "m1" {
		t.Errorf("fqn resolve: %q %q %v", id, model, found)
	}
	id, _, pool, found := cfg.ResolveProviderModel("shared")
	if !found || len(pool) != 2 {
		t.Errorf("pool resolve: id=%q pool=%v found=%v", id, pool, found)
	}
	if pool[0] != "a" || pool[1] != "b" {
		t.Errorf("pool order: %v", pool)
	}
	id, _, pool, found = cfg.ResolveProviderModel("solo")
	if found {
		t.Errorf("disabled provider must not resolve: %q %v %v", id, pool, found)
	}
	if _, _, _, found := cfg.ResolveProviderModel("missing"); found {
		t.Error("missing model must not resolve")
	}
}

func TestConfig_ProviderKeysNotInJSON(t *testing.T) {
	t.Setenv("PROVIDER_TEST_KEY", "sk-test-1")
	cfg, err := loadProvidersYAML(t, providersBaseYAML)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// ProviderKey.Value has json:"-", so a JSON round trip of the config map
	// must not contain the secret.
	data, err := json.Marshal(cfg.Providers)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "sk-test-1") {
		t.Fatalf("secret leaked through JSON: %s", data)
	}
}

func TestConfig_ProviderEnvMissingFails(t *testing.T) {
	// Ensure a missing env var fails the load (substituteEnvMacros behavior).
	if _, exists := os.LookupEnv("PROVIDER_DEFINITELY_UNSET_VAR_42"); exists {
		t.Skip("variable unexpectedly set")
	}
	yaml := strings.ReplaceAll(providersBaseYAML, "${env.PROVIDER_TEST_KEY}", "${PROVIDER_DEFINITELY_UNSET_VAR_42}")
	_, err := loadProvidersYAML(t, yaml)
	if err == nil || !strings.Contains(err.Error(), "PROVIDER_DEFINITELY_UNSET_VAR_42") {
		t.Fatalf("expected missing env error, got %v", err)
	}
}

func TestConfig_ProviderGeminiType(t *testing.T) {
	t.Setenv("PROVIDER_TEST_KEY", "sk-test-1")
	yaml := `
models:
  local-model:
    cmd: echo ${PORT}
providers:
  google:
    type: gemini
    endpoint: https://generativelanguage.googleapis.com
    apiKeys:
      - ${env.PROVIDER_TEST_KEY}
    models:
      - gemini-2.0-flash
`
	cfg, err := loadProvidersYAML(t, yaml)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p, ok := cfg.Providers["google"]
	if !ok {
		t.Fatal("provider google missing")
	}
	if p.Type != ProviderTypeGemini {
		t.Errorf("type = %q, want %q", p.Type, ProviderTypeGemini)
	}
}

func TestConfig_ProviderSessionHeadersNormalized(t *testing.T) {
	t.Setenv("PROVIDER_TEST_KEY", "sk-test-1")
	yaml := `
models:
  local-model:
    cmd: echo ${PORT}
providers:
  deepseek:
    type: openai
    endpoint: https://api.deepseek.com
    sessionHeaders: [" x-session-id ", "X-Session-Id", "X-Litellm-Session-Id", ""]
    apiKeys:
      - ${env.PROVIDER_TEST_KEY}
    models:
      - deepseek-chat
`
	cfg, err := loadProvidersYAML(t, yaml)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := cfg.Providers["deepseek"]
	// Trimmed, canonicalized and de-duplicated case-insensitively.
	want := []string{"X-Session-Id", "X-Litellm-Session-Id"}
	if len(p.SessionHeaders) != len(want) {
		t.Fatalf("sessionHeaders = %v, want %v", p.SessionHeaders, want)
	}
	for i := range want {
		if p.SessionHeaders[i] != want[i] {
			t.Errorf("sessionHeaders[%d] = %q, want %q", i, p.SessionHeaders[i], want[i])
		}
	}
	// The field must round-trip through config JSON for the providers UI.
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"sessionHeaders":["X-Session-Id","X-Litellm-Session-Id"]`) {
		t.Errorf("sessionHeaders missing from JSON: %s", data)
	}
}

func TestConfig_ProviderSessionHeadersInvalid(t *testing.T) {
	t.Setenv("PROVIDER_TEST_KEY", "sk-test-1")
	yaml := `
models:
  local-model:
    cmd: echo ${PORT}
providers:
  deepseek:
    type: openai
    endpoint: https://api.deepseek.com
    sessionHeaders: ["X-Session-Id: injection"]
    apiKeys:
      - ${env.PROVIDER_TEST_KEY}
    models:
      - deepseek-chat
`
	_, err := loadProvidersYAML(t, yaml)
	if err == nil || !strings.Contains(err.Error(), "not a valid HTTP header name") {
		t.Fatalf("expected invalid header name error, got %v", err)
	}
}

func TestConfig_ProviderAdapters(t *testing.T) {
	t.Setenv("PROVIDER_TEST_KEY", "sk-test-1")
	yaml := `
models:
  local-model:
    cmd: echo ${PORT}
providers:
  opencode-go:
    sessionHeaders: [x-opencode-session]
    headers: {user-agent: llama-swap, x-opencode-org-id: org_1}
    template: opencode-go
    apiKeys:
      - ${env.PROVIDER_TEST_KEY}
    adapters:
      - name: Responses
        type: openai
        endpoint: https://opencode.ai/inference/go
        modelMatch: ["grok-*", "gpt-*"]
        models:
          - grok-4.7
          - gpt-6-luna
      - name: Messages
        type: anthropic
        endpoint: https://opencode.ai/inference/go/v1
        models:
          - claude-haiku-5-5: claude-haiku-5-5-20260219
      - name: Google
        type: gemini
        endpoint: https://opencode.ai/inference/go/google
        pathMap: {"/v1beta/models/": "/v1/models/"}
        default: true
        models:
          - gemini-4-argon-contributor
`
	cfg, err := loadProvidersYAML(t, yaml)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := cfg.Providers["opencode-go"]
	if p.Type != "" || p.Endpoint != "" || len(p.Models) != 0 {
		t.Errorf("adapter-style provider must not fill the shorthand: type=%q endpoint=%q models=%v", p.Type, p.Endpoint, p.Models)
	}
	adapters := p.EffectiveAdapters()
	if len(adapters) != 3 {
		t.Fatalf("adapters = %d, want 3", len(adapters))
	}
	if adapters[0].Name != "Responses" || adapters[0].Type != ProviderTypeOpenAI {
		t.Errorf("adapter0 = %+v", adapters[0])
	}
	// A trailing /v1 is stripped per adapter.
	if adapters[1].Endpoint != "https://opencode.ai/inference/go" {
		t.Errorf("adapter1 endpoint = %q", adapters[1].Endpoint)
	}
	if adapters[2].PathMap["/v1beta/models/"] != "/v1/models/" || !adapters[2].Default {
		t.Errorf("adapter2 = %+v", adapters[2])
	}
	if p.Template != "opencode-go" {
		t.Errorf("template = %q", p.Template)
	}
	// Header names are canonicalized.
	if p.Headers["User-Agent"] != "llama-swap" || p.Headers["X-Opencode-Org-Id"] != "org_1" {
		t.Errorf("headers = %+v", p.Headers)
	}
	// AllModels flattens in adapter order.
	all := p.AllModels()
	if len(all) != 4 {
		t.Fatalf("AllModels = %d models, want 4", len(all))
	}
	if all[2].Local != "claude-haiku-5-5" || all[2].Remote != "claude-haiku-5-5-20260219" {
		t.Errorf("model mapping lost: %+v", all[2])
	}
	// Resolution sees adapter models.
	if id, model, _, found := cfg.ResolveProviderModel("opencode-go/grok-4.7"); !found || id != "opencode-go" || model != "grok-4.7" {
		t.Errorf("resolve = %q %q %v", id, model, found)
	}
	// Keys are validated at the provider level and inherited by every adapter.
	if len(p.ApiKeys) != 1 || p.ApiKeys[0].Value != "sk-test-1" {
		t.Errorf("apiKeys = %+v", p.ApiKeys)
	}
}

func TestConfig_ProviderAdapterValidationErrors(t *testing.T) {
	t.Setenv("PROVIDER_TEST_KEY", "sk-test-1")
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name: "both spellings",
			body: `
    type: openai
    endpoint: https://x.com
    adapters:
      - type: openai
        endpoint: https://y.com
        models: [m]`,
			wantErr: "cannot be combined with top-level",
		},
		{
			name: "bad pathMap",
			body: `
    adapters:
      - type: openai
        endpoint: https://x.com
        pathMap: {v1beta/models/: /v1/models/}
        models: [m]`,
			wantErr: "pathMap entries must be path prefixes",
		},
		{
			name: "credential header in headers",
			body: `
    headers: {Authorization: "Bearer nope"}
    type: openai
    endpoint: https://x.com
    models: [m]`,
			wantErr: "credential headers are managed by the gateway",
		},
		{
			name: "two defaults",
			body: `
    adapters:
      - name: A
        type: openai
        endpoint: https://x.com
        default: true
        models: [m1]
      - name: B
        type: gemini
        endpoint: https://y.com
        default: true
        models: [m2]`,
			wantErr: "at most one adapter may set default",
		},
		{
			name: "no models anywhere",
			body: `
    adapters:
      - name: A
        type: openai
        endpoint: https://x.com
        models: []
      - name: B
        type: gemini
        endpoint: https://y.com
        models: []`,
			wantErr: "at least one model is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yaml := `
models:
  local-model:
    cmd: echo ${PORT}
providers:
  p:
    apiKeys:
      - ${env.PROVIDER_TEST_KEY}` + tc.body + "\n"
			_, err := loadProvidersYAML(t, yaml)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

// A name declared by two models of one adapter is resolved rather than
// rejected: the model that declares it keeps the name, an alias only adds a
// second surface for it.
func TestConfig_ProviderAdapterDuplicateNameAccepted(t *testing.T) {
	t.Setenv("PROVIDER_TEST_KEY", "sk-test-1")
	yaml := `
models:
  local-model:
    cmd: echo ${PORT}
providers:
  p:
    apiKeys: [${env.PROVIDER_TEST_KEY}]
    adapters:
      - name: A
        type: openai
        endpoint: https://x.com
        models: [{local: shared, remote: shared}, {local: shared, aliases: [shared]}]
      - name: B
        type: gemini
        endpoint: https://y.com
        models: [other]
`
	cfg, err := loadProvidersYAML(t, yaml)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	adapters := cfg.Providers["p"].EffectiveAdapters()
	if len(adapters) != 2 || len(adapters[0].Models) != 2 {
		t.Fatalf("both declarations must survive: %+v", adapters)
	}
	if _, _, _, found := cfg.ResolveProviderModel("shared"); !found {
		t.Fatal("the shared name must resolve")
	}
}

func TestConfig_ProviderAdapterKeysOverride(t *testing.T) {
	t.Setenv("PROVIDER_TEST_KEY", "sk-test-1")
	yaml := `
models:
  local-model:
    cmd: echo ${PORT}
providers:
  p:
    weight: 3
    adapters:
      - name: A
        type: openai
        endpoint: https://x.com
        models: [m1]
      - name: B
        type: gemini
        endpoint: https://y.com
        apiKeys:
          - valueEnv: PROVIDER_TEST_BACKUP
        models: [m2]
`
	t.Setenv("PROVIDER_TEST_BACKUP", "sk-backup")
	cfg, err := loadProvidersYAML(t, yaml)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := cfg.Providers["p"]
	// No provider-level keys at all: the adapter-level set satisfies the
	// requirement.
	if len(p.ApiKeys) != 0 {
		t.Fatalf("provider keys = %+v", p.ApiKeys)
	}
	adapters := p.EffectiveAdapters()
	if len(adapters[1].ApiKeys) != 1 || adapters[1].ApiKeys[0].ValueEnv != "PROVIDER_TEST_BACKUP" {
		t.Fatalf("adapter keys = %+v", adapters[1].ApiKeys)
	}
	// Adapter keys inherit provider defaults too.
	if adapters[1].ApiKeys[0].Weight != 3 {
		t.Errorf("adapter key weight = %d, want inherited 3", adapters[1].ApiKeys[0].Weight)
	}
}

func TestConfig_ProviderEgressProxyLevels(t *testing.T) {
	t.Setenv("PROVIDER_TEST_KEY", "sk-test-1")
	yaml := `
models:
  local-model:
    cmd: echo ${PORT}
providers:
  mixed:
    proxy: http://provider-proxy:3128
    apiKeys:
      - ${env.PROVIDER_TEST_KEY}
    adapters:
      - name: LAN
        type: openai
        endpoint: http://192.168.1.28:7863
        proxy: direct
        models: [lan-model]
      - name: Internet
        type: openai
        endpoint: https://api.deepseek.com
        proxy: socks5://127.0.0.1:1080
        models:
          - local: overridden
            remote: overridden
            proxy: http://127.0.0.1:7890
          - inherits
`
	cfg, err := loadProvidersYAML(t, yaml)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	adapters := cfg.Providers["mixed"].EffectiveAdapters()
	if adapters[0].Proxy != ProxyDirect {
		t.Errorf("adapter0 proxy = %q, want direct", adapters[0].Proxy)
	}
	if adapters[1].Proxy != "socks5://127.0.0.1:1080" {
		t.Errorf("adapter1 proxy = %q", adapters[1].Proxy)
	}
	if adapters[1].Models[0].Proxy != "http://127.0.0.1:7890" {
		t.Errorf("model proxy = %q", adapters[1].Models[0].Proxy)
	}
	if adapters[1].Models[1].Proxy != "" {
		t.Errorf("inheriting model must stay empty, got %q", adapters[1].Models[1].Proxy)
	}
	if adapters[1].Models[1].Remote != "inherits" {
		t.Errorf("scalar shorthand broke: %+v", adapters[1].Models[1])
	}

	// Invalid proxy values are rejected at both levels.
	for _, body := range []string{
		"      - name: A\n        type: openai\n        endpoint: https://x.com\n        proxy: ftp://nope\n        models: [m]",
		"      - name: A\n        type: openai\n        endpoint: https://x.com\n        models: [{local: m, remote: m, proxy: \"not a url\"}]",
	} {
		bad := `
models:
  local-model:
    cmd: echo ${PORT}
providers:
  p:
    apiKeys: [${env.PROVIDER_TEST_KEY}]
    adapters:
` + body + "\n"
		if _, err := loadProvidersYAML(t, bad); err == nil {
			t.Fatalf("invalid proxy accepted:\n%s", body)
		}
	}
}

func TestConfig_ProviderModelPrefix(t *testing.T) {
	t.Setenv("PROVIDER_TEST_KEY", "sk-test-1")
	yaml := `
models:
  local-model:
    cmd: echo ${PORT}
providers:
  go:
    modelPrefix: "go-"
    endpoint: https://opencode.ai/inference/go
    apiKeys: [${env.PROVIDER_TEST_KEY}]
    models: [glm-5.1, kimi-k3]
  plain:
    endpoint: https://api.deepseek.com
    apiKeys: [${env.PROVIDER_TEST_KEY}]
    models: [deepseek-flash]
`
	cfg, err := loadProvidersYAML(t, yaml)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Providers["go"].ModelPrefix != "go-" {
		t.Fatalf("prefix = %q", cfg.Providers["go"].ModelPrefix)
	}
	if cfg.Providers["go"].ExposedName("glm-5.1") != "go-glm-5.1" {
		t.Errorf("exposed name = %q", cfg.Providers["go"].ExposedName("glm-5.1"))
	}
	// The prefixed name resolves, and the legacy provider/model form does too.
	if id, model, pool, found := cfg.ResolveProviderModel("go-glm-5.1"); !found || id != "go" || model != "glm-5.1" || len(pool) != 0 {
		t.Errorf("prefixed resolve = %q %q %v %v", id, model, pool, found)
	}
	if id, model, _, found := cfg.ResolveProviderModel("go/glm-5.1"); !found || id != "go" || model != "glm-5.1" {
		t.Errorf("legacy FQN resolve = %q %q %v", id, model, found)
	}
	// Without a prefix the bare ID is the exposed name.
	if id, model, _, found := cfg.ResolveProviderModel("deepseek-flash"); !found || id != "plain" || model != "deepseek-flash" {
		t.Errorf("bare resolve = %q %q %v", id, model, found)
	}
	// The un-prefixed ID of a prefixed provider is not a routing name.
	if _, _, _, found := cfg.ResolveProviderModel("glm-5.1"); found {
		t.Error("a prefixed provider must not answer to the bare model ID")
	}

	// A prefix with a slash or whitespace is rejected: it would be ambiguous
	// with the legacy provider/model form.
	bad := strings.ReplaceAll(yaml, `modelPrefix: "go-"`, `modelPrefix: "go/inner-"`)
	if _, err := loadProvidersYAML(t, bad); err == nil || !strings.Contains(err.Error(), "modelPrefix") {
		t.Fatalf("slash prefix must be rejected, got %v", err)
	}
}

func TestConfig_ProviderCooldownPolicy(t *testing.T) {
	t.Setenv("PROVIDER_TEST_KEY", "sk-test-1")
	yaml := `
models:
  local-model:
    cmd: echo ${PORT}
providers:
  off:
    cooldown: disabled
    endpoint: https://x.com
    apiKeys: [${env.PROVIDER_TEST_KEY}]
    models: [m1]
  tuned:
    cooldown:
      policy: enabled
      auth: 30s
      transient: 5s
    endpoint: https://y.com
    apiKeys: [${env.PROVIDER_TEST_KEY}]
    models: [m2]
`
	cfg, err := loadProvidersYAML(t, yaml)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.Providers["off"].Cooldown.Disabled() {
		t.Error("scalar 'disabled' must be recognized")
	}
	if cfg.Providers["tuned"].Cooldown.Policy != ProviderCooldownEnabled {
		t.Errorf("policy = %q", cfg.Providers["tuned"].Cooldown.Policy)
	}
	if cfg.Providers["tuned"].Cooldown.Auth != "30s" || cfg.Providers["tuned"].Cooldown.Transient != "5s" {
		t.Errorf("durations = %+v", cfg.Providers["tuned"].Cooldown)
	}
	if cfg.Providers["off"].Cooldown.Policy == ProviderCooldownEnabled {
		t.Error("policy must differ per provider")
	}

	bad := strings.ReplaceAll(yaml, "cooldown: disabled", "cooldown: sometimes")
	if _, err := loadProvidersYAML(t, bad); err == nil || !strings.Contains(err.Error(), "cooldown policy must be") {
		t.Fatalf("invalid policy must be rejected, got %v", err)
	}
	bad2 := strings.ReplaceAll(yaml, "transient: 5s", "transient: soon")
	if _, err := loadProvidersYAML(t, bad2); err == nil || !strings.Contains(err.Error(), "not a duration") {
		t.Fatalf("invalid duration must be rejected, got %v", err)
	}
}

func TestConfig_GatewayCooldownDurationValidation(t *testing.T) {
	t.Setenv("PROVIDER_TEST_KEY", "sk-test-1")
	bad := `
models:
  local-model:
    cmd: echo ${PORT}
gateway:
  cooldown:
    authProbe: soon
providers:
  p:
    endpoint: https://x.com
    apiKeys: [${env.PROVIDER_TEST_KEY}]
    models: [m]
`
	if _, err := loadProvidersYAML(t, bad); err == nil || !strings.Contains(err.Error(), "gateway.cooldown.authProbe") {
		t.Fatalf("unparsable duration must be rejected, got %v", err)
	}
}

func TestConfig_ProviderModelAliases(t *testing.T) {
	t.Setenv("PROVIDER_TEST_KEY", "sk-test-1")
	yaml := `
models:
  local-model:
    cmd: echo ${PORT}
providers:
  go:
    endpoint: https://opencode.ai/inference/go
    apiKeys: [${env.PROVIDER_TEST_KEY}]
    models:
      - remote: gemini-4-argon-contributor
        local: gemini-4-argon
        aliases: [" argon-short ", "gemini-4-argon", "", "another-alias"]
`
	cfg, err := loadProvidersYAML(t, yaml)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	models := cfg.Providers["go"].AllModels()
	if len(models) != 1 {
		t.Fatalf("models = %+v", models)
	}
	// Trimmed, de-duplicated against the local name and each other.
	if got := models[0].Aliases; len(got) != 2 || got[0] != "argon-short" || got[1] != "another-alias" {
		t.Fatalf("aliases = %v", got)
	}
	// Names() is what the entry declares; ModelNames() adds the upstream name,
	// which stays callable by default so renaming a model for clients never
	// takes the vendor's own name away.
	names := models[0].Names()
	if len(names) != 3 || names[0] != "gemini-4-argon" {
		t.Fatalf("names = %v", names)
	}
	callable := cfg.Providers["go"].ModelNames(models[0])
	if len(callable) != 4 || callable[3] != "gemini-4-argon-contributor" {
		t.Fatalf("callable names = %v", callable)
	}

	// Every name resolves to the same local model id, including the upstream
	// name and the legacy provider/name form.
	for _, search := range []string{"gemini-4-argon", "argon-short", "another-alias", "gemini-4-argon-contributor", "go/gemini-4-argon-contributor", "go/argon-short"} {
		id, model, pool, found := cfg.ResolveProviderModel(search)
		if !found || id != "go" || model != "gemini-4-argon" || len(pool) != 0 {
			t.Errorf("resolve %q = %q %q %v %v", search, id, model, pool, found)
		}
	}
	// The upstream name is callable: a client may use an alias or the name the
	// vendor itself uses, and an entry that sets no local name takes that name.
	if _, model, _, found := cfg.ResolveProviderModel("gemini-4-argon-contributor"); !found || model != "gemini-4-argon" {
		t.Errorf("the upstream name did not resolve to the entry: %q %v", model, found)
	}
}

// TestConfig_ProviderUpstreamNameSwitch covers the knob: exposeUpstreamName:
// false publishes only the names the provider declares.
func TestConfig_ProviderUpstreamNameSwitch(t *testing.T) {
	t.Setenv("PROVIDER_TEST_KEY", "sk-test-1")
	yaml := `
providers:
  go:
    endpoint: https://opencode.ai/inference/go
    exposeUpstreamName: false
    apiKeys: [${env.PROVIDER_TEST_KEY}]
    models:
      - remote: gemini-4-argon-contributor
        local: gemini-4-argon
        aliases: [argon-short]
`
	cfg, err := loadProvidersYAML(t, yaml)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	models := cfg.Providers["go"].AllModels()
	if callable := cfg.Providers["go"].ModelNames(models[0]); len(callable) != 2 {
		t.Fatalf("callable names = %v, want the local name and the alias only", callable)
	}
	for _, search := range []string{"gemini-4-argon", "argon-short"} {
		if _, _, _, found := cfg.ResolveProviderModel(search); !found {
			t.Errorf("declared name %q no longer resolves", search)
		}
	}
	for _, search := range []string{"gemini-4-argon-contributor", "go/gemini-4-argon-contributor"} {
		if _, _, _, found := cfg.ResolveProviderModel(search); found {
			t.Errorf("upstream name %q still resolves with exposeUpstreamName: false", search)
		}
	}
}

func TestConfig_ProviderModelAliasCollision(t *testing.T) {
	t.Setenv("PROVIDER_TEST_KEY", "sk-test-1")
	// An alias may collide with another model's name: the model keeps the
	// name, the alias only adds a second surface for it (see the gateway's
	// two-pass registration).
	dup := `
models:
  local-model:
    cmd: echo ${PORT}
providers:
  p:
    endpoint: https://x.com
    apiKeys: [${env.PROVIDER_TEST_KEY}]
    models:
      - {local: a, remote: a}
      - {local: b, remote: b, aliases: [a]}
`
	cfg, err := loadProvidersYAML(t, dup)
	if err != nil {
		t.Fatalf("alias collision must be accepted, got %v", err)
	}
	if _, _, _, found := cfg.ResolveProviderModel("a"); !found {
		t.Fatal("the colliding name must still resolve to the provider")
	}
}

func TestConfig_ProviderAdapterTypes(t *testing.T) {
	t.Setenv("PROVIDER_TEST_KEY", "sk-test-1")
	yaml := `
models:
  local-model:
    cmd: echo ${PORT}
providers:
  go:
    apiKeys: [${env.PROVIDER_TEST_KEY}]
    adapters:
      - name: Responses
        type: openai-responses
        endpoint: https://x.com
        models: [m1]
      - name: Completions
        type: openai
        endpoint: https://y.com
        models: [m2]
      - name: Messages
        type: anthropic
        endpoint: https://z.com
        models: [m3]
`
	cfg, err := loadProvidersYAML(t, yaml)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	adapters := cfg.Providers["go"].EffectiveAdapters()
	if adapters[0].Type != ProviderTypeOpenAIResponses || adapters[1].Type != ProviderTypeOpenAI || adapters[2].Type != ProviderTypeAnthropic {
		t.Fatalf("types = %q %q %q", adapters[0].Type, adapters[1].Type, adapters[2].Type)
	}

	bad := strings.ReplaceAll(yaml, "type: openai-responses", "type: responses")
	if _, err := loadProvidersYAML(t, bad); err == nil || !strings.Contains(err.Error(), "type must be one of") {
		t.Fatalf("unknown type must be rejected, got %v", err)
	}
}

func TestConfig_ProviderModelOnSeveralSurfaces(t *testing.T) {
	t.Setenv("PROVIDER_TEST_KEY", "sk-test-1")
	// One model exposed by two adapters of the same provider is allowed: they
	// are two API surfaces of one upstream and the gateway picks between them.
	yaml := `
models:
  local-model:
    cmd: echo ${PORT}
providers:
  go:
    apiKeys: [${env.PROVIDER_TEST_KEY}]
    adapters:
      - name: Responses
        type: openai-responses
        endpoint: https://x.com
        models: [claude-haiku-5-5]
      - name: Messages
        type: anthropic
        endpoint: https://x.com
        models: [claude-haiku-5-5]
`
	cfg, err := loadProvidersYAML(t, yaml)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if id, model, pool, found := cfg.ResolveProviderModel("claude-haiku-5-5"); !found || id != "go" || model != "claude-haiku-5-5" || len(pool) != 0 {
		t.Fatalf("resolve = %q %q %v %v", id, model, pool, found)
	}
	if len(cfg.Providers["go"].AllModels()) != 2 {
		t.Fatalf("both declarations must survive: %+v", cfg.Providers["go"].AllModels())
	}
}
