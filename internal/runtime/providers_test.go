package runtime

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	stdruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func nilRuntimeProviderContext() context.Context { return nil }

func writeTestVLLMEntrypoints(t *testing.T, directory string) {
	t.Helper()
	binDir := filepath.Join(directory, ".venv", "bin")
	if err := os.MkdirAll(binDir, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"python", "vllm"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\nexit 0\n"), 0o750); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRuntime_SourcePathAcceptsLocalFileURL(t *testing.T) {
	path, ok := runtimeSourcePath("file:///var/lib/llama-swap/runtime%20source")
	if !ok || path != filepath.FromSlash("/var/lib/llama-swap/runtime source") {
		t.Fatalf("file URL = %q, ok=%v", path, ok)
	}
	path, ok = runtimeSourcePath("/var/lib/llama-swap/runtime")
	if !ok || path != "/var/lib/llama-swap/runtime" {
		t.Fatalf("absolute path = %q, ok=%v", path, ok)
	}
}

func TestRuntime_SourcePathRejectsRemoteAndUnsafeFileURLs(t *testing.T) {
	for _, source := range []string{
		"https://example.test/runtime.whl",
		"file://remote.example/runtime.whl",
		"file:///tmp/runtime.whl?download=1",
		"file:///%2e%2e/etc/passwd",
		"git+https://example.test/repo.git",
		"git@example.test:repo.git",
		"relative/runtime.whl",
	} {
		if path, ok := runtimeSourcePath(source); ok {
			t.Errorf("source %q was accepted as %q", source, path)
		}
	}
}

func TestRuntime_StructuredBuildArgsPreserveSpaces(t *testing.T) {
	spec := Spec{
		Build: map[string]string{"cmake": "-DLEGACY=1", "python": "3.11"},
		BuildArgs: map[string][]string{
			"cmake":  {"-DKEY=value with spaces"},
			"extras": {"flash-attn", "xformers"},
		},
	}
	if got := buildArgs(spec, "cmake"); len(got) != 1 || got[0] != "-DKEY=value with spaces" {
		t.Fatalf("cmake args = %#v", got)
	}
	if got := firstBuildArg(spec, "python"); got != "3.11" {
		t.Fatalf("python = %q", got)
	}
	got := buildArgs(spec, "extras")
	got[0] = "changed"
	if spec.BuildArgs["extras"][0] != "flash-attn" {
		t.Fatal("buildArgs returned the caller's backing slice")
	}
}

func TestRuntime_ProviderStageRejectsUnsafeStructuredInputs(t *testing.T) {
	cases := []struct {
		name string
		spec Spec
		want string
	}{
		{
			name: "invalid environment key",
			spec: Spec{Kind: "vllm", Version: "1", SourceType: "pypi", Source: "pypi", BuildEnv: map[string]string{"BAD=KEY": "1"}},
			want: "environment key",
		},
		{
			name: "control in install arg",
			spec: Spec{Kind: "vllm", Version: "1", SourceType: "pypi", Source: "pypi", InstallArgs: []string{"--index-url\nhttps://example.test"}},
			want: "control",
		},
		{
			name: "step parent path",
			spec: Spec{Kind: "llamacpp", Version: "1", SourceType: "local", Source: "/tmp/source", BuildSteps: []BuildStep{{Command: "make", WorkDir: "../outside"}}},
			want: "parent path",
		},
		{
			name: "artifact parent path",
			spec: Spec{Kind: "llamacpp", Version: "1", SourceType: "local", Source: "/tmp/source", Artifacts: []Artifact{{From: "build/bin", To: "../outside"}}},
			want: "parent path",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateProviderStage(tc.spec, tc.spec.Kind)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.want)) {
				t.Fatalf("validateProviderStage error=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestRuntime_LlamaCPPStageUsesConfiguredMakeAndBuildEnvironment(t *testing.T) {
	if stdruntime.GOOS != "linux" {
		t.Skip("managed llama.cpp providers are Linux-only")
	}
	root := t.TempDir()
	source := filepath.Join(root, "llama-src")
	if err := os.MkdirAll(source, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "CMakeLists.txt"), []byte("project(llama)"), 0o640); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(root, "stage")
	var command string
	var args []string
	var env map[string]string
	writeEntrypoint := func() error {
		entrypoint := filepath.Join(stage, "build", "bin", "llama-server")
		if err := os.MkdirAll(filepath.Dir(entrypoint), 0o750); err != nil {
			return err
		}
		return os.WriteFile(entrypoint, []byte("#!/bin/sh\nexit 0\n"), 0o750)
	}
	provider := LlamaCPPProvider{
		SourceAllowlist: []string{root},
		Run: func(_ context.Context, _ string, name string, values ...string) ([]byte, error) {
			command, args = name, append([]string(nil), values...)
			if name == "make" {
				if err := writeEntrypoint(); err != nil {
					return nil, err
				}
			}
			return nil, nil
		},
		RunEnv: func(_ context.Context, _ string, values map[string]string, name string, argv ...string) ([]byte, error) {
			env = values
			command, args = name, append([]string(nil), argv...)
			if name == "make" {
				if err := writeEntrypoint(); err != nil {
					return nil, err
				}
			}
			return nil, nil
		},
	}
	manifest, err := provider.Stage(context.Background(), Spec{
		Name: "llama", Kind: "llamacpp", Version: "b5999", SourceType: "local", Source: source,
		Build: map[string]string{"driver": "make"}, BuildArgs: map[string][]string{"make": {"-j4", "llama-server"}, "compiler": {"clang"}}, BuildEnv: map[string]string{"GGML_CUDA": "1"},
	}, stage)
	if err != nil {
		t.Fatal(err)
	}
	if command != "make" || len(args) != 2 || args[0] != "-j4" || args[1] != "llama-server" {
		t.Fatalf("make command = %q %#v", command, args)
	}
	if env["GGML_CUDA"] != "1" || env["CC"] != "clang" || manifest.Metadata["driver"] != "make" || manifest.Metadata["compiler"] != "clang" {
		t.Fatalf("build env/metadata = %#v %#v", env, manifest.Metadata)
	}
	if err := verifyLlamaCPPEntrypoint(stage); err != nil {
		t.Fatalf("normalized llama.cpp entrypoint: %v", err)
	}
}

func TestRuntime_LlamaCPPProviderStageMaterializesCurrentEntrypoint(t *testing.T) {
	if stdruntime.GOOS != "linux" {
		t.Skip("managed llama.cpp providers are Linux-only")
	}
	root := t.TempDir()
	source := filepath.Join(root, "llama-src")
	if err := os.MkdirAll(source, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "CMakeLists.txt"), []byte("project(llama)"), 0o640); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(root, "stage")
	provider := LlamaCPPProvider{
		SourceAllowlist: []string{root},
		Run: func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
			if name == "cmake" && len(args) >= 1 && args[0] == "--build" {
				entrypoint := filepath.Join(stage, "build", "bin", "llama-server")
				if err := os.MkdirAll(filepath.Dir(entrypoint), 0o750); err != nil {
					return nil, err
				}
				if err := os.WriteFile(entrypoint, []byte("#!/bin/sh\nexit 0\n"), 0o750); err != nil {
					return nil, err
				}
			}
			return nil, nil
		},
	}
	manifest, err := provider.Stage(context.Background(), Spec{
		Name: "llama", Kind: "llamacpp", Version: "b6000", SourceType: "local", Source: source,
	}, stage)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.Verify(context.Background(), manifest, stage); err != nil {
		t.Fatalf("provider verification failed: %v", err)
	}
	entrypoint := filepath.Join(stage, "bin", "llama-server")
	if info, err := os.Stat(entrypoint); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("normalized entrypoint info=%v err=%v", info, err)
	}
}

func TestRuntime_LlamaCPPStageMaterializesSharedLibraries(t *testing.T) {
	if stdruntime.GOOS != "linux" {
		t.Skip("managed llama.cpp providers are Linux-only")
	}
	root := t.TempDir()
	source := filepath.Join(root, "llama-src")
	if err := os.MkdirAll(source, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "CMakeLists.txt"), []byte("project(llama)"), 0o640); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(root, "stage")
	var configureArgs []string
	provider := LlamaCPPProvider{
		SourceAllowlist: []string{root},
		Run: func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
			if name == "cmake" && len(args) >= 1 && args[0] == "-S" {
				configureArgs = append(configureArgs, args...)
			}
			if name == "cmake" && len(args) >= 1 && args[0] == "--build" {
				bin := filepath.Join(stage, "build", "bin")
				if err := os.MkdirAll(bin, 0o750); err != nil {
					return nil, err
				}
				if err := os.WriteFile(filepath.Join(bin, "llama-server"), []byte("#!/bin/sh\nexit 0\n"), 0o750); err != nil {
					return nil, err
				}
				// Mimic the llama.cpp CMake layout: real object plus a
				// SONAME symlink chain pointing at it.
				real := filepath.Join(bin, "libllama.so.0.22.0")
				if err := os.WriteFile(real, []byte("lib"), 0o750); err != nil {
					return nil, err
				}
				if err := os.Symlink("libllama.so.0.22.0", filepath.Join(bin, "libllama.so.0")); err != nil {
					return nil, err
				}
				if err := os.Symlink("libllama.so.0", filepath.Join(bin, "libllama.so")); err != nil {
					return nil, err
				}
				if err := os.WriteFile(filepath.Join(bin, "libggml-cpu.so"), []byte("cpu"), 0o750); err != nil {
					return nil, err
				}
				if err := os.WriteFile(filepath.Join(bin, "README.txt"), []byte("skip"), 0o640); err != nil {
					return nil, err
				}
			}
			return nil, nil
		},
	}
	manifest, err := provider.Stage(context.Background(), Spec{
		Name: "llama", Kind: "llamacpp", Version: "b6001", SourceType: "local", Source: source,
	}, stage)
	if err != nil {
		t.Fatal(err)
	}
	for _, lib := range []string{"libllama.so", "libllama.so.0", "libllama.so.0.22.0", "libggml-cpu.so"} {
		path := filepath.Join(stage, "bin", lib)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("library %s not materialized as a regular file: info=%v err=%v", lib, info, err)
		}
	}
	if content, err := os.ReadFile(filepath.Join(stage, "bin", "libllama.so.0.22.0")); err != nil || string(content) != "lib" {
		t.Fatalf("library content = %q err=%v", content, err)
	}
	if _, err := os.Stat(filepath.Join(stage, "bin", "README.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("non-library file should not be copied: err=%v", err)
	}
	foundOriginRPath := false
	for _, arg := range configureArgs {
		if arg == "-DCMAKE_BUILD_RPATH_USE_ORIGIN=ON" {
			foundOriginRPath = true
		}
	}
	if !foundOriginRPath {
		t.Fatalf("cmake configure args missing -DCMAKE_BUILD_RPATH_USE_ORIGIN=ON: %v", configureArgs)
	}
	if err := provider.Verify(context.Background(), manifest, stage); err != nil {
		t.Fatalf("provider verification failed: %v", err)
	}
}

