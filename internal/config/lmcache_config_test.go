package config

import (
	"strings"
	"testing"
)

func TestConfig_LMCacheModuleDefaults(t *testing.T) {
	var m LMCacheModuleConfig
	if err := m.Validate(); err != nil {
		t.Fatalf("empty LMCache module must be valid: %v", err)
	}
	if got := m.EffectivePackage(); got != "lmcache" {
		t.Fatalf("EffectivePackage() = %q, want %q", got, "lmcache")
	}
	want := LMCacheServerConfig{
		Host: "localhost", Port: 5555, HTTPHost: "127.0.0.1", HTTPPort: 8900,
		L1SizeGB: 20, EvictionPolicy: "LRU",
		// 0 means auto: the --chunk-size flag is omitted and the server applies
		// its own library default.
		ChunkSize: 0,
	}
	if got := m.Server.Effective(); got != want {
		t.Fatalf("Server.Effective() = %+v, want %+v", got, want)
	}
}

func TestConfig_LMCacheModuleValidation(t *testing.T) {
	valid := LMCacheModuleConfig{
		Package:  "lmcache==0.4.1",
		IndexURL: "https://pypi.org/simple",
		Enabled:  true,
		Server:   LMCacheServerConfig{Enabled: true, EvictionPolicy: "IsolatedLRU", L1SizeGB: 8},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid configuration rejected: %v", err)
	}
	cases := map[string]struct {
		mutate func(*LMCacheModuleConfig)
		want   string
	}{
		"package shell metacharacters": {func(m *LMCacheModuleConfig) { m.Package = "lmcache && curl evil" }, "must be a simple pip requirement name"},
		"package version range":        {func(m *LMCacheModuleConfig) { m.Package = "lmcache>=0.3" }, "must be a simple pip requirement name"},
		"package leading dot":          {func(m *LMCacheModuleConfig) { m.Package = ".hidden" }, "must be a simple pip requirement name"},
		"http indexURL":                {func(m *LMCacheModuleConfig) { m.IndexURL = "http://pypi.org/simple" }, "must be an absolute HTTPS URL"},
		"indexURL credentials":         {func(m *LMCacheModuleConfig) { m.IndexURL = "https://user:pass@pypi.org" }, "must be an absolute HTTPS URL"},
		"server without module":        {func(m *LMCacheModuleConfig) { m.Enabled = false }, "requires lmcache.enabled"},
		"host leading space":           {func(m *LMCacheModuleConfig) { m.Server.Host = " localhost" }, "must be normalized"},
		"host embedded space":          {func(m *LMCacheModuleConfig) { m.Server.Host = "local host" }, "single host or IP literal"},
		"httpHost NUL":                 {func(m *LMCacheModuleConfig) { m.Server.HTTPHost = "bad\x00host" }, "single host or IP literal"},
		"port too large":               {func(m *LMCacheModuleConfig) { m.Server.Port = 65536 }, "lmcache.server.port"},
		"httpPort negative":            {func(m *LMCacheModuleConfig) { m.Server.HTTPPort = -1 }, "lmcache.server.httpPort"},
		"l1SizeGB negative":            {func(m *LMCacheModuleConfig) { m.Server.L1SizeGB = -1 }, "lmcache.server.l1SizeGB"},
		"chunkSize too large":          {func(m *LMCacheModuleConfig) { m.Server.ChunkSize = 1<<20 + 1 }, "lmcache.server.chunkSize"},
		"unknown eviction policy":      {func(m *LMCacheModuleConfig) { m.Server.EvictionPolicy = "FIFO" }, "lmcache.server.evictionPolicy"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := valid
			tc.mutate(&m)
			err := m.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tc.want)
			}
		})
	}
}

func TestConfig_LMCacheModuleValidation_AcceptsPythonMinorVersions(t *testing.T) {
	for _, pythonVersion := range []string{"3.11", "3.12.10"} {
		t.Run(pythonVersion, func(t *testing.T) {
			module := LMCacheModuleConfig{
				PythonVersion: pythonVersion,
				Enabled:       true,
				Server:        LMCacheServerConfig{Enabled: true},
			}
			if err := module.Validate(); err != nil {
				t.Fatalf("Validate() rejected Python %s: %v", pythonVersion, err)
			}
		})
	}
}

