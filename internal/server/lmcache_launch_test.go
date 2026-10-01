package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	runtimeManager "github.com/mostlygeek/llama-swap/internal/runtime"
)

// writeActiveVLLMRuntime stages one native vLLM version and points current at
// it, following the staged-runtime layout the launch binder reads.
func writeActiveVLLMRuntime(t *testing.T, root, name, version, vllmVersion string) {
	t.Helper()
	writeRuntimeVersion(t, root, name, runtimeManager.Manifest{
		Name: name, Version: version, Kind: "vllm", Source: "pypi", VLLM: vllmVersion,
		Metadata: map[string]string{"mode": runtimeManager.RuntimeModeNative, "sourceType": "pypi"},
	})
	if err := os.Symlink(filepath.Join("versions", version), filepath.Join(root, name, "current")); err != nil {
		t.Fatal(err)
	}
}

// lmcacheBindFixture builds a launch-binding config with one active vLLM
// runtime and a model whose LMCache block the test may mutate.
func lmcacheBindFixture(t *testing.T, name, version, vllmVersion string) (config.Config, *config.LMCacheModelConfig) {
	t.Helper()
	root := t.TempDir()
	writeActiveVLLMRuntime(t, root, name, version, vllmVersion)
	lm := &config.LMCacheModelConfig{Enabled: true}
	cfg := config.Config{
		RuntimeManager: config.RuntimeManagerConfig{Root: root},
		Runtimes:       map[string]config.RuntimeConfig{name: {Kind: "vllm"}},
		Models: map[string]config.ModelConfig{
			"model": {Backend: config.BackendConfig{
				Type: "vllm", Runtime: name,
				Arguments: []string{"vllm", "serve", "model"},
				LMCache:   lm,
			}},
		},
	}
	return cfg, lm
}