// TestRuntime_LlamaCPPStageMaterializesVersionedReleaseLayout mirrors the
// stock llama.cpp release archive (llama-bXXXX-bin-*.tar.gz), which ships the
// prebuilt binary under a versioned top-level directory. No build driver
// output exists; materialization must find source/<version>/llama-server.
func TestRuntime_LlamaCPPStageMaterializesVersionedReleaseLayout(t *testing.T) {
	if stdruntime.GOOS != "linux" {
		t.Skip("managed llama.cpp providers are Linux-only")
	}
	root := t.TempDir()
	releaseDir := filepath.Join(root, "llama-src", "llama-b10785")
	if err := os.MkdirAll(releaseDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(releaseDir, "llama-server"), []byte("#!/bin/sh\nexit 0\n"), 0o750); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(releaseDir, "libllama.so.0.22.0")
	if err := os.WriteFile(real, []byte("lib"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("libllama.so.0.22.0", filepath.Join(releaseDir, "libllama.so.0")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("libllama.so.0", filepath.Join(releaseDir, "libllama.so")); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(root, "stage")
	provider := LlamaCPPProvider{
		SourceAllowlist: []string{root},
		Run: func(context.Context, string, string, ...string) ([]byte, error) {
			return nil, nil
		},
	}
	manifest, err := provider.Stage(context.Background(), Spec{
		Name: "llama", Kind: "llamacpp", Version: "b10785", SourceType: "local", Source: filepath.Join(root, "llama-src"),
	}, stage)
	if err != nil {
		t.Fatal(err)
	}
	entrypoint := filepath.Join(stage, "bin", "llama-server")
	if info, err := os.Stat(entrypoint); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("versioned release entrypoint not materialized: info=%v err=%v", info, err)
	}
	for _, lib := range []string{"libllama.so", "libllama.so.0", "libllama.so.0.22.0"} {
		if content, err := os.ReadFile(filepath.Join(stage, "bin", lib)); err != nil || string(content) != "lib" {
			t.Fatalf("library %s not materialized: content=%q err=%v", lib, content, err)
		}
	}
	if err := provider.Verify(context.Background(), manifest, stage); err != nil {
		t.Fatalf("provider verification failed: %v", err)
	}
}

func TestRuntime_VLLMStageAcceptsLocalSourceDirectory(t *testing.T) {
	if stdruntime.GOOS != "linux" {
		t.Skip("managed vLLM providers are Linux-only")
	}
	root := t.TempDir()
	source := filepath.Join(root, "1cat-vllm")
	if err := os.MkdirAll(source, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "pyproject.toml"), []byte("[project]\nname='vllm'\nversion='1.2.0'\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	var installed string
	provider := VLLMProvider{
		SourceAllowlist: []string{root},
		Run: func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
			if name == "uv" && len(args) > 0 && args[0] == "pip" {
				installed = args[len(args)-1]
			}
			return []byte(`{"pythonVersion":"3.12","vllm":"1.2.0"}`), nil
		},
	}
	_, err := provider.Stage(context.Background(), Spec{Name: "vllm", Kind: "vllm", Version: "1.2.0", SourceType: "local", Source: source}, filepath.Join(root, "stage"))
	if err != nil {
		t.Fatal(err)
	}
	if installed != filepath.Join(root, "stage", "source") {
		t.Fatalf("local vLLM package path = %q", installed)
	}
}

func TestRuntime_VLLMStageFallsBackToPipWhenUVMissing(t *testing.T) {
	if stdruntime.GOOS != "linux" {
		t.Skip("managed vLLM providers are Linux-only")
	}
	t.Setenv("PATH", t.TempDir())
	var calls [][]string
	run := func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return []byte(`{"pythonVersion":"3.12","vllm":"1.2.0"}`), nil
	}
	provider := VLLMProvider{Run: run, AllowPipFallback: true}
	manifest, err := provider.Stage(context.Background(), Spec{Name: "vllm", Kind: "vllm", Version: "1.2.0"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) < 2 {
		t.Fatalf("missing venv/install calls: %#v", calls)
	}
	// venv creation must use the stdlib venv module, not uv.
	if calls[0][0] != "python3" || len(calls[0]) != 4 || calls[0][1] != "-m" || calls[0][2] != "venv" {
		t.Fatalf("expected stdlib venv creation, got: %#v", calls[0])
	}
	// install must run through the venv interpreter with pip.
	venvPython := filepath.Join(calls[0][3], "bin", "python")
	want := []string{venvPython, "-m", "pip", "install", "vllm==1.2.0"}
	if len(calls[1]) != len(want) {
		t.Fatalf("pip fallback install = %#v, want %#v", calls[1], want)
	}
	for i := range want {
		if calls[1][i] != want[i] {
			t.Fatalf("pip fallback install = %#v, want %#v", calls[1], want)
		}
	}
	if manifest.Metadata["uv"] != "pip" {
		t.Fatalf("manifest uv metadata = %q, want pip", manifest.Metadata["uv"])
	}
}

func TestRuntime_VLLMStageKeepsUVWhenAvailable(t *testing.T) {
	if stdruntime.GOOS != "linux" {
		t.Skip("managed vLLM providers are Linux-only")
	}
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "uv"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	var calls [][]string
	run := func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return []byte(`{"pythonVersion":"3.12","vllm":"1.2.0"}`), nil
	}
	provider := VLLMProvider{Run: run, AllowPipFallback: true}
	manifest, err := provider.Stage(context.Background(), Spec{Name: "vllm", Kind: "vllm", Version: "1.2.0"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) < 2 {
		t.Fatalf("missing venv/install calls: %#v", calls)
	}
	// With uv present the original uv venv / uv pip install path must be
	// unchanged.
	if calls[0][0] != "uv" || calls[0][1] != "venv" {
		t.Fatalf("expected uv venv, got: %#v", calls[0])
	}
	if calls[1][0] != "uv" || calls[1][1] != "pip" {
		t.Fatalf("expected uv pip install, got: %#v", calls[1])
	}
	if manifest.Metadata["uv"] != "uv" {
		t.Fatalf("manifest uv metadata = %q, want uv", manifest.Metadata["uv"])
	}
}

func TestRuntime_VLLMStageWithoutFallbackFlagStillUsesUV(t *testing.T) {
	if stdruntime.GOOS != "linux" {
		t.Skip("managed vLLM providers are Linux-only")
	}
	t.Setenv("PATH", t.TempDir())
	var calls [][]string
	run := func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return []byte(`{"pythonVersion":"3.12","vllm":"1.2.0"}`), nil
	}
	// Zero-value provider (no AllowPipFallback): behavior is unchanged and
	// uv commands are attempted even when uv is not on PATH.
	provider := VLLMProvider{Run: run}
	if _, err := provider.Stage(context.Background(), Spec{Name: "vllm", Kind: "vllm", Version: "1.2.0"}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if len(calls) < 2 || calls[0][0] != "uv" || calls[0][1] != "venv" || calls[1][0] != "uv" || calls[1][1] != "pip" {
		t.Fatalf("expected uv calls without fallback flag, got: %#v", calls)
	}
}

func TestRuntime_VLLMCustomGitBuildUsesManagedSourceBeforeInstall(t *testing.T) {
	if stdruntime.GOOS != "linux" {
		t.Skip("managed vLLM providers are Linux-only")
	}
	destination := filepath.Join(t.TempDir(), "runtime")
	var calls []string
	var gitEnvs []map[string]string
	record := func(directory, command string, args ...string) ([]byte, error) {
		calls = append(calls, directory+"|"+command+"|"+strings.Join(args, " "))
		if command == "git" && len(args) >= 4 && args[0] == "clone" {
			if err := os.MkdirAll(args[len(args)-1], 0o750); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	provider := VLLMProvider{
		SourceAllowlist: []string{"https://github.com"},
		Run: func(_ context.Context, directory, command string, args ...string) ([]byte, error) {
			return record(directory, command, args...)
		},
		RunEnv: func(_ context.Context, directory string, env map[string]string, command string, args ...string) ([]byte, error) {
			if env["CUDA_HOME"] != "/usr/local/cuda-12.8" {
				t.Fatalf("custom build env=%#v", env)
			}
			if command == "git" {
				gitEnvs = append(gitEnvs, maps.Clone(env))
			}
			return record(directory, command, args...)
		},
	}
	manifest, err := provider.Stage(context.Background(), Spec{
		Name: "vllm-1cat", Kind: "vllm", Version: "git-2222222222222222222222222222222222222222",
		SourceType: "git", Source: "https://github.com/1CatAI/1Cat-vLLM.git", Ref: "2222222222222222222222222222222222222222",
		Build:    map[string]string{"driver": "custom", "python": "3.12"},
		BuildEnv: map[string]string{"CUDA_HOME": "/usr/local/cuda-12.8"},
		BuildSteps: []BuildStep{{
			WorkDir: "${SOURCE_DIR}", Command: "custom-install", Args: []string{"--python", "${PYTHON}", "${SOURCE_DIR}"},
		}},
	}, destination)
	if err != nil {
		t.Fatal(err)
	}
	sourceDir := filepath.Join(destination, "source")
	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"git|clone --no-checkout https://github.com/1CatAI/1Cat-vLLM.git " + sourceDir,
		sourceDir + "|git|checkout --detach 2222222222222222222222222222222222222222",
		sourceDir + "|custom-install|--python " + filepath.Join(destination, ".venv", "bin", "python") + " " + sourceDir,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("calls did not contain %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "|uv|pip install") {
		t.Fatalf("custom driver also ran default install:\n%s", joined)
	}
	if len(gitEnvs) != 2 || gitEnvs[0]["CUDA_HOME"] != "/usr/local/cuda-12.8" || gitEnvs[1]["CUDA_HOME"] != "/usr/local/cuda-12.8" {
		t.Fatalf("git clone/checkout build environments=%#v", gitEnvs)
	}
	if manifest.Metadata["package"] != "git+https://github.com/1CatAI/1Cat-vLLM.git@2222222222222222222222222222222222222222" {
		t.Fatalf("manifest package=%q", manifest.Metadata["package"])
	}
}

func TestRuntime_StructuredBuildStepEnvWithoutBaseEnv(t *testing.T) {
	if stdruntime.GOOS != "linux" {
		t.Skip("managed runtime providers are Linux-only")
	}
	root := t.TempDir()
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatal(err)
	}
	var gotEnv map[string]string
	runEnv := func(_ context.Context, _ string, env map[string]string, command string, args ...string) ([]byte, error) {
		gotEnv = env
		if command != "make" || len(args) != 1 || args[0] != "all" {
			t.Fatalf("build step argv = %q %#v", command, args)
		}
		return nil, nil
	}
	steps := []BuildStep{{Command: "make", Args: []string{"all"}, Env: map[string]string{"CC": "clang"}}}
	if err := runStructuredBuildSteps(context.Background(), root, root, steps, nil, map[string]string{"RUNTIME_DIR": root}, nil, runEnv); err != nil {
		t.Fatal(err)
	}
	if gotEnv["CC"] != "clang" {
		t.Fatalf("build step env = %#v", gotEnv)
	}
}

func TestRuntime_CopyTreeRejectsExistingSymlinkTarget(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	destination := filepath.Join(root, "destination")
	outside := filepath.Join(root, "outside.txt")
	if err := os.MkdirAll(source, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(destination, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "model.bin"), []byte("source"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("outside"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(destination, "model.bin")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := copyTree(context.Background(), source, destination); err == nil || !strings.Contains(strings.ToLower(err.Error()), "regular file") {
		t.Fatalf("copyTree error = %v, want symlink target rejection", err)
	}
	data, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "outside" {
		t.Fatalf("outside target changed to %q", data)
	}
}

func TestRuntime_CopyArtifactsRejectsExistingSymlinkTarget(t *testing.T) {
	root := t.TempDir()
	destination := filepath.Join(root, "destination")
	source := filepath.Join(destination, "source.txt")
	outside := filepath.Join(root, "outside.txt")
	if err := os.MkdirAll(destination, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("source"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("outside"), 0o640); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(destination, "model.bin")
	if err := os.Symlink(outside, target); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := copyRuntimeArtifacts(context.Background(), destination, []Artifact{{From: source, To: "model.bin"}}, nil); err == nil || !strings.Contains(strings.ToLower(err.Error()), "regular file") {
		t.Fatalf("copyRuntimeArtifacts error = %v, want symlink target rejection", err)
	}
	data, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "outside" {
		t.Fatalf("outside target changed to %q", data)
	}
}

func TestRuntime_ProbeVLLMEnvironmentIsBestEffortAndBounded(t *testing.T) {
	called := false
	probe := probeVLLMEnvironment(context.Background(), func(_ context.Context, directory, name string, args ...string) ([]byte, error) {
		called = true
		if directory != "/stage" || name != "/stage/.venv/bin/python" || len(args) != 2 || args[0] != "-c" {
			t.Fatalf("probe argv = directory=%q name=%q args=%#v", directory, name, args)
		}
		return []byte(`{"pythonVersion":"3.12.1","vllm":"0.8.5","torch":"2.7.0","cuda":"12.8","rocm":""}`), nil
	}, "/stage", "/stage/.venv/bin/python")
	if !called || probe["pythonVersion"] != "3.12.1" || probe["vllm"] != "0.8.5" || probe["torch"] != "2.7.0" || probe["cuda"] != "12.8" {
		t.Fatalf("probe metadata=%#v called=%v", probe, called)
	}

	tooLong := strings.Repeat("x", 129)
	probe = probeVLLMEnvironment(context.Background(), func(context.Context, string, string, ...string) ([]byte, error) {
		return []byte(`{"pythonVersion":"` + tooLong + `","vllm":123}`), nil
	}, "/stage", "/stage/.venv/bin/python")
	if len(probe) != 0 {
		t.Fatalf("invalid probe values should be ignored: %#v", probe)
	}
	probe = probeVLLMEnvironment(context.Background(), func(context.Context, string, string, ...string) ([]byte, error) {
		return bytes.Repeat([]byte{'x'}, maxVLLMProbeBytes+1), nil
	}, "/stage", "/stage/.venv/bin/python")
	if len(probe) != 0 {
		t.Fatalf("oversized probe output should be ignored: %#v", probe)
	}
}

func TestRuntime_RunCommandChecksCancellationAroundCustomRunner(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	if _, err := runRuntimeCommand(ctx, func(context.Context, string, string, ...string) ([]byte, error) {
		calls++
		return nil, nil
	}, "/stage", "tool"); err != nil {
		t.Fatalf("unexpected command error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("runner calls=%d", calls)
	}
	cancel()
	if _, err := runRuntimeCommand(ctx, func(context.Context, string, string, ...string) ([]byte, error) {
		calls++
		return nil, nil
	}, "/stage", "tool"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled command error=%v", err)
	}
	if calls != 1 {
		t.Fatalf("canceled context reached runner: calls=%d", calls)
	}

	ctx, cancel = context.WithCancel(context.Background())
	_, err := runRuntimeCommand(ctx, func(context.Context, string, string, ...string) ([]byte, error) {
		cancel()
		return nil, nil
	}, "/stage", "tool")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("post-run cancellation error=%v", err)
	}
}

func TestRuntime_DefaultCommandRunnerStreamsStdoutAndStderr(t *testing.T) {
	if stdruntime.GOOS == "windows" {
		t.Skip("managed runtime commands use POSIX process semantics")
	}
	type outputEvent struct {
		stream string
		data   string
	}
	events := make(chan outputEvent, 4)
	ctx := withRuntimeCommandOutputState(context.Background())
	ctx = withRuntimeLog(ctx, func(stream string, output []byte) {
		events <- outputEvent{stream: stream, data: string(output)}
	})
	if _, err := defaultCommandRunner(ctx, "", "sh", "-c", "printf 'stdout line\\n'; printf 'stderr line\\n' >&2"); err != nil {
		t.Fatalf("default command runner error: %v", err)
	}
	var stdout, stderr string
	for range 2 {
		select {
		case event := <-events:
			switch event.stream {
			case "stdout":
				stdout += event.data
			case "stderr":
				stderr += event.data
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for streamed command output")
		}
	}
	if stdout != "stdout line\n" || stderr != "stderr line\n" {
		t.Fatalf("streamed output = stdout %q stderr %q", stdout, stderr)
	}
}

// TestRuntime_StalledLogPublisherDoesNotStallCommand pins the same contract for
// build/install commands that holds for model processes: the collector
// publishes every write into the runtime log stream, and a publisher that
// backpressures must not be able to stop the command producing that log. Bound
// inline, a stalled subscriber fills the command's output pipe and the build
// dies with SIGPIPE.
func TestRuntime_StalledLogPublisherDoesNotStallCommand(t *testing.T) {
	if stdruntime.GOOS == "windows" {
		t.Skip("managed runtime commands use POSIX process semantics")
	}
	marker := filepath.Join(t.TempDir(), "command-finished")
	gate := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(gate) }) }
	defer unblock()

	ctx := withRuntimeLog(context.Background(), func(string, []byte) {
		<-gate
	})

	done := make(chan error, 1)
	go func() {
		// Well past a pipe buffer, then proof that the command reached its end.
		_, err := defaultCommandRunner(ctx, "", "sh", "-c",
			fmt.Sprintf("head -c 200000 /dev/zero; touch %s", marker))
		done <- err
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("command never finished while the log publisher was stalled: backpressure reached its output pipe")
		}
		time.Sleep(20 * time.Millisecond)
	}

	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("default command runner error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("default command runner did not return after the publisher resumed")
	}
}

func TestRuntime_DownloadArtifactReportsBoundedProgress(t *testing.T) {
	root := t.TempDir()
	payload := strings.Repeat("runtime", 1024)
	var reports []struct{ completed, total int64 }
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://example.test/runtime.whl" {
			return nil, errors.New("unexpected URL: " + req.URL.String())
		}
		return &http.Response{
			StatusCode:    http.StatusOK,
			ContentLength: int64(len(payload)),
			Body:          io.NopCloser(strings.NewReader(payload)),
			Header:        make(http.Header),
			Request:       req,
		}, nil
	})}
	artifact, checksum, err := downloadRuntimeArtifactWithProgress(context.Background(), client, "https://example.test/runtime.whl", filepath.Join(root, "stage"), 1<<20, nil, func(completed, total int64) {
		reports = append(reports, struct{ completed, total int64 }{completed, total})
	})
	if err != nil {
		t.Fatal(err)
	}
	if artifact == "" || checksum == "" || len(reports) < 2 {
		t.Fatalf("artifact=%q checksum=%q reports=%#v", artifact, checksum, reports)
	}
	last := reports[len(reports)-1]
	if last.completed != int64(len(payload)) || last.total != int64(len(payload)) {
		t.Fatalf("final progress=%+v, want %d/%d", last, len(payload), len(payload))
	}
	previous := int64(0)
	for _, report := range reports {
		if report.completed < previous || report.completed > report.total {
			t.Fatalf("non-monotonic/out-of-range report=%+v previous=%d", report, previous)
		}
		previous = report.completed
	}
}