func TestConfig_LMCacheVersionResolution(t *testing.T) {
	cases := []struct {
		name string
		m    LMCacheModuleConfig
		want string
	}{
		{"no pin resolves nothing", LMCacheModuleConfig{}, ""},
		{"update version is canonical", LMCacheModuleConfig{Update: &RuntimeUpdateConfig{Version: "0.5.5"}, Version: "0.5.4"}, "0.5.5"},
		{"top-level version is compatibility alias", LMCacheModuleConfig{Version: "0.5.4"}, "0.5.4"},
		{"legacy package pin", LMCacheModuleConfig{Package: "lmcache==0.4.1"}, "0.4.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.m.EffectiveVersion(); got != tc.want {
				t.Fatalf("EffectiveVersion() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestConfig_LMCacheUpdateVersionConflictsWithLegacyAliases(t *testing.T) {
	cases := []LMCacheModuleConfig{
		{Version: "0.5.4", Update: &RuntimeUpdateConfig{Version: "0.5.5"}},
		{Package: "lmcache==0.5.4", Update: &RuntimeUpdateConfig{Version: "0.5.5"}},
	}
	for _, cfg := range cases {
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "lmcache.update.version") {
			t.Fatalf("Validate(%+v) = %v, want update.version conflict", cfg, err)
		}
	}
}

func TestConfig_LMCacheRejectsRollbackDisable(t *testing.T) {
	cfg := LMCacheModuleConfig{Update: &RuntimeUpdateConfig{RollbackOnFailure: false, rollbackOnFailureSet: true}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "rollbackOnFailure") {
		t.Fatalf("Validate() = %v, want rollbackOnFailure rejection", err)
	}
}

func TestConfig_LMCachePackageName(t *testing.T) {
	cases := map[string]struct {
		m    LMCacheModuleConfig
		want string
	}{
		"empty defaults":      {LMCacheModuleConfig{}, "lmcache"},
		"bare name":           {LMCacheModuleConfig{Package: "lmcache"}, "lmcache"},
		"extras stripped":     {LMCacheModuleConfig{Package: "lmcache[cu126]"}, "lmcache"},
		"pin stripped":        {LMCacheModuleConfig{Package: "lmcache==0.4.1"}, "lmcache"},
		"extras and pin":      {LMCacheModuleConfig{Package: "lmcache[cu126]==0.4.1"}, "lmcache"},
		"custom distribution": {LMCacheModuleConfig{Package: "lmcache-nightly"}, "lmcache-nightly"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := tc.m.PackageName(); got != tc.want {
				t.Fatalf("PackageName() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestConfig_LMCacheVersionConflicts(t *testing.T) {
	// A conflicting legacy pin must be rejected; an agreeing one is redundant
	// but harmless.
	both := LMCacheModuleConfig{Package: "lmcache==0.4.1", Version: "0.5.4"}
	if err := both.Validate(); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("Validate() = %v, want conflict error", err)
	}
	same := LMCacheModuleConfig{Package: "lmcache==0.4.1", Version: "0.4.1"}
	if err := same.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want the matching pin accepted", err)
	}
	bad := LMCacheModuleConfig{Version: "0.5.4\n"}
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "lmcache.version") {
		t.Fatalf("Validate() = %v, want invalid version rejected", err)
	}
}

func TestConfig_LMCacheRuntimeNameReserved(t *testing.T) {
	cfg := Config{Runtimes: map[string]RuntimeConfig{LMCacheRuntimeName: {Kind: "vllm"}}}
	if err := cfg.ValidateExtensionConfig(); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("ValidateExtensionConfig() = %v, want the reserved-name rejection", err)
	}
}

func TestConfig_LMCacheModelValidation(t *testing.T) {
	valid := LMCacheModelConfig{
		Enabled: true, Mode: LMCacheModeMP, Role: LMCacheRoleProducer,
		Host: "10.0.0.9", Port: 7000, ChunkSize: 512,
	}
	if err := valid.Validate("model"); err != nil {
		t.Fatalf("valid configuration rejected: %v", err)
	}
	cases := map[string]struct {
		mutate func(*LMCacheModelConfig)
		want   string
	}{
		"unknown mode":        {func(c *LMCacheModelConfig) { c.Mode = "gpu" }, "mode must be"},
		"unknown role":        {func(c *LMCacheModelConfig) { c.Role = "kv_all" }, "role must be"},
		"host leading space":  {func(c *LMCacheModelConfig) { c.Host = " 10.0.0.9" }, "host must be normalized"},
		"host embedded space": {func(c *LMCacheModelConfig) { c.Host = "10.0.0.9 extra" }, "single host or IP literal"},
		"port too large":      {func(c *LMCacheModelConfig) { c.Port = 65536 }, "port must be 0..65535"},
		"port negative":       {func(c *LMCacheModelConfig) { c.Port = -1 }, "port must be 0..65535"},
		"chunkSize negative":  {func(c *LMCacheModelConfig) { c.ChunkSize = -1 }, "chunkSize must be"},
		"chunkSize too large": {func(c *LMCacheModelConfig) { c.ChunkSize = 1<<20 + 1 }, "chunkSize must be"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := valid
			tc.mutate(&c)
			err := c.Validate("model")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tc.want)
			}
		})
	}
}