// boundKVTransferConfig extracts the injected --kv-transfer-config payload
// from a bound model argv.
func boundKVTransferConfig(t *testing.T, bound config.Config) (string, bool) {
	t.Helper()
	args := bound.Models["model"].Backend.Arguments
	for i, arg := range args {
		if arg == lmcacheKVTransferFlag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func TestServer_VLLMSupportsShippedMPConnector(t *testing.T) {
	cases := map[string]bool{
		"0.10.0":     false,
		"0.19.1":     false,
		"0.19.1-rc1": false,
		"v0.19.0":    false,
		"0.20.0":     true,
		"0.21.2":     true,
		"1.0.0":      true,
		"":           true,
		"dev":        true,
	}
	for version, want := range cases {
		if got := vllmSupportsShippedMPConnector(version); got != want {
			t.Errorf("vllmSupportsShippedMPConnector(%q) = %v, want %v", version, got, want)
		}
	}
}

func TestServer_LMCacheKVTransferConfig(t *testing.T) {
	t.Run("mp default", func(t *testing.T) {
		got, err := lmcacheKVTransferConfig(config.LMCacheModelConfig{Enabled: true}, "0.19.0")
		if err != nil {
			t.Fatal(err)
		}
		want := `{"kv_connector":"LMCacheMPConnector","kv_connector_extra_config":{"lmcache.mp.host":"localhost","lmcache.mp.port":5555},"kv_role":"kv_both"}`
		if got != want {
			t.Fatalf("kv-transfer config = %s, want %s", got, want)
		}
	})
	t.Run("mp module path on current vLLM", func(t *testing.T) {
		got, err := lmcacheKVTransferConfig(config.LMCacheModelConfig{Enabled: true}, "")
		if err != nil {
			t.Fatal(err)
		}
		want := `{"kv_connector":"LMCacheMPConnector","kv_connector_extra_config":{"kv_connector_module_path":"lmcache.integration.vllm.lmcache_mp_connector","lmcache.mp.host":"localhost","lmcache.mp.port":5555},"kv_role":"kv_both"}`
		if got != want {
			t.Fatalf("kv-transfer config = %s, want %s", got, want)
		}
	})
	t.Run("mp custom endpoint and role", func(t *testing.T) {
		lm := config.LMCacheModelConfig{Enabled: true, Host: "10.0.0.5", Port: 6000, Role: config.LMCacheRoleConsumer}
		got, err := lmcacheKVTransferConfig(lm, "0.19.0")
		if err != nil {
			t.Fatal(err)
		}
		want := `{"kv_connector":"LMCacheMPConnector","kv_connector_extra_config":{"lmcache.mp.host":"10.0.0.5","lmcache.mp.port":6000},"kv_role":"kv_consumer"}`
		if got != want {
			t.Fatalf("kv-transfer config = %s, want %s", got, want)
		}
	})
	t.Run("inProcess", func(t *testing.T) {
		lm := config.LMCacheModelConfig{Enabled: true, Mode: config.LMCacheModeInProcess, Role: config.LMCacheRoleProducer}
		got, err := lmcacheKVTransferConfig(lm, "0.21.0")
		if err != nil {
			t.Fatal(err)
		}
		want := `{"kv_connector":"LMCacheConnectorV1","kv_role":"kv_producer"}`
		if got != want {
			t.Fatalf("kv-transfer config = %s, want %s", got, want)
		}
	})
}

func TestServer_LMCacheBindInjectsKVTransferArgs(t *testing.T) {
	cfg, lm := lmcacheBindFixture(t, "vllm", "1", "0.19.0")
	lm.Host = "10.0.0.5"
	lm.Port = 6000
	lm.Role = config.LMCacheRoleConsumer

	bound, err := bindManagedRuntimeLaunchConfig(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"kv_connector":"LMCacheMPConnector","kv_connector_extra_config":{"lmcache.mp.host":"10.0.0.5","lmcache.mp.port":6000},"kv_role":"kv_consumer"}`
	if got, ok := boundKVTransferConfig(t, bound); !ok || got != want {
		t.Fatalf("bound kv-transfer config = %q (ok=%v), want %s", got, ok, want)
	}
	args := bound.Models["model"].Backend.Arguments
	if want := filepath.Join(".venv", "bin", "vllm"); !strings.HasSuffix(args[0], want) {
		t.Fatalf("bound argv[0] = %q, want suffix %q", args[0], want)
	}

	// The public configuration keeps the operator-written argv and no env.
	if got := cfg.Models["model"].Backend.Arguments; len(got) != 3 || got[0] != "vllm" {
		t.Fatalf("source config argv mutated: %v", got)
	}
	if got := cfg.Models["model"].Env; len(got) != 0 {
		t.Fatalf("source config env mutated: %v", got)
	}
}

func TestServer_LMCacheBindInProcessAddsChunkSizeEnv(t *testing.T) {
	cfg, lm := lmcacheBindFixture(t, "vllm", "1", "0.19.0")
	lm.Mode = config.LMCacheModeInProcess
	lm.ChunkSize = 512

	bound, err := bindManagedRuntimeLaunchConfig(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"kv_connector":"LMCacheConnectorV1","kv_role":"kv_both"}`
	if got, ok := boundKVTransferConfig(t, bound); !ok || got != want {
		t.Fatalf("bound kv-transfer config = %q (ok=%v), want %s", got, ok, want)
	}
	found := false
	for _, entry := range bound.Models["model"].Env {
		if entry == "LMCACHE_CHUNK_SIZE=512" {
			found = true
		}
	}
	if !found {
		t.Fatalf("bound env missing LMCACHE_CHUNK_SIZE=512: %v", bound.Models["model"].Env)
	}
}

func TestServer_LMCacheBindInProcessWithoutChunkSizeOmitsEnv(t *testing.T) {
	cfg, lm := lmcacheBindFixture(t, "vllm", "1", "0.19.0")
	lm.Mode = config.LMCacheModeInProcess

	bound, err := bindManagedRuntimeLaunchConfig(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range bound.Models["model"].Env {
		if strings.HasPrefix(entry, "LMCACHE_CHUNK_SIZE=") {
			t.Fatalf("unexpected chunk size env: %v", bound.Models["model"].Env)
		}
	}
}

func TestServer_LMCacheBindCandidateVersionReadsCandidateManifest(t *testing.T) {
	root := t.TempDir()
	writeRuntimeVersion(t, root, "vllm", runtimeManager.Manifest{
		Name: "vllm", Version: "1", Kind: "vllm", Source: "pypi", VLLM: "0.19.0",
		Metadata: map[string]string{"mode": runtimeManager.RuntimeModeNative, "sourceType": "pypi"},
	})
	writeRuntimeVersion(t, root, "vllm", runtimeManager.Manifest{
		Name: "vllm", Version: "2", Kind: "vllm", Source: "pypi", VLLM: "0.21.0",
		Metadata: map[string]string{"mode": runtimeManager.RuntimeModeNative, "sourceType": "pypi"},
	})
	if err := os.Symlink(filepath.Join("versions", "1"), filepath.Join(root, "vllm", "current")); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		RuntimeManager: config.RuntimeManagerConfig{Root: root},
		Runtimes:       map[string]config.RuntimeConfig{"vllm": {Kind: "vllm"}},
		Models: map[string]config.ModelConfig{
			"model": {Backend: config.BackendConfig{
				Type: "vllm", Runtime: "vllm",
				Arguments: []string{"vllm", "serve", "model"},
				LMCache:   &config.LMCacheModelConfig{Enabled: true},
			}},
		},
	}

	candidate, err := bindManagedRuntimeLaunchConfigForVersions(cfg, map[string]string{"vllm": "2"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := boundKVTransferConfig(t, candidate); !strings.Contains(got, `"kv_connector_module_path":"`+lmcacheMPModulePath+`"`) {
		t.Fatalf("candidate kv-transfer config missing module path: %s", got)
	}

	current, err := bindManagedRuntimeLaunchConfig(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := boundKVTransferConfig(t, current); strings.Contains(got, lmcacheMPModulePathKey) {
		t.Fatalf("current kv-transfer config unexpectedly has module path: %s", got)
	}
}

func TestServer_LMCacheBindDisabledIsNoop(t *testing.T) {
	cfg, lm := lmcacheBindFixture(t, "vllm", "1", "0.19.0")
	lm.Enabled = false
	bound, err := bindManagedRuntimeLaunchConfig(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The bind always normalizes the launch args; a disabled LMCache must add
	// nothing to that canonical form.
	want := []string{
		filepath.Join(cfg.RuntimeManager.Root, "vllm", "versions", "1", ".venv", "bin", "vllm"),
		"serve", "--model", "model", "--host", "127.0.0.1",
	}
	if got := bound.Models["model"].Backend.Arguments; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("disabled lmcache changed args: %v", got)
	}

	cfg, _ = lmcacheBindFixture(t, "vllm", "1", "0.19.0")
	model := cfg.Models["model"]
	model.Backend.LMCache = nil
	cfg.Models["model"] = model
	bound, err = bindManagedRuntimeLaunchConfig(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	want[0] = filepath.Join(cfg.RuntimeManager.Root, "vllm", "versions", "1", ".venv", "bin", "vllm")
	if got := bound.Models["model"].Backend.Arguments; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("nil lmcache changed args: %v", got)
	}
}
