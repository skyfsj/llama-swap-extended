package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRuntime_ContainerRunArgsRejectsUnsafeEntrypoint(t *testing.T) {
	for _, entrypoint := range [][]string{
		{"/bin/sh\n"},
		{"\x00"},
		{"safe", "bad\u200b"},
	} {
		launch := ContainerLaunch{Entrypoint: entrypoint}
		if _, err := launch.RunArgs("ghcr.io/example/model:latest"); err == nil || !strings.Contains(err.Error(), "entrypoint") {
			t.Fatalf("entrypoint %q accepted: %v", entrypoint, err)
		}
	}
}

func TestRuntime_ContainerRunArgsKeepsSafeEntrypointAndCommand(t *testing.T) {
	args, err := (ContainerLaunch{
		Entrypoint: []string{"/usr/local/bin/server", "--config"},
		Command:    []string{"--model", "qwen"}, StopTimeout: 1500 * time.Millisecond,
	}).RunArgs("ghcr.io/example/model:latest")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"run", "--init", "--rm", "--stop-timeout", "2", "--entrypoint", "/usr/local/bin/server", "ghcr.io/example/model:latest", "--config", "--model", "qwen"}
	if len(args) != len(want) {
		t.Fatalf("args=%q, want %q", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args=%q, want %q", args, want)
		}
	}
}

func TestRuntime_ContainerRunArgsUsesConfiguredImageWhenArgumentIsEmpty(t *testing.T) {
	args, err := (ContainerLaunch{Image: "ghcr.io/example/model:stable", PullPolicy: "never"}).RunArgs("")
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 6 || args[0] != "run" || args[3] != "--pull" || args[4] != "never" || args[5] != "ghcr.io/example/model:stable" {
		t.Fatalf("args = %#v", args)
	}
}

func TestRuntime_ContainerLaunchJSONAcceptsDurationString(t *testing.T) {
	var launch ContainerLaunch
	if err := json.Unmarshal([]byte(`{"image":"ghcr.io/example/model:stable","stopTimeout":"15s"}`), &launch); err != nil {
		t.Fatal(err)
	}
	if launch.Image == "" || launch.StopTimeout != 15*time.Second {
		t.Fatalf("launch = %+v", launch)
	}
	if err := json.Unmarshal([]byte(`{"stopTimeout":2000000000}`), &launch); err != nil {
		t.Fatal(err)
	}
	if launch.StopTimeout != 2*time.Second {
		t.Fatalf("numeric stopTimeout = %v", launch.StopTimeout)
	}
}

func TestRuntime_ContainerProviderStagesPinnedDigestAndLaunches(t *testing.T) {
	var calls [][]string
	run := func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		if len(args) >= 2 && args[0] == "image" && args[1] == "inspect" && strings.Contains(strings.Join(args, " "), "RepoDigests") {
			return []byte(`["ghcr.io/example/vllm@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"]`), nil
		}
		return nil, nil
	}
	root := filepath.Join(t.TempDir(), "stage")
	spec := Spec{Name: "vllm", Kind: "vllm", Mode: RuntimeModeContainer, Version: "image-a", SourceType: "image", Source: "ghcr.io/example/vllm:stable", Metadata: map[string]string{"pullPolicy": "always"}, Container: &ContainerLaunch{Engine: "docker", Command: []string{"serve", "/models"}}}
	provider := ContainerProvider{Engine: "docker", AllowedRegistries: []string{"ghcr.io"}, Run: run}
	manifest, err := provider.Stage(context.Background(), spec, root)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Metadata["imageDigest"] != "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || manifest.Metadata["mode"] != RuntimeModeContainer {
		t.Fatalf("manifest metadata = %#v", manifest.Metadata)
	}
	args, err := provider.LaunchArgs(spec, manifest.Metadata["imageDigest"])
	if err != nil {
		t.Fatal(err)
	}
	if len(args) < 5 || args[0] != "docker" || args[1] != "run" || !strings.Contains(args[len(args)-3], "@sha256:") {
		t.Fatalf("launch args = %#v", args)
	}
	if len(calls) == 0 || calls[0][0] != "docker" || calls[0][1] != "pull" {
		t.Fatalf("container calls = %#v", calls)
	}
}

