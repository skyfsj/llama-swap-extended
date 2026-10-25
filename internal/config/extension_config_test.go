package config

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestRuntimeConfigValidateRejectsUnsafeSources(t *testing.T) {
	base := RuntimeConfig{Kind: "vllm", Source: RuntimeSource{Type: "pypi"}}
	if err := base.Validate("vllm"); err != nil {
		t.Fatal(err)
	}
	unsafe := base
	unsafe.Source.Checksum = "not-a-checksum"
	if err := unsafe.Validate("vllm"); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("checksum validation error = %v", err)
	}
	unsafe = base
	unsafe.Source.Type = "unknown"
	if err := unsafe.Validate("vllm"); err == nil {
		t.Fatal("unknown source type should be rejected")
	}
	unsafe = base
	unsafe.Source.Path = "relative/runtime"
	if err := unsafe.Validate("vllm"); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative source path error = %v", err)
	}
	unsafe = base
	unsafe.Source.URL = "file://remote.example/runtime"
	if err := unsafe.Validate("vllm"); err == nil || !strings.Contains(err.Error(), "HTTPS or file") {
		t.Fatalf("remote file URL error = %v", err)
	}
	unsafe = base
	unsafe.Source.URL = "file:///%2e%2e/etc/runtime"
	if err := unsafe.Validate("vllm"); err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("traversal file URL error = %v", err)
	}
}

func TestRuntimeManagerValidateSourceAllowlist(t *testing.T) {
	valid := Config{RuntimeManager: RuntimeManagerConfig{SourceAllowlist: []string{
		"/var/lib/llama-swap/wheels",
		"https://pypi.org/simple",
		"git@github.com:ggml-org/llama.cpp.git",
		"git@[2001:db8::1]:ggml-org/llama.cpp.git",
	}}}
	if err := valid.ValidateExtensionConfig(); err != nil {
		t.Fatalf("valid source allowlist rejected: %v", err)
	}
	for _, entry := range []string{
		"",
		"relative/mirror",
		"http://mirror.example/simple",
		"https://user:pass@mirror.example/simple",
		"https://mirror.example/simple?token=secret",
		"git@github.com",
		"git@[2001:db8::1]ggml-org/llama.cpp.git",
		"git@[2001:db8::1]:",
	} {
		cfg := Config{RuntimeManager: RuntimeManagerConfig{SourceAllowlist: []string{entry}}}
		if err := cfg.ValidateExtensionConfig(); err == nil {
			t.Errorf("unsafe source allowlist entry accepted: %q", entry)
		}
	}
}

