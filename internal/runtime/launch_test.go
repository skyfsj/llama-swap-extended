package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRuntime_CurrentLaunchBindingResolvesNativeEntrypoints(t *testing.T) {
	root := filepath.FromSlash("/var/lib/llama-swap/runtimes")

	t.Run("vllm", func(t *testing.T) {
		binding, err := CurrentLaunchBinding(root, "vllm-cuda", "vllm", "vllm")
		if err != nil {
			t.Fatal(err)
		}
		wantDir := filepath.Join(root, "vllm-cuda", "current", ".venv", "bin")
		if binding.BinDir != wantDir || binding.Executable != filepath.Join(wantDir, "vllm") {
			t.Fatalf("binding=%+v, want bin=%q executable=%q", binding, wantDir, filepath.Join(wantDir, "vllm"))
		}
	})

	t.Run("vllm python", func(t *testing.T) {
		binding, err := CurrentLaunchBinding(root, "vllm-cuda", "vllm", "python3")
		if err != nil {
			t.Fatal(err)
		}
		if got, want := binding.Executable, filepath.Join(root, "vllm-cuda", "current", ".venv", "bin", "python"); got != want {
			t.Fatalf("executable=%q want %q", got, want)
		}
	})

	t.Run("llamacpp", func(t *testing.T) {
		binding, err := CurrentLaunchBinding(root, "llama-cpu", "llamacpp", "llama-server")
		if err != nil {
			t.Fatal(err)
		}
		wantDir := filepath.Join(root, "llama-cpu", "current", "bin")
		if binding.BinDir != wantDir || binding.Executable != filepath.Join(wantDir, "llama-server") {
			t.Fatalf("binding=%+v, want bin=%q executable=%q", binding, wantDir, filepath.Join(wantDir, "llama-server"))
		}
		wantLib := []string{wantDir, filepath.Join(root, "llama-cpu", "current", "build", "bin")}
		if len(binding.LibraryDirs) != len(wantLib) || binding.LibraryDirs[0] != wantLib[0] || binding.LibraryDirs[1] != wantLib[1] {
			t.Fatalf("library dirs=%q, want %q", binding.LibraryDirs, wantLib)
		}
	})
}

func TestRuntime_CurrentLaunchBindingRejectsHostFallbacks(t *testing.T) {
	for _, input := range []struct {
		root, name, kind, command string
	}{
		{"relative", "vllm", "vllm", "vllm"},
		{"/var/lib/runtimes", "../vllm", "vllm", "vllm"},
		{"/var/lib/runtimes", "vllm", "vllm", "/usr/bin/vllm"},
		{"/var/lib/runtimes", "vllm", "vllm", "uvicorn"},
		{"/var/lib/runtimes", "llama", "llamacpp", "llama-cli"},
		{"/var/lib/runtimes", "other", "generic", "server"},
	} {
		if _, err := CurrentLaunchBinding(input.root, input.name, input.kind, input.command); err == nil {
			t.Fatalf("CurrentLaunchBinding(%q, %q, %q, %q) unexpectedly succeeded", input.root, input.name, input.kind, input.command)
		}
	}
}

func TestRuntime_CurrentVersionLaunchBindingPinsResolvedGeneration(t *testing.T) {
	root := t.TempDir()
	writeNativeRuntimeVersion(t, root, "vllm-cpu", "vllm", "v1")
	writeNativeRuntimeVersion(t, root, "vllm-cpu", "vllm", "v2")
	if err := os.Symlink(filepath.Join("versions", "v1"), filepath.Join(root, "vllm-cpu", "current")); err != nil {
		t.Fatal(err)
	}

	current, found, err := CurrentVersionLaunchBinding(root, "vllm-cpu", "vllm", "vllm")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("current version binding was not found")
	}
	if want := filepath.Join(root, "vllm-cpu", "versions", "v1", ".venv", "bin", "vllm"); current.Executable != want {
		t.Fatalf("current executable=%q want %q", current.Executable, want)
	}

	candidate, err := LaunchBindingForVersion(root, "vllm-cpu", "vllm", "vllm", "v2")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "vllm-cpu", "versions", "v2", ".venv", "bin", "vllm"); candidate.Executable != want {
		t.Fatalf("candidate executable=%q want %q", candidate.Executable, want)
	}
	if candidate.Executable == current.Executable {
		t.Fatal("explicit candidate binding followed current instead of the requested version")
	}
}

func writeNativeRuntimeVersion(t *testing.T, root, name, kind, version string) {
	t.Helper()
	directory := filepath.Join(root, name, "versions", version)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{
		Name:        name,
		Version:     version,
		Kind:        kind,
		Source:      "pypi",
		InstalledAt: time.Now().UTC(),
		Metadata: map[string]string{
			"mode":       RuntimeModeNative,
			"sourceType": "pypi",
		},
	}
	payload, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "manifest.json"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
}
