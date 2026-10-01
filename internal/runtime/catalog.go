package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	maxRuntimeCatalogEntries = 256
	maxRuntimeGitRefsBytes   = 4 << 20
)

// ListVersions implements the provider-neutral catalog contract for the mux.
// A source such as a local directory may legitimately return an empty list;
// the manager will still add its installed manifests to the response.
func (p ProviderMux) ListVersions(ctx context.Context, desired Spec, policy UpdatePolicy) ([]VersionCandidate, error) {
	provider := p.Native
	if specUsesContainer(desired) {
		provider = p.Container
	}
	if isNilProvider(provider) {
		return nil, fmt.Errorf("runtime provider for mode %q is not registered", normalizeRuntimeMode(desired.Mode))
	}
	cataloger, ok := provider.(VersionCataloger)
	if !ok {
		return nil, nil
	}
	return cataloger.ListVersions(ctx, desired, policy)
}

// ListVersions resolves the configured image tag to one immutable registry
// manifest digest. OCI registries do not expose a portable, authenticated
// "list all tags" operation through the Docker/Podman CLI, so the safe
// catalog for a container is the exact digest currently published for that
// configured image. The UI can then install that digest without asking the
// operator to type an image version.
func (p ContainerProvider) ListVersions(ctx context.Context, desired Spec, _ UpdatePolicy) ([]VersionCandidate, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	image := strings.TrimSpace(desired.Source)
	if value := strings.TrimSpace(desired.Metadata["image"]); value != "" {
		image = value
	}
	if image == "" && desired.Container != nil {
		image = strings.TrimSpace(desired.Container.Image)
	}
	if err := validateContainerImage(image, p.AllowedRegistries); err != nil {
		return nil, err
	}
	digest := imageDigest(image)
	if digest == "" {
		engine := normalizeContainerEngine(p.Engine)
		if configured := strings.TrimSpace(desired.Metadata["engine"]); configured != "" {
			engine = normalizeContainerEngine(configured)
		} else if desired.Container != nil && strings.TrimSpace(desired.Container.Engine) != "" {
			engine = normalizeContainerEngine(desired.Container.Engine)
		}
		if engine != "docker" && engine != "podman" {
			return nil, fmt.Errorf("unsupported container engine %q", engine)
		}
		binary := strings.TrimSpace(p.Binary)
		if binary == "" {
			binary = engine
		}
		run := p.Run
		if run == nil {
			run = defaultCommandRunner
		}
		var err error
		digest, err = inspectRemoteContainerDigest(ctx, run, "", binary, image)
		if err != nil {
			return nil, err
		}
	}
	version := "image-" + strings.TrimPrefix(strings.ToLower(digest), "sha256:")
	candidate, ok := safeVersionCandidate(version, image, "", "")
	if !ok || !imageDigestPattern.MatchString(strings.ToLower(digest)) {
		return nil, errors.New("container catalog returned an invalid image digest")
	}
	candidate.Digest = strings.ToLower(digest)
	candidate.Recommended = true
	return []VersionCandidate{candidate}, nil
}

// ListVersions returns the versions published by the configured vLLM source.
// PyPI and Git are enumerable; arbitrary wheel/local sources intentionally
// fall back to the manager's installed-version list and advanced exact ref.
func (p VLLMProvider) ListVersions(ctx context.Context, desired Spec, policy UpdatePolicy) ([]VersionCandidate, error) {
	sourceType := strings.ToLower(strings.TrimSpace(desired.SourceType))
	if sourceType == "" {
		sourceType = inferVLLMSourceType(desired.Source)
	}
	if sourceType == "" {
		sourceType = "pypi"
	}
	switch sourceType {
	case "pypi":
		return listPyPIVersionCandidates(ctx, p.Client, p.UpdateURL, "vllm", policy.Channel, p.SourceAllowlist, "vLLM")
	case "git", "tag", "commit":
		return listGitVersionCandidates(ctx, p.Run, p.SourceAllowlist, desired, policy)
	default:
		return nil, nil
	}
}

