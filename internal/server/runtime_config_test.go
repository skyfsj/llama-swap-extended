package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	runtimeManager "github.com/mostlygeek/llama-swap/internal/runtime"
)

func TestServer_DiscoverBundledLlamaCPPFromOwnedDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "llama-server"), []byte("#!/bin/sh\n"), 0o750); err != nil {
		t.Fatal(err)
	}
	spec, found := discoverBundledLlamaCPPFromCandidates([]string{root})
	if !found {
		t.Fatal("bundled llama.cpp was not discovered")
	}
	if spec.Name != "llamacpp" || spec.Kind != "llamacpp" || spec.Source != root || spec.SourceType != "bundled" {
		t.Fatalf("discovered spec=%+v", spec)
	}
}

func TestServer_DiscoverBundledLlamaCPPIgnoresRelativeAndSymlinkPaths(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "llama-server"), []byte("#!/bin/sh\n"), 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, found := discoverBundledLlamaCPPFromCandidates([]string{"relative", link}); found {
		t.Fatal("relative or symlink bundled path was discovered")
	}
}

func TestServer_DiscoverBundledLlamaCPPSkipsExplicitRuntime(t *testing.T) {
	if !hasConfiguredLlamaCPPRuntime(map[string]config.RuntimeConfig{
		"custom-llama": {Kind: "llamacpp"},
	}) {
		t.Fatal("explicit llama.cpp runtime was not recognized")
	}
}

func TestServer_RegisterDiscoveredBundledLlamaCPPSeedsManager(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "llama-server"), []byte("#!/bin/sh\nexit 0\n"), 0o750); err != nil {
		t.Fatal(err)
	}
	t.Setenv(bundledLlamaCPPPathEnv, source)

	manager, err := runtimeManager.NewManager(t.TempDir(), runtimeProvidersForConfig(config.Config{}))
	if err != nil {
		t.Fatal(err)
	}
	seeded, err := registerDiscoveredBundledLlamaCPP(manager, config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if !seeded {
		t.Fatal("discovered bundled llama.cpp was not seeded")
	}
	status, ok := manager.Get("llamacpp")
	if !ok || status.Kind != "llamacpp" || status.Current != "bundled" || status.State != runtimeManager.StateActive {
		t.Fatalf("registered llama.cpp status=%+v found=%v", status, ok)
	}
	definition, ok := manager.Definition("llamacpp")
	if !ok || definition.Spec.Source != source || definition.Spec.SourceType != "bundled" || definition.Spec.Version != "bundled" {
		t.Fatalf("registered llama.cpp definition=%+v found=%v", definition, ok)
	}
}

func TestServer_RuntimeSpecFromConfigPreservesProviderSourceAndBuild(t *testing.T) {
	value := config.RuntimeConfig{
		Kind:   "vllm",
		Source: config.RuntimeSource{Type: "pypi", Ref: "stable", Checksum: "sha256:abc"},
		Build:  config.RuntimeBuildConfig{Python: "3.12", Backend: "cuda", IndexURL: "https://pypi.example/simple", CUDAArchitectures: []string{"90", "80"}, CMake: []string{"-D A=B"}, Extras: []string{"flash-attn"}},
		Update: config.RuntimeUpdateConfig{Version: "0.6.4", Commit: "abc123", Channel: "stable"},
	}
	spec := runtimeSpecFromConfig("vllm", value)
	if spec.Name != "vllm" || spec.Kind != "vllm" || spec.Source != "pypi" || spec.Ref != "stable" || spec.Version != "0.6.4" || spec.Commit != "abc123" || spec.Checksum != "sha256:abc" {
		t.Fatalf("spec = %+v", spec)
	}
	if spec.Build["python"] != "3.12" || spec.Build["backend"] != "cuda" || spec.Build["indexURL"] != "https://pypi.example/simple" || spec.Build["cudaArchitectures"] != "90,80" || spec.Build["cmake"] != "-D A=B" || spec.Build["extras"] != "flash-attn" {
		t.Fatalf("build = %+v", spec.Build)
	}
}