func TestRuntimeConfigValidateAcceptsReleaseChannelAndGitRefSources(t *testing.T) {
	tests := []struct {
		name string
		cfg  RuntimeConfig
	}{
		{
			name: "release",
			cfg:  RuntimeConfig{Kind: "llamacpp", Source: RuntimeSource{Type: "release", URL: "https://example.test/llama.tar.gz"}},
		},
		{
			name: "channel",
			cfg:  RuntimeConfig{Kind: "llamacpp", Source: RuntimeSource{Type: "channel", URL: "https://example.test/nightly.tar.gz"}},
		},
		{
			name: "tag",
			cfg:  RuntimeConfig{Kind: "llamacpp", Source: RuntimeSource{Type: "tag", Repository: "https://github.com/example/llama.cpp.git", Ref: "v1.2.3"}},
		},
		{
			name: "commit",
			cfg:  RuntimeConfig{Kind: "llamacpp", Source: RuntimeSource{Type: "commit", Repository: "https://github.com/example/llama.cpp.git", Ref: "0123456789abcdef"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.Validate(tt.name); err != nil {
				t.Fatalf("source validation failed: %v", err)
			}
		})
	}
}

func TestRuntimeConfigValidateAcceptsContainerAndCustomBuild(t *testing.T) {
	cfg := RuntimeConfig{
		Kind: "vllm", Mode: "container",
		Source:    RuntimeSource{Type: "image", Image: "ghcr.io/1catai/vllm:stable", Platform: "linux/amd64", PullPolicy: "if-missing"},
		Container: RuntimeContainerConfig{Engine: "docker", Name: "vllm-runtime", GPUs: "all", ShmSize: "16g", Command: []string{"serve", "/models/Qwen3"}},
		Build:     RuntimeBuildConfig{Driver: "custom", WorkDir: "${SOURCE_DIR}", Env: map[string]string{"CUDA_HOME": "/usr/local/cuda"}, Steps: []RuntimeBuildStep{{Command: "make", Args: []string{"-j4"}}}, Artifacts: []RuntimeArtifact{{From: "build/bin/llama-server", To: "bin/llama-server"}}},
	}
	if err := cfg.Validate("vllm-runtime"); err != nil {
		t.Fatal(err)
	}
	// Prebuilt release archives need no build at all: driver none is the
	// explicit way to say "materialize the fetched tree, compile nothing".
	if err := (RuntimeConfig{
		Kind:   "llamacpp",
		Source: RuntimeSource{Type: "release", URL: "https://example.test/llama-b1.tar.gz", Checksum: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		Build:  RuntimeBuildConfig{Driver: "none"},
	}).Validate("llama-release"); err != nil {
		t.Fatalf("driver none rejected: %v", err)
	}
	for _, bad := range []RuntimeConfig{
		{Kind: "vllm", Mode: "container", Source: RuntimeSource{Type: "image", Image: "https://example.test/image"}},
		{Kind: "llamacpp", Source: RuntimeSource{Type: "git", Repository: "https://github.com/example/llama.cpp.git", Ref: "main"}, Build: RuntimeBuildConfig{Driver: "make", WorkDir: "../outside"}},
		{Kind: "vllm", Source: RuntimeSource{Type: "pypi"}, Build: RuntimeBuildConfig{Steps: []RuntimeBuildStep{{Command: "make\n"}}}},
		{Kind: "llamacpp", Source: RuntimeSource{Type: "release", URL: "https://example.test/llama-b1.tar.gz", Checksum: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}, Build: RuntimeBuildConfig{Driver: "none", Steps: []RuntimeBuildStep{{Command: "rsync", Args: []string{"-a", "x/"}}}}},
	} {
		if err := bad.Validate("bad"); err == nil {
			t.Fatalf("unsafe runtime accepted: %+v", bad)
		}
	}
}

func TestRuntimeConfigValidateAcceptsContainerImageInContainerBlock(t *testing.T) {
	cfg := RuntimeConfig{
		Kind:      "vllm",
		Mode:      "container",
		Container: RuntimeContainerConfig{Engine: "docker", Image: "ghcr.io/example/vllm:stable"},
	}
	if err := cfg.Validate("vllm-container"); err != nil {
		t.Fatalf("container image fallback rejected: %v", err)
	}
}

func TestRuntimeManagerValidateContainerRegistries(t *testing.T) {
	if err := (Config{RuntimeManager: RuntimeManagerConfig{ContainerRegistries: []string{"docker.io", "ghcr.io:443"}}}).ValidateExtensionConfig(); err != nil {
		t.Fatal(err)
	}
	for _, registry := range []string{"https://docker.io", "docker.io/path", "user@docker.io", ""} {
		if err := (Config{RuntimeManager: RuntimeManagerConfig{ContainerRegistries: []string{registry}}}).ValidateExtensionConfig(); err == nil {
			t.Fatalf("unsafe registry accepted: %q", registry)
		}
	}
}

func TestRuntimeConfigValidateRejectsIncompleteArchiveAndRefSources(t *testing.T) {
	tests := []RuntimeConfig{
		{Kind: "llamacpp", Source: RuntimeSource{Type: "release"}},
		{Kind: "llamacpp", Source: RuntimeSource{Type: "channel"}},
		{Kind: "llamacpp", Source: RuntimeSource{Type: "tag", Repository: "https://github.com/example/llama.cpp.git"}},
		{Kind: "llamacpp", Source: RuntimeSource{Type: "commit", URL: "https://github.com/example/llama.cpp.git"}},
	}
	for _, cfg := range tests {
		if err := cfg.Validate("runtime"); err == nil {
			t.Fatalf("expected incomplete source to be rejected: %+v", cfg.Source)
		}
	}
}

func TestRuntimeConfigValidateRejectsMalformedRepositories(t *testing.T) {
	for _, repository := range []string{
		"git@github.com",
		"git@[2001:db8::1]ggml-org/llama.cpp.git",
		"https://github.com",
		"https://github.com/ggml-org/llama.cpp.git?token=secret",
		"https://user:pass@github.com/ggml-org/llama.cpp.git",
	} {
		cfg := RuntimeConfig{Kind: "llamacpp", Source: RuntimeSource{Type: "tag", Repository: repository, Ref: "v1.0.0"}}
		if err := cfg.Validate("runtime"); err == nil {
			t.Errorf("malformed repository was accepted: %q", repository)
		}
	}
}

func TestBackendConfigValidateRejectsNULArguments(t *testing.T) {
	backend := BackendConfig{Type: "vllm", Arguments: []string{"serve\x00"}}
	if err := backend.Validate("model"); err == nil || !strings.Contains(err.Error(), "NUL") {
		t.Fatalf("backend validation error = %v", err)
	}
}

func TestBackendConfigValidateRejectsUnknownAPIs(t *testing.T) {
	if err := (BackendConfig{Type: "vllm", APIs: []string{"respones"}}).Validate("model"); err == nil || !strings.Contains(err.Error(), "unsupported capability") {
		t.Fatalf("unknown backend API validation error = %v", err)
	}
	for _, api := range []string{"chat", "responses", "embeddings", "anthropic", "realtime", "all", "*"} {
		if err := (BackendConfig{Type: "vllm", APIs: []string{api}}).Validate("model"); err != nil {
			t.Fatalf("known backend API %q rejected: %v", api, err)
		}
	}
}

func TestExtensionConfigValidateRejectsUnsafeRuntimeReferences(t *testing.T) {
	for _, name := range []string{"../escape", "nested/runtime", "runtime name", "runtime\\name", "runtime\x00name", "runtime\u00a0name", "runtime\u200bname", "runtime\u2003name"} {
		cfg := Config{Runtimes: map[string]RuntimeConfig{
			name: {Kind: "vllm", Source: RuntimeSource{Type: "pypi"}},
		}}
		if err := cfg.ValidateExtensionConfig(); err == nil {
			t.Errorf("unsafe runtime name was accepted: %q", name)
		}
	}
	for _, name := range []string{"../escape", "nested/runtime", "runtime name", "runtime\\name", "runtime\x00name", "runtime\u00a0name", "runtime\u200bname", "runtime\u2003name"} {
		cfg := BackendConfig{Type: "vllm", Runtime: name}
		if err := cfg.Validate("model"); err == nil {
			t.Errorf("unsafe backend runtime reference was accepted: %q", name)
		}
	}
	if err := (BackendConfig{Type: "vllm", Runtime: "vllm-cuda_12.4"}).Validate("model"); err != nil {
		t.Fatalf("valid runtime reference rejected: %v", err)
	}
}

func TestExtensionConfigValidateManagedRuntimeModelContracts(t *testing.T) {
	base := Config{
		RuntimeManager: RuntimeManagerConfig{Root: t.TempDir()},
		Runtimes: map[string]RuntimeConfig{
			"llama-cpu": {Kind: "llamacpp", Source: RuntimeSource{Type: "local", Path: t.TempDir()}},
			"vllm-cpu":  {Kind: "vllm", Source: RuntimeSource{Type: "pypi"}},
		},
	}
	valid := base
	valid.Models = map[string]ModelConfig{
		"llama": {Backend: BackendConfig{Type: "llamacpp", Runtime: "llama-cpu", Arguments: []string{"llama-server", "--model", "model", "--port", "8080"}}},
		"vllm":  {Backend: BackendConfig{Type: "vllm", Runtime: "vllm-cpu", Arguments: []string{"vllm", "serve", "model"}}},
	}
	if err := valid.ValidateExtensionConfig(); err != nil {
		t.Fatalf("valid managed runtime models rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{
			name: "unknown runtime",
			mutate: func(cfg *Config) {
				cfg.Models["llama"] = ModelConfig{Backend: BackendConfig{Type: "llamacpp", Runtime: "missing", Arguments: []string{"llama-server"}}}
			},
			want: "does not exist",
		},
		{
			name: "kind mismatch",
			mutate: func(cfg *Config) {
				cfg.Models["llama"] = ModelConfig{Backend: BackendConfig{Type: "vllm", Runtime: "llama-cpu", Arguments: []string{"vllm"}}}
			},
			want: "must match",
		},
		{
			name: "legacy command",
			mutate: func(cfg *Config) {
				cfg.Models["llama"] = ModelConfig{Cmd: "llama-server", Backend: BackendConfig{Type: "llamacpp", Runtime: "llama-cpu", Arguments: []string{"llama-server"}}}
			},
			want: "legacy cmd",
		},
		{
			name: "missing model target",
			mutate: func(cfg *Config) {
				cfg.Models["vllm"] = ModelConfig{Backend: BackendConfig{Type: "vllm", Runtime: "vllm-cpu", Arguments: []string{"vllm", "serve", "--dtype", "float16"}}}
			},
			want: "must include a model target",
		},
		{
			name: "dangling model flag",
			mutate: func(cfg *Config) {
				cfg.Models["vllm"] = ModelConfig{Backend: BackendConfig{Type: "vllm", Runtime: "vllm-cpu", Arguments: []string{"vllm", "serve", "--model"}}}
			},
			want: "missing its value",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			cfg.Models = make(map[string]ModelConfig, len(valid.Models))
			for name, model := range valid.Models {
				cfg.Models[name] = model
			}
			tt.mutate(&cfg)
			if err := cfg.ValidateExtensionConfig(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ValidateExtensionConfig error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestRuntimeManager_LegacyEnabledFieldDoesNotDisableManager(t *testing.T) {
	root := t.TempDir()
	cfg, err := LoadConfigFromReader(strings.NewReader(fmt.Sprintf(`
runtimeManager:
  enabled: false
  root: %q
runtimes:
  llama:
    kind: llamacpp
    source: {type: local, path: %q}
models:
  llama:
    proxy: http://127.0.0.1:9000
    backend:
      type: llamacpp
      runtime: llama
      args: [llama-server, --model, /models/model.gguf]
`, root, root)))
	if err != nil {
		t.Fatalf("legacy enabled field disabled or invalidated Runtime Manager: %v", err)
	}
	if cfg.RuntimeManager.Root != root {
		t.Fatalf("runtime root = %q, want %q", cfg.RuntimeManager.Root, root)
	}
}

func TestRuntimeUpdateEffectiveDefaultsAndExplicitValues(t *testing.T) {
	defaults := (RuntimeUpdateConfig{}).Effective()
	if defaults.Policy != "automatic" || defaults.Channel != "stable" || defaults.CheckEvery != DefaultRuntimeCheckEvery || defaults.MinIdle != DefaultRuntimeMinIdle || defaults.KeepVersions != DefaultRuntimeKeep || !defaults.ActivateOnlyIdle || !defaults.RollbackOnFailure {
		t.Fatalf("defaults = %+v", defaults)
	}
	var parsed RuntimeConfig
	if err := yaml.Unmarshal([]byte("update:\n  policy: manual\n  channel: prerelease\n  checkEvery: 0s\n  minIdle: 0s\n  activateOnlyWhenIdle: false\n  keepVersions: 0\n  rollbackOnFailure: false\n"), &parsed); err != nil {
		t.Fatal(err)
	}
	effective := parsed.Effective().Update
	if effective.Policy != "manual" || effective.Channel != "prerelease" || effective.CheckEvery != 0 || effective.MinIdle != 0 || effective.ActivateOnlyIdle || effective.KeepVersions != 0 || effective.RollbackOnFailure {
		t.Fatalf("explicit values were overwritten: %+v", effective)
	}
	if DefaultRuntimeMinIdle != 30*time.Minute {
		t.Fatalf("unexpected idle default: %v", DefaultRuntimeMinIdle)
	}
}

func TestRuntimeUpdateDurationStringDecoding(t *testing.T) {
	var parsed RuntimeUpdateConfig
	if err := yaml.Unmarshal([]byte("checkEvery: 12h\nminIdle: 45m\n"), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.CheckEvery != 12*time.Hour || parsed.MinIdle != 45*time.Minute {
		t.Fatalf("durations = %v/%v", parsed.CheckEvery, parsed.MinIdle)
	}
}

func TestRuntimeContainerAndManagerDurationStringDecoding(t *testing.T) {
	var container RuntimeContainerConfig
	if err := yaml.Unmarshal([]byte("stopTimeout: 15s\nimage: ghcr.io/example/vllm:stable\n"), &container); err != nil {
		t.Fatal(err)
	}
	if container.StopTimeout != 15*time.Second || container.Image == "" {
		t.Fatalf("container = %+v", container)
	}
	var manager RuntimeManagerConfig
	if err := yaml.Unmarshal([]byte("operationTimeout: 2m\ncontainerRegistries: [ghcr.io]\n"), &manager); err != nil {
		t.Fatal(err)
	}
	if manager.OperationTimeout != 2*time.Minute || len(manager.ContainerRegistries) != 1 || manager.ContainerRegistries[0] != "ghcr.io" {
		t.Fatalf("manager = %+v", manager)
	}
}

func TestLoadConfigAppliesManagedRuntimeUpdateDefaults(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader("runtimes:\n  vllm:\n    kind: vllm\n    source: {type: pypi}\nmodels: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	update := cfg.Runtimes["vllm"].Update
	if update.Policy != "automatic" || update.Channel != "stable" || update.MinIdle != DefaultRuntimeMinIdle || update.KeepVersions != DefaultRuntimeKeep || !update.ActivateOnlyIdle || !update.RollbackOnFailure {
		t.Fatalf("runtime update defaults = %+v", update)
	}
}

// TestBackendConfig_EffectiveProtocol pins the Chat-only upstream format while
// keeping the legacy protocol values valid in configuration.
func TestBackendConfig_EffectiveProtocol(t *testing.T) {
	for _, tc := range []struct {
		name     string
		declared string
		want     string
	}{
		{"unset converts", "", AdapterProtocolResponsesToChat},
		{"whitespace converts", "   ", AdapterProtocolResponsesToChat},
		{"legacy native is normalized to chat", AdapterProtocolNative, AdapterProtocolResponsesToChat},
		{"padded legacy native is normalized to chat", " native ", AdapterProtocolResponsesToChat},
		{"explicit conversion", AdapterProtocolResponsesToChat, AdapterProtocolResponsesToChat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := BackendConfig{Protocol: tc.declared}
			if got := backend.EffectiveProtocol(); got != tc.want {
				t.Fatalf("EffectiveProtocol(%q) = %q, want %q", tc.declared, got, tc.want)
			}
		})
	}
}

func TestBackendConfig_AdaptsResponses(t *testing.T) {
	for _, tc := range []struct {
		protocol string
		want     bool
	}{
		{"", true},
		{AdapterProtocolResponsesToChat, true},
		{AdapterProtocolNative, true},
	} {
		if got := (BackendConfig{Protocol: tc.protocol}).AdaptsResponses(); got != tc.want {
			t.Errorf("AdaptsResponses(%q) = %t, want %t", tc.protocol, got, tc.want)
		}
	}
}

func TestBackendConfigRejectsProtocolTargetsOtherThanChat(t *testing.T) {
	for _, protocol := range []string{"openaiChat", "openaiResponses", "anthropicMessages", "gemini"} {
		if err := (BackendConfig{Protocol: protocol}).Validate("model"); err == nil {
			t.Errorf("backend.protocol %q must not select a non-Chat upstream", protocol)
		}
	}
}

// TestModelsDevEnabled_DistinguishesUnsetFromFalse pins the switch's three
// states. A plain bool collapses "absent" and "explicitly off", which is what
// silently disabled synchronization for anyone who configured a URL or a refresh
// interval without restating the switch — and what made an explicit
// `enabled: false` impossible to honor.
func TestModelsDevEnabled_DistinguishesUnsetFromFalse(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want bool
	}{
		{"section absent", "models:\n  a:\n    cmd: srv\n    proxy: \"http://127.0.0.1:9000\"\n", true},
		{"empty block", "pricing:\n  modelsDev: {}\n", true},
		{"url only", "pricing:\n  modelsDev:\n    url: https://mirror.example/api.json\n", true},
		{"refreshEvery only", "pricing:\n  modelsDev:\n    refreshEvery: 1h\n", true},
		{"explicitly on", "pricing:\n  modelsDev:\n    enabled: true\n", true},
		{"explicitly off", "pricing:\n  modelsDev:\n    enabled: false\n", false},
		{"explicitly off alongside a url", "pricing:\n  modelsDev:\n    enabled: false\n    url: https://mirror.example/api.json\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadConfigFromReader(strings.NewReader(tc.yaml))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			modelsDev := cfg.EffectivePricing().ModelsDev
			if got := modelsDev.ModelsDevEnabled(); got != tc.want {
				t.Fatalf("ModelsDevEnabled()=%v want %v", got, tc.want)
			}
			// The defaults have to hold regardless of the switch, so an operator
			// who only sets the interval still gets a working source.
			if modelsDev.URL == "" {
				t.Error("EffectivePricing left the URL empty")
			}
			if modelsDev.RefreshEvery <= 0 {
				t.Errorf("EffectivePricing left RefreshEvery at %v", modelsDev.RefreshEvery)
			}
		})
	}
}

// TestEffectivePricing_KeepsConfiguredValues pins that the effective view does
// not overwrite what the operator wrote.
func TestEffectivePricing_KeepsConfiguredValues(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader(`
pricing:
  modelsDev:
    enabled: false
    refreshEvery: 6h
    url: https://mirror.example/api.json
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	modelsDev := cfg.EffectivePricing().ModelsDev
	if modelsDev.URL != "https://mirror.example/api.json" {
		t.Errorf("URL=%q want the configured mirror", modelsDev.URL)
	}
	if modelsDev.RefreshEvery != 6*time.Hour {
		t.Errorf("RefreshEvery=%v want 6h", modelsDev.RefreshEvery)
	}
	if modelsDev.ModelsDevEnabled() {
		t.Error("ModelsDevEnabled()=true want false")
	}
}

// TestEffectivePricing_AmbiguityDefaultAndValidation pins the ambiguity knob:
// absent reads as provider-first, a chosen policy is kept, and a typo fails at
// load time instead of silently reading as the default.
func TestEffectivePricing_AmbiguityDefaultAndValidation(t *testing.T) {
	t.Run("absent defaults to provider-first", func(t *testing.T) {
		cfg, err := LoadConfigFromReader(strings.NewReader("models:\n  a:\n    cmd: srv\n    proxy: \"http://127.0.0.1:9000\"\n"))
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if got := cfg.EffectivePricing().ModelsDev.Ambiguity; got != PricingAmbiguityProviderFirst {
			t.Fatalf("Ambiguity=%q want %q", got, PricingAmbiguityProviderFirst)
		}
	})

	for _, policy := range []string{PricingAmbiguityStrict, PricingAmbiguityProviderFirst, PricingAmbiguityLowest} {
		t.Run("accepts "+policy, func(t *testing.T) {
			yaml := "pricing:\n  modelsDev:\n    ambiguity: " + policy + "\n"
			cfg, err := LoadConfigFromReader(strings.NewReader(yaml))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if got := cfg.EffectivePricing().ModelsDev.Ambiguity; got != policy {
				t.Fatalf("Ambiguity=%q want %q", got, policy)
			}
		})
	}

	t.Run("rejects an unknown policy", func(t *testing.T) {
		yaml := "pricing:\n  modelsDev:\n    ambiguity: cheapest\n"
		if _, err := LoadConfigFromReader(strings.NewReader(yaml)); err == nil {
			t.Fatal("load accepted ambiguity: cheapest")
		} else if !strings.Contains(err.Error(), "ambiguity") {
			t.Fatalf("error %q does not mention ambiguity", err)
		}
	})
}

// TestProviderPricingRefs_Parse pins the provider-side pricing surface: a
// provider-level catalog id and per-model refs survive the loader, and absent
// fields stay empty so the lookup falls back to the provider name.
func TestProviderPricingRefs_Parse(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader(`
providers:
  DeepSeek:
    type: openai
    endpoint: https://api.deepseek.com
    apiKeys: [sk-test]
    models:
      - local: deepseek-flash
        remote: deepseek-flash
  opencode-go:
    type: openai
    endpoint: https://relay.example
    apiKeys: [sk-test]
    pricingProvider: zhipuai
    models:
      - local: glm-5.1
        remote: glm-5.1
        pricing:
          provider: opencode
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	deepseek := cfg.Providers["DeepSeek"]
	if deepseek.PricingProvider != "" {
		t.Errorf("PricingProvider=%q want empty (falls back to the provider id)", deepseek.PricingProvider)
	}
	if got := deepseek.Models[0].Pricing; got != (PricingRef{}) {
		t.Errorf("unconfigured pricing = %+v want zero", got)
	}

	relay := cfg.Providers["opencode-go"]
	if relay.PricingProvider != "zhipuai" {
		t.Errorf("PricingProvider=%q want zhipuai", relay.PricingProvider)
	}
	if got := relay.Models[0].Pricing; got.Provider != "opencode" || got.Model != "" {
		t.Errorf("model pricing = %+v want provider=opencode with empty model", got)
	}
}

// TestPricingVendors_ParseAndValidate pins the vendor override surface: entries
// survive the loader and merge over the built-in table at lookup time, and an
// empty prefix or vendor fails at load.
func TestPricingVendors_ParseAndValidate(t *testing.T) {
	t.Run("entries parse", func(t *testing.T) {
		cfg, err := LoadConfigFromReader(strings.NewReader(`
pricing:
  modelsDev:
    vendors:
      gemini: google
      qwen: alibaba
`))
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		vendors := cfg.Pricing.ModelsDev.Vendors
		if vendors["gemini"] != "google" || vendors["qwen"] != "alibaba" {
			t.Fatalf("vendors = %v, want gemini->google and qwen->alibaba", vendors)
		}
	})

	t.Run("empty prefix is rejected", func(t *testing.T) {
		yaml := "pricing:\n  modelsDev:\n    vendors:\n      \"\": google\n"
		if _, err := LoadConfigFromReader(strings.NewReader(yaml)); err == nil {
			t.Fatal("load accepted an empty vendor prefix")
		}
	})

	t.Run("empty vendor is rejected", func(t *testing.T) {
		yaml := "pricing:\n  modelsDev:\n    vendors:\n      gemini: \"\"\n"
		if _, err := LoadConfigFromReader(strings.NewReader(yaml)); err == nil {
			t.Fatal("load accepted an empty vendor")
		}
	})

	t.Run("built-in table covers the majors", func(t *testing.T) {
		for model, want := range map[string]string{
			// Production inventory: every name the vendors themselves list.
			"gemini-4-argon":           "google",
			"Qwen/Qwen3.6-35B-A3B-FP8": "alibaba",
			"qwen3.8-max":              "alibaba",
			"qvq-max":                  "alibaba",
			"qwq-plus":                 "alibaba",
			"glm-5.2":                  "zhipuai",
			"deepseek-flash":           "deepseek",
			"gpt-6-luna":               "openai",
			"gpt-5.6-terra":            "openai",
			"o3-mini":                  "openai",
			"o4-mini":                  "openai",
			"claude-haiku-5-5":         "anthropic",
			"kimi-k3":                  "moonshotai",
			"minimax-m3":               "minimax",
			"grok-4.7":                 "xai",
			"mimo-v2.6-flash":          "xiaomi",
			"step-5-preview":           "stepfun",
			"mistral-large-4":          "mistral",
			"codestral-latest":         "mistral",
			"ministral-8b-latest":      "mistral",
			"devstral-medium-latest":   "mistral",
			"pixtral-12b":              "mistral",
			"magistral-medium-latest":  "mistral",
			"voxtral-small-latest":     "mistral",
			"doubao-seed-2-0-pro":      "volcengine",
			"command-a-plus-05-2026":   "cohere",
			"sonar-pro":                "perplexity",
			"muse-spark-1.2":           "meta",
			"phi-4":                    "azure",
			// The prefix still resolves even when the vendor spells it with a
			// namespace; the exact row may or may not exist.
			"Qwen3.8-27B-Uncensored": "alibaba",
		} {
			vendor, ok := VendorForModel(PricingVendorTable(nil), model)
			if !ok || vendor != want {
				t.Errorf("%s -> %q ok=%v, want %q", model, vendor, ok, want)
			}
		}
	})

	t.Run("families without an official catalog entry stay unmapped", func(t *testing.T) {
		for _, model := range []string{"HY-MT2-1.8B", "nova-pro", "gemma-3-27b", "llama-4-maverick", "ernie-5.0"} {
			if vendor, ok := VendorForModel(PricingVendorTable(nil), model); ok {
				t.Errorf("%s unexpectedly resolved to %q", model, vendor)
			}
		}
	})
}