// ListVersions returns every safe release tag exposed by a llama.cpp GitHub
// release endpoint, or the immutable refs from a Git repository.
func (p LlamaCPPProvider) ListVersions(ctx context.Context, desired Spec, policy UpdatePolicy) ([]VersionCandidate, error) {
	sourceType := strings.ToLower(strings.TrimSpace(desired.SourceType))
	if sourceType == "" {
		sourceType = inferLlamaSourceType(desired.Source)
	}
	switch sourceType {
	case "release", "channel":
		endpoint := strings.TrimSpace(p.UpdateURL)
		if endpoint == "" {
			endpoint = githubReleaseEndpoint(desired.Source)
		}
		if endpoint == "" {
			return nil, nil
		}
		return listLlamaReleaseCandidates(ctx, p.Client, endpoint, p.SourceAllowlist, policy.Channel)
	case "git", "tag", "commit":
		return listGitVersionCandidates(ctx, p.Run, p.SourceAllowlist, desired, policy)
	default:
		return nil, nil
	}
}

// ListVersions returns published LMCache package versions from its PyPI
// endpoint. The package name is configurable because private mirrors often
// publish a renamed build of the server package.
func (p LMCacheProvider) ListVersions(ctx context.Context, desired Spec, policy UpdatePolicy) ([]VersionCandidate, error) {
	sourceType := strings.ToLower(strings.TrimSpace(desired.SourceType))
	if sourceType == "" {
		sourceType = inferLMCacheSourceType(desired.Source)
	}
	if sourceType == "" {
		sourceType = "pypi"
	}
	switch sourceType {
	case "pypi":
		packageName := firstBuildArg(desired, "package")
		if packageName == "" {
			packageName = "lmcache"
		}
		return listPyPIVersionCandidates(ctx, p.Client, p.UpdateURL, packageName, policy.Channel, p.SourceAllowlist, "LMCache")
	case "git":
		return listGitVersionCandidates(ctx, p.Run, p.SourceAllowlist, desired, policy)
	default:
		return nil, nil
	}
}

func runtimeCatalogSourceType(spec Spec) string {
	sourceType := strings.ToLower(strings.TrimSpace(spec.SourceType))
	if sourceType != "" {
		return sourceType
	}
	switch strings.ToLower(strings.TrimSpace(spec.Kind)) {
	case "vllm":
		sourceType = inferVLLMSourceType(spec.Source)
	case "llamacpp":
		sourceType = inferLlamaSourceType(spec.Source)
	case "lmcache":
		sourceType = inferLMCacheSourceType(spec.Source)
	}
	if sourceType == "" && (strings.EqualFold(strings.TrimSpace(spec.Kind), "vllm") || strings.EqualFold(strings.TrimSpace(spec.Kind), "lmcache")) {
		return "pypi"
	}
	return sourceType
}

func mergeVersionCatalog(candidates []VersionCandidate, detail Detail) []VersionCandidate {
	status := detail.Status
	versions := detail.Versions
	seen := make(map[string]struct{}, len(candidates)+len(versions))
	merged := make([]VersionCandidate, 0, len(candidates)+len(versions))
	for _, candidate := range candidates {
		if !catalogCandidateSafe(candidate) {
			continue
		}
		if _, exists := seen[candidate.Version]; exists {
			continue
		}
		seen[candidate.Version] = struct{}{}
		if manifest, ok := catalogManifestForVersion(versions, candidate.Version); ok {
			candidate.Installed = true
			if candidate.Ref == "" {
				candidate.Ref = manifest.Ref
			}
			if candidate.Commit == "" {
				candidate.Commit = manifest.Commit
			}
			if candidate.Digest == "" {
				candidate.Digest = manifest.Metadata["imageDigest"]
			}
		}
		applyCatalogLifecycle(&candidate, status)
		merged = append(merged, candidate)
	}
	installed := make([]VersionCandidate, 0, len(versions))
	for version, manifest := range versions {
		if _, exists := seen[version]; exists {
			continue
		}
		actualVersion := strings.TrimSpace(manifest.Version)
		if actualVersion == "" {
			actualVersion = version
		}
		candidate, safe := safeVersionCandidate(actualVersion, "", manifest.Ref, manifest.Commit)
		if !safe {
			continue
		}
		candidate.Installed = true
		candidate.Digest = manifest.Metadata["imageDigest"]
		applyCatalogLifecycle(&candidate, status)
		installed = append(installed, candidate)
		seen[version] = struct{}{}
	}
	sortCatalogCandidatesWithoutRecommendation(installed)
	merged = append(merged, installed...)
	if len(merged) > 0 {
		hasRecommended := false
		for _, candidate := range merged {
			if candidate.Recommended {
				hasRecommended = true
				break
			}
		}
		if !hasRecommended {
			for index := range merged {
				if !merged[index].Installed {
					merged[index].Recommended = true
					break
				}
			}
		}
	}
	return merged
}