func TestRuntime_ContainerProviderRejectsUnallowlistedRegistry(t *testing.T) {
	provider := ContainerProvider{Engine: "docker", AllowedRegistries: []string{"ghcr.io"}, Run: func(context.Context, string, string, ...string) ([]byte, error) { return nil, nil }}
	_, err := provider.Stage(context.Background(), Spec{Name: "vllm", Kind: "vllm", Mode: RuntimeModeContainer, Version: "image-a", SourceType: "image", Source: "docker.io/library/vllm:latest"}, filepath.Join(t.TempDir(), "stage"))
	if err == nil || !strings.Contains(err.Error(), "not allowlisted") {
		t.Fatalf("registry error = %v", err)
	}
}

func TestRuntime_ContainerProviderUsesStructuredContainerDefaults(t *testing.T) {
	var calls [][]string
	run := func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		if len(args) >= 2 && args[0] == "image" && args[1] == "inspect" && strings.Contains(strings.Join(args, " "), "RepoDigests") {
			return []byte(`[
  "ghcr.io/example/vllm@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
]`), nil
		}
		return nil, nil
	}
	provider := ContainerProvider{AllowedRegistries: []string{"ghcr.io"}, Run: run}
	manifest, err := provider.Stage(context.Background(), Spec{
		Name: "vllm", Kind: "vllm", Mode: RuntimeModeContainer, Version: "image-b",
		SourceType: "image", Container: &ContainerLaunch{Engine: "podman", Image: "ghcr.io/example/vllm:stable", PullPolicy: "never", Platform: "linux/amd64"},
	}, filepath.Join(t.TempDir(), "stage"))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Metadata["engine"] != "podman" || manifest.Metadata["platform"] != "linux/amd64" || manifest.Metadata["pullPolicy"] != "never" {
		t.Fatalf("container metadata = %#v", manifest.Metadata)
	}
	if len(calls) != 1 || calls[0][0] != "podman" || calls[0][1] != "image" {
		t.Fatalf("container inspect calls = %#v", calls)
	}
}

