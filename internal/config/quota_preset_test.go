package config

import (
	"strings"
	"testing"
)

func TestQuotaPresetLoads(t *testing.T) {
	presets, err := QuotaPresets()
	if err != nil {
		t.Fatalf("presets: %v", err)
	}
	if len(presets) == 0 {
		t.Fatal("no built-in quota presets")
	}
	preset, ok := QuotaPresetByID("opencode-go")
	if !ok {
		t.Fatal("opencode-go preset missing")
	}
	if preset.Endpoint != "https://opencode.ai/inference/go/v1/usage" {
		t.Errorf("endpoint = %q", preset.Endpoint)
	}
	if len(preset.Windows) != 3 || preset.Windows[0].ID != "rolling" || preset.Windows[2].ID != "monthly" {
		t.Errorf("windows = %+v", preset.Windows)
	}
	if preset.Windows[0].Path != "usage.rolling" || preset.Windows[0].Percent != "percent" || preset.Windows[0].PercentMode != "used" {
		t.Errorf("window0 = %+v", preset.Windows[0])
	}
	if preset.Auth.Style != QuotaAuthBearer {
		t.Errorf("auth = %+v", preset.Auth)
	}
}

// DeepSeek 的余额映射：没有百分比窗口，四行标量直接构成"额度"卡片内容。
// 合同见 api-docs.deepseek.com/zh-cn/api/get-user-balance。
func TestQuotaPreset_DeepSeekBalance(t *testing.T) {
	preset, ok := QuotaPresetByID("deepseek")
	if !ok {
		t.Fatal("deepseek 额度预设缺失")
	}
	if preset.Endpoint != "https://api.deepseek.com/user/balance" || preset.Method != "GET" {
		t.Fatalf("endpoint/method = %q %q", preset.Endpoint, preset.Method)
	}
	if preset.Auth.Style != QuotaAuthBearer {
		t.Fatalf("auth = %+v（余额接口用 Bearer 鉴权）", preset.Auth)
	}
	if len(preset.Windows) != 0 {
		t.Fatalf("余额没有百分比窗口，实际 = %+v", preset.Windows)
	}
	want := map[string]string{
		"available": "is_available",
		"total":     "balance_infos.0.total_balance",
		"granted":   "balance_infos.0.granted_balance",
		"topped_up": "balance_infos.0.topped_up_balance",
	}
	for id, path := range want {
		found := false
		for _, extra := range preset.Extras {
			if extra.ID == id {
				found = true
				if extra.Path != path {
					t.Errorf("extra %s path = %q，期望 %q", id, extra.Path, path)
				}
				if extra.Label == "" {
					t.Errorf("extra %s 缺少显示名称", id)
				}
			}
		}
		if !found {
			t.Errorf("缺少 extra %s（现有 %+v）", id, preset.Extras)
		}
	}
}

// 只有 extras 的额度必须能通过校验：这是余额式映射的前提。
func TestConfig_ProviderQuotaExtrasOnly(t *testing.T) {
	t.Setenv("PROVIDER_TEST_KEY", "sk-test-1")
	yaml := `
models:
  local-model:
    cmd: echo ${PORT}
providers:
  ds:
    type: openai
    endpoint: https://api.deepseek.com
    apiKeys: [${env.PROVIDER_TEST_KEY}]
    models: [deepseek-flash]
    quota:
      preset: deepseek
`
	cfg, err := loadProvidersYAML(t, yaml)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	quota := cfg.Providers["ds"].Quota
	if quota == nil {
		t.Fatal("quota block missing")
	}
	if quota.Endpoint != "https://api.deepseek.com/user/balance" {
		t.Errorf("endpoint = %q", quota.Endpoint)
	}
	if len(quota.Windows) != 0 || len(quota.Extras) == 0 {
		t.Errorf("windows=%d extras=%d，期望只有 extras", len(quota.Windows), len(quota.Extras))
	}
}

func TestConfig_ProviderQuotaPresetMergeAndOverrides(t *testing.T) {
	t.Setenv("PROVIDER_TEST_KEY", "sk-test-1")
	yaml := `
models:
  local-model:
    cmd: echo ${PORT}
providers:
  sub:
    type: openai
    endpoint: https://x.com
    apiKeys: [${env.PROVIDER_TEST_KEY}]
    models: [m1]
    quota:
      preset: opencode-go
      timeoutSeconds: 30
      windows:
        - {id: rolling, path: usage.rolling, percent: used_percent, percentMode: remaining}
`
	cfg, err := loadProvidersYAML(t, yaml)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	quota := cfg.Providers["sub"].Quota
	if quota == nil {
		t.Fatal("quota block missing")
	}
	// Preset supplies the endpoint and auth style.
	if quota.Endpoint != "https://opencode.ai/inference/go/v1/usage" || quota.Auth.Style != QuotaAuthBearer {
		t.Errorf("preset merge = %+v", quota)
	}
	// Inline values win over the preset.
	if quota.TimeoutSeconds != 30 {
		t.Errorf("timeout = %d, want the inline 30", quota.TimeoutSeconds)
	}
	if len(quota.Windows) != 1 || quota.Windows[0].Percent != "used_percent" || quota.Windows[0].PercentMode != "remaining" {
		t.Errorf("inline windows must replace the preset's: %+v", quota.Windows)
	}
	// The single window's label falls back to its id.
	if quota.Windows[0].Label != "rolling" {
		t.Errorf("label fallback = %q", quota.Windows[0].Label)
	}
	// A disabled enforce block stays empty instead of growing defaults.
	if quota.Enforce.Enabled || quota.Enforce.Threshold != 0 || quota.Enforce.FallbackCooldown != "" {
		t.Errorf("enforce defaults leaked: %+v", quota.Enforce)
	}
}