func TestServer_RuntimeSpecFromConfigDefaultsBundledVersion(t *testing.T) {
	spec := runtimeSpecFromConfig("llamacpp", config.RuntimeConfig{
		Kind:   "llamacpp",
		Source: config.RuntimeSource{Type: "bundled", Path: "/opt/llama-swap/runtimes/llamacpp"},
	})
	if spec.Version != "bundled" || spec.SourceType != "bundled" || spec.Source != "/opt/llama-swap/runtimes/llamacpp" {
		t.Fatalf("bundled runtime spec = %+v", spec)
	}
}

func TestServer_RuntimePolicyFromConfigAppliesSafeDefaults(t *testing.T) {
	policy := runtimePolicyFromConfig(config.RuntimeUpdateConfig{Policy: "automatic", Channel: "stable"})
	if policy.Policy != "automatic" || policy.Channel != "stable" || policy.CheckEvery != 24*time.Hour || policy.MinIdle != 30*time.Minute || !policy.ActivateOnlyIdle || policy.KeepVersions != 2 || !policy.RollbackOnFailure {
		t.Fatalf("policy = %+v", policy)
	}
	if !policy.ActivateOnlyIdleSet || !policy.KeepVersionsSet || !policy.RollbackOnFailureSet {
		t.Fatal("server policy must preserve effective boolean/int values")
	}

	// A direct zero-value policy still receives the same defaults in the
	// provider-neutral runtime package.
	var direct runtimeManager.UpdatePolicy
	if direct.Policy != "" {
		t.Fatal("zero-value policy unexpectedly changed")
	}
}

func TestServer_RuntimeSpecFromConfigPreservesSourceAndBuildArgs(t *testing.T) {
	spec := runtimeSpecFromConfig("vllm", config.RuntimeConfig{
		Kind:   "vllm",
		Source: config.RuntimeSource{Type: "wheel", URL: "https://example.test/vllm.whl", Ref: "r1"},
		Build:  config.RuntimeBuildConfig{Python: "3.12", Extras: []string{"flash-attn", "xformers"}, CMake: []string{"-DKEY=value with spaces"}},
	})
	if spec.SourceType != "wheel" || spec.Source != "https://example.test/vllm.whl" || spec.Ref != "r1" {
		t.Fatalf("spec source = %+v", spec)
	}
	if got := spec.BuildArgs["extras"]; len(got) != 2 || got[1] != "xformers" {
		t.Fatalf("spec build args = %#v", spec.BuildArgs)
	}
	if got := spec.BuildArgs["cmake"]; len(got) != 1 || got[0] != "-DKEY=value with spaces" {
		t.Fatalf("spec cmake args = %#v", spec.BuildArgs)
	}
}

func TestServer_RuntimeSpecFromConfigMapsGitRepositoryAndContainerLaunch(t *testing.T) {
	spec := runtimeSpecFromConfig("vllm-1cat", config.RuntimeConfig{
		Kind:   "vllm",
		Source: config.RuntimeSource{Type: "git", Repository: "https://github.com/1CatAI/1Cat-vLLM.git", TrackRef: "v1.2.0"},
		Build: config.RuntimeBuildConfig{
			Python:      "3.12",
			InstallArgs: []string{"--no-build-isolation"},
			Env:         map[string]string{"CUDA_HOME": "/usr/local/cuda"},
			Steps:       []config.RuntimeBuildStep{{Command: "python", Args: []string{"-m", "build"}}},
		},
	})
	if spec.Mode != runtimeManager.RuntimeModeNative || spec.SourceType != "git" || spec.Source != "https://github.com/1CatAI/1Cat-vLLM.git" || spec.Ref != "v1.2.0" {
		t.Fatalf("git runtime spec = %+v", spec)
	}
	if len(spec.InstallArgs) != 1 || spec.InstallArgs[0] != "--no-build-isolation" || spec.BuildEnv["CUDA_HOME"] != "/usr/local/cuda" || len(spec.BuildSteps) != 1 {
		t.Fatalf("git build settings = %+v", spec)
	}

	container := runtimeSpecFromConfig("vllm-container", config.RuntimeConfig{
		Kind:      "vllm",
		Source:    config.RuntimeSource{Type: "image", Image: "ghcr.io/example/vllm:stable"},
		Container: config.RuntimeContainerConfig{Engine: "podman", Image: "ghcr.io/example/vllm:stable", Command: []string{"serve", "/models"}},
	})
	if container.Mode != runtimeManager.RuntimeModeContainer || container.Source != "ghcr.io/example/vllm:stable" || container.Container == nil || container.Container.Engine != "podman" {
		t.Fatalf("container runtime spec = %+v", container)
	}
}