// TestConfig_LMCacheModelAttachmentModes covers the per-model attachment
// block's mode and role vocabulary. The non-standalone (inProcess) mode is
// spelled with a capital P, so the comparison must be case-insensitive: a
// case-sensitive compare rejects the documented spelling and makes the mode
// unreachable from a config file.
func TestConfig_LMCacheModelAttachmentModes(t *testing.T) {
	accepted := []string{"", "mp", "MP", "inProcess", "inprocess", "INPROCESS"}
	for _, mode := range accepted {
		c := LMCacheModelConfig{Enabled: true, Mode: mode, Role: "kv_both", Port: 5555, ChunkSize: 256}
		if err := c.Validate("model"); err != nil {
			t.Errorf("Validate(mode=%q) = %v, want accepted", mode, err)
		}
	}
	for _, mode := range []string{"standalone", "mpx"} {
		c := LMCacheModelConfig{Enabled: true, Mode: mode}
		if err := c.Validate("model"); err == nil || !strings.Contains(err.Error(), "backend.lmcache.mode") {
			t.Errorf("Validate(mode=%q) = %v, want an error naming backend.lmcache.mode", mode, err)
		}
	}
	for _, role := range []string{"", "kv_both", "kv_producer", "KV_CONSUMER"} {
		if err := (&LMCacheModelConfig{Enabled: true, Role: role}).Validate("model"); err != nil {
			t.Errorf("Validate(role=%q) = %v, want accepted", role, err)
		}
	}
	if err := (&LMCacheModelConfig{Enabled: true, Role: "kv_reader"}).Validate("model"); err == nil {
		t.Error("Validate(role=kv_reader) = nil, want an error")
	}
}

func TestConfig_LMCacheModelEffective(t *testing.T) {
	c := LMCacheModelConfig{Enabled: true}
	if got := c.EffectiveMode(); got != LMCacheModeMP {
		t.Fatalf("EffectiveMode() = %q, want %q", got, LMCacheModeMP)
	}
	if got := c.EffectiveRole(); got != LMCacheRoleBoth {
		t.Fatalf("EffectiveRole() = %q, want %q", got, LMCacheRoleBoth)
	}
	if got := c.EffectiveMPHost(); got != "localhost" {
		t.Fatalf("EffectiveMPHost() = %q, want %q", got, "localhost")
	}
	if got := c.EffectiveMPPort(); got != 5555 {
		t.Fatalf("EffectiveMPPort() = %d, want 5555", got)
	}

	custom := LMCacheModelConfig{Enabled: true, Mode: "inProcess", Role: "KV_PRODUCER", Host: " 10.0.0.9 ", Port: 7000, ChunkSize: 512}
	if got := custom.EffectiveMode(); got != LMCacheModeInProcess {
		t.Fatalf("EffectiveMode() = %q, want %q", got, LMCacheModeInProcess)
	}
	if got := custom.EffectiveRole(); got != LMCacheRoleProducer {
		t.Fatalf("EffectiveRole() = %q, want %q", got, LMCacheRoleProducer)
	}
	if got := custom.EffectiveMPHost(); got != "10.0.0.9" {
		t.Fatalf("EffectiveMPHost() = %q, want %q", got, "10.0.0.9")
	}
	if got := custom.EffectiveMPPort(); got != 7000 {
		t.Fatalf("EffectiveMPPort() = %d, want 7000", got)
	}
}