func TestRuntime_DownloadArtifactProgressSupportsUnknownLength(t *testing.T) {
	root := t.TempDir()
	var total int64
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			ContentLength: -1,
			Body:          io.NopCloser(strings.NewReader("unknown length")),
			Header:        make(http.Header),
			Request:       req,
		}, nil
	})}
	_, _, err := downloadRuntimeArtifactWithProgress(context.Background(), client, "https://example.test/runtime.whl", filepath.Join(root, "stage"), 1<<20, nil, func(_ int64, reportedTotal int64) {
		total = reportedTotal
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != -1 {
		t.Fatalf("unknown content length was reported as %d", total)
	}
}

func TestRuntime_LockFingerprintIncludesStructuredInputs(t *testing.T) {
	base := Spec{Kind: "vllm", Version: "0.8.5", SourceType: "pypi", Source: "pypi", Build: map[string]string{"python": "3.12"}, BuildArgs: map[string][]string{"extras": {"flash-attn"}}}
	first := runtimeLockFingerprint(base, "vllm==0.8.5", "python3", "https://pypi.org/simple", []string{"flash-attn"}, "abc")
	second := runtimeLockFingerprint(base, "vllm==0.8.5", "python3", "https://pypi.org/simple", []string{"flash-attn"}, "abc")
	if first == "" || first != second {
		t.Fatalf("fingerprint not deterministic: %q %q", first, second)
	}
	base.BuildArgs["extras"][0] = "xformers"
	if changed := runtimeLockFingerprint(base, "vllm==0.8.5", "python3", "https://pypi.org/simple", []string{"flash-attn"}, "abc"); changed == first {
		t.Fatal("structured build args did not affect lock fingerprint")
	}
	base.BuildArgs["extras"][0] = "flash-attn"
	base.Metadata = map[string]string{"verify.healthPath": "/health/ready"}
	if changed := runtimeLockFingerprint(base, "vllm==0.8.5", "python3", "https://pypi.org/simple", []string{"flash-attn"}, "abc"); changed == first {
		t.Fatal("runtime verification metadata did not affect lock fingerprint")
	}
}

func TestRuntime_SourceTypeInference(t *testing.T) {
	if got := inferVLLMSourceType("git+https://example.test/vllm.git"); got != "git" {
		t.Fatalf("vLLM source type = %q", got)
	}
	if got := inferVLLMSourceType("https://github.com/1CatAI/1Cat-vLLM"); got != "git" {
		t.Fatalf("GitHub repository source type = %q", got)
	}
	if got := inferVLLMSourceType("https://example.test/vllm.git"); got != "git" {
		t.Fatalf(".git repository source type = %q", got)
	}
	if got := inferVLLMSourceType("https://example.test/vllm.whl"); got != "wheel" {
		t.Fatalf("vLLM wheel source type = %q", got)
	}
	if got := inferLlamaSourceType("https://example.test/llama.cpp"); got != "git" {
		t.Fatalf("llama source type = %q", got)
	}
	if got := inferLlamaSourceType("https://github.com/acme/llama.cpp/releases/download/b4200/source.tar.gz"); got != "release" {
		t.Fatalf("llama release source type = %q", got)
	}
}

func TestRuntime_ProviderStageValidatesSpecBeforeCommands(t *testing.T) {
	tests := []struct {
		name     string
		expected string
		kind     string
		source   string
		wantPart string
	}{
		{name: "vllm kind", expected: "vllm", kind: "llamacpp", source: "pypi", wantPart: "cannot stage runtime kind"},
		{name: "vllm source type", expected: "vllm", kind: "vllm", source: "unsupported", wantPart: "source type"},
		{name: "llama source type", expected: "llamacpp", kind: "llamacpp", source: "unsupported", wantPart: "source type"},
		{name: "version path", expected: "vllm", kind: "vllm", source: "pypi", wantPart: "invalid runtime version"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := Spec{Name: "runtime", Kind: test.kind, SourceType: test.source, Version: "1"}
			if test.name == "version path" {
				spec.Version = "../escape"
			}
			if err := validateProviderStage(spec, test.expected); err == nil || !strings.Contains(err.Error(), test.wantPart) {
				t.Fatalf("validation error = %v, want %q", err, test.wantPart)
			}
		})
	}
}

func TestRuntime_ProviderRefValidationRejectsGitSpecialForms(t *testing.T) {
	valid := []string{"v1.2.3", "refs/tags/v1.2.3", "feature/model-v2", "deadbeef", "release+cuda"}
	for _, ref := range valid {
		if err := validateRuntimeRef("runtime ref", ref, true); err != nil {
			t.Errorf("valid ref %q rejected: %v", ref, err)
		}
	}
	invalid := []string{
		"-c", "refs/heads/../main", "refs/heads/main..", "refs/heads/main@{1}",
		"refs//heads/main", ".hidden", "refs/heads/.hidden", "refs/heads/main.",
		"refs/heads/main.lock", "refs/heads/main~1", "refs/heads/main?x", "refs/heads/main\\x",
		"refs/heads/main\u200b", "refs/heads/main ",
	}
	for _, ref := range invalid {
		if err := validateRuntimeRef("runtime ref", ref, true); err == nil {
			t.Errorf("unsafe git ref %q was accepted", ref)
		}
	}
	if err := validateRuntimeRef("runtime ref", "", true); err == nil {
		t.Fatal("missing exact ref was accepted")
	}
	if err := validateRuntimeRef("runtime ref", "", false); err != nil {
		t.Fatalf("optional ref rejected: %v", err)
	}
}