func TestRuntime_ContainerProviderCheckForUpdateUsesRemoteManifestDigest(t *testing.T) {
	const (
		localDigest  = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		remoteDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	var calls [][]string
	provider := ContainerProvider{
		AllowedRegistries: []string{"ghcr.io"},
		Run: func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
			calls = append(calls, append([]string{name}, args...))
			switch {
			case len(args) >= 2 && args[0] == "manifest" && args[1] == "inspect":
				return []byte(`{"Descriptor":{"digest":"` + remoteDigest + `"}}`), nil
			case len(args) >= 2 && args[0] == "image" && args[1] == "inspect":
				return []byte(`["ghcr.io/example/vllm@` + localDigest + `"]`), nil
			default:
				return nil, nil
			}
		},
	}
	current := Manifest{Name: "vllm", Version: "image-" + strings.TrimPrefix(localDigest, "sha256:"), Kind: "vllm", Source: "ghcr.io/example/vllm:stable", Metadata: map[string]string{"mode": RuntimeModeContainer, "imageDigest": localDigest}}
	desired := Spec{Name: "vllm", Kind: "vllm", Mode: RuntimeModeContainer, SourceType: "image", Source: "ghcr.io/example/vllm:stable"}
	candidate, available, err := provider.CheckForUpdate(context.Background(), current, desired, UpdatePolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if !available || candidate.Version != "image-"+strings.TrimPrefix(remoteDigest, "sha256:") {
		t.Fatalf("candidate=%+v available=%v", candidate, available)
	}
	if got := candidate.Metadata["expectedImageDigest"]; got != remoteDigest {
		t.Fatalf("expected remote digest=%q", got)
	}
	if len(calls) != 1 || len(calls[0]) < 3 || calls[0][1] != "manifest" || calls[0][2] != "inspect" {
		t.Fatalf("check must inspect remote manifest before any local image: %#v", calls)
	}
}

func TestRuntime_ContainerProviderVersionCatalogResolvesRemoteDigest(t *testing.T) {
	const digest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	var called bool
	provider := ContainerProvider{
		Engine:            "docker",
		AllowedRegistries: []string{"ghcr.io"},
		Run: func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
			if name != "docker" || strings.Join(args, " ") != "manifest inspect --verbose ghcr.io/example/vllm:stable" {
				t.Fatalf("unexpected catalog command: %s %q", name, args)
			}
			called = true
			return []byte(`{"Descriptor":{"digest":"` + digest + `"}}`), nil
		},
	}
	candidates, err := provider.ListVersions(context.Background(), Spec{
		Kind: "vllm", Mode: RuntimeModeContainer, SourceType: "image", Source: "ghcr.io/example/vllm:stable",
	}, UpdatePolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if !called || len(candidates) != 1 || candidates[0].Version != "image-"+strings.TrimPrefix(digest, "sha256:") || candidates[0].Digest != digest || !candidates[0].Recommended {
		t.Fatalf("container catalog candidates=%+v called=%v", candidates, called)
	}
}

func TestRuntime_ContainerProviderCheckForUpdateKeepsMatchingRemoteDigest(t *testing.T) {
	const digest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	provider := ContainerProvider{
		AllowedRegistries: []string{"ghcr.io"},
		Run: func(_ context.Context, _ string, _ string, args ...string) ([]byte, error) {
			if len(args) >= 2 && args[0] == "manifest" && args[1] == "inspect" {
				return []byte(`{"Descriptor":{"digest":"` + digest + `"}}`), nil
			}
			return nil, errors.New("local image lookup should not run")
		},
	}
	current := Manifest{Name: "vllm", Version: "image-" + strings.TrimPrefix(digest, "sha256:"), Kind: "vllm", Source: "ghcr.io/example/vllm:stable", Metadata: map[string]string{"mode": RuntimeModeContainer, "imageDigest": digest}}
	desired := Spec{Name: "vllm", Kind: "vllm", Mode: RuntimeModeContainer, SourceType: "image", Source: "ghcr.io/example/vllm:stable"}
	_, available, err := provider.CheckForUpdate(context.Background(), current, desired, UpdatePolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if available {
		t.Fatal("matching remote digest unexpectedly reported an update")
	}
}

func TestRuntime_ContainerProviderStageRejectsStaleExpectedDigest(t *testing.T) {
	const (
		localDigest  = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		remoteDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	provider := ContainerProvider{
		AllowedRegistries: []string{"ghcr.io"},
		Run: func(_ context.Context, _ string, _ string, args ...string) ([]byte, error) {
			if len(args) >= 2 && args[0] == "image" && args[1] == "inspect" {
				return []byte(`["ghcr.io/example/vllm@` + localDigest + `"]`), nil
			}
			return nil, nil
		},
	}
	_, err := provider.Stage(context.Background(), Spec{
		Name: "vllm", Kind: "vllm", Mode: RuntimeModeContainer,
		Version: "image-" + strings.TrimPrefix(remoteDigest, "sha256:"), SourceType: "image", Source: "ghcr.io/example/vllm:stable",
		Metadata: map[string]string{"pullPolicy": "never", "expectedImageDigest": remoteDigest},
	}, filepath.Join(t.TempDir(), "stage"))
	if err == nil || !strings.Contains(err.Error(), "did not reach expected remote digest") {
		t.Fatalf("stale staged digest error=%v", err)
	}
}

func TestRuntime_ProviderMuxForwardsContainerRefreshWithConfiguredBinary(t *testing.T) {
	const (
		oldDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		newDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	var calls [][]string
	provider := ProviderMux{Container: ContainerProvider{
		Engine:            "docker",
		Binary:            "docker-runtime-test",
		AllowedRegistries: []string{"ghcr.io"},
		Run: func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
			calls = append(calls, append([]string{name}, args...))
			if len(args) >= 2 && args[0] == "image" && args[1] == "inspect" {
				return []byte(`["ghcr.io/example/vllm@` + newDigest + `"]`), nil
			}
			return nil, nil
		},
	}}
	desired := Spec{
		Name: "vllm", Kind: "vllm", Mode: RuntimeModeContainer,
		SourceType: "image", Source: "ghcr.io/example/vllm:stable",
		Metadata: map[string]string{"pullPolicy": "always"},
	}
	if !provider.NeedsRefresh(desired) {
		t.Fatal("provider mux did not expose container refresh")
	}
	candidate, available, err := provider.RefreshForUpdate(context.Background(), Manifest{
		Name: "vllm", Kind: "vllm", Version: "image-old", Source: desired.Source,
		Metadata: map[string]string{"mode": RuntimeModeContainer, "imageDigest": oldDigest},
	}, desired, UpdatePolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if !available || candidate.Metadata["expectedImageDigest"] != newDigest {
		t.Fatalf("candidate=%+v available=%v", candidate, available)
	}
	if len(calls) != 2 {
		t.Fatalf("refresh calls=%#v", calls)
	}
	for _, call := range calls {
		if call[0] != "docker-runtime-test" {
			t.Fatalf("refresh did not use configured binary: %#v", calls)
		}
	}
	if calls[0][1] != "pull" || calls[1][1] != "image" {
		t.Fatalf("refresh call sequence=%#v", calls)
	}
}

func TestRuntime_ManagerAutomaticContainerRefreshUsesProviderMux(t *testing.T) {
	const (
		oldDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		newDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	var pulls, remoteChecks int
	provider := ProviderMux{Container: ContainerProvider{
		Engine:            "docker",
		AllowedRegistries: []string{"ghcr.io"},
		Run: func(_ context.Context, _ string, _ string, args ...string) ([]byte, error) {
			switch {
			case len(args) >= 2 && args[0] == "manifest" && args[1] == "inspect":
				remoteChecks++
				// The metadata check itself must remain fail-open. The subsequent
				// pull is permitted only through ProviderMux after Manager's idle
				// gate has admitted it.
				return nil, errors.New("registry metadata temporarily unavailable")
			case len(args) >= 1 && args[0] == "pull":
				pulls++
				return nil, nil
			case len(args) >= 2 && args[0] == "image" && args[1] == "inspect":
				digest := oldDigest
				if pulls >= 2 {
					digest = newDigest
				}
				return []byte(`["ghcr.io/example/vllm@` + digest + `"]`), nil
			default:
				return nil, nil
			}
		},
	}}
	manager, err := NewManager(filepath.Join(t.TempDir(), "runtimes"), map[string]Provider{"vllm": provider})
	if err != nil {
		t.Fatal(err)
	}
	manager.SetIdleProbe(func() bool { return true })
	currentSpec := Spec{
		Name: "vllm", Kind: "vllm", Mode: RuntimeModeContainer, Version: "image-old",
		SourceType: "image", Source: "ghcr.io/example/vllm:stable", Metadata: map[string]string{"pullPolicy": "always"},
	}
	if _, err := manager.Stage(context.Background(), currentSpec); err != nil {
		t.Fatal(err)
	}
	if err := manager.Activate(context.Background(), "vllm", currentSpec.Version); err != nil {
		t.Fatal(err)
	}
	if err := manager.Configure("vllm", Spec{
		Name: "vllm", Kind: "vllm", Mode: RuntimeModeContainer,
		SourceType: "image", Source: "ghcr.io/example/vllm:stable", Metadata: map[string]string{"pullPolicy": "always"},
	}, UpdatePolicy{Policy: "automatic", CheckEvery: time.Nanosecond, MinIdle: 0, MinIdleSet: true}); err != nil {
		t.Fatal(err)
	}
	if err := manager.AutoUpdateOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, found := manager.Get("vllm")
	if !found || status.Current != "image-"+strings.TrimPrefix(newDigest, "sha256:") {
		t.Fatalf("automatic refreshed runtime status=%+v found=%v", status, found)
	}
	if remoteChecks == 0 || pulls < 3 {
		t.Fatalf("remote checks=%d pulls=%d, want metadata check plus initial/refresh/stage pulls", remoteChecks, pulls)
	}
}
