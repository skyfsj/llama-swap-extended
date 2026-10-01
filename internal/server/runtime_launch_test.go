package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/hw"
	runtimeManager "github.com/mostlygeek/llama-swap/internal/runtime"
)

func TestServer_ManagedRuntimeRootUsesUserCacheByDefault(t *testing.T) {
	want := fallbackRuntimeManagerRoot
	if cacheRoot, err := os.UserCacheDir(); err == nil && filepath.IsAbs(cacheRoot) {
		want = filepath.Join(cacheRoot, "llama-swap", "runtimes")
	}
	if got := managedRuntimeRoot(config.Config{}); got != want {
		t.Fatalf("default runtime root = %q, want %q", got, want)
	}
}

func TestServer_BindManagedRuntimeLaunchConfigUsesCurrentVLLM(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{
		RuntimeManager: config.RuntimeManagerConfig{Root: root},
		Runtimes:       map[string]config.RuntimeConfig{"vllm-cpu": {Kind: "vllm"}},
		Models: map[string]config.ModelConfig{
			"model": {Env: []string{"KEEP=value", "PATH=/operator/bin"}, Backend: config.BackendConfig{
				Type: "vllm", Runtime: "vllm-cpu", Arguments: []string{"vllm", "serve", "model"},
			}},
		},
	}

	bound, err := bindManagedRuntimeLaunchConfig(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantBin := filepath.Join(root, "vllm-cpu", "current", ".venv", "bin")
	if got, want := bound.Models["model"].Backend.Arguments[0], filepath.Join(wantBin, "vllm"); got != want {
		t.Fatalf("bound argv[0]=%q want %q", got, want)
	}
	if got := strings.Join(bound.Models["model"].Env, "\n"); !strings.Contains(got, "PATH="+wantBin+string(filepath.ListSeparator)+"/operator/bin") || !strings.Contains(got, "KEEP=value") {
		t.Fatalf("bound env=%q", got)
	}
	if got := cfg.Models["model"].Backend.Arguments[0]; got != "vllm" {
		t.Fatalf("source config mutated: argv[0]=%q", got)
	}
}

func TestServer_BindManagedRuntimeLaunchConfigInjectsVLLMSleepModeForTTL(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{
		RuntimeManager: config.RuntimeManagerConfig{Root: root},
		Runtimes:       map[string]config.RuntimeConfig{"vllm-cpu": {Kind: "vllm"}},
		Models: map[string]config.ModelConfig{
			"with-ttl": {
				UnloadAfter: 30,
				Backend: config.BackendConfig{
					Type: "vllm", Runtime: "vllm-cpu",
					Arguments: []string{"vllm", "serve", "qwen"},
				},
			},
			"without-ttl": {
				Backend: config.BackendConfig{
					Type: "vllm", Runtime: "vllm-cpu",
					Arguments: []string{"vllm", "serve", "qwen"},
				},
			},
		},
	}

	bound, err := bindManagedRuntimeLaunchConfig(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	withTTL := bound.Models["with-ttl"].Backend.Arguments
	if got := withTTL[len(withTTL)-1]; got != "--enable-sleep-mode" {
		t.Fatalf("ttl argv=%q, want sleep flag appended", withTTL)
	}
	withoutTTL := bound.Models["without-ttl"].Backend.Arguments
	for _, arg := range withoutTTL {
		if arg == "--enable-sleep-mode" {
			t.Fatalf("ttl=0 unexpectedly injected sleep flag: %q", withoutTTL)
		}
	}
}

func TestServer_BindManagedRuntimeLaunchConfigUsesCurrentLlamaServer(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{
		RuntimeManager: config.RuntimeManagerConfig{Root: root},
		Runtimes:       map[string]config.RuntimeConfig{"llama-cpu": {Kind: "llamacpp"}},
		Models: map[string]config.ModelConfig{
			"model": {
				Proxy:   "http://127.0.0.1:5801",
				Backend: config.BackendConfig{Type: "llamacpp", Runtime: "llama-cpu", Arguments: []string{"llama-server", "--model", "model", "--host", "0.0.0.0", "--port", "9000"}},
			},
		},
	}

	bound, err := bindManagedRuntimeLaunchConfig(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantBin := filepath.Join(root, "llama-cpu", "current", "bin")
	want := []string{filepath.Join(wantBin, "llama-server"), "--model", "model", "--host", "127.0.0.1", "--port", "5801"}
	if got := strings.Join(bound.Models["model"].Backend.Arguments, "\n"); got != strings.Join(want, "\n") {
		t.Fatalf("bound argv=%q want %q", got, want)
	}
	if got := strings.Join(bound.Models["model"].Env, "\n"); !strings.Contains(got, "PATH="+wantBin) {
		t.Fatalf("bound env=%q", got)
	}
}

func TestServer_RebuildManagedLaunchArgumentsCanonicalPrefix(t *testing.T) {
	got, err := config.RebuildManagedLaunchArguments(
		[]string{"vllm", "serve", "--model", "/models/qwen", "--host", "0.0.0.0", "--port", "9999", "--dtype", "float16"},
		"vllm", "http://10.0.0.5:5801",
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"vllm", "serve", "--model", "/models/qwen", "--host", "10.0.0.5", "--port", "5801", "--dtype", "float16"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("rebuilt=%q want %q", got, want)
	}
}

func TestServer_RebuildManagedLaunchArgumentsSplitsShellStyleItems(t *testing.T) {
	// Hand-edited configs write "flag value" on one line per args item; the
	// rebuild shell-splits those items and unquotes JSON values.
	got, err := config.RebuildManagedLaunchArguments(
		[]string{
			"--model /models/Qwen3.8-27B-FP8",
			"--gpu-memory-utilization 0.90",
			"--max-model-len 262144",
			`--default-chat-template-kwargs '{"enable_thinking":true,"reasoning_effort":"xhigh"}'`,
		},
		"vllm", "http://127.0.0.1:5801",
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"vllm", "serve", "--model", "/models/Qwen3.8-27B-FP8", "--host", "127.0.0.1", "--port", "5801",
		"--gpu-memory-utilization", "0.90",
		"--max-model-len", "262144",
		"--default-chat-template-kwargs", `{"enable_thinking":true,"reasoning_effort":"xhigh"}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("rebuilt=%q want %q", got, want)
	}
}

func TestServer_RebuildManagedLaunchArgumentsKeepsSpacedModelPath(t *testing.T) {
	got, err := config.RebuildManagedLaunchArguments(
		[]string{"/models/My Model.gguf", "--ctx-size 32768"},
		"llamacpp", "http://127.0.0.1:8080",
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"llama-server", "--model", "/models/My Model.gguf", "--host", "127.0.0.1", "--port", "8080", "--ctx-size", "32768"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("rebuilt=%q want %q", got, want)
	}
}

func TestServer_RebuildManagedLaunchArgumentsUnquotesWrappedItems(t *testing.T) {
	// A fully quoted item without whitespace must still lose its wrapping
	// quotes, or the engine receives literal quote characters.
	got, err := config.RebuildManagedLaunchArguments(
		[]string{"--model /models/qwen", "--json-flag", `'{"a":true}'`},
		"vllm", "http://127.0.0.1:5801",
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"vllm", "serve", "--model", "/models/qwen", "--host", "127.0.0.1", "--port", "5801",
		"--json-flag", `{"a":true}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("rebuilt=%q want %q", got, want)
	}
}

func TestServer_ManagedLaunchSurvivesHandWrittenShellLineConfig(t *testing.T) {
	// Hand-edited configs often write "flag value" on one line per args item
	// (前名后参). The YAML items must parse as plain strings and the launch
	// rebuild must turn them into a clean canonical argv.
	content := `
runtimes:
  vllm:
    kind: vllm
models:
  qwen:
    proxy: "http://127.0.0.1:5801"
    backend:
      type: vllm
      runtime: vllm
      args:
        - --model /models/Qwen3.8-27B-FP8
        - --served-model-name Qwen/Qwen3.8-27B-FP8
        - --trust-remote-code
        - --gpu-memory-utilization 0.90
        - --max-model-len 262144
        - --max-num-seqs 2
        - --max-num-batched-tokens 8192
        - --attention-backend FLASH_ATTN_V100
        - --kv-cache-dtype fp8_e5m2
        - --enable-prefix-caching
        - --default-chat-template-kwargs '{"enable_thinking":true,"reasoning_effort":"xhigh"}'
        - --reasoning-parser qwen3
        - --enable-auto-tool-choice
        - --tool-call-parser qwen3_coder
        - --speculative-config '{"method":"dflash","model":"/models/Qwen3.8-27B-DFlash2","num_speculative_tokens":7,"kv_cache_dtype":"auto"}'
`
	cfg, err := config.LoadConfigFromReader(strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	model := cfg.Models["qwen"]
	// Loading migrates the hand-written command into the structured launch
	// block and reduces args to the unmanaged extras; the build merges them
	// back into one canonical argv.
	if model.Backend.Launch == nil {
		t.Fatal("expected the structured launch block to be migrated on load")
	}
	if model.Backend.Launch.Model != "/models/Qwen3.8-27B-FP8" ||
		model.Backend.Launch.ServedModelName != "Qwen/Qwen3.8-27B-FP8" ||
		model.Backend.Launch.ContextPerRequest != 262144 ||
		model.Backend.Launch.MaxConcurrency != 2 {
		t.Fatalf("launch block mismatch: %+v", model.Backend.Launch)
	}
	rebuilt, _, err := config.BuildManagedLaunchArguments(model.Backend.Arguments, model.Backend.Launch, "vllm", model.Proxy)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"vllm", "serve", "--model", "/models/Qwen3.8-27B-FP8",
		"--host", "127.0.0.1", "--port", "5801",
		"--served-model-name", "Qwen/Qwen3.8-27B-FP8",
		"--max-model-len", "262144",
		"--max-num-seqs", "2",
		"--gpu-memory-utilization", "0.9",
		"--trust-remote-code",
		"--max-num-batched-tokens", "8192",
		"--attention-backend", "FLASH_ATTN_V100",
		"--kv-cache-dtype", "fp8_e5m2",
		"--enable-prefix-caching",
		"--default-chat-template-kwargs", `{"enable_thinking":true,"reasoning_effort":"xhigh"}`,
		"--reasoning-parser", "qwen3",
		"--enable-auto-tool-choice",
		"--tool-call-parser", "qwen3_coder",
		"--speculative-config", `{"method":"dflash","model":"/models/Qwen3.8-27B-DFlash2","num_speculative_tokens":7,"kv_cache_dtype":"auto"}`,
	}
	if strings.Join(rebuilt, "\n") != strings.Join(want, "\n") {
		t.Fatalf("rebuilt=%q want %q", rebuilt, want)
	}
}

func TestServer_RebuildManagedLaunchArgumentsRequiresModelTarget(t *testing.T) {
	if _, err := config.RebuildManagedLaunchArguments([]string{"vllm", "serve", "--dtype", "float16"}, "vllm", "http://127.0.0.1:8000"); err == nil {
		t.Fatal("rebuild without a model target unexpectedly succeeded")
	}
	if _, err := config.RebuildManagedLaunchArguments([]string{"vllm", "serve", "--model"}, "vllm", "http://127.0.0.1:8000"); err == nil {
		t.Fatal("rebuild with a dangling --model unexpectedly succeeded")
	}
}

func TestServer_BindManagedRuntimeLaunchConfigNormalizesHostExecutable(t *testing.T) {
	// An operator-written argv[0] that names an ambient host executable is
	// ignored at launch: the bound process always executes the runtime-owned
	// entrypoint.
	root := t.TempDir()
	cfg := config.Config{
		RuntimeManager: config.RuntimeManagerConfig{Root: root},
		Runtimes:       map[string]config.RuntimeConfig{"vllm": {Kind: "vllm"}},
		Models: map[string]config.ModelConfig{
			"model": {
				Proxy:   "http://127.0.0.1:5801",
				Backend: config.BackendConfig{Type: "vllm", Runtime: "vllm", Arguments: []string{"/usr/bin/vllm", "serve", "--model", "model"}},
			},
		},
	}

	bound, err := bindManagedRuntimeLaunchConfig(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "vllm", "current", ".venv", "bin", "vllm")
	if got, want := bound.Models["model"].Backend.Arguments[0], want; got != want {
		t.Fatalf("bound argv[0]=%q want %q", got, want)
	}
	if got, want := bound.Models["model"].Backend.Arguments[3], "model"; got != want {
		t.Fatalf("bound model argument=%q want %q; argv=%v", got, want, bound.Models["model"].Backend.Arguments)
	}
}

func TestServer_BindManagedRuntimeLaunchConfigForVersionsPinsNativeCandidate(t *testing.T) {
	root := t.TempDir()
	for _, version := range []string{"1", "2"} {
		writeRuntimeVersion(t, root, "vllm-git", runtimeManager.Manifest{
			Name: "vllm-git", Version: version, Kind: "vllm", Source: "pypi",
			Metadata: map[string]string{"mode": runtimeManager.RuntimeModeNative, "sourceType": "pypi"},
		})
	}
	if err := os.Symlink(filepath.Join("versions", "1"), filepath.Join(root, "vllm-git", "current")); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		RuntimeManager: config.RuntimeManagerConfig{Root: root},
		Runtimes:       map[string]config.RuntimeConfig{"vllm-git": {Kind: "vllm"}},
		Models: map[string]config.ModelConfig{
			"model": {Backend: config.BackendConfig{Type: "vllm", Runtime: "vllm-git", Arguments: []string{"vllm", "serve", "model"}}},
		},
	}

	current, err := bindManagedRuntimeLaunchConfig(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := bindManagedRuntimeLaunchConfigForVersions(cfg, map[string]string{"vllm-git": "2"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := current.Models["model"].Backend.Arguments[0], filepath.Join(root, "vllm-git", "versions", "1", ".venv", "bin", "vllm"); got != want {
		t.Fatalf("current executable=%q want %q", got, want)
	}
	if got, want := candidate.Models["model"].Backend.Arguments[0], filepath.Join(root, "vllm-git", "versions", "2", ".venv", "bin", "vllm"); got != want {
		t.Fatalf("candidate executable=%q want %q", got, want)
	}
}

func TestServer_BindManagedRuntimeLaunchConfigUsesCurrentContainerDigest(t *testing.T) {
	root := t.TempDir()
	digest := "sha256:" + strings.Repeat("a", 64)
	writeCurrentContainerRuntime(t, root, "vllm-container", runtimeManager.Manifest{
		Name: "vllm-container", Version: "image-a", Kind: "vllm", Source: "ghcr.io/example/vllm:stable",
		Metadata: map[string]string{
			"mode": runtimeManager.RuntimeModeContainer, "engine": "podman", "image": "ghcr.io/example/vllm:stable", "imageDigest": digest,
		},
	})
	cfg := config.Config{
		RuntimeManager: config.RuntimeManagerConfig{Root: root},
		Runtimes: map[string]config.RuntimeConfig{
			"vllm-container": {
				Kind: "vllm", Mode: "container",
				Source: config.RuntimeSource{Type: "image", Image: "ghcr.io/example/vllm:stable", PullPolicy: "always"},
				Container: config.RuntimeContainerConfig{
					Engine: "docker", GPUs: "all", ShmSize: "16g", Command: []string{"serve", "/models/qwen", "--port", "8000"},
				},
			},
		},
		Models: map[string]config.ModelConfig{
			"model": {Backend: config.BackendConfig{Type: "vllm", Runtime: "vllm-container"}},
		},
	}

	bound, err := bindManagedRuntimeLaunchConfig(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	container := bound.Models["model"].Backend.Container
	if container.Image != "ghcr.io/example/vllm:stable@"+digest {
		t.Fatalf("bound image=%q", container.Image)
	}
	if container.Engine != "podman" || container.PullPolicy != "never" || container.GPUs != "all" {
		t.Fatalf("bound container=%+v", container)
	}
	boundModel := bound.Models["model"]
	args, err := boundModel.SanitizedCommand()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(args, " "); !strings.Contains(got, "podman run") || !strings.Contains(got, container.Image) || !strings.Contains(got, "serve /models/qwen") {
		t.Fatalf("container command=%q", args)
	}
	if cfg.Models["model"].Backend.Container.Image != "" {
		t.Fatalf("source config mutated: %+v", cfg.Models["model"].Backend.Container)
	}
}

func TestServer_BindManagedRuntimeLaunchConfigForVersionsPinsContainerCandidate(t *testing.T) {
	root := t.TempDir()
	firstDigest := "sha256:" + strings.Repeat("a", 64)
	secondDigest := "sha256:" + strings.Repeat("b", 64)
	writeRuntimeVersion(t, root, "vllm-container", runtimeManager.Manifest{
		Name: "vllm-container", Version: "image-a", Kind: "vllm", Source: "ghcr.io/example/vllm:stable",
		Metadata: map[string]string{
			"mode": runtimeManager.RuntimeModeContainer, "engine": "docker", "image": "ghcr.io/example/vllm:stable", "imageDigest": firstDigest,
		},
	})
	writeRuntimeVersion(t, root, "vllm-container", runtimeManager.Manifest{
		Name: "vllm-container", Version: "image-b", Kind: "vllm", Source: "ghcr.io/example/vllm:stable",
		Metadata: map[string]string{
			"mode": runtimeManager.RuntimeModeContainer, "engine": "podman", "image": "ghcr.io/example/vllm:stable", "imageDigest": secondDigest, "platform": "linux/arm64",
		},
	})
	if err := os.Symlink(filepath.Join("versions", "image-a"), filepath.Join(root, "vllm-container", "current")); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		RuntimeManager: config.RuntimeManagerConfig{Root: root},
		Runtimes: map[string]config.RuntimeConfig{
			"vllm-container": {Kind: "vllm", Mode: "container", Source: config.RuntimeSource{Type: "image", Image: "ghcr.io/example/vllm:stable"}},
		},
		Models: map[string]config.ModelConfig{
			"model": {Backend: config.BackendConfig{Type: "vllm", Runtime: "vllm-container", Container: config.RuntimeContainerConfig{Command: []string{"serve", "/models/qwen"}, Ports: []config.RuntimePort{{Host: 8001, Container: 8000}}}}},
		},
	}

	bound, err := bindManagedRuntimeLaunchConfigForVersions(cfg, map[string]string{"vllm-container": "image-b"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	container := bound.Models["model"].Backend.Container
	if want := "ghcr.io/example/vllm:stable@" + secondDigest; container.Image != want {
		t.Fatalf("candidate image=%q want %q", container.Image, want)
	}
	if container.Engine != "podman" || container.Platform != "linux/arm64" || container.PullPolicy != "never" {
		t.Fatalf("candidate launch identity=%+v", container)
	}
	if len(container.Ports) != 1 || container.Ports[0] != (config.RuntimePort{Host: 8001, Container: 8000}) {
		t.Fatalf("model port overlay lost: %+v", container.Ports)
	}
	if strings.Contains(container.Image, firstDigest) {
		t.Fatal("candidate binding followed current instead of its staged manifest")
	}
}

func TestServer_BindManagedRuntimeLaunchConfigRefusesUnstagedContainerTag(t *testing.T) {
	cfg := config.Config{
		RuntimeManager: config.RuntimeManagerConfig{Root: t.TempDir()},
		Runtimes: map[string]config.RuntimeConfig{
			"llama-container": {
				Kind: "llamacpp", Mode: "container",
				Source:    config.RuntimeSource{Type: "image", Image: "ghcr.io/example/llama:stable", PullPolicy: "always"},
				Container: config.RuntimeContainerConfig{Engine: "docker", Command: []string{"--model", "/models/model.gguf"}},
			},
		},
		Models: map[string]config.ModelConfig{
			"model": {Backend: config.BackendConfig{Type: "llamacpp", Runtime: "llama-container"}},
		},
	}

	bound, err := bindManagedRuntimeLaunchConfig(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	model := bound.Models["model"]
	container := model.Backend.Container
	if container.Image != "" || len(model.Backend.Arguments) != 0 {
		t.Fatalf("unstaged runtime retained a launchable tag: %+v", model.Backend)
	}
	if _, err := model.SanitizedCommand(); err == nil || !strings.Contains(err.Error(), "empty command") {
		t.Fatalf("unstaged runtime command error=%v, want empty command", err)
	}
	if got := cfg.Runtimes["llama-container"].Source.Image; got != "ghcr.io/example/llama:stable" {
		t.Fatalf("source configuration mutated: %q", got)
	}
}

func writeCurrentContainerRuntime(t *testing.T, root, name string, manifest runtimeManager.Manifest) {
	t.Helper()
	writeRuntimeVersion(t, root, name, manifest)
	if err := os.Symlink(filepath.Join("versions", manifest.Version), filepath.Join(root, name, "current")); err != nil {
		t.Fatal(err)
	}
}

func writeRuntimeVersion(t *testing.T, root, name string, manifest runtimeManager.Manifest) {
	t.Helper()
	directory := filepath.Join(root, name, "versions", manifest.Version)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	manifest.InstalledAt = time.Now().UTC()
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "manifest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestServer_BindManagedRuntimeLaunchConfigRejectsUnsafeFallbacks(t *testing.T) {
	base := config.Config{RuntimeManager: config.RuntimeManagerConfig{Root: t.TempDir()}, Runtimes: map[string]config.RuntimeConfig{"vllm": {Kind: "vllm"}}}
	for name, model := range map[string]config.ModelConfig{
		"unknown runtime": {Backend: config.BackendConfig{Type: "vllm", Runtime: "missing", Arguments: []string{"vllm"}}},
		"kind mismatch":   {Backend: config.BackendConfig{Type: "llamacpp", Runtime: "vllm", Arguments: []string{"llama-server"}}},
		"legacy command":  {Cmd: "vllm serve", Backend: config.BackendConfig{Type: "vllm", Runtime: "vllm", Arguments: []string{"vllm"}}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := base
			cfg.Models = map[string]config.ModelConfig{"model": model}
			if _, err := bindManagedRuntimeLaunchConfig(cfg, nil); err == nil {
				t.Fatal("binding unexpectedly succeeded")
			}
		})
	}
}

func TestServer_PrependManagedRuntimePathPreservesInput(t *testing.T) {
	input := []string{"A=1", "PATH=/custom/bin", "B=2"}
	got := prependManagedRuntimePath(input, "/runtime/bin")
	if strings.Join(input, "\n") != "A=1\nPATH=/custom/bin\nB=2" {
		t.Fatalf("input mutated: %q", input)
	}
	if want := "PATH=/runtime/bin" + string(filepath.ListSeparator) + "/custom/bin"; !strings.Contains(strings.Join(got, "\n"), want) {
		t.Fatalf("PATH not prepended: %q", got)
	}
}

func TestServer_PrependManagedRuntimeLibraryPathPreservesInput(t *testing.T) {
	input := []string{"A=1", "LD_LIBRARY_PATH=/operator/lib", "B=2"}
	got := prependManagedRuntimeLibraryPath(input, []string{"/runtime/bin", "/runtime/build/bin", "/runtime/bin"})
	if strings.Join(input, "\n") != "A=1\nLD_LIBRARY_PATH=/operator/lib\nB=2" {
		t.Fatalf("input mutated: %q", input)
	}
	want := "LD_LIBRARY_PATH=/runtime/bin" + string(filepath.ListSeparator) + "/runtime/build/bin" + string(filepath.ListSeparator) + "/operator/lib"
	if !strings.Contains(strings.Join(got, "\n"), want) {
		t.Fatalf("LD_LIBRARY_PATH not prepended: %q", got)
	}
}

func TestServer_NvidiaLaunchDevicesFiltersNonNVidiaSelections(t *testing.T) {
	strPtr := func(value string) *string { return &value }
	hardware := &hw.HardwareSnapshot{Accelerators: []hw.Accelerator{
		{Index: 0, Kind: "gpu", Vendor: strPtr("Apple")},
		{Index: 1, Kind: "gpu", Vendor: strPtr("NVIDIA"), UUID: strPtr("GPU-abc")},
		{Index: 2, Kind: "gpu", Vendor: strPtr("NVIDIA")},
		{Index: 3, Kind: "gpu", Vendor: strPtr("AMD")},
	}}

	if got := nvidiaLaunchDevices(nil, []string{"index:0"}); !reflect.DeepEqual(got, []string{"index:0"}) {
		t.Fatalf("nil snapshot pass-through = %v", got)
	}
	if got := nvidiaLaunchDevices(hardware, []string{"index:0", "index:1", "index:2", "index:3", "index:9"}); !reflect.DeepEqual(got, []string{"1", "2"}) {
		t.Fatalf("vendor filter = %v, want [1 2]", got)
	}
	if got := nvidiaLaunchDevices(hardware, []string{"GPU-abc", "index:1", "index:2"}); !reflect.DeepEqual(got, []string{"GPU-abc", "2"}) {
		t.Fatalf("uuid form + dedupe = %v, want [GPU-abc 2]", got)
	}
	if got := nvidiaLaunchDevices(hardware, []string{"GPU-stale"}); len(got) != 0 {
		t.Fatalf("stale uuid kept: %v", got)
	}
}

func TestServer_BindManagedRuntimeLaunchConfigScopesCudaEnvToNVIDIA(t *testing.T) {
	strPtr := func(value string) *string { return &value }
	hardware := &hw.HardwareSnapshot{Accelerators: []hw.Accelerator{
		{Index: 0, Kind: "gpu", Vendor: strPtr("Apple")},
		{Index: 1, Kind: "gpu", Vendor: strPtr("NVIDIA")},
		{Index: 2, Kind: "gpu", Vendor: strPtr("NVIDIA")},
	}}
	mkConfig := func(gpus []string) config.Config {
		return config.Config{
			RuntimeManager: config.RuntimeManagerConfig{Root: t.TempDir()},
			Runtimes:       map[string]config.RuntimeConfig{"vllm-cpu": {Kind: "vllm"}},
			Models: map[string]config.ModelConfig{
				"model": {Backend: config.BackendConfig{
					Type: "vllm", Runtime: "vllm-cpu", Arguments: []string{"vllm", "serve", "qwen"},
					Launch: &config.ModelLaunchConfig{ServedModelName: "qwen", GPUs: gpus},
				}},
			},
		}
	}

	bound, err := bindManagedRuntimeLaunchConfig(mkConfig([]string{"index:0"}), hardware)
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Join(bound.Models["model"].Env, "\n")
	if strings.Contains(env, "CUDA_VISIBLE_DEVICES") || strings.Contains(env, "CUDA_DEVICE_ORDER") {
		t.Fatalf("apple selection leaked CUDA env: %q", env)
	}

	bound, err = bindManagedRuntimeLaunchConfig(mkConfig([]string{"index:1", "index:2"}), hardware)
	if err != nil {
		t.Fatal(err)
	}
	env = strings.Join(bound.Models["model"].Env, "\n")
	for _, want := range []string{"CUDA_DEVICE_ORDER=PCI_BUS_ID", "CUDA_VISIBLE_DEVICES=1,2"} {
		if !strings.Contains(env, want) {
			t.Fatalf("nvidia selection missing %q in env %q", want, env)
		}
	}
	argv := strings.Join(bound.Models["model"].Backend.Arguments, " ")
	if !strings.Contains(argv, "--tensor-parallel-size 2") {
		t.Fatalf("two nvidia selections did not auto-set tensor parallel: %q", argv)
	}
}

func TestServer_BindManagedRuntimeLaunchConfigDerivesGPUsFromEnvironment(t *testing.T) {
	strPtr := func(value string) *string { return &value }
	hardware := &hw.HardwareSnapshot{Accelerators: []hw.Accelerator{
		{Index: 0, Kind: "gpu", Vendor: strPtr("NVIDIA")},
		{Index: 1, Kind: "gpu", Vendor: strPtr("NVIDIA")},
		{Index: 2, Kind: "gpu", Vendor: strPtr("NVIDIA")},
	}}
	cfg := config.Config{
		RuntimeManager: config.RuntimeManagerConfig{Root: t.TempDir()},
		Runtimes:       map[string]config.RuntimeConfig{"vllm-cpu": {Kind: "vllm"}},
		Models: map[string]config.ModelConfig{
			"model": {
				Env: []string{"CUDA_DEVICE_ORDER=PCI_BUS_ID", "CUDA_VISIBLE_DEVICES=1,2"},
				Backend: config.BackendConfig{
					Type: "vllm", Runtime: "vllm-cpu", Arguments: []string{"vllm", "serve", "qwen"},
					Launch: &config.ModelLaunchConfig{ServedModelName: "qwen"},
				},
			},
		},
	}

	bound, err := bindManagedRuntimeLaunchConfig(cfg, hardware)
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Join(bound.Models["model"].Env, "\n")
	if !strings.Contains(env, "CUDA_VISIBLE_DEVICES=1,2") {
		t.Fatalf("environment GPU selection was not bound: %q", env)
	}
	argv := strings.Join(bound.Models["model"].Backend.Arguments, " ")
	if !strings.Contains(argv, "--tensor-parallel-size 2") {
		t.Fatalf("environment selection did not derive tensor parallelism: %q", argv)
	}
}

func TestServer_BindManagedRuntimeLaunchConfigDropsLegacyNumericEnvironmentContinuations(t *testing.T) {
	strPtr := func(value string) *string { return &value }
	hardware := &hw.HardwareSnapshot{Accelerators: []hw.Accelerator{
		{Index: 1, Kind: "gpu", Vendor: strPtr("NVIDIA")},
		{Index: 2, Kind: "gpu", Vendor: strPtr("NVIDIA")},
		{Index: 3, Kind: "gpu", Vendor: strPtr("NVIDIA")},
		{Index: 4, Kind: "gpu", Vendor: strPtr("NVIDIA")},
	}}
	cfg := config.Config{
		RuntimeManager: config.RuntimeManagerConfig{Root: t.TempDir()},
		Runtimes:       map[string]config.RuntimeConfig{"vllm-cpu": {Kind: "vllm"}},
		Models: map[string]config.ModelConfig{
			"model": {
				Env: []string{"CUDA_DEVICE_ORDER=PCI_BUS_ID", "CUDA_VISIBLE_DEVICES=1", "2", "3", "4"},
				Backend: config.BackendConfig{
					Type: "vllm", Runtime: "vllm-cpu", Arguments: []string{"vllm", "serve", "qwen"},
					Launch: &config.ModelLaunchConfig{ServedModelName: "qwen"},
				},
			},
		},
	}

	bound, err := bindManagedRuntimeLaunchConfig(cfg, hardware)
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Join(bound.Models["model"].Env, "\n")
	if !strings.Contains(env, "CUDA_VISIBLE_DEVICES=1,2,3,4") {
		t.Fatalf("legacy continuation values were not recovered: %q", env)
	}
	for _, value := range []string{"\n2\n", "\n3\n", "\n4\n"} {
		if strings.Contains("\n"+env+"\n", value) {
			t.Fatalf("bare numeric continuation leaked into process environment: %q", env)
		}
	}
	argv := strings.Join(bound.Models["model"].Backend.Arguments, " ")
	if !strings.Contains(argv, "--tensor-parallel-size 4") {
		t.Fatalf("four recovered GPU selections did not derive tensor parallelism: %q", argv)
	}
}

// A launch-only managed model (empty backend.args, structured launch block)
// is a valid configuration shape — the loader validates it and
// MigrateLaunchBlocks produces it from legacy args-only configs — so the
// bind guard must accept it and rebuild the full argv from the launch block.
func TestServer_BindManagedRuntimeLaunchConfigAcceptsLaunchOnlyModel(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{
		RuntimeManager: config.RuntimeManagerConfig{Root: root},
		Runtimes:       map[string]config.RuntimeConfig{"vllm-cpu": {Kind: "vllm"}},
		Models: map[string]config.ModelConfig{
			"launch-only": {Backend: config.BackendConfig{
				Type:    "vllm",
				Runtime: "vllm-cpu",
				Launch:  &config.ModelLaunchConfig{Model: "/models/qwen"},
			}},
		},
	}

	bound, err := bindManagedRuntimeLaunchConfig(cfg, nil)
	if err != nil {
		t.Fatalf("launch-only model must bind: %v", err)
	}
	args := bound.Models["launch-only"].Backend.Arguments
	if len(args) < 2 {
		t.Fatalf("rebuilt argv too short: %q", args)
	}
	if got, want := args[0], filepath.Join(root, "vllm-cpu", "current", ".venv", "bin", "vllm"); got != want {
		t.Fatalf("bound argv[0]=%q want %q", got, want)
	}
	if !slicesContains(args, "/models/qwen") {
		t.Fatalf("launch.model missing from rebuilt argv: %q", args)
	}
}

func slicesContains(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