func TestRuntime_ProviderStageRequiresAndValidatesInferredGitRefs(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec Spec
		want string
	}{
		{name: "vllm missing", spec: Spec{Name: "runtime", Kind: "vllm", Source: "git+https://example.test/vllm.git"}, want: "requires an exact ref"},
		{name: "llama missing", spec: Spec{Name: "runtime", Kind: "llamacpp", Source: "https://example.test/llama.cpp"}, want: "requires an exact ref"},
		{name: "vllm option", spec: Spec{Name: "runtime", Kind: "vllm", SourceType: "git", Ref: "-c"}, want: "must not begin"},
		{name: "llama special", spec: Spec{Name: "runtime", Kind: "llamacpp", SourceType: "commit", Ref: "refs/heads/main@{1}"}, want: "not a valid git ref"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateProviderStage(tc.spec, tc.spec.Kind); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validation error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRuntime_ValidGitSourceAcceptsAllowlistedFileURLOnly(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, "llama.git")
	if err := os.Mkdir(gitDir, 0o750); err != nil {
		t.Fatal(err)
	}
	source := "file://" + filepath.ToSlash(gitDir)
	if !validGitSource(source, []string{root}) {
		t.Fatalf("allowlisted file git source was rejected: %q", source)
	}
	if validGitSource(source, []string{t.TempDir()}) {
		t.Fatal("file git source outside allowlist was accepted")
	}
	if validGitSource(source, []string{"https://github.com/ggml-org"}) {
		t.Fatal("file git source matched an unrelated URL allowlist")
	}
}

func TestRuntime_LlamaCPPProviderStagesFromAllowlistedBareGitRepository(t *testing.T) {
	if stdruntime.GOOS != "linux" {
		t.Skip("managed llama.cpp providers are Linux-only")
	}
	root := t.TempDir()
	working := filepath.Join(root, "working")
	bare := filepath.Join(root, "llama.git")
	runGit := func(dir string, args ...string) []byte {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
		return output
	}
	if err := os.MkdirAll(working, 0o750); err != nil {
		t.Fatal(err)
	}
	runGit(root, "init", "--quiet", working)
	runGit(working, "config", "user.email", "runtime-test@example.invalid")
	runGit(working, "config", "user.name", "runtime-test")
	if err := os.WriteFile(filepath.Join(working, "CMakeLists.txt"), []byte("project(llama)\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(working, "SOURCE_MARKER"), []byte("bare-git-source\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	runGit(working, "add", "CMakeLists.txt", "SOURCE_MARKER")
	runGit(working, "commit", "--quiet", "-m", "fixture")
	commit := strings.TrimSpace(string(runGit(working, "rev-parse", "HEAD")))
	runGit(root, "clone", "--quiet", "--bare", working, bare)

	provider := LlamaCPPProvider{
		SourceAllowlist: []string{root},
		Run: func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
			if name == "git" {
				return defaultCommandRunner(ctx, dir, name, args...)
			}
			if name != "cmake" {
				return nil, fmt.Errorf("unexpected provider command %q", name)
			}
			if len(args) > 0 && args[0] == "--build" {
				buildDir := args[1]
				if err := os.MkdirAll(filepath.Join(buildDir, "bin"), 0o750); err != nil {
					return nil, err
				}
				return nil, os.WriteFile(filepath.Join(buildDir, "bin", "llama-server"), []byte("#!/bin/sh\nexit 0\n"), 0o750)
			}
			return nil, nil
		},
	}
	destination := filepath.Join(root, "stage")
	manifest, err := provider.Stage(context.Background(), Spec{
		Name: "llama", Kind: "llamacpp", Version: "git-fixture", SourceType: "git",
		Source: "file://" + filepath.ToSlash(bare), Ref: commit,
	}, destination)
	if err != nil {
		t.Fatal(err)
	}
	marker, err := os.ReadFile(filepath.Join(destination, "source", "SOURCE_MARKER"))
	if err != nil {
		t.Fatal(err)
	}
	if string(marker) != "bare-git-source\n" || manifest.Source != "file://"+filepath.ToSlash(bare) {
		t.Fatalf("staged source marker=%q manifest source=%q", marker, manifest.Source)
	}
	if _, err := os.Stat(filepath.Join(destination, "bin", "llama-server")); err != nil {
		t.Fatalf("materialized llama-server missing: %v", err)
	}
}

func TestRuntime_PrepareProviderDestinationRejectsUnsafePaths(t *testing.T) {
	root := t.TempDir()
	if err := prepareProviderDestination(filepath.Join(root, "new", "candidate")); err != nil {
		t.Fatalf("new absolute destination rejected: %v", err)
	}
	if info, err := os.Stat(filepath.Join(root, "new", "candidate")); err != nil || !info.IsDir() {
		t.Fatalf("destination was not created as a directory: info=%v err=%v", info, err)
	}
	if err := prepareProviderDestination("relative/candidate"); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative destination error = %v", err)
	}
	regular := filepath.Join(root, "regular")
	if err := os.WriteFile(regular, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareProviderDestination(regular); err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("regular-file destination error = %v", err)
	}
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(root, "linked")
	if err := os.Symlink(outside, linked); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := prepareProviderDestination(linked); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink destination error = %v", err)
	}
	withSpace := filepath.Join(root, "candidate with space ")
	if err := prepareProviderDestination(withSpace); err != nil {
		t.Fatalf("destination with trailing space rejected: %v", err)
	}
	if _, err := os.Stat(withSpace); err != nil {
		t.Fatalf("destination with trailing space was not preserved: %v", err)
	}
}

func TestRuntime_ParseSmokeCommandShellFree(t *testing.T) {
	args, err := parseRuntimeSmokeCommand(`python -c 'print("hello world")' --flag "value with spaces" escaped\ value`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"python", "-c", `print("hello world")`, "--flag", "value with spaces", "escaped value"}
	if len(args) != len(want) {
		t.Fatalf("argv=%#v, want %#v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("argv[%d]=%q, want %q (all=%#v)", i, args[i], want[i], args)
		}
	}
	for _, invalid := range []string{"'unterminated", `trailing\`, "python\x00 -c test", strings.Repeat("x", maxRuntimeSmokeCommandBytes+1)} {
		if _, err := parseRuntimeSmokeCommand(invalid); err == nil {
			t.Errorf("invalid smoke command %q was accepted", invalid)
		}
	}
}

func TestRuntime_ProviderHealthRunsSmokeCommandAndValidatesHealthPath(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "runtime")
	if err := os.MkdirAll(filepath.Join(directory, ".venv"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "metadata.json"), []byte(`{"verify.healthPath":"/health/ready"}`), 0o640); err != nil {
		t.Fatal(err)
	}
	writeTestVLLMEntrypoints(t, directory)
	var calls [][]string
	run := func(_ context.Context, dir, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{dir, name}, args...))
		return nil, nil
	}
	manifest := Manifest{Kind: "vllm", Metadata: map[string]string{
		"verify.healthPath":   "/health/ready",
		"verify.smokeCommand": `python -c 'print("ok")'`,
	}}
	if err := (VLLMProvider{Run: run}).Health(context.Background(), manifest, directory); err != nil {
		t.Fatalf("health failed: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("smoke calls=%#v, want one call", calls)
	}
	want := []string{directory, "python", "-c", `print("ok")`}
	if len(calls[0]) != len(want) {
		t.Fatalf("smoke argv=%#v, want %#v", calls[0], want)
	}
	for i := range want {
		if calls[0][i] != want[i] {
			t.Fatalf("smoke argv[%d]=%q, want %q", i, calls[0][i], want[i])
		}
	}

	for _, healthPath := range []string{"https://127.0.0.1/health", "/health?probe=1", "/health#fragment", "/health\u200b"} {
		manifest.Metadata["verify.healthPath"] = healthPath
		if err := (VLLMProvider{Run: run}).Health(context.Background(), manifest, directory); err == nil {
			t.Errorf("unsafe health path %q was accepted", healthPath)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	if err := runRuntimeSmokeCommand(ctx, func(context.Context, string, string, ...string) ([]byte, error) {
		called = true
		return nil, nil
	}, directory, "python -c test"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled smoke command error = %v", err)
	}
	if called {
		t.Fatal("canceled smoke command reached runner")
	}
	if err := runRuntimeSmokeCommand(context.Background(), func(context.Context, string, string, ...string) ([]byte, error) {
		return nil, errors.New("smoke failed")
	}, directory, "python -c test"); err == nil || !strings.Contains(err.Error(), "runtime smoke command") {
		t.Fatalf("smoke failure error = %v", err)
	}
}

func TestRuntime_RejectsImplicitPrivateRuntimeURLs(t *testing.T) {
	for _, raw := range []string{
		"https://127.0.0.1/runtime.whl",
		"https://10.0.0.8/runtime.whl",
		"https://192.168.1.10/runtime.whl",
		"https://169.254.169.254/latest/meta-data",
		"https://[::1]/runtime.whl",
		"https://[::1%25lo0]/runtime.whl",      // loopback IPv6 with a zone id
		"https://[fe80::1%25eth0]/runtime.whl", // link-local IPv6 with a zone id
		"https://localhost/runtime.whl",
		"https://metadata.google.internal/compute",
		"https://service.cluster.local/runtime.whl",
		"https://2130706433/runtime.whl", // 127.0.0.1 as a decimal integer
		"https://0x7f000001/runtime.whl", // 127.0.0.1 as hexadecimal
		"https://0177.0.0.1/runtime.whl", // legacy octal dotted form
		"https://0127.0.0.1/runtime.whl", // octal/decimal ambiguity must fail closed
		"https://127.1/runtime.whl",      // abbreviated dotted loopback
	} {
		if validRuntimeURL(raw, nil) {
			t.Errorf("private runtime URL was accepted: %s", raw)
		}
	}
	for _, host := range []string{"2130706433", "0x7f000001", "0177.0.0.1", "127.1"} {
		if ip, ok := numericIPv4(host); !ok || !ip.IsLoopback() {
			t.Errorf("numeric host %q parsed as %v, ok=%v; want loopback", host, ip, ok)
		}
	}
	if ip, ok := numericIPv4("0127.0.0.1"); !ok || ip.Equal(net.IPv4(87, 0, 0, 1)) == false {
		t.Errorf("numeric host %q parsed as %v, ok=%v; want historical octal result", "0127.0.0.1", ip, ok)
	}
	for _, host := range []string{"::1%lo0", "fe80::1%eth0"} {
		ip, ok := runtimeIPLiteral(host)
		if !ok || !restrictedRuntimeIP(ip) {
			t.Errorf("zoned IPv6 host %q parsed as %v, ok=%v; want restricted literal", host, ip, ok)
		}
	}
	if !validRuntimeURL("https://127.0.0.1/runtime.whl", []string{"https://127.0.0.1"}) {
		t.Fatal("an explicitly allowlisted private mirror should be accepted")
	}
	if validGitSource("https://127.0.0.1/repo.git", nil) {
		t.Fatal("private HTTPS git source was accepted")
	}
	if validGitSource("git@127.0.0.1:repo.git", nil) {
		t.Fatal("private SSH git source was accepted")
	}
	if validGitSource("git@[::1]:repo.git", nil) {
		t.Fatal("private IPv6 SSH git source was accepted")
	}
	if validGitSource("git@[::1]repo.git", nil) {
		t.Fatal("malformed bracketed SSH git source was accepted")
	}
	if !validGitSource("git@[::1]:repo.git", []string{"git@[::1]:repo.git"}) {
		t.Fatal("explicitly allowlisted private IPv6 SSH source was rejected")
	}
	for _, source := range []string{
		" https://github.com/example/repo.git",
		"https://github.com/example/repo.git ",
		"https://github.com/example/repo.git?ref=main",
		"https://github.com/example/repo.git#fragment",
		"https://github.com/example/repo.git%0a",
	} {
		if validGitSource(source, nil) {
			t.Fatalf("unsafe HTTPS git source was accepted: %q", source)
		}
	}
}

func TestRuntime_PrivateArtifactRedirectRequiresExplicitAllowlist(t *testing.T) {
	u, err := url.Parse("https://127.0.0.1/artifact.whl")
	if err != nil {
		t.Fatal(err)
	}
	if runtimeURLHostSafe(u, nil) {
		t.Fatal("private redirect destination should be unsafe without an allowlist")
	}
	if !runtimeURLHostSafe(u, []string{"https://127.0.0.1"}) {
		t.Fatal("explicit private redirect allowlist was ignored")
	}
}

func TestRuntime_PrivateArtifactRedirectCannotBeIntroducedByCustomPolicy(t *testing.T) {
	root := t.TempDir()
	transportCalls := 0
	client := &http.Client{
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if req == nil || req.URL == nil {
				t.Fatal("redirect callback received an invalid request")
			}
			// A custom callback is allowed to rewrite a redirect request. The
			// runtime boundary must validate the final URL after this callback,
			// not only the Location header supplied by the first response.
			req.URL, _ = url.Parse("https://127.0.0.1/secret")
			return nil
		},
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			transportCalls++
			if transportCalls == 1 {
				return &http.Response{
					StatusCode: http.StatusFound,
					Header:     http.Header{"Location": []string{"https://example.test/next"}},
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    req,
				}, nil
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("unsafe")), Header: make(http.Header), Request: req}, nil
		}),
	}
	_, _, err := downloadRuntimeArtifact(context.Background(), client, "https://example.test/start.whl", filepath.Join(root, "stage"), 1<<20, nil)
	if err == nil || !strings.Contains(err.Error(), "redirect is not allowed") {
		t.Fatalf("unsafe callback redirect error = %v", err)
	}
	if transportCalls != 1 {
		t.Fatalf("unsafe redirect reached transport: calls=%d", transportCalls)
	}
}

func TestRuntime_VLLMCheckForUpdateSelectsStableAndPrereleaseChannels(t *testing.T) {
	const endpoint = "https://pypi.test/pypi/vllm/json"
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != endpoint {
			return nil, errors.New("unexpected update endpoint: " + req.URL.String())
		}
		body := `{"info":{"version":"0.6.0"},"releases":{"0.5.9":{},"0.6.0":{},"0.7.0rc1":{}}}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
	})}
	provider := VLLMProvider{Client: client, UpdateURL: endpoint}
	desired := Spec{Name: "vllm", Kind: "vllm", SourceType: "pypi", Source: "pypi"}
	stable, available, err := provider.CheckForUpdate(context.Background(), Manifest{Version: "0.5.9"}, desired, UpdatePolicy{Channel: "stable"})
	if err != nil || !available || stable.Version != "0.6.0" {
		t.Fatalf("stable candidate=%+v available=%v err=%v", stable, available, err)
	}
	prerelease, available, err := provider.CheckForUpdate(context.Background(), Manifest{Version: "0.6.0"}, desired, UpdatePolicy{Channel: "prerelease"})
	if err != nil || !available || prerelease.Version != "0.7.0rc1" {
		t.Fatalf("prerelease candidate=%+v available=%v err=%v", prerelease, available, err)
	}
}

