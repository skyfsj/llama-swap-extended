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
    type: gemini
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
			name: "duplicate model",
			yaml: `
models:
  m0:
    cmd: echo ${PORT}
providers:
  p:
    endpoint: https://x.com
    apiKeys: [k]
    models: [m, m]`,
			wantErr: "duplicate model",
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