func catalogCandidateSafe(candidate VersionCandidate) bool {
	if validateVersion(candidate.Version) != nil {
		return false
	}
	if candidate.Label != "" {
		if len(candidate.Label) > 1024 || strings.IndexByte(candidate.Label, 0) >= 0 {
			return false
		}
		for _, r := range candidate.Label {
			if r == '\r' || r == '\n' || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				return false
			}
		}
	}
	if candidate.Ref != "" && !validGitCommit(candidate.Ref) && validateRuntimeRef("catalog ref", candidate.Ref, false) != nil {
		return false
	}
	if candidate.Commit != "" && !validGitCommit(candidate.Commit) {
		return false
	}
	return candidate.Digest == "" || imageDigestPattern.MatchString(strings.ToLower(candidate.Digest))
}

func catalogManifestForVersion(versions map[string]Manifest, version string) (Manifest, bool) {
	if manifest, ok := versions[version]; ok {
		return manifest, true
	}
	for installedVersion, manifest := range versions {
		if safeVersion(installedVersion) == safeVersion(version) || manifest.Version == version {
			return manifest, true
		}
	}
	return Manifest{}, false
}

func applyCatalogLifecycle(candidate *VersionCandidate, status Status) {
	if candidate == nil {
		return
	}
	candidate.Current = catalogVersionMatches(candidate.Version, status.Current)
	candidate.Previous = catalogVersionMatches(candidate.Version, status.Previous)
	candidate.Staged = catalogVersionMatches(candidate.Version, status.Staged)
	candidate.Pinned = catalogVersionMatches(candidate.Version, status.Pinned)
}

func catalogVersionMatches(left, right string) bool {
	return strings.TrimSpace(left) != "" && strings.TrimSpace(left) == strings.TrimSpace(right)
}

func sortCatalogCandidatesWithoutRecommendation(candidates []VersionCandidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		if order := compareRuntimeVersions(candidates[i].Version, candidates[j].Version); order != 0 {
			return order > 0
		}
		return strings.ToLower(candidates[i].Version) < strings.ToLower(candidates[j].Version)
	})
}

func listPyPIVersionCandidates(ctx context.Context, configuredClient *http.Client, configuredEndpoint, packageName, channel string, allowlist []string, label string) ([]VersionCandidate, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	name := strings.TrimSpace(packageName)
	if name == "" {
		return nil, errors.New("package name is required for version catalog")
	}
	if strings.ContainsAny(name, "/\\?#%\x00") {
		return nil, errors.New("package name is invalid for version catalog")
	}
	endpoint := strings.TrimSpace(configuredEndpoint)
	if endpoint == "" {
		endpoint = "https://pypi.org/pypi/" + url.PathEscape(name) + "/json"
	}
	if !validRuntimeURL(endpoint, allowlist) {
		return nil, fmt.Errorf("%s version catalog URL is not allowed", label)
	}
	client := guardedRuntimeClient(configuredClient, allowlist, 30*time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s version catalog returned HTTP %d", label, resp.StatusCode)
	}
	var metadata struct {
		Info struct {
			Version string `json:"version"`
		} `json:"info"`
		Releases map[string]json.RawMessage `json:"releases"`
	}
	if err := decodeBoundedRuntimeJSON(resp.Body, maxRuntimeMetadataBytes, &metadata); err != nil {
		return nil, fmt.Errorf("decode %s version catalog: %w", label, err)
	}
	seen := make(map[string]struct{}, len(metadata.Releases)+1)
	candidates := make([]VersionCandidate, 0, len(metadata.Releases)+1)
	add := func(version string) {
		version = strings.TrimSpace(version)
		if !isRuntimeVersionCandidate(version, channel) {
			return
		}
		if _, exists := seen[version]; exists {
			return
		}
		candidate, ok := safeVersionCandidate(version, "", "", "")
		if !ok {
			return
		}
		seen[version] = struct{}{}
		candidates = append(candidates, candidate)
	}
	add(metadata.Info.Version)
	for version := range metadata.Releases {
		add(version)
	}
	sortCatalogCandidates(candidates)
	return candidates, nil
}