func TestServer_RuntimeSpecFromConfigInfersGitForRepositoryShorthand(t *testing.T) {
	spec := runtimeSpecFromConfig("vllm-1cat", config.RuntimeConfig{
		Kind:   "vllm",
		Source: config.RuntimeSource{Repository: "https://github.com/1CatAI/1Cat-vLLM", Ref: "v1.2.0"},
	})
	if spec.SourceType != "git" || spec.Source != "https://github.com/1CatAI/1Cat-vLLM" || spec.Ref != "v1.2.0" {
		t.Fatalf("repository shorthand spec = %+v", spec)
	}
}

func TestServer_RuntimeSpecFromConfigPreservesVerificationSettings(t *testing.T) {
	spec := runtimeSpecFromConfig("vllm", config.RuntimeConfig{
		Kind:   "vllm",
		Source: config.RuntimeSource{Type: "pypi"},
		Verify: config.RuntimeVerifyConfig{
			SHA256:       "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			HealthPath:   "/health/ready",
			SmokeCommand: "python -c 'print(1)'",
		},
	})
	if spec.Metadata["verify.sha256"] == "" || spec.Metadata["verify.healthPath"] != "/health/ready" || spec.Metadata["verify.smokeCommand"] == "" {
		t.Fatalf("verification settings were dropped: %#v", spec.Metadata)
	}
}

func TestServer_RuntimeProvidersForConfigUsesLiveAllowlists(t *testing.T) {
	cfg := config.Config{RuntimeManager: config.RuntimeManagerConfig{
		SourceAllowlist:     []string{"github.com/1CatAI/1Cat-vLLM"},
		ContainerRegistries: []string{"registry.example.test"},
	}}
	providers := runtimeProvidersForConfig(cfg)
	mux, ok := providers["vllm"].(runtimeManager.ProviderMux)
	if !ok {
		t.Fatalf("vllm provider = %T, want ProviderMux", providers["vllm"])
	}
	native, ok := mux.Native.(runtimeManager.VLLMProvider)
	if !ok || len(native.SourceAllowlist) != 1 || native.SourceAllowlist[0] != "github.com/1CatAI/1Cat-vLLM" {
		t.Fatalf("native provider = %#v", mux.Native)
	}
	container, ok := mux.Container.(runtimeManager.ContainerProvider)
	if !ok || len(container.AllowedRegistries) != 1 || container.AllowedRegistries[0] != "registry.example.test" {
		t.Fatalf("container provider = %#v", mux.Container)
	}

	cfg.RuntimeManager.SourceAllowlist[0] = "changed.invalid"
	cfg.RuntimeManager.ContainerRegistries[0] = "changed.invalid"
	if native.SourceAllowlist[0] != "github.com/1CatAI/1Cat-vLLM" || container.AllowedRegistries[0] != "registry.example.test" {
		t.Fatal("provider settings alias the mutable config slices")
	}
}

func TestServer_LMCacheProviderUsesConfiguredIndexMetadataEndpoint(t *testing.T) {
	cfg := config.Config{LMCache: config.LMCacheModuleConfig{
		Package:  "lmcache",
		IndexURL: "https://mirror.example.test/simple/",
	}}
	providers := runtimeProvidersForConfig(cfg)
	provider, ok := providers["lmcache"].(runtimeManager.LMCacheProvider)
	if !ok {
		t.Fatalf("lmcache provider = %T, want LMCacheProvider", providers["lmcache"])
	}
	if provider.IndexURL != cfg.LMCache.IndexURL || provider.UpdateURL != "https://mirror.example.test/pypi/lmcache/json" {
		t.Fatalf("lmcache provider endpoints = index=%q update=%q", provider.IndexURL, provider.UpdateURL)
	}
}