func TestConfig_ProviderQuotaEnforceDefaults(t *testing.T) {
	t.Setenv("PROVIDER_TEST_KEY", "sk-test-1")
	yaml := `
models:
  local-model:
    cmd: echo ${PORT}
providers:
  sub:
    type: openai
    endpoint: https://x.com
    apiKeys: [${env.PROVIDER_TEST_KEY}]
    models: [m1]
    quota:
      preset: opencode-go
      enforce: {enabled: true}
`
	cfg, err := loadProvidersYAML(t, yaml)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	quota := cfg.Providers["sub"].Quota
	if !quota.Enforce.Enabled || quota.Enforce.Threshold != QuotaDefaultThreshold {
		t.Errorf("enforce = %+v", quota.Enforce)
	}
	if quota.Enforce.FallbackCooldown != QuotaDefaultFallbackCooldown.String() {
		t.Errorf("fallback = %q", quota.Enforce.FallbackCooldown)
	}
	// Enforcement without an explicit interval refreshes in the background.
	if quota.RefreshInterval != QuotaDefaultEnforceRefreshInterval {
		t.Errorf("refreshInterval = %q, want %q", quota.RefreshInterval, QuotaDefaultEnforceRefreshInterval)
	}
	if quota.RefreshIntervalDuration() == 0 {
		t.Error("effective refresh interval must be > 0")
	}
}