func TestRuntime_VLLMCheckForUpdateNeverDowngradesFromStaleIndex(t *testing.T) {
	const endpoint = "https://pypi.test/pypi/vllm/json"
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{"info":{"version":"0.8.4"},"releases":{"0.8.4":{}}}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
	})}
	provider := VLLMProvider{Client: client, UpdateURL: endpoint}
	desired := Spec{Name: "vllm", Kind: "vllm", SourceType: "pypi", Source: "pypi"}
	candidate, available, err := provider.CheckForUpdate(context.Background(), Manifest{Version: "0.8.5"}, desired, UpdatePolicy{Channel: "stable"})
	if err != nil || available || candidate.Version != "" {
		t.Fatalf("stale index advertised a downgrade: candidate=%+v available=%v err=%v", candidate, available, err)
	}
}

func TestRuntime_LMCacheCheckForUpdateHonorsChannelAndStaleIndex(t *testing.T) {
	const endpoint = "https://pypi.test/pypi/lmcache/json"
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != endpoint {
			return nil, errors.New("unexpected update endpoint: " + req.URL.String())
		}
		body := `{"info":{"version":"0.6.0rc1"},"releases":{"0.5.9":{},"0.6.0":{},"0.7.0rc1":{}}}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
	})}
	provider := LMCacheProvider{Client: client, UpdateURL: endpoint}
	desired := Spec{Name: "lmcache", Kind: "lmcache", SourceType: "pypi", Source: "pypi", BuildArgs: map[string][]string{"package": {"lmcache"}}}
	stable, available, err := provider.CheckForUpdate(context.Background(), Manifest{Version: "0.5.9"}, desired, UpdatePolicy{Channel: "stable"})
	if err != nil || !available || stable.Version != "0.6.0" {
		t.Fatalf("stable candidate=%+v available=%v err=%v", stable, available, err)
	}
	prerelease, available, err := provider.CheckForUpdate(context.Background(), Manifest{Version: "0.6.0"}, desired, UpdatePolicy{Channel: "prerelease"})
	if err != nil || !available || prerelease.Version != "0.7.0rc1" {
		t.Fatalf("prerelease candidate=%+v available=%v err=%v", prerelease, available, err)
	}
	latestPrerelease, err := provider.LatestVersionForChannel(context.Background(), "lmcache", "prerelease")
	if err != nil || latestPrerelease != "0.7.0rc1" {
		t.Fatalf("LatestVersionForChannel(prerelease) = %q, want 0.7.0rc1 (err=%v)", latestPrerelease, err)
	}
	stale, available, err := provider.CheckForUpdate(context.Background(), Manifest{Version: "0.8.0"}, desired, UpdatePolicy{Channel: "stable"})
	if err != nil || available || stale.Version != "" {
		t.Fatalf("stale index advertised a downgrade: candidate=%+v available=%v err=%v", stale, available, err)
	}
}

func TestRuntime_LMCacheLatestVersionUsesConfiguredIndexMirror(t *testing.T) {
	const (
		indexURL = "https://mirror.example.test/repository/simple"
		endpoint = "https://mirror.example.test/repository/pypi/lmcache/json"
	)
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != endpoint {
			return nil, errors.New("unexpected update endpoint: " + req.URL.String())
		}
		body := `{"info":{"version":"0.6.0"},"releases":{"0.6.0":{}}}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
	})}
	provider := LMCacheProvider{
		Client:          client,
		IndexURL:        indexURL,
		SourceAllowlist: []string{indexURL},
	}
	version, err := provider.LatestVersionForChannel(context.Background(), "lmcache", "stable")
	if err != nil || version != "0.6.0" {
		t.Fatalf("LatestVersionForChannel() = %q, want 0.6.0 (err=%v)", version, err)
	}
}

func TestRuntime_LMCacheCheckForUpdateUsesContextCancellation(t *testing.T) {
	const endpoint = "https://pypi.test/pypi/lmcache/json"
	started := make(chan struct{})
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(started)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	provider := LMCacheProvider{Client: client, UpdateURL: endpoint}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, _, err := provider.CheckForUpdate(ctx, Manifest{Version: "0.5.9"}, Spec{Name: "lmcache", Kind: "lmcache", SourceType: "pypi", Source: "pypi"}, UpdatePolicy{Channel: "stable"})
		result <- err
	}()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("CheckForUpdate() error = %v, want context cancellation", err)
	}
}

func TestRuntime_GitTrackRefCheckForUpdateResolvesImmutableCommit(t *testing.T) {
	const (
		oldCommit = "1111111111111111111111111111111111111111"
		newCommit = "2222222222222222222222222222222222222222"
		trackRef  = "refs/heads/main"
	)
	for name, checker := range map[string]UpdateChecker{
		"vllm": VLLMProvider{
			SourceAllowlist: []string{"https://github.com"},
			Run: func(_ context.Context, _ string, command string, args ...string) ([]byte, error) {
				if command != "git" || strings.Join(args, " ") != "ls-remote --exit-code https://github.com/1CatAI/1Cat-vLLM.git "+trackRef {
					t.Fatalf("unexpected vLLM update command: %s %q", command, args)
				}
				return []byte(newCommit + "\t" + trackRef + "\n"), nil
			},
		},
		"llamacpp": LlamaCPPProvider{
			SourceAllowlist: []string{"https://github.com"},
			Run: func(_ context.Context, _ string, command string, args ...string) ([]byte, error) {
				if command != "git" || strings.Join(args, " ") != "ls-remote --exit-code https://github.com/ggerganov/llama.cpp.git "+trackRef {
					t.Fatalf("unexpected llama.cpp update command: %s %q", command, args)
				}
				return []byte(newCommit + "\t" + trackRef + "\n"), nil
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			repository := "https://github.com/1CatAI/1Cat-vLLM.git"
			kind := "vllm"
			if name == "llamacpp" {
				repository = "https://github.com/ggerganov/llama.cpp.git"
				kind = "llamacpp"
			}
			desired := Spec{
				Name: name, Kind: kind, SourceType: "git", Source: repository, Ref: trackRef,
				Metadata: map[string]string{"trackRef": trackRef},
			}
			candidate, available, err := checker.CheckForUpdate(context.Background(), Manifest{
				Name: name, Kind: kind, Version: "git-" + oldCommit, Source: repository, Ref: oldCommit, Commit: oldCommit,
			}, desired, UpdatePolicy{})
			if err != nil {
				t.Fatal(err)
			}
			if !available || candidate.Version != "git-"+newCommit || candidate.Ref != newCommit || candidate.Commit != newCommit {
				t.Fatalf("candidate=%+v available=%v", candidate, available)
			}
			if candidate.Metadata["trackRef"] != trackRef {
				t.Fatalf("candidate trackRef=%q", candidate.Metadata["trackRef"])
			}
			candidate, available, err = checker.CheckForUpdate(context.Background(), Manifest{}, desired, UpdatePolicy{})
			if err != nil {
				t.Fatal(err)
			}
			if !available || candidate.Commit != newCommit {
				t.Fatalf("initial candidate=%+v available=%v", candidate, available)
			}
		})
	}
}