func listLlamaReleaseCandidates(ctx context.Context, configuredClient *http.Client, endpoint string, allowlist []string, channel string) ([]VersionCandidate, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !validRuntimeURL(endpoint, allowlist) {
		return nil, errors.New("llama.cpp release catalog URL is not allowed")
	}
	client := guardedRuntimeClient(configuredClient, allowlist, 30*time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("llama.cpp release catalog returned HTTP %d", resp.StatusCode)
	}
	var releases []struct {
		TagName string `json:"tag_name"`
		Draft   bool   `json:"draft"`
		Pre     bool   `json:"prerelease"`
	}
	if err := decodeBoundedRuntimeJSON(resp.Body, maxRuntimeMetadataBytes, &releases); err != nil {
		return nil, fmt.Errorf("decode llama.cpp release catalog: %w", err)
	}
	candidates := make([]VersionCandidate, 0, len(releases))
	seen := make(map[string]struct{}, len(releases))
	for _, release := range releases {
		version := strings.TrimSpace(release.TagName)
		if release.Draft || version == "" {
			continue
		}
		if _, exists := seen[version]; exists {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(channel), "prerelease") {
			// The prerelease channel includes stable releases too. This mirrors
			// CheckForUpdate so the catalog and automatic check agree.
		} else if release.Pre || !isRuntimeVersionCandidate(version, "stable") {
			continue
		}
		candidate, ok := safeVersionCandidate(version, "", "", "")
		if !ok {
			continue
		}
		seen[version] = struct{}{}
		candidates = append(candidates, candidate)
	}
	sortCatalogCandidates(candidates)
	return candidates, nil
}

func listGitVersionCandidates(ctx context.Context, run CommandRunner, allowlist []string, desired Spec, policy UpdatePolicy) ([]VersionCandidate, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	source := strings.TrimPrefix(strings.TrimSpace(desired.Source), "git+")
	if !validGitSource(source, allowlist) {
		return nil, errors.New("git version catalog source is not allowed")
	}
	trackRef := strings.TrimSpace(desired.Metadata["trackRef"])
	if run == nil {
		run = defaultCommandRunner
	}
	output, err := runRuntimeCommand(ctx, run, "", "git", "ls-remote", "--heads", "--tags", "--refs", source)
	if err != nil {
		return nil, fmt.Errorf("list Git refs: %w", err)
	}
	if len(output) > maxRuntimeGitRefsBytes {
		return nil, fmt.Errorf("git ref catalog exceeds %d bytes", maxRuntimeGitRefsBytes)
	}
	var tags, heads []gitRemoteRef
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !validGitCommit(fields[0]) {
			continue
		}
		ref := strings.TrimSpace(fields[1])
		switch {
		case strings.HasPrefix(ref, "refs/tags/"):
			name := strings.TrimPrefix(ref, "refs/tags/")
			if name != "" {
				tags = append(tags, gitRemoteRef{name: name, ref: ref, commit: strings.ToLower(fields[0])})
			}
		case strings.HasPrefix(ref, "refs/heads/"):
			name := strings.TrimPrefix(ref, "refs/heads/")
			if name != "" {
				heads = append(heads, gitRemoteRef{name: name, ref: ref, commit: strings.ToLower(fields[0])})
			}
		}
	}
	candidates := make([]VersionCandidate, 0, minCatalogEntries(len(tags), maxRuntimeCatalogEntries))
	seen := make(map[string]struct{}, len(tags)+len(heads))
	trackedVersion := ""
	if trackRef != "" {
		for _, remote := range append(append([]gitRemoteRef{}, heads...), tags...) {
			if !gitRemoteRefMatches(remote, trackRef) {
				continue
			}
			trackedVersion = "git-" + remote.commit
			candidate, ok := safeVersionCandidate(trackedVersion, trackRef, remote.commit, remote.commit)
			if !ok {
				return nil, errors.New("tracked Git catalog returned an invalid candidate")
			}
			candidates = append(candidates, candidate)
			seen[trackedVersion] = struct{}{}
			break
		}
		if trackedVersion == "" {
			// Pull requests and other fully-qualified refs are not included by
			// --heads/--tags. Retain the existing exact-ref fallback for those
			// sources while keeping the common branch case to one remote call.
			candidate, _, err := checkTrackedGitUpdate(ctx, run, allowlist, Manifest{}, desired)
			if err != nil {
				return nil, err
			}
			candidateVersion, ok := safeVersionCandidate(candidate.Version, trackRef, candidate.Ref, candidate.Commit)
			if !ok {
				return nil, errors.New("tracked Git catalog returned an invalid candidate")
			}
			trackedVersion = candidateVersion.Version
			candidates = append(candidates, candidateVersion)
			seen[trackedVersion] = struct{}{}
		}
	}
	for _, remote := range tags {
		if !isRuntimeVersionCandidate(remote.name, policy.Channel) {
			continue
		}
		candidate, ok := safeVersionCandidate(remote.name, "", remote.commit, remote.commit)
		if !ok || !markCatalogVersion(seen, candidate.Version) {
			continue
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		// Repositories without version-like tags still get a useful catalog:
		// expose a bounded set of branch snapshots as immutable git-<sha>
		// choices. The configured ref is preferred when it is present.
		for _, remote := range orderedGitHeads(heads, strings.TrimSpace(desired.Ref)) {
			version := "git-" + remote.commit
			candidate, ok := safeVersionCandidate(version, remote.commit, remote.commit, remote.commit)
			if !ok || !markCatalogVersion(seen, candidate.Version) {
				continue
			}
			candidate.Label = remote.name
			candidates = append(candidates, candidate)
			if len(candidates) >= maxRuntimeCatalogEntries {
				break
			}
		}
	}
	if len(candidates) > maxRuntimeCatalogEntries {
		candidates = candidates[:maxRuntimeCatalogEntries]
	}
	sortCatalogCandidates(candidates)
	if trackedVersion != "" {
		for index := range candidates {
			candidates[index].Recommended = candidates[index].Version == trackedVersion
		}
	}
	return candidates, nil
}