func TestServer_LMCacheUpdateURLPreservesMirrorPrefix(t *testing.T) {
	if got := lmcacheUpdateURL("https://mirror.example.test/repository/simple", "lmcache-nightly"); got != "https://mirror.example.test/repository/pypi/lmcache-nightly/json" {
		t.Fatalf("lmcacheUpdateURL() = %q", got)
	}
}

func TestServer_SyncManagedRuntimeDefinitionsReplacesAndUnconfigures(t *testing.T) {
	root := t.TempDir()
	manager, err := runtimeManager.NewManager(root, runtimeProvidersForConfig(config.Config{}))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{runtime: manager}
	active := config.Config{
		Runtimes: map[string]config.RuntimeConfig{
			"removed": {Kind: "vllm", Source: config.RuntimeSource{Type: "pypi"}, Update: config.RuntimeUpdateConfig{Version: "0.1.0"}},
			"tracked": {Kind: "vllm", Source: config.RuntimeSource{Type: "pypi"}, Update: config.RuntimeUpdateConfig{Version: "0.2.0"}},
		},
	}
	if err := s.syncManagedRuntimeDefinitions(config.Config{}, active); err != nil {
		t.Fatal(err)
	}
	if err := manager.FlushControlUpdate(); err != nil {
		t.Fatal(err)
	}

	desired := config.Config{
		RuntimeManager: config.RuntimeManagerConfig{BuildWhileBusy: true},
		Runtimes: map[string]config.RuntimeConfig{
			"tracked": {
				Kind:   "vllm",
				Source: config.RuntimeSource{Type: "git", Repository: "https://github.com/1CatAI/1Cat-vLLM.git", TrackRef: "main"},
				Build:  config.RuntimeBuildConfig{Driver: "custom", Steps: []config.RuntimeBuildStep{{Command: "python3", Args: []string{"-m", "pip", "install", "."}}}},
				Update: config.RuntimeUpdateConfig{Policy: "manual"},
			},
			"added": {Kind: "llamacpp", Source: config.RuntimeSource{Type: "git", Repository: "https://github.com/ggerganov/llama.cpp.git", TrackRef: "master"}},
		},
	}
	if err := s.syncManagedRuntimeDefinitions(active, desired); err != nil {
		t.Fatal(err)
	}
	if err := manager.FlushControlUpdate(); err != nil {
		t.Fatal(err)
	}
	if _, ok := manager.Definition("removed"); ok {
		t.Fatal("removed runtime remained configured for automatic updates")
	}
	if _, ok := manager.Get("removed"); !ok {
		t.Fatal("removed runtime lost its durable status")
	}
	tracked, ok := manager.Definition("tracked")
	if !ok || tracked.Spec.Source != "https://github.com/1CatAI/1Cat-vLLM.git" || tracked.Spec.Metadata["trackRef"] != "main" || tracked.Policy.Policy != "manual" {
		t.Fatalf("tracked definition = %+v, found=%v", tracked, ok)
	}
	if added, ok := manager.Definition("added"); !ok || added.Spec.Kind != "llamacpp" {
		t.Fatalf("added definition = %+v, found=%v", added, ok)
	}
}

func TestServer_SyncManagedRuntimeDefinitionsRejectsChangesWithoutManager(t *testing.T) {
	s := &Server{}
	err := s.syncManagedRuntimeDefinitions(config.Config{}, config.Config{
		RuntimeManager: config.RuntimeManagerConfig{BuildWhileBusy: true},
	})
	if err == nil || !strings.Contains(err.Error(), "restart the daemon") {
		t.Fatalf("sync error = %v, want explicit daemon restart requirement", err)
	}
}

func TestServer_SyncManagedRuntimeDefinitionsAllowsUnrelatedReloadWhenManagerUnavailable(t *testing.T) {
	active := config.Config{
		LogLevel: "info",
		Runtimes: map[string]config.RuntimeConfig{
			"vllm": {Kind: "vllm", Source: config.RuntimeSource{Type: "pypi"}},
		},
	}
	desired := active
	desired.LogLevel = "debug"
	if err := (&Server{}).syncManagedRuntimeDefinitions(active, desired); err != nil {
		t.Fatalf("unrelated reload was rejected: %v", err)
	}
}