func TestRuntime_GitExactRefDoesNotAdvanceWithoutTrackRef(t *testing.T) {
	run := func(context.Context, string, string, ...string) ([]byte, error) {
		t.Fatal("exact Git ref unexpectedly queried the remote")
		return nil, nil
	}
	provider := VLLMProvider{SourceAllowlist: []string{"https://github.com"}, Run: run}
	desired := Spec{Name: "vllm", Kind: "vllm", SourceType: "git", Source: "https://github.com/1CatAI/1Cat-vLLM.git", Ref: "v1.2.0"}
	_, available, err := provider.CheckForUpdate(context.Background(), Manifest{Name: "vllm", Kind: "vllm", Version: "v1.2.0", Ref: "v1.2.0"}, desired, UpdatePolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if available {
		t.Fatal("exact Git ref unexpectedly reported an update")
	}
}

func TestRuntime_GitTrackRefNormalizesConciseBranchName(t *testing.T) {
	const commit = "3333333333333333333333333333333333333333"
	provider := VLLMProvider{
		SourceAllowlist: []string{"https://github.com"},
		Run: func(_ context.Context, _ string, command string, args ...string) ([]byte, error) {
			if command != "git" || len(args) != 4 || args[0] != "ls-remote" || args[3] != "refs/heads/main" {
				t.Fatalf("unexpected normalized ref command: %s %q", command, args)
			}
			return []byte(commit + "\trefs/heads/main\n"), nil
		},
	}
	candidate, available, err := provider.CheckForUpdate(context.Background(), Manifest{Version: "git-old", Commit: "4444444444444444444444444444444444444444"}, Spec{
		Name: "vllm", Kind: "vllm", SourceType: "git", Source: "https://github.com/1CatAI/1Cat-vLLM.git", Ref: "main",
		Metadata: map[string]string{"trackRef": "main"},
	}, UpdatePolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if !available || candidate.Commit != commit {
		t.Fatalf("candidate=%+v available=%v", candidate, available)
	}
}

func TestRuntime_GitTrackRefUsesLegacyVersionCommit(t *testing.T) {
	const commit = "7777777777777777777777777777777777777777"
	provider := VLLMProvider{
		SourceAllowlist: []string{"https://github.com"},
		Run: func(_ context.Context, _ string, command string, args ...string) ([]byte, error) {
			if command != "git" || len(args) != 4 || args[0] != "ls-remote" || args[3] != "refs/heads/main" {
				t.Fatalf("unexpected normalized ref command: %s %q", command, args)
			}
			return []byte(commit + "\trefs/heads/main\n"), nil
		},
	}
	candidate, available, err := provider.CheckForUpdate(context.Background(), Manifest{
		Version: "git-" + commit,
	}, Spec{
		Name: "vllm", Kind: "vllm", SourceType: "git", Source: "https://github.com/1CatAI/1Cat-vLLM.git", Ref: "main",
		Metadata: map[string]string{"trackRef": "main"},
	}, UpdatePolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if available || candidate.Version != "git-"+commit {
		t.Fatalf("candidate=%+v available=%v", candidate, available)
	}
}

func TestRuntime_BoundedMetadataJSONRejectsOversizeAndTrailingValues(t *testing.T) {
	var value struct {
		Version string `json:"version"`
	}
	if err := decodeBoundedRuntimeJSON(strings.NewReader(`{"version":"ok"} {"version":"ignored"}`), 1<<20, &value); err == nil {
		t.Fatal("trailing JSON value was accepted")
	}
	if err := decodeBoundedRuntimeJSON(strings.NewReader(`{"version":"12345"}`), 8, &value); err == nil || !strings.Contains(err.Error(), "exceeds 8 bytes") {
		t.Fatalf("oversized metadata error = %v", err)
	}
}

func TestRuntime_ProviderUpdateChecksRejectOversizedMetadata(t *testing.T) {
	oversizedVLLM := `{"info":{"version":"0.9.0"},"releases":{"` + strings.Repeat("x", int(maxRuntimeMetadataBytes)) + `":{}}}`
	oversizedRelease := `[{"tag_name":"b4200","prerelease":false,"padding":"` + strings.Repeat("x", int(maxRuntimeMetadataBytes)) + `"}]`
	tests := []struct {
		name  string
		check func(*http.Client) error
	}{
		{
			name: "vllm",
			check: func(client *http.Client) error {
				provider := VLLMProvider{Client: client, UpdateURL: "https://pypi.test/pypi/vllm/json"}
				_, _, err := provider.CheckForUpdate(context.Background(), Manifest{Version: "0.8.0"}, Spec{Name: "vllm", Kind: "vllm", SourceType: "pypi", Source: "pypi"}, UpdatePolicy{Channel: "stable"})
				return err
			},
		},
		{
			name: "llamacpp",
			check: func(client *http.Client) error {
				provider := LlamaCPPProvider{Client: client, UpdateURL: "https://api.github.test/repos/acme/llama.cpp/releases"}
				_, _, err := provider.CheckForUpdate(context.Background(), Manifest{Version: "b4100"}, Spec{Name: "llama", Kind: "llamacpp", SourceType: "release", Source: "https://github.com/acme/llama.cpp/releases/download/current/source.tar.gz"}, UpdatePolicy{Channel: "stable"})
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := oversizedVLLM
			if test.name == "llamacpp" {
				body = oversizedRelease
			}
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
			})}
			err := test.check(client)
			if err == nil || !strings.Contains(err.Error(), "runtime metadata exceeds") {
				t.Fatalf("oversized metadata error = %v", err)
			}
		})
	}
}

func TestRuntime_UpdateChecksRejectPrivateRedirects(t *testing.T) {
	tests := []struct {
		name  string
		check func(context.Context, *http.Client) error
	}{
		{
			name: "vllm",
			check: func(ctx context.Context, client *http.Client) error {
				provider := VLLMProvider{Client: client, UpdateURL: "https://example.test/pypi/vllm/json"}
				_, _, err := provider.CheckForUpdate(ctx, Manifest{Version: "0.8.0"}, Spec{Name: "vllm", Kind: "vllm", SourceType: "pypi", Source: "pypi"}, UpdatePolicy{Channel: "stable"})
				return err
			},
		},
		{
			name: "llamacpp",
			check: func(ctx context.Context, client *http.Client) error {
				provider := LlamaCPPProvider{Client: client, UpdateURL: "https://example.test/repos/acme/llama.cpp/releases"}
				_, _, err := provider.CheckForUpdate(ctx, Manifest{Version: "b4100"}, Spec{Name: "llama", Kind: "llamacpp", SourceType: "release", Source: "https://github.com/acme/llama.cpp/releases/download/current/source.tar.gz"}, UpdatePolicy{Channel: "stable"})
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if calls > 1 {
					return nil, errors.New("private redirect reached transport")
				}
				return &http.Response{
					StatusCode: http.StatusFound,
					Header:     http.Header{"Location": []string{"https://127.0.0.1/metadata"}},
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    req,
				}, nil
			})}
			err := test.check(context.Background(), client)
			if err == nil || !strings.Contains(err.Error(), "redirect is not allowed") {
				t.Fatalf("redirect error = %v", err)
			}
			if calls != 1 {
				t.Fatalf("private redirect reached transport %d times", calls)
			}
		})
	}
}

func TestRuntime_VersionComparisonDoesNotUseLexicalOrdering(t *testing.T) {
	if compareRuntimeVersions("0.10.0", "0.9.0") <= 0 {
		t.Fatal("0.10.0 should sort after 0.9.0")
	}
	if compareRuntimeVersions("0.7.0rc1", "0.7.0") >= 0 {
		t.Fatal("prerelease should sort before stable release")
	}
}

func TestRuntime_LlamaCPPCheckForUpdateSelectsReleaseChannel(t *testing.T) {
	const endpoint = "https://api.github.test/repos/acme/llama.cpp/releases"
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != endpoint || req.Header.Get("Accept") == "" {
			return nil, errors.New("unexpected release request")
		}
		body := `[{"tag_name":"b4200","prerelease":false},{"tag_name":"b4300-rc1","prerelease":true},{"tag_name":"b4100","prerelease":false}]`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
	})}
	provider := LlamaCPPProvider{Client: client, UpdateURL: endpoint}
	desired := Spec{Name: "llama", Kind: "llamacpp", SourceType: "release", Source: "https://github.com/acme/llama.cpp/releases/download/current/source.tar.gz"}
	candidate, available, err := provider.CheckForUpdate(context.Background(), Manifest{Version: "b4100"}, desired, UpdatePolicy{Channel: "stable"})
	if err != nil || !available || candidate.Version != "b4200" || candidate.Source != "https://github.com/acme/llama.cpp/releases/download/b4200/source.tar.gz" {
		t.Fatalf("stable candidate=%+v available=%v err=%v", candidate, available, err)
	}
	candidate, available, err = provider.CheckForUpdate(context.Background(), Manifest{Version: "b4200"}, desired, UpdatePolicy{Channel: "prerelease"})
	if err != nil || !available || candidate.Version != "b4300-rc1" {
		t.Fatalf("prerelease candidate=%+v available=%v err=%v", candidate, available, err)
	}
}

func TestRuntime_GitHubReleaseEndpoint(t *testing.T) {
	if got := githubReleaseEndpoint("https://github.com/acme/llama.cpp/archive/refs/tags/b4200.tar.gz"); got != "https://api.github.com/repos/acme/llama.cpp/releases" {
		t.Fatalf("endpoint = %q", got)
	}
	if got := githubReleaseEndpoint("https://example.test/acme/llama.cpp"); got != "" {
		t.Fatalf("non-GitHub source endpoint = %q", got)
	}
	for _, source := range []string{
		"https://user:secret@github.com/acme/llama.cpp",
		"https://github.com/acme/llama.cpp?download=1",
		"https://github.com/acme/llama.cpp#releases",
	} {
		if got := githubReleaseEndpoint(source); got != "" {
			t.Fatalf("unsafe GitHub source %q produced endpoint %q", source, got)
		}
	}
}

func TestRuntime_LlamaReleaseSourceDerivesExactTagArchive(t *testing.T) {
	tests := []struct {
		name    string
		source  string
		version string
		want    string
		ok      bool
	}{
		{
			name:    "repository root",
			source:  "https://github.com/ggml-org/llama.cpp",
			version: "b4200",
			want:    "https://github.com/ggml-org/llama.cpp/archive/refs/tags/b4200.tar.gz",
			ok:      true,
		},
		{
			name:    "tag archive preserves zip suffix",
			source:  "https://github.com/ggml-org/llama.cpp/archive/refs/tags/b4100.zip",
			version: "b4200",
			want:    "https://github.com/ggml-org/llama.cpp/archive/refs/tags/b4200.zip",
			ok:      true,
		},
		{
			name:    "release asset replaces tag only",
			source:  "https://github.com/ggml-org/llama.cpp/releases/download/current/llama-source.tar.gz",
			version: "b4200",
			want:    "https://github.com/ggml-org/llama.cpp/releases/download/b4200/llama-source.tar.gz",
			ok:      true,
		},
		{
			name:   "custom mirror unchanged",
			source: "https://mirror.example/llama/source.tar.gz",
			want:   "",
		},
		{
			name:   "query is not rewritten",
			source: "https://github.com/ggml-org/llama.cpp?download=1",
			want:   "",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := llamaReleaseSource(test.source, test.version)
			if ok != test.ok || got != test.want {
				t.Fatalf("source=%q version=%q -> %q, ok=%v; want %q, ok=%v", test.source, test.version, got, ok, test.want, test.ok)
			}
		})
	}
	for _, version := range []string{"b4200/evil", "b4200?download=1", "b4200#fragment", " b4200", "b4200\u200b"} {
		if got, ok := llamaReleaseSource("https://github.com/ggml-org/llama.cpp", version); ok || got != "" {
			t.Fatalf("unsafe release version %q was rewritten to %q (ok=%v)", version, got, ok)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestRuntime_LlamaCPPProviderRejectsSymlinkedLocalSource(t *testing.T) {
	if stdruntime.GOOS != "linux" {
		t.Skip("managed llama.cpp providers are Linux-only")
	}
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "source")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	called := false
	provider := LlamaCPPProvider{Run: func(context.Context, string, string, ...string) ([]byte, error) {
		called = true
		return nil, errors.New("runner should not be called")
	}}
	_, err := provider.Stage(context.Background(), Spec{Name: "llama", Kind: "llamacpp", Version: "1", SourceType: "local", Source: link}, filepath.Join(root, "destination"))
	if err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("symlink source error = %v", err)
	}
	if called {
		t.Fatal("provider invoked a build command for a symlinked source")
	}
}

func TestRuntime_CloneMetadataCopiesAndSkipsEmptyKeys(t *testing.T) {
	input := map[string]string{"torch": "2.5", " ": "ignored"}
	copy := cloneMetadata(input)
	copy["torch"] = "2.6"
	if input["torch"] != "2.5" || len(copy) != 1 || copy["torch"] != "2.6" {
		t.Fatalf("metadata copy = %#v, input = %#v", copy, input)
	}
}