func TestConfig_LMCacheRequiresManagedVLLM(t *testing.T) {
	vllmModel := func() ModelConfig {
		return ModelConfig{Backend: BackendConfig{
			Type: "vllm", Runtime: "vllm",
			Arguments: []string{"vllm", "serve", "model"},
			LMCache:   &LMCacheModelConfig{Enabled: true},
		}}
	}
	cases := map[string]struct {
		cfg  Config
		want string
	}{
		"non-vllm backend": {
			cfg: Config{Models: map[string]ModelConfig{"model": {
				Backend: BackendConfig{Type: "llamacpp", Runtime: "llama", Arguments: []string{"llama-server"},
					LMCache: &LMCacheModelConfig{Enabled: true}},
			}}},
			want: "backend.lmcache is only supported for vllm backends",
		},
		"vllm without runtime": {
			cfg: Config{Models: map[string]ModelConfig{"model": {
				Backend: BackendConfig{Type: "vllm", Arguments: []string{"vllm", "serve"},
					LMCache: &LMCacheModelConfig{Enabled: true}},
			}}},
			want: "backend.lmcache requires a native managed vllm backend.runtime",
		},
		"container vllm runtime": {
			cfg: Config{
				Runtimes: map[string]RuntimeConfig{"vllm-container": {
					Kind: "vllm", Mode: "container",
					Source: RuntimeSource{Type: "image", Image: "ghcr.io/example/vllm:stable"},
				}},
				Models: map[string]ModelConfig{"model": {
					Backend: BackendConfig{Type: "vllm", Runtime: "vllm-container",
						LMCache: &LMCacheModelConfig{Enabled: true}},
				}},
			},
			want: "backend.lmcache requires a native vllm runtime",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := tc.cfg.ValidateExtensionConfig()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateExtensionConfig() = %v, want error containing %q", err, tc.want)
			}
		})
	}

	t.Run("native vllm runtime is valid", func(t *testing.T) {
		cfg := Config{
			Runtimes: map[string]RuntimeConfig{"vllm": {Kind: "vllm"}},
			Models:   map[string]ModelConfig{"model": vllmModel()},
		}
		if err := cfg.ValidateExtensionConfig(); err != nil {
			t.Fatalf("valid configuration rejected: %v", err)
		}
	})
}

func TestConfig_LMCacheDisabledDoesNotRequireRuntime(t *testing.T) {
	cfg := Config{
		Models: map[string]ModelConfig{"model": {
			Backend: BackendConfig{Type: "vllm", Arguments: []string{"vllm", "serve"},
				LMCache: &LMCacheModelConfig{Enabled: false}},
		}},
	}
	if err := cfg.ValidateExtensionConfig(); err != nil {
		t.Fatalf("disabled lmcache must not require a runtime: %v", err)
	}
}

func TestConfig_ParseByteSize(t *testing.T) {
	cases := map[string]struct {
		in      string
		want    int64
		wantErr string
	}{
		"bare bytes":        {"1000000", 1000000, ""},
		"decimal KB":        {"2KB", 2000, ""},
		"decimal MB":        {"3MB", 3 * 1000 * 1000, ""},
		"decimal GB":        {"30GB", 30 * 1000 * 1000 * 1000, ""},
		"binary KiB":        {"4KiB", 4 << 10, ""},
		"binary MiB":        {"512MiB", 512 << 20, ""},
		"binary GiB":        {"20GiB", 20 << 30, ""},
		"surrounding space": {" 8GB ", 8 * 1000 * 1000 * 1000, ""},
		"empty":             {"", 0, "missing its number"},
		"suffix only":       {"GB", 0, "missing its number"},
		"negative":          {"-5GB", 0, "integer"},
		"fractional":        {"1.5GB", 0, "integer"},
		"non-numeric":       {"abc", 0, "integer"},
		"unknown suffix":    {"5XB", 0, "integer"},
		"zero":              {"0", 0, "positive"},
		"zero with suffix":  {"0GiB", 0, "positive"},
		"overflow":          {"99999999999999999999GB", 0, "overflow"},
		"unlisted unit":     {"2PiB", 0, "integer"},
		"above 1 PiB":       {"1000000000GB", 0, "1 PiB"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ParseByteSize(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ParseByteSize(%q) = %d, %v; want error containing %q", tc.in, got, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("ParseByteSize(%q) = %d, %v; want %d", tc.in, got, err, tc.want)
			}
		})
	}
}

