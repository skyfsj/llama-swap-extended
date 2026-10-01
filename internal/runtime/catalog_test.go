package runtime

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type catalogTestProvider struct{}

func (catalogTestProvider) Stage(_ context.Context, spec Spec, _ string) (Manifest, error) {
	return Manifest{Name: spec.Name, Version: spec.Version, Kind: spec.Kind, Source: spec.Source}, nil
}

func (catalogTestProvider) Verify(context.Context, Manifest, string) error { return nil }
func (catalogTestProvider) Health(context.Context, Manifest, string) error { return nil }

func (catalogTestProvider) ListVersions(context.Context, Spec, UpdatePolicy) ([]VersionCandidate, error) {
	return []VersionCandidate{{Version: "2.0.0", Recommended: true}}, nil
}

func TestRuntime_VLLMVersionCatalogListsStablePyPIReleases(t *testing.T) {
	const endpoint = "https://pypi.test/pypi/vllm/json"
	provider := VLLMProvider{
		UpdateURL: endpoint,
		Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.String() != endpoint {
				t.Fatalf("catalog endpoint = %q, want %q", req.URL, endpoint)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"info":{"version":"0.7.0"},"releases":{"0.5.0":{},"0.6.0":{},"0.7.0rc1":{},"0.7.0":{}}}`)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		})},
	}
	candidates, err := provider.ListVersions(context.Background(), Spec{Kind: "vllm", SourceType: "pypi"}, UpdatePolicy{Channel: "stable"})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 3 || candidates[0].Version != "0.7.0" || candidates[1].Version != "0.6.0" || candidates[2].Version != "0.5.0" {
		t.Fatalf("catalog candidates = %+v", candidates)
	}
	if !candidates[0].Recommended {
		t.Fatalf("latest catalog candidate is not recommended: %+v", candidates[0])
	}
}

func TestRuntime_GitVersionCatalogCarriesImmutableTagCommit(t *testing.T) {
	const (
		source = "https://github.com/example/runtime.git"
		old    = "1111111111111111111111111111111111111111"
		latest = "2222222222222222222222222222222222222222"
	)
	provider := VLLMProvider{
		SourceAllowlist: []string{"https://github.com"},
		Run: func(_ context.Context, _ string, command string, args ...string) ([]byte, error) {
			if command != "git" || strings.Join(args, " ") != "ls-remote --heads --tags --refs "+source {
				t.Fatalf("unexpected catalog command: %s %q", command, args)
			}
			return []byte(latest + "\trefs/tags/v1.2.0\n" + old + "\trefs/tags/v1.1.0\n" + latest + "\trefs/tags/v1.3.0rc1\n"), nil
		},
	}
	candidates, err := provider.ListVersions(context.Background(), Spec{Kind: "vllm", SourceType: "git", Source: source}, UpdatePolicy{Channel: "stable"})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 || candidates[0].Version != "v1.2.0" || candidates[1].Version != "v1.1.0" {
		t.Fatalf("catalog candidates = %+v", candidates)
	}
	if candidates[0].Ref != latest || candidates[0].Commit != latest {
		t.Fatalf("tag candidate did not carry immutable commit: %+v", candidates[0])
	}
}

func TestRuntime_GitTrackVersionCatalogResolvesConfiguredRef(t *testing.T) {
	const (
		source   = "https://github.com/example/runtime.git"
		commit   = "3333333333333333333333333333333333333333"
		trackRef = "refs/heads/main"
	)
	provider := LlamaCPPProvider{
		SourceAllowlist: []string{"https://github.com"},
		Run: func(_ context.Context, _ string, command string, args ...string) ([]byte, error) {
			if command != "git" || strings.Join(args, " ") != "ls-remote --heads --tags --refs "+source {
				t.Fatalf("unexpected tracked catalog command: %s %q", command, args)
			}
			return []byte(commit + "\t" + trackRef + "\n"), nil
		},
	}
	candidates, err := provider.ListVersions(context.Background(), Spec{
		Kind: "llamacpp", SourceType: "git", Source: source,
		Metadata: map[string]string{"trackRef": trackRef},
	}, UpdatePolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].Version != "git-"+commit || candidates[0].Ref != commit || candidates[0].Label != trackRef {
		t.Fatalf("tracked catalog candidate = %+v", candidates)
	}
}

func TestRuntime_MergeVersionCatalogAddsLifecycleMarkersAndInstalledVersions(t *testing.T) {
	currentCommit := strings.Repeat("a", 40)
	detail := Detail{
		Status: Status{Current: "1.0.0", Staged: "1.1.0", Pinned: "1.0.0"},
		Versions: map[string]Manifest{
			"1.0.0":                {Version: "1.0.0", Ref: "stable"},
			"git-" + currentCommit: {Version: "git-" + currentCommit, Commit: currentCommit},
		},
	}
	merged := mergeVersionCatalog([]VersionCandidate{{Version: "1.1.0", Recommended: true}}, detail)
	if len(merged) != 3 {
		t.Fatalf("merged catalog length = %d, entries=%+v", len(merged), merged)
	}
	var foundCurrent, foundStaged, foundInstalled bool
	for _, candidate := range merged {
		switch candidate.Version {
		case "1.0.0":
			foundCurrent = candidate.Installed && candidate.Current && candidate.Pinned
		case "1.1.0":
			foundStaged = candidate.Staged && !candidate.Installed
		case "git-" + currentCommit:
			foundInstalled = candidate.Installed
		}
	}
	if !foundCurrent || !foundStaged || !foundInstalled {
		t.Fatalf("lifecycle markers missing: %+v", merged)
	}
}

func TestRuntime_ManagerVersionCatalogUsesConfiguredProviderAndSource(t *testing.T) {
	manager, err := NewManager(t.TempDir(), map[string]Provider{"catalog": catalogTestProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Configure("demo", Spec{Name: "demo", Kind: "catalog", SourceType: "git", Source: "https://github.com/example/demo.git", Ref: "main"}, UpdatePolicy{Policy: "manual"}); err != nil {
		t.Fatal(err)
	}
	result, err := manager.VersionCatalog(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Supported || result.SourceType != "git" || len(result.Versions) != 1 || result.Versions[0].Version != "2.0.0" {
		t.Fatalf("manager catalog = %+v", result)
	}
}