func TestRuntime_RejectSymlinkPath(t *testing.T) {
	root := t.TempDir()
	regular := filepath.Join(root, "regular")
	if err := os.WriteFile(regular, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rejectSymlinkPath(regular); err != nil {
		t.Fatalf("regular path rejected: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	if err := rejectSymlinkPath(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink error = %v", err)
	}
}

func TestRuntime_RejectSymlinkPathRejectsParentComponents(t *testing.T) {
	if stdruntime.GOOS == "windows" {
		t.Skip("directory symlinks require elevated privileges on Windows")
	}
	parent := t.TempDir()
	realParent := filepath.Join(parent, "real-parent")
	if err := os.MkdirAll(realParent, 0o750); err != nil {
		t.Fatal(err)
	}
	linkedParent := filepath.Join(parent, "linked-parent")
	if err := os.Symlink(realParent, linkedParent); err != nil {
		t.Fatal(err)
	}
	if err := rejectSymlinkPath(filepath.Join(linkedParent, "artifact.whl")); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlinked parent was accepted: %v", err)
	}
}

func TestRuntime_CopyProviderRejectsSymlinkRootsAndSupportsNilContext(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "runtime.bin"), []byte("runtime"), 0o640); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "destination")
	if err := os.Mkdir(destination, 0o750); err != nil {
		t.Fatal(err)
	}
	provider := CopyProvider{}
	manifest, err := provider.Stage(nilRuntimeProviderContext(), Spec{Name: "copy", Kind: "copy", Version: "1", Source: source}, destination)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Version != "1" {
		t.Fatalf("manifest = %+v", manifest)
	}
	if got, err := os.ReadFile(filepath.Join(destination, "runtime.bin")); err != nil || string(got) != "runtime" {
		t.Fatalf("copied runtime = %q, err=%v", got, err)
	}
	if err := provider.Verify(nilRuntimeProviderContext(), manifest, destination); err != nil {
		t.Fatalf("verify with nil context: %v", err)
	}
	if err := provider.Health(nilRuntimeProviderContext(), manifest, destination); err != nil {
		t.Fatalf("health with nil context: %v", err)
	}

	link := filepath.Join(root, "source-link")
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Stage(context.Background(), Spec{Name: "copy", Kind: "copy", Version: "2", Source: link}, filepath.Join(root, "destination-link")); err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("symlink source error = %v", err)
	}

	badDestination := filepath.Join(root, "destination-link-target")
	if err := os.Symlink(destination, badDestination); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Stage(context.Background(), Spec{Name: "copy", Kind: "copy", Version: "3", Source: source}, badDestination); err == nil || !strings.Contains(err.Error(), "destination must be a real directory") {
		t.Fatalf("symlink destination error = %v", err)
	}
}