func TestConfig_ProviderQuotaValidationErrors(t *testing.T) {
	t.Setenv("PROVIDER_TEST_KEY", "sk-test-1")
	cases := []struct {
		name    string
		quota   string
		wantErr string
	}{
		{"unknown preset", "      preset: nope", "is unknown"},
		{"missing endpoint", "      windows: [{id: a, path: a}]", "quota.endpoint is required"},
		{"bad scheme", "      endpoint: ftp://x.com\n      windows: [{id: a, path: a}]", "must use http or https"},
		{"nothing to show", "      endpoint: https://x.com/usage", "at least one window or extra"},
		{"window without path", "      endpoint: https://x.com/usage\n      windows: [{id: a}]", "needs a path"},
		{"duplicate window", "      endpoint: https://x.com/usage\n      windows: [{id: a, path: a}, {id: a, path: b}]", "duplicate quota window"},
		{"bad percent mode", "      endpoint: https://x.com/usage\n      windows: [{id: a, path: a, percentMode: half}]", "percentMode must be"},
		{"bad auth style", "      endpoint: https://x.com/usage\n      auth: {style: basic}\n      windows: [{id: a, path: a}]", "auth.style must be"},
		{"header auth without header", "      endpoint: https://x.com/usage\n      auth: {style: header}\n      windows: [{id: a, path: a}]", "auth.header is required"},
		{"threshold out of range", "      endpoint: https://x.com/usage\n      windows: [{id: a, path: a}]\n      enforce: {enabled: true, threshold: 120}", "threshold must be between"},
		{"refresh too fast", "      endpoint: https://x.com/usage\n      refreshInterval: 5s\n      windows: [{id: a, path: a}]", "at least 30s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yaml := `
models:
  local-model:
    cmd: echo ${PORT}
providers:
  sub:
    type: openai
    endpoint: https://x.com
    apiKeys: [${env.PROVIDER_TEST_KEY}]
    models: [m1]
    quota:
` + tc.quota + "\n"
			_, err := loadProvidersYAML(t, yaml)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestConfig_GatewayFailoverValidation(t *testing.T) {
	valid := GatewayFailoverConfig{Statuses: []int{400, 404, 500}, ExcludeStatuses: []int{413}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if !valid.CrossProviderEnabled() {
		t.Error("cross-provider failover must default to enabled")
	}
	disabled := GatewayFailoverConfig{CrossProvider: boolPointer(false)}
	if disabled.CrossProviderEnabled() {
		t.Error("explicit false must disable cross-provider failover")
	}
	for _, bad := range []GatewayFailoverConfig{
		{Statuses: []int{99}},
		{Statuses: []int{600}},
		{ExcludeStatuses: []int{-1}},
	} {
		if err := bad.Validate(); err == nil {
			t.Fatalf("invalid failover config accepted: %+v", bad)
		}
	}

	t.Setenv("PROVIDER_TEST_KEY", "sk-test-1")
	yaml := `
models:
  local-model:
    cmd: echo ${PORT}
gateway:
  failover:
    crossProvider: false
    statuses: [400, 429, 500, 502, 503]
    excludeStatuses: [400]
providers:
  p:
    endpoint: https://x.com
    apiKeys: [${env.PROVIDER_TEST_KEY}]
    models: [m1]
`
	cfg, err := loadProvidersYAML(t, yaml)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Gateway.Failover.CrossProviderEnabled() {
		t.Error("crossProvider: false not applied")
	}
	if len(cfg.Gateway.Failover.Statuses) != 5 || len(cfg.Gateway.Failover.ExcludeStatuses) != 1 {
		t.Errorf("failover = %+v", cfg.Gateway.Failover)
	}

	bad := strings.ReplaceAll(yaml, "excludeStatuses: [400]", "excludeStatuses: [999]")
	if _, err := loadProvidersYAML(t, bad); err == nil || !strings.Contains(err.Error(), "not an HTTP status code") {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func boolPointer(v bool) *bool { return &v }

// skip-slow 的校验：能表达出来的都不该是"看着配了却什么都不做"。
func TestConfig_GatewaySkipSlowValidation(t *testing.T) {
	t.Setenv("PROVIDER_TEST_KEY", "sk-test-1")
	load := func(block string) error {
		yaml := `
models:
  local-model:
    cmd: echo ${PORT}
providers:
  p:
    type: openai
    endpoint: https://x.com
    apiKeys: [${env.PROVIDER_TEST_KEY}]
    models: [m1]
gateway:
` + block + "\n"
		_, err := loadProvidersYAML(t, yaml)
		return err
	}

	if err := load("  skipSlow:\n    maxFirstTokenMs: 15000"); err != nil {
		t.Fatalf("绝对上限配置被拒：%v", err)
	}
	if err := load("  skipSlow:\n    relativeFactor: 2.5"); err != nil {
		t.Fatalf("相对倍数配置被拒：%v", err)
	}
	if err := load("  skipSlow:\n    enabled: false"); err != nil {
		t.Fatalf("显式关闭被拒：%v", err)
	}
	// 空块等于"全默认"：相对倍数规则按默认值开启，这正是默认行为，
	// 因此必须被接受（早期版本把它当笔误拒绝，结果每一份既有配置都加载失败）。
	if err := load("  skipSlow: {}"); err != nil {
		t.Fatalf("空的 skipSlow 块应表示全默认：%v", err)
	}
	// 但显式 enabled 却什么规则都没给，是自相矛盾的笔误。
	if err := load("  skipSlow:\n    enabled: true"); err == nil {
		t.Fatal("enabled: true 且无任何规则应被拒绝")
	}
	// 非法数值要报错，而不是悄悄取默认。
	for _, bad := range []string{
		"  skipSlow:\n    maxFirstTokenMs: -1",
		"  skipSlow:\n    relativeFactor: -2",
		"  skipSlow:\n    minSamples: -1",
	} {
		if err := load(bad); err == nil {
			t.Fatalf("非法配置未被拒绝：%q", bad)
		}
	}
}

// 只有绝对上限、未给倍数时，倍数规则应当保持关闭而不是回落到默认倍数
// （否则"我只想按上限筛"会被静默加上一条相对规则）。
func TestConfig_GatewaySkipSlowCeilingOnlyKeepsFactorOff(t *testing.T) {
	t.Setenv("PROVIDER_TEST_KEY", "sk-test-1")
	yaml := `
models:
  local-model:
    cmd: echo ${PORT}
providers:
  p:
    type: openai
    endpoint: https://x.com
    apiKeys: [${env.PROVIDER_TEST_KEY}]
    models: [m1]
gateway:
  skipSlow:
    maxFirstTokenMs: 12000
`
	cfg, err := loadProvidersYAML(t, yaml)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	skip := cfg.Gateway.SkipSlow
	if skip.MaxFirstTokenMs != 12000 {
		t.Fatalf("maxFirstTokenMs = %d", skip.MaxFirstTokenMs)
	}
	if skip.RelativeFactor != 0 {
		t.Fatalf("显式给了上限时不应带出默认倍数，实际 %v", skip.RelativeFactor)
	}
	if !skip.EnabledValue() {
		t.Fatal("写了 skipSlow 块应默认启用")
	}
	if skip.EffectiveMinSamples() != GatewaySkipSlowDefaultMinSamples {
		t.Fatalf("最小样本默认值 = %d", skip.EffectiveMinSamples())
	}
}