func safeVersionCandidate(version, label, ref, commit string) (VersionCandidate, bool) {
	if validateVersion(version) != nil {
		return VersionCandidate{}, false
	}
	if label != "" && (len(label) > 1024 || strings.IndexByte(label, 0) >= 0) {
		return VersionCandidate{}, false
	}
	if ref != "" {
		if validGitCommit(ref) {
			// An immutable commit is always a safe ref.
		} else if validateRuntimeRef("catalog ref", ref, false) != nil {
			return VersionCandidate{}, false
		}
	}
	if commit != "" && !validGitCommit(commit) {
		return VersionCandidate{}, false
	}
	return VersionCandidate{Version: version, Label: label, Ref: ref, Commit: commit}, true
}

func sortCatalogCandidates(candidates []VersionCandidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		if order := compareRuntimeVersions(candidates[i].Version, candidates[j].Version); order != 0 {
			return order > 0
		}
		return strings.ToLower(candidates[i].Version) < strings.ToLower(candidates[j].Version)
	})
	for i := range candidates {
		candidates[i].Recommended = i == 0
	}
}

func markCatalogVersion(seen map[string]struct{}, version string) bool {
	if _, exists := seen[version]; exists {
		return false
	}
	seen[version] = struct{}{}
	return true
}

func minCatalogEntries(value, maximum int) int {
	if value < maximum {
		return value
	}
	return maximum
}

type gitRemoteRef struct {
	name   string
	ref    string
	commit string
}

func gitRemoteRefMatches(remote gitRemoteRef, requested string) bool {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return false
	}
	if strings.HasPrefix(requested, "refs/") {
		return remote.ref == requested
	}
	return remote.ref == "refs/heads/"+requested || remote.ref == "refs/tags/"+requested
}

func orderedGitHeads(heads []gitRemoteRef, preferred string) []gitRemoteRef {
	// This helper is kept separate so the branch fallback remains deterministic
	// even though git's remote response order is not a catalog contract.
	preferred = strings.TrimPrefix(preferred, "refs/heads/")
	ordered := append([]gitRemoteRef(nil), heads...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].name == preferred {
			return true
		}
		if ordered[j].name == preferred {
			return false
		}
		return ordered[i].name < ordered[j].name
	})
	return ordered
}