func TestRuntime_CopyProviderRejectsParentDirectorySymlinkTarget(t *testing.T) {
	if stdruntime.GOOS == "windows" {
		t.Skip("directory symlinks require elevated privileges on Windows")
	}
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(source, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..", filepath.Join(source, "escape")); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "destination")
	if err := os.MkdirAll(destination, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := (CopyProvider{}).Stage(context.Background(), Spec{Name: "copy", Kind: "copy", Version: "1", Source: source}, destination); err == nil || !strings.Contains(err.Error(), "escapes source") {
		t.Fatalf("parent-directory symlink target accepted: %v", err)
	}
}

func TestRuntime_VLLMProviderVerifiesLocalWheelChecksum(t *testing.T) {
	if stdruntime.GOOS != "linux" {
		t.Skip("managed vLLM providers are Linux-only")
	}
	root := t.TempDir()
	wheel := filepath.Join(root, "vllm.whl")
	if err := os.WriteFile(wheel, []byte("wheel-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("wheel-bytes"))
	correct := hex.EncodeToString(digest[:])
	var calls [][]string
	run := func(_ context.Context, dir, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		if name == "uv" && len(args) > 0 && args[0] == "venv" {
			if err := os.MkdirAll(args[len(args)-1], 0o750); err != nil {
				return nil, err
			}
		}
		_ = dir
		return nil, nil
	}
	provider := VLLMProvider{Run: run, SourceAllowlist: []string{root}}
	destination := filepath.Join(root, "stage")
	manifest, err := provider.Stage(context.Background(), Spec{Name: "vllm", Kind: "vllm", Version: "1", SourceType: "local", Source: wheel, Checksum: correct}, destination)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Metadata["sourceChecksum"] != correct || manifest.Metadata["checksum"] != correct {
		t.Fatalf("manifest checksum metadata = %#v", manifest.Metadata)
	}
	// A best-effort environment probe may invoke the venv Python after the
	// install.  The two uv operations are the contract; tolerate that optional
	// probe so the checksum test remains valid on Linux as well as Darwin.
	if len(calls) < 2 || calls[0][0] != "uv" || len(calls[0]) < 2 || calls[0][1] != "venv" || calls[1][0] != "uv" || len(calls[1]) < 2 || calls[1][1] != "pip" {
		t.Fatalf("unexpected uv calls: %#v", calls)
	}
	_, err = provider.Stage(context.Background(), Spec{Name: "vllm", Kind: "vllm", Version: "2", SourceType: "local", Source: wheel, Checksum: strings.Repeat("0", 64)}, filepath.Join(root, "bad"))
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("checksum mismatch error = %v", err)
	}
}

func TestRuntime_VLLMProviderVerifyRequiresLaunchEntrypoints(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "vllm")
	if err := os.MkdirAll(filepath.Join(directory, ".venv"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "metadata.json"), []byte(`{}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := (VLLMProvider{}).Verify(context.Background(), Manifest{Kind: "vllm"}, directory); err == nil || !strings.Contains(err.Error(), "entrypoint python") {
		t.Fatalf("missing vLLM entrypoint error = %v", err)
	}
	writeTestVLLMEntrypoints(t, directory)
	if err := (VLLMProvider{}).Verify(context.Background(), Manifest{Kind: "vllm"}, directory); err != nil {
		t.Fatalf("valid vLLM entrypoints rejected: %v", err)
	}
	if err := os.Chmod(filepath.Join(directory, ".venv", "bin", "vllm"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := (VLLMProvider{}).Verify(context.Background(), Manifest{Kind: "vllm"}, directory); err == nil || !strings.Contains(err.Error(), "not executable") {
		t.Fatalf("non-executable vLLM entrypoint error = %v", err)
	}
}

func TestRuntime_ProvidersHonorVerifySHA256Metadata(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "vllm")
	if err := os.MkdirAll(filepath.Join(directory, ".venv"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeTestVLLMEntrypoints(t, directory)
	digest := sha256.Sum256([]byte("runtime-artifact"))
	correct := hex.EncodeToString(digest[:])
	metadata := []byte(`{"sourceChecksum":"` + correct + `","verify.sha256":"sha256:` + correct + `"}`)
	if err := os.WriteFile(filepath.Join(directory, "metadata.json"), metadata, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := (VLLMProvider{}).Verify(context.Background(), Manifest{Kind: "vllm", Metadata: map[string]string{"verify.sha256": correct}}, directory); err != nil {
		t.Fatalf("matching verify.sha256 rejected: %v", err)
	}
	badMetadata := []byte(`{"sourceChecksum":"` + correct + `","verify.sha256":"` + strings.Repeat("0", 64) + `"}`)
	if err := os.WriteFile(filepath.Join(directory, "metadata.json"), badMetadata, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := (VLLMProvider{}).Verify(context.Background(), Manifest{Kind: "vllm"}, directory); err == nil || !strings.Contains(err.Error(), "verification checksum mismatch") {
		t.Fatalf("mismatched verify.sha256 error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(directory, "metadata.json"), []byte(`{"verify.sha256":"invalid"}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := (VLLMProvider{}).Verify(context.Background(), Manifest{Kind: "vllm"}, directory); err == nil || !strings.Contains(err.Error(), "verification checksum") {
		t.Fatalf("invalid verify.sha256 error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(directory, "metadata.json"), bytes.Repeat([]byte("x"), int(maxRuntimeManifestBytes)+1), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := (VLLMProvider{}).Verify(context.Background(), Manifest{Kind: "vllm"}, directory); err == nil || !strings.Contains(err.Error(), "metadata exceeds") {
		t.Fatalf("oversized metadata error = %v", err)
	}
}

func TestRuntime_ProviderVerifyRejectsMalformedManifestReferences(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "vllm")
	if err := os.MkdirAll(filepath.Join(directory, ".venv"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeTestVLLMEntrypoints(t, directory)
	if err := os.WriteFile(filepath.Join(directory, "metadata.json"), []byte(`{"sourceType":"git","source":"https://example.test/vllm.git"}`), 0o640); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"", "-c", "refs/heads/main@{1}"} {
		manifest := Manifest{Kind: "vllm", Ref: ref}
		if err := (VLLMProvider{}).Verify(context.Background(), manifest, directory); err == nil {
			t.Fatalf("manifest ref %q unexpectedly passed provider verification", ref)
		}
	}
	if err := os.WriteFile(filepath.Join(directory, "metadata.json"), []byte("null"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := (VLLMProvider{}).Verify(context.Background(), Manifest{Kind: "vllm"}, directory); err == nil || !strings.Contains(err.Error(), "must be an object") {
		t.Fatalf("null metadata error = %v", err)
	}
	// Older manifests may not have copied source/sourceType into metadata. The
	// provider must still infer the exact-ref requirement from the manifest's
	// source instead of treating the missing metadata as a bundled runtime.
	if err := os.WriteFile(filepath.Join(directory, "metadata.json"), []byte(`{}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := (VLLMProvider{}).Verify(context.Background(), Manifest{Kind: "vllm", Source: "git+https://example.test/vllm.git"}, directory); err == nil || !strings.Contains(err.Error(), "requires an exact ref") {
		t.Fatalf("missing legacy metadata ref error = %v", err)
	}
	if err := (VLLMProvider{}).Verify(context.Background(), Manifest{Kind: "vllm", Source: "git+https://example.test/vllm.git", Ref: "refs/tags/v0.8.5"}, directory); err != nil {
		t.Fatalf("valid ref with legacy metadata rejected: %v", err)
	}
	legacy := Manifest{
		Kind: "vllm", Version: "git-" + strings.Repeat("5", 40),
		Source: "https://example.test/vllm.git",
	}
	if err := (VLLMProvider{}).Verify(context.Background(), legacy, directory); err != nil {
		t.Fatalf("legacy immutable version with missing ref rejected: %v", err)
	}
}

func TestRuntime_ExtractArchiveRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "source.tar.gz")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gz)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "../escape", Mode: 0o600, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	err = extractRuntimeArchive(context.Background(), archivePath, filepath.Join(root, "dest"), 1<<20)
	if err == nil || !strings.Contains(err.Error(), "escapes source root") {
		t.Fatalf("traversal archive error = %v", err)
	}
}

func TestRuntime_ExtractArchiveAcceptsLibrarySymlinkChain(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "source.tar.gz")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gz)
	writeReg := func(name string, content string) {
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o750, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	writeSym := func(name, target string) {
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: target, Mode: 0o777}); err != nil {
			t.Fatal(err)
		}
	}
	writeReg("pkg/libllama.so.0.22.0", "lib-content")
	writeSym("pkg/libllama.so.0", "libllama.so.0.22.0")
	writeSym("pkg/libllama.so", "libllama.so.0")
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "dest")
	if err := extractRuntimeArchive(context.Background(), archivePath, destination, 1<<20); err != nil {
		t.Fatalf("symlink chain archive rejected: %v", err)
	}
	if info, err := os.Lstat(filepath.Join(destination, "pkg", "libllama.so")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("libllama.so not extracted as a symlink: info=%v err=%v", info, err)
	}
	if data, err := os.ReadFile(filepath.Join(destination, "pkg", "libllama.so")); err != nil || string(data) != "lib-content" {
		t.Fatalf("symlink chain does not resolve: data=%q err=%v", data, err)
	}
}

// Prebuilt release archives mark their binaries 0755; extraction must keep
// the executable bits or the materialized llama-server fails with a clear
// "not executable" error. Regular data files keep a non-executable mode.
func TestRuntime_ExtractArchivePreservesExecutableBit(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "source.tar.gz")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gz)
	for _, entry := range []struct {
		name string
		mode int64
		data string
	}{
		{"bin/llama-server", 0o755, "#!/bin/sh\nexit 0\n"},
		{"README.txt", 0o644, "readme"},
	} {
		if err := tarWriter.WriteHeader(&tar.Header{Name: entry.name, Mode: entry.mode, Size: int64(len(entry.data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write([]byte(entry.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "dest")
	if err := extractRuntimeArchive(context.Background(), archivePath, destination, 1<<20); err != nil {
		t.Fatalf("archive rejected: %v", err)
	}
	for name, wantExec := range map[string]bool{"bin/llama-server": true, "README.txt": false} {
		info, err := os.Stat(filepath.Join(destination, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if got := (info.Mode().Perm() & 0o111) != 0; got != wantExec {
			t.Fatalf("%s executable=%v mode=%v, want executable=%v", name, got, info.Mode(), wantExec)
		}
	}
}

func TestRuntime_ExtractArchiveRejectsUnsafeSymlinks(t *testing.T) {
	cases := []struct {
		name     string
		linkname string
		wantErr  string
	}{
		{"absolute", "/etc/passwd", "unsafe target"},
		{"escaping", "../../../../etc/passwd", "escapes the source root"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			archivePath := filepath.Join(root, "source.tar.gz")
			file, err := os.Create(archivePath)
			if err != nil {
				t.Fatal(err)
			}
			gz := gzip.NewWriter(file)
			tarWriter := tar.NewWriter(gz)
			if err := tarWriter.WriteHeader(&tar.Header{Name: "pkg/libllama.so", Typeflag: tar.TypeSymlink, Linkname: tc.linkname, Mode: 0o777}); err != nil {
				t.Fatal(err)
			}
			_ = tarWriter.Close()
			_ = gz.Close()
			_ = file.Close()
			err = extractRuntimeArchive(context.Background(), archivePath, filepath.Join(root, "dest"), 1<<20)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("symlink %q error = %v, want %q", tc.linkname, err, tc.wantErr)
			}
		})
	}
}

func TestRuntime_ExtractArchiveCreatesSourceTree(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "source.tar.gz")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gz)
	content := []byte("cmake")
	if err := tarWriter.WriteHeader(&tar.Header{Name: "llama.cpp/CMakeLists.txt", Mode: 0o640, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(content); err != nil {
		t.Fatal(err)
	}
	_ = tarWriter.Close()
	_ = gz.Close()
	_ = file.Close()
	destination := filepath.Join(root, "dest")
	if err := extractRuntimeArchive(context.Background(), archivePath, destination, 1<<20); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(destination, "llama.cpp", "CMakeLists.txt")); err != nil || !bytes.Equal(data, content) {
		t.Fatalf("extracted content = %q, err=%v", data, err)
	}
}

func TestRuntime_ExtractArchiveRejectsSymlinkedDestinationParents(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "source.tar.gz")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gz)
	content := []byte("do not escape")
	if err := tarWriter.WriteHeader(&tar.Header{Name: "linked/escaped.txt", Mode: 0o600, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "dest")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(destination, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(destination, "linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	err = extractRuntimeArchive(context.Background(), archivePath, destination, 1<<20)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlinked destination parent error = %v, want rejection", err)
	}
	if _, statErr := os.Stat(filepath.Join(outside, "escaped.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("archive wrote through destination symlink: %v", statErr)
	}
}

func TestRuntime_ProvidersRejectSymlinkedMetadataAndRuntimeRoots(t *testing.T) {
	root := t.TempDir()
	vllmDir := filepath.Join(root, "vllm")
	llamaDir := filepath.Join(root, "llama")
	for _, directory := range []string{vllmDir, llamaDir} {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(vllmDir, ".venv"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(llamaDir, "build"), 0o750); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	outsideMetadata := filepath.Join(outside, "metadata.json")
	if err := os.WriteFile(outsideMetadata, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{vllmDir, llamaDir} {
		if err := os.Symlink(outsideMetadata, filepath.Join(directory, "metadata.json")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	if err := (VLLMProvider{}).Verify(context.Background(), Manifest{Kind: "vllm"}, vllmDir); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("vLLM metadata error = %v, want symlink rejection", err)
	}
	if err := (LlamaCPPProvider{}).Verify(context.Background(), Manifest{Kind: "llamacpp"}, llamaDir); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("llama.cpp metadata error = %v, want symlink rejection", err)
	}
	for _, directory := range []string{vllmDir, llamaDir} {
		if err := os.Remove(filepath.Join(directory, "metadata.json")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "metadata.json"), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(vllmDir, ".venv")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(vllmDir, ".venv")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := (VLLMProvider{}).Verify(context.Background(), Manifest{Kind: "vllm"}, vllmDir); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("vLLM virtualenv error = %v, want symlink rejection", err)
	}
	if err := os.Remove(filepath.Join(llamaDir, "build")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(llamaDir, "build")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := (LlamaCPPProvider{}).Verify(context.Background(), Manifest{Kind: "llamacpp"}, llamaDir); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("llama.cpp build error = %v, want symlink rejection", err)
	}
}

// fakeUVRuntime simulates uv for the venv independence tests: `uv venv`
// creates the interpreter tree, and `uv pip install --python <py>` records
// the installed package in that venv's site-packages file, so a test can
// prove exactly which venv every pip operation targeted.
type fakeUVRuntime struct {
	installs []string // "<venv-python> <package-spec>" per install
}

func (f *fakeUVRuntime) run(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
	if name == "uv" {
		switch {
		case len(args) > 0 && args[0] == "venv":
			venv := args[len(args)-1]
			binDir := filepath.Join(venv, "bin")
			if err := os.MkdirAll(binDir, 0o750); err != nil {
				return nil, err
			}
			for _, name := range []string{"python", "vllm"} {
				if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\nexit 0\n"), 0o750); err != nil {
					return nil, err
				}
			}
			return nil, nil
		case len(args) > 2 && args[0] == "pip" && args[1] == "install":
			python := ""
			for i, arg := range args {
				if arg == "--python" && i+1 < len(args) {
					python = args[i+1]
				}
			}
			if python == "" {
				return nil, errors.New("uv pip install without --python")
			}
			packageSpec := args[len(args)-1]
			f.installs = append(f.installs, python+" "+packageSpec)
			venv := filepath.Dir(filepath.Dir(python))
			file, err := os.OpenFile(filepath.Join(venv, "site-packages.txt"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
			if err != nil {
				return nil, err
			}
			_, writeErr := file.WriteString(packageSpec + "\n")
			closeErr := file.Close()
			if writeErr != nil {
				return nil, writeErr
			}
			return nil, closeErr
		}
	}
	return []byte(`{"pythonVersion":"3.12"}`), nil
}

// TestRuntime_VLLMTwoVersionsKeepIndependentVenvs is scenario T9: two vLLM
// versions staged side by side each own a dedicated virtualenv, every pip
// operation targets only the version being staged, and both versions stay
// independently launchable — no shared venv, no package pollution.
func TestRuntime_VLLMTwoVersionsKeepIndependentVenvs(t *testing.T) {
	if stdruntime.GOOS != "linux" {
		t.Skip("managed vLLM providers are Linux-only")
	}
	root := t.TempDir()
	fakeUV := &fakeUVRuntime{}
	manager, err := NewManager(root, map[string]Provider{
		"vllm": VLLMProvider{Run: func(ctx context.Context, directory, command string, args ...string) ([]byte, error) {
			return fakeUV.run(ctx, directory, command, args...)
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.SetIdleProbe(func() bool { return true })
	for _, version := range []string{"1.0.0", "2.0.0"} {
		if _, err := manager.Stage(context.Background(), Spec{
			Name: "vllm", Kind: "vllm", Mode: RuntimeModeNative, Version: version, SourceType: "pypi", Source: "pypi",
		}); err != nil {
			t.Fatalf("stage vllm %s: %v", version, err)
		}
	}
	detail, ok, err := manager.Detail("vllm")
	if err != nil || !ok {
		t.Fatalf("detail vllm: ok=%v err=%v", ok, err)
	}
	if len(detail.Versions) != 2 {
		t.Fatalf("versions = %d, want both staged versions side by side", len(detail.Versions))
	}
	venv := func(version string) string {
		return filepath.Join(root, "vllm", "versions", version, ".venv")
	}
	for _, want := range []struct{ version, spec string }{{"1.0.0", "vllm==1.0.0"}, {"2.0.0", "vllm==2.0.0"}} {
		data, err := os.ReadFile(filepath.Join(venv(want.version), "site-packages.txt"))
		if err != nil {
			t.Fatalf("version %s venv has no recorded installs: %v", want.version, err)
		}
		if string(data) != want.spec+"\n" {
			t.Fatalf("version %s venv packages = %q, want only %q (no cross-version pollution)", want.version, string(data), want.spec)
		}
	}
	// Every pip operation must target a venv under the runtime root, and the
	// two versions must have received distinct venvs: a shared target would
	// let one install pollute the other. (The manager stages each version in
	// a temporary directory before renaming it into versions/<v>, so the
	// install paths are staging paths, not the final version paths.)
	venvs := map[string]struct{}{}
	for _, line := range fakeUV.installs {
		parts := strings.Fields(line)
		if !strings.HasPrefix(parts[0], filepath.Join(root, "vllm")+string(filepath.Separator)) {
			t.Fatalf("pip install %q targeted outside the runtime root", line)
		}
		venvs[filepath.Dir(filepath.Dir(parts[0]))] = struct{}{}
	}
	if len(venvs) != 2 {
		t.Fatalf("pip installs used %d venvs for two versions, want exactly one venv per version: %v", len(venvs), venvs)
	}
	for _, version := range []string{"1.0.0", "2.0.0"} {
		binding, err := LaunchBindingForVersion(root, "vllm", "vllm", "vllm", version)
		if err != nil {
			t.Fatalf("launch binding for %s: %v", version, err)
		}
		if binding.Executable != filepath.Join(venv(version), "bin", "vllm") {
			t.Fatalf("version %s executable = %s, want its own venv vllm", version, binding.Executable)
		}
	}
}

func TestRuntime_ManagedRuntimesAreLinuxOnly(t *testing.T) {
	if stdruntime.GOOS == "linux" {
		t.Skip("managed runtime stage guard only exists off-Linux")
	}
	_, vllmErr := VLLMProvider{}.Stage(context.Background(), Spec{Name: "demo", Kind: "vllm", Version: "1.0.0", SourceType: "pypi", Source: "pypi"}, t.TempDir())
	if vllmErr == nil || !strings.Contains(vllmErr.Error(), "Linux only") {
		t.Fatalf("vLLM stage err=%v, want the Linux-only guard", vllmErr)
	}
	_, llamaErr := LlamaCPPProvider{}.Stage(context.Background(), Spec{Name: "demo", Kind: "llamacpp", Version: "b1", SourceType: "git", Source: "file:///tmp/llama.cpp"}, t.TempDir())
	if llamaErr == nil || !strings.Contains(llamaErr.Error(), "Linux only") {
		t.Fatalf("llama.cpp stage err=%v, want the Linux-only guard", llamaErr)
	}
}