func TestConfig_LMCacheL2EffectiveBytes(t *testing.T) {
	cases := map[string]struct {
		server LMCacheServerConfig
		want   int64
	}{
		"all defaults":                     {LMCacheServerConfig{}, 20 << 30},
		"legacy l1SizeGB":                  {LMCacheServerConfig{L1SizeGB: 8}, 8 << 30},
		"l2 maxBytes wins":                 {LMCacheServerConfig{L1SizeGB: 8, L2: LMCacheL2Config{Enabled: true, MaxBytes: "30GB"}}, 30 * 1000 * 1000 * 1000},
		"l2 disabled falls back to legacy": {LMCacheServerConfig{L1SizeGB: 8, L2: LMCacheL2Config{Enabled: false, MaxBytes: "30GB"}}, 8 << 30},
		"l2 enabled without maxBytes falls back to legacy": {LMCacheServerConfig{L1SizeGB: 8, L2: LMCacheL2Config{Enabled: true}}, 8 << 30},
		"l2 maxBytes without legacy":                       {LMCacheServerConfig{L2: LMCacheL2Config{Enabled: true, MaxBytes: "1GiB"}}, 1 << 30},
		"l2 maxBytes with zero legacy":                     {LMCacheServerConfig{L1SizeGB: 0, L2: LMCacheL2Config{Enabled: true, MaxBytes: "1GiB"}}, 1 << 30},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := tc.server.EffectiveL2MaxBytes()
			if err != nil || got != tc.want {
				t.Fatalf("EffectiveL2MaxBytes() = %d, %v; want %d", got, err, tc.want)
			}
		})
	}
}

func TestConfig_LMCacheL2L3Validation(t *testing.T) {
	valid := LMCacheModuleConfig{
		Package: "lmcache",
		Enabled: true,
		Server: LMCacheServerConfig{
			Enabled: true,
			L2:      LMCacheL2Config{Enabled: true, MaxBytes: "30GB"},
			L3:      LMCacheL3Config{Enabled: true, Path: "/var/lib/lmcache", MaxBytes: "100GB"},
		},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid configuration rejected: %v", err)
	}
	cases := map[string]struct {
		mutate func(*LMCacheModuleConfig)
		want   string
	}{
		"l2 bad suffix":           {func(m *LMCacheModuleConfig) { m.Server.L2.MaxBytes = "10XB" }, "lmcache.server.l2.maxBytes"},
		"l2 zero":                 {func(m *LMCacheModuleConfig) { m.Server.L2.MaxBytes = "0GB" }, "lmcache.server.l2.maxBytes"},
		"l2 fractional":           {func(m *LMCacheModuleConfig) { m.Server.L2.MaxBytes = "1.5GB" }, "lmcache.server.l2.maxBytes"},
		"l3 enabled without path": {func(m *LMCacheModuleConfig) { m.Server.L3.Enabled = true; m.Server.L3.Path = "" }, "l3.path is required"},
		"l3 relative path":        {func(m *LMCacheModuleConfig) { m.Server.L3.Path = "relative/dir" }, "absolute path"},
		"l3 leading space":        {func(m *LMCacheModuleConfig) { m.Server.L3.Path = " /var/lib/lmcache" }, "normalized"},
		"l3 path without enabled": {func(m *LMCacheModuleConfig) { m.Server.L3.Enabled = false; m.Server.L3.Path = "/var/lib/lmcache" }, "l3.path requires"},
		"l3 bad maxBytes":         {func(m *LMCacheModuleConfig) { m.Server.L3.MaxBytes = "ten" }, "lmcache.server.l3.maxBytes"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := valid
			tc.mutate(&m)
			err := m.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tc.want)
			}
		})
	}
}
