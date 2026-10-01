package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	RuntimeModeNative    = "native"
	RuntimeModeContainer = "container"

	// A remote OCI manifest is metadata, not an image layer. Keep its parsing
	// bounded anyway: the automatic check runs in the control plane and must
	// never allocate unbounded memory for a malformed registry response.
	maxContainerManifestBytes = 1 << 20
)

var imageDigestPattern = regexp.MustCompile(`^sha256:[0-9a-fA-F]{64}$`)

// ProviderMux keeps the public Provider interface stable while selecting a
// native or container implementation from the runtime spec/manifest. Existing
// callers that register one native provider continue to work unchanged.
// Container manifests record mode in metadata so Verify/Health can make the
// same choice after a restart when the original Spec is no longer available.
type ProviderMux struct {
	Native    Provider
	Container Provider
}

func (p ProviderMux) CheckForUpdate(ctx context.Context, current Manifest, desired Spec, policy UpdatePolicy) (Spec, bool, error) {
	provider := p.Native
	if specUsesContainer(desired) || manifestMode(current) == RuntimeModeContainer {
		provider = p.Container
	}
	if isNilProvider(provider) {
		return desired, false, fmt.Errorf("runtime provider for mode %q is not registered", normalizeRuntimeMode(desired.Mode))
	}
	checker, ok := provider.(UpdateChecker)
	if ok {
		return checker.CheckForUpdate(ctx, current, desired, policy)
	}
	if desired.Name == "" {
		desired.Name = current.Name
	}
	return desired, desiredDiff(current, desired), nil
}

// NeedsRefresh forwards the optional admitted-refresh contract to the
// provider selected by the declarative runtime spec. ProviderMux is what the
// Server registers with Manager, so omitting this forwarding would make a
// ContainerProvider's safe idle-time pull unreachable in production.
func (p ProviderMux) NeedsRefresh(spec Spec) bool {
	provider := p.Native
	if specUsesContainer(spec) {
		provider = p.Container
	}
	if isNilProvider(provider) {
		return false
	}
	refresher, ok := provider.(RefreshProvider)
	return ok && refresher.NeedsRefresh(spec)
}

// RefreshForUpdate forwards a previously admitted refresh to the provider
// that owns the desired/current materialization. A caller normally reaches it
// only after NeedsRefresh returned true, but retain a no-op result for native
// providers that do not implement RefreshProvider so the mux remains a
// compatible Provider implementation.
func (p ProviderMux) RefreshForUpdate(ctx context.Context, current Manifest, desired Spec, policy UpdatePolicy) (Spec, bool, error) {
	provider := p.Native
	if specUsesContainer(desired) || manifestMode(current) == RuntimeModeContainer {
		provider = p.Container
	}
	if isNilProvider(provider) {
		return desired, false, fmt.Errorf("runtime provider for mode %q is not registered", normalizeRuntimeMode(desired.Mode))
	}
	refresher, ok := provider.(RefreshProvider)
	if !ok {
		return desired, false, nil
	}
	return refresher.RefreshForUpdate(ctx, current, desired, policy)
}

func (p ProviderMux) Stage(ctx context.Context, spec Spec, destination string) (Manifest, error) {
	provider := p.Native
	if specUsesContainer(spec) {
		provider = p.Container
	}
	if isNilProvider(provider) {
		return Manifest{}, fmt.Errorf("runtime provider for mode %q is not registered", normalizeRuntimeMode(spec.Mode))
	}
	return provider.Stage(ctx, spec, destination)
}

func specUsesContainer(spec Spec) bool {
	if normalizeRuntimeMode(spec.Mode) == RuntimeModeContainer {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(spec.SourceType), "image") {
		return true
	}
	if strings.TrimSpace(spec.Metadata["image"]) != "" {
		return true
	}
	return spec.Container != nil && strings.TrimSpace(spec.Container.Image) != ""
}

func (p ProviderMux) Verify(ctx context.Context, manifest Manifest, directory string) error {
	provider := p.Native
	if manifestMode(manifest) == RuntimeModeContainer {
		provider = p.Container
	}
	if isNilProvider(provider) {
		return fmt.Errorf("runtime provider for mode %q is not registered", manifestMode(manifest))
	}
	return provider.Verify(ctx, manifest, directory)
}

func (p ProviderMux) Health(ctx context.Context, manifest Manifest, directory string) error {
	provider := p.Native
	if manifestMode(manifest) == RuntimeModeContainer {
		provider = p.Container
	}
	if isNilProvider(provider) {
		return fmt.Errorf("runtime provider for mode %q is not registered", manifestMode(manifest))
	}
	return provider.Health(ctx, manifest, directory)
}

func normalizeRuntimeMode(mode string) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		return RuntimeModeNative
	}
	return mode
}

func manifestMode(manifest Manifest) string {
	if manifest.Metadata != nil {
		return normalizeRuntimeMode(manifest.Metadata["mode"])
	}
	return RuntimeModeNative
}

func cloneBuildSteps(input []BuildStep) []BuildStep {
	if len(input) == 0 {
		return nil
	}
	output := make([]BuildStep, 0, len(input))
	for _, step := range input {
		output = append(output, BuildStep{
			WorkDir: step.WorkDir,
			Command: step.Command,
			Args:    append([]string(nil), step.Args...),
			Env:     cloneMetadata(step.Env),
		})
	}
	return output
}

func cloneArtifacts(input []Artifact) []Artifact {
	if len(input) == 0 {
		return nil
	}
	output := make([]Artifact, len(input))
	copy(output, input)
	return output
}

func cloneContainerLaunch(input *ContainerLaunch) *ContainerLaunch {
	if input == nil {
		return nil
	}
	output := *input
	output.Entrypoint = append([]string(nil), input.Entrypoint...)
	output.Command = append([]string(nil), input.Command...)
	output.Env = cloneMetadata(input.Env)
	output.Mounts = append([]ContainerMount(nil), input.Mounts...)
	output.Ports = append([]ContainerPort(nil), input.Ports...)
	return &output
}

// ContainerProvider stages an OCI image through a local Docker/Podman CLI.
// The CLI is intentionally injected behind CommandRunner so tests and
// embedders can use a remote wrapper without importing a Docker SDK. Every
// invocation is argv-based and image references are checked against an
// explicit registry allowlist before a command is started.
type ContainerProvider struct {
	Engine            string
	Binary            string
	AllowedRegistries []string
	Run               CommandRunner
}

// NeedsRefresh reports whether the configured image is a mutable tag that may
// require a side-effecting pull. It is deliberately a pure declaration check;
// callers can use it while inference is active without touching the daemon.
func (p ContainerProvider) NeedsRefresh(spec Spec) bool {
	if strings.TrimSpace(spec.Version) != "" {
		return false
	}
	policy := strings.ToLower(strings.TrimSpace(spec.Metadata["pullPolicy"]))
	if policy == "" && spec.Container != nil {
		policy = strings.ToLower(strings.TrimSpace(spec.Container.PullPolicy))
	}
	if policy == "never" {
		return false
	}
	image := strings.TrimSpace(spec.Source)
	if value := strings.TrimSpace(spec.Metadata["image"]); value != "" {
		image = value
	}
	if image == "" && spec.Container != nil {
		image = strings.TrimSpace(spec.Container.Image)
	}
	return image != "" && imageDigest(image) == ""
}

// RefreshForUpdate performs the admitted pull phase for a mutable image tag,
// then resolves the local immutable RepoDigest. It is separate from
// CheckForUpdate so the manager can guarantee that no pull happens while a
// model is serving traffic.
func (p ContainerProvider) RefreshForUpdate(ctx context.Context, current Manifest, desired Spec, policy UpdatePolicy) (Spec, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !p.NeedsRefresh(desired) {
		return desired, false, nil
	}
	image := strings.TrimSpace(desired.Source)
	if value := strings.TrimSpace(desired.Metadata["image"]); value != "" {
		image = value
	}
	if image == "" && desired.Container != nil {
		image = strings.TrimSpace(desired.Container.Image)
	}
	if image == "" {
		image = strings.TrimSpace(current.Metadata["image"])
	}
	if err := validateContainerImage(image, p.AllowedRegistries); err != nil {
		return desired, false, err
	}
	engine := normalizeContainerEngine(p.Engine)
	if configured := strings.TrimSpace(desired.Metadata["engine"]); configured != "" {
		engine = normalizeContainerEngine(configured)
	} else if desired.Container != nil && strings.TrimSpace(desired.Container.Engine) != "" {
		engine = normalizeContainerEngine(desired.Container.Engine)
	}
	if engine != "docker" && engine != "podman" {
		return desired, false, fmt.Errorf("unsupported container engine %q", engine)
	}
	run := p.Run
	if run == nil {
		run = defaultCommandRunner
	}
	binary := strings.TrimSpace(p.Binary)
	if binary == "" {
		binary = engine
	}
	args := []string{"pull"}
	platform := strings.TrimSpace(desired.Metadata["platform"])
	if platform == "" && desired.Container != nil {
		platform = strings.TrimSpace(desired.Container.Platform)
	}
	if platform != "" {
		if err := validateContainerPlatform(platform); err != nil {
			return desired, false, err
		}
		args = append(args, "--platform", platform)
	}
	args = append(args, image)
	if _, err := runRuntimeCommand(ctx, run, "", binary, args...); err != nil {
		return desired, false, fmt.Errorf("refresh container image: %w", err)
	}
	digest, err := inspectContainerDigest(ctx, run, "", binary, image)
	if err != nil {
		return desired, false, err
	}
	candidate := desired
	candidate.Mode = RuntimeModeContainer
	candidate.Source = image
	candidate.Metadata = cloneMetadata(desired.Metadata)
	if candidate.Metadata == nil {
		candidate.Metadata = make(map[string]string)
	}
	candidate.Metadata["resolvedImageDigest"] = digest
	candidate.Metadata["expectedImageDigest"] = digest
	candidate.Version = "image-" + strings.TrimPrefix(strings.ToLower(digest), "sha256:")
	return candidate, currentDigestForContainer(current, image) != digest, nil
}

func currentDigestForContainer(current Manifest, image string) string {
	digest := strings.ToLower(strings.TrimSpace(current.Metadata["imageDigest"]))
	if digest == "" {
		digest = imageDigest(current.Source)
	}
	if digest == "" {
		digest = imageDigest(image)
	}
	return digest
}

// CheckForUpdate resolves the remote manifest digest without pulling layers.
// This makes a mutable image tag observable even when the local image store
// still holds the old digest, while preserving the manager's busy-path rule:
// the check is metadata-only and Stage remains the only operation that pulls.
// If a registry cannot be reached, a locally inspected digest is still useful
// for an operator- or daemon-pulled image and keeps the check fail-open.
func (p ContainerProvider) CheckForUpdate(ctx context.Context, current Manifest, desired Spec, _ UpdatePolicy) (Spec, bool, error) {
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
	if image == "" {
		image = strings.TrimSpace(current.Metadata["image"])
	}
	if err := validateContainerImage(image, p.AllowedRegistries); err != nil {
		return desired, false, err
	}
	if strings.TrimSpace(desired.Version) != "" {
		return desired, desiredDiff(current, desired), nil
	}
	binary := strings.TrimSpace(p.Binary)
	engine := normalizeContainerEngine(p.Engine)
	if configured := strings.TrimSpace(desired.Metadata["engine"]); configured != "" {
		engine = normalizeContainerEngine(configured)
	} else if desired.Container != nil && strings.TrimSpace(desired.Container.Engine) != "" {
		engine = normalizeContainerEngine(desired.Container.Engine)
	}
	if binary == "" {
		binary = engine
	}
	run := p.Run
	if run == nil {
		run = defaultCommandRunner
	}
	digest, remoteErr := inspectRemoteContainerDigest(ctx, run, "", binary, image)
	remote := remoteErr == nil
	if !remote {
		var localErr error
		digest, localErr = inspectContainerDigest(ctx, run, "", binary, image)
		if localErr != nil {
			// A daemon without a local image cannot prove an update without a
			// registry response. Leave the candidate unchanged and let an
			// explicit Stage operation report an actionable pull/inspect error.
			return desired, false, nil
		}
	}
	currentDigest := currentDigestForContainer(current, image)
	candidate := desired
	candidate.Mode = RuntimeModeContainer
	candidate.Source = image
	candidate.Metadata = cloneMetadata(desired.Metadata)
	if candidate.Metadata == nil {
		candidate.Metadata = make(map[string]string)
	}
	if remote {
		// Stage verifies this exact remotely observed digest after its controlled
		// pull. A stale `never`/`if-missing` local image therefore fails closed
		// instead of silently activating the old bytes under a new version.
		candidate.Metadata["expectedImageDigest"] = digest
	} else {
		delete(candidate.Metadata, "expectedImageDigest")
	}
	if strings.TrimSpace(candidate.Version) == "" {
		candidate.Version = "image-" + strings.TrimPrefix(strings.ToLower(digest), "sha256:")
	}
	if currentDigest == "" || !strings.EqualFold(currentDigest, digest) {
		return candidate, true, nil
	}
	return candidate, false, nil
}

func (p ContainerProvider) Stage(ctx context.Context, spec Spec, destination string) (Manifest, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	kind := strings.ToLower(strings.TrimSpace(spec.Kind))
	if kind != "vllm" && kind != "llamacpp" {
		return Manifest{}, fmt.Errorf("container provider does not support runtime kind %q", spec.Kind)
	}
	if err := validateProviderStage(spec, kind); err != nil {
		return Manifest{}, err
	}
	if err := prepareProviderDestination(destination); err != nil {
		return Manifest{}, fmt.Errorf("container stage destination: %w", err)
	}
	if normalizeRuntimeMode(spec.Mode) != RuntimeModeContainer {
		return Manifest{}, errors.New("container provider requires mode container")
	}
	image := strings.TrimSpace(spec.Source)
	if value := strings.TrimSpace(spec.Metadata["image"]); value != "" {
		image = value
	}
	if image == "" && spec.Container != nil {
		image = strings.TrimSpace(spec.Container.Image)
	}
	if err := validateContainerImage(image, p.AllowedRegistries); err != nil {
		return Manifest{}, err
	}
	engine := normalizeContainerEngine(p.Engine)
	if configured := strings.TrimSpace(spec.Metadata["engine"]); configured != "" {
		engine = normalizeContainerEngine(configured)
	} else if spec.Container != nil && strings.TrimSpace(spec.Container.Engine) != "" {
		engine = normalizeContainerEngine(spec.Container.Engine)
	}
	if engine != "docker" && engine != "podman" {
		return Manifest{}, fmt.Errorf("unsupported container engine %q", engine)
	}
	binary := strings.TrimSpace(p.Binary)
	if binary == "" {
		binary = engine
	}
	run := p.Run
	if run == nil {
		run = defaultCommandRunner
	}
	policy := strings.ToLower(strings.TrimSpace(spec.Metadata["pullPolicy"]))
	if policy == "" && spec.Container != nil {
		policy = strings.ToLower(strings.TrimSpace(spec.Container.PullPolicy))
	}
	if policy == "" {
		policy = "always"
	}
	if policy != "always" && policy != "if-missing" && policy != "never" {
		return Manifest{}, fmt.Errorf("container image pull policy %q is invalid", policy)
	}
	configuredPolicy := policy
	if policy == "if-missing" {
		if _, err := runRuntimeCommand(ctx, run, destination, binary, "image", "inspect", image); err == nil {
			// The local image exists; inspect below still resolves its immutable
			// RepoDigest and fails closed when a daemon cannot report one.
			policy = "never"
		}
	}
	if policy != "never" {
		args := []string{"pull"}
		platform := strings.TrimSpace(spec.Metadata["platform"])
		if platform == "" && spec.Container != nil {
			platform = strings.TrimSpace(spec.Container.Platform)
		}
		if platform != "" {
			if err := validateContainerPlatform(platform); err != nil {
				return Manifest{}, err
			}
			args = append(args, "--platform", platform)
		}
		args = append(args, image)
		if _, err := runRuntimeCommand(ctx, run, destination, binary, args...); err != nil {
			return Manifest{}, fmt.Errorf("pull container image: %w", err)
		}
	}
	digest, err := inspectContainerDigest(ctx, run, destination, binary, image)
	if err != nil {
		return Manifest{}, err
	}
	if expected := strings.ToLower(strings.TrimSpace(spec.Metadata["expectedImageDigest"])); expected != "" {
		if !imageDigestPattern.MatchString(expected) {
			return Manifest{}, errors.New("container expected image digest is invalid")
		}
		if !strings.EqualFold(digest, expected) {
			return Manifest{}, fmt.Errorf("container image digest did not reach expected remote digest: have %s, want %s", digest, expected)
		}
	}
	metadata := cloneMetadata(spec.Metadata)
	if metadata == nil {
		metadata = make(map[string]string)
	}
	metadata["mode"] = RuntimeModeContainer
	metadata["engine"] = engine
	metadata["image"] = image
	metadata["sourceType"] = "image"
	metadata["source"] = image
	metadata["imageDigest"] = digest
	metadata["pullPolicy"] = configuredPolicy
	metadata["pullPolicyEffective"] = policy
	platform := strings.TrimSpace(metadata["platform"])
	if platform == "" && spec.Container != nil {
		platform = strings.TrimSpace(spec.Container.Platform)
	}
	if platform != "" {
		if err := validateContainerPlatform(platform); err != nil {
			return Manifest{}, err
		}
		metadata["platform"] = platform
	}
	metadata["lockFingerprint"] = runtimeLockFingerprint(spec, image+"@"+digest, "", "", nil, digest)
	return Manifest{
		Name:        spec.Name,
		Version:     spec.Version,
		Kind:        spec.Kind,
		Source:      image,
		Ref:         spec.Ref,
		Commit:      spec.Commit,
		Checksum:    spec.Checksum,
		Fingerprint: fingerprintMetadata(metadata),
		Metadata:    metadata,
	}, nil
}

func (p ContainerProvider) Verify(ctx context.Context, manifest Manifest, directory string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if manifestMode(manifest) != RuntimeModeContainer {
		return errors.New("container provider requires a container manifest")
	}
	image := strings.TrimSpace(manifest.Metadata["image"])
	if image == "" {
		image = strings.TrimSpace(manifest.Source)
	}
	if err := validateContainerImage(image, p.AllowedRegistries); err != nil {
		return err
	}
	binary := strings.TrimSpace(p.Binary)
	engine := normalizeContainerEngine(p.Engine)
	if configured := strings.TrimSpace(manifest.Metadata["engine"]); configured != "" {
		engine = normalizeContainerEngine(configured)
	}
	if binary == "" {
		binary = engine
	}
	run := p.Run
	if run == nil {
		run = defaultCommandRunner
	}
	digest, err := inspectContainerDigest(ctx, run, directory, binary, image)
	if err != nil {
		return err
	}
	expected := strings.TrimSpace(manifest.Metadata["imageDigest"])
	if expected == "" {
		expected = imageDigest(image)
	}
	if expected == "" {
		return errors.New("container image manifest has no immutable digest")
	}
	if digest != expected {
		return fmt.Errorf("container image digest changed: have %s, want %s", digest, expected)
	}
	return nil
}

func (p ContainerProvider) Health(ctx context.Context, manifest Manifest, directory string) error {
	return p.Verify(ctx, manifest, directory)
}

// LaunchArgs builds the complete engine argv for a staged container runtime.
// The image is taken from the spec/metadata and pinned to the staged digest
// when one is available; callers can pass the result directly to exec.Cmd.
func (p ContainerProvider) LaunchArgs(spec Spec, digest string) ([]string, error) {
	image := strings.TrimSpace(spec.Source)
	if value := strings.TrimSpace(spec.Metadata["image"]); value != "" {
		image = value
	}
	if image == "" && spec.Container != nil {
		image = strings.TrimSpace(spec.Container.Image)
	}
	if image == "" {
		return nil, errors.New("container launch image is required")
	}
	if digest = strings.TrimSpace(digest); digest != "" {
		if !imageDigestPattern.MatchString(strings.ToLower(digest)) {
			return nil, errors.New("container launch digest is invalid")
		}
		image = strings.Split(image, "@")[0] + "@" + strings.ToLower(digest)
	}
	launch := ContainerLaunch{Engine: p.Engine}
	if spec.Container != nil {
		launch = *cloneContainerLaunch(spec.Container)
	}
	if launch.Engine == "" {
		launch.Engine = normalizeContainerEngine(spec.Metadata["engine"])
	}
	args, err := launch.RunArgs(image)
	if err != nil {
		return nil, err
	}
	return append([]string{normalizeContainerEngine(launch.Engine)}, args...), nil
}

func normalizeContainerEngine(engine string) string {
	engine = strings.ToLower(strings.TrimSpace(engine))
	if engine == "" {
		return "docker"
	}
	return engine
}

func validateContainerPlatform(platform string) error {
	if len(platform) > 128 || strings.TrimSpace(platform) != platform || platform == "" {
		return errors.New("container platform is invalid")
	}
	for _, r := range platform {
		if unicode.IsControl(r) || unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) {
			return errors.New("container platform contains whitespace or control characters")
		}
	}
	return nil
}

func validateContainerImage(image string, allowed []string) error {
	image = strings.TrimSpace(image)
	if image == "" || len(image) > 1024 || image != strings.TrimSpace(image) || strings.ContainsAny(image, "\x00\r\n\t") {
		return errors.New("container image is required and must not contain whitespace or NUL")
	}
	if strings.Contains(image, "://") || strings.HasPrefix(image, "-") || strings.Contains(image, "@sha256:sha256:") {
		return fmt.Errorf("container image %q is invalid", image)
	}
	for _, r := range image {
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return fmt.Errorf("container image %q contains whitespace or control characters", image)
		}
	}
	registry := imageRegistry(image)
	if len(allowed) == 0 {
		return nil
	}
	for _, entry := range allowed {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if strings.TrimSuffix(entry, "/") == registry {
			return nil
		}
	}
	return fmt.Errorf("container registry %q is not allowlisted", registry)
}

func imageRegistry(image string) string {
	name := image
	if at := strings.IndexByte(name, '@'); at >= 0 {
		name = name[:at]
	}
	parts := strings.Split(name, "/")
	if len(parts) == 1 {
		return "docker.io"
	}
	first := strings.ToLower(parts[0])
	if strings.Contains(first, ".") || strings.Contains(first, ":") || first == "localhost" {
		return first
	}
	return "docker.io"
}

func imageDigest(image string) string {
	if at := strings.IndexByte(image, '@'); at >= 0 {
		digest := strings.TrimSpace(image[at+1:])
		if imageDigestPattern.MatchString(digest) {
			return strings.ToLower(digest)
		}
	}
	return ""
}

func inspectContainerDigest(ctx context.Context, run CommandRunner, directory, binary, image string) (string, error) {
	output, err := runRuntimeCommand(ctx, run, directory, binary, "image", "inspect", "--format={{json .RepoDigests}}", image)
	if err != nil {
		return "", fmt.Errorf("inspect container image: %w", err)
	}
	var digests []string
	if err := json.Unmarshal(output, &digests); err != nil {
		return "", fmt.Errorf("decode container image digests: %w", err)
	}
	for _, value := range digests {
		if at := strings.LastIndexByte(value, '@'); at >= 0 {
			digest := strings.ToLower(strings.TrimSpace(value[at+1:]))
			if imageDigestPattern.MatchString(digest) {
				return digest, nil
			}
		}
	}
	if digest := imageDigest(image); digest != "" {
		return digest, nil
	}
	return "", errors.New("container image has no immutable RepoDigest; use a registry image with a digest")
}

// inspectRemoteContainerDigest asks Docker/Podman for registry manifest
// metadata only. Docker's --verbose form contains Descriptor.digest, which is
// the immutable identity of the tag itself rather than a platform child
// digest. The parser also accepts a top-level digest for compatible engines.
func inspectRemoteContainerDigest(ctx context.Context, run CommandRunner, directory, binary, image string) (string, error) {
	if digest := imageDigest(image); digest != "" {
		return digest, nil
	}
	output, err := runRuntimeCommand(ctx, run, directory, binary, "manifest", "inspect", "--verbose", image)
	if err != nil {
		return "", fmt.Errorf("inspect remote container manifest: %w", err)
	}
	if len(output) > maxContainerManifestBytes {
		return "", fmt.Errorf("remote container manifest exceeds %d bytes", maxContainerManifestBytes)
	}
	var document struct {
		Digest     string `json:"digest"`
		Descriptor struct {
			Digest string `json:"digest"`
		} `json:"Descriptor"`
	}
	if err := json.Unmarshal(output, &document); err != nil {
		return "", fmt.Errorf("decode remote container manifest: %w", err)
	}
	for _, value := range []string{document.Descriptor.Digest, document.Digest} {
		digest := strings.ToLower(strings.TrimSpace(value))
		if imageDigestPattern.MatchString(digest) {
			return digest, nil
		}
	}
	return "", errors.New("remote container manifest has no immutable digest")
}

// ContainerLaunch is the shell-free `run` portion of a containerized model.
// It is intentionally independent from the image provider so the model
// process layer can build argv without importing Docker libraries.
type ContainerLaunch struct {
	Engine      string            `json:"engine,omitempty"`
	Name        string            `json:"name,omitempty"`
	Image       string            `json:"image,omitempty"`
	Platform    string            `json:"platform,omitempty"`
	PullPolicy  string            `json:"pullPolicy,omitempty"`
	Entrypoint  []string          `json:"entrypoint,omitempty"`
	Command     []string          `json:"command,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Mounts      []ContainerMount  `json:"mounts,omitempty"`
	Ports       []ContainerPort   `json:"ports,omitempty"`
	GPUs        string            `json:"gpus,omitempty"`
	ShmSize     string            `json:"shmSize,omitempty"`
	StopTimeout time.Duration     `json:"stopTimeout,omitempty"`
}

// UnmarshalJSON accepts both the numeric nanoseconds emitted by the default
// encoding/json representation of time.Duration and the human-readable form
// used by the HTTP/configuration schemas (for example "15s"). Keeping this
// compatibility at the runtime boundary lets Runtime API callers use the
// same launch payload as YAML without weakening the typed contract.
func (c *ContainerLaunch) UnmarshalJSON(data []byte) error {
	type rawContainerLaunch struct {
		Engine      string            `json:"engine,omitempty"`
		Name        string            `json:"name,omitempty"`
		Image       string            `json:"image,omitempty"`
		Platform    string            `json:"platform,omitempty"`
		PullPolicy  string            `json:"pullPolicy,omitempty"`
		Entrypoint  []string          `json:"entrypoint,omitempty"`
		Command     []string          `json:"command,omitempty"`
		Env         map[string]string `json:"env,omitempty"`
		Mounts      []ContainerMount  `json:"mounts,omitempty"`
		Ports       []ContainerPort   `json:"ports,omitempty"`
		GPUs        string            `json:"gpus,omitempty"`
		ShmSize     string            `json:"shmSize,omitempty"`
		StopTimeout json.RawMessage   `json:"stopTimeout,omitempty"`
	}
	var raw rawContainerLaunch
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var stopTimeout time.Duration
	if len(raw.StopTimeout) > 0 && string(raw.StopTimeout) != "null" {
		var numeric int64
		if err := json.Unmarshal(raw.StopTimeout, &numeric); err == nil {
			stopTimeout = time.Duration(numeric)
		} else {
			var textValue string
			if stringErr := json.Unmarshal(raw.StopTimeout, &textValue); stringErr != nil {
				return fmt.Errorf("container stopTimeout must be a duration string or nanoseconds: %w", stringErr)
			}
			parsed, parseErr := time.ParseDuration(strings.TrimSpace(textValue))
			if parseErr != nil {
				return fmt.Errorf("container stopTimeout: %w", parseErr)
			}
			stopTimeout = parsed
		}
	}
	*c = ContainerLaunch{
		Engine: raw.Engine, Name: raw.Name, Image: raw.Image, Platform: raw.Platform,
		PullPolicy: raw.PullPolicy, Entrypoint: append([]string(nil), raw.Entrypoint...),
		Command: append([]string(nil), raw.Command...), Env: cloneMetadata(raw.Env),
		Mounts: append([]ContainerMount(nil), raw.Mounts...), Ports: append([]ContainerPort(nil), raw.Ports...),
		GPUs: raw.GPUs, ShmSize: raw.ShmSize, StopTimeout: stopTimeout,
	}
	return nil
}

type ContainerMount struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"readOnly,omitempty"`
}

type ContainerPort struct {
	Host      int `json:"host"`
	Container int `json:"container"`
}

// RunArgs returns arguments after the engine executable. The image is always
// supplied by the Runtime Manager (normally as an immutable digest), while
// model-specific command arguments remain configurable. The returned slice is
// detached from all input maps/slices.
func (c ContainerLaunch) RunArgs(image string) ([]string, error) {
	if strings.TrimSpace(image) == "" {
		image = strings.TrimSpace(c.Image)
	}
	if err := validateContainerImage(image, nil); err != nil {
		return nil, err
	}
	engine := normalizeContainerEngine(c.Engine)
	if engine != "docker" && engine != "podman" {
		return nil, fmt.Errorf("unsupported container engine %q", c.Engine)
	}
	args := []string{"run", "--init", "--rm"}
	if c.PullPolicy != "" {
		policy := strings.ToLower(strings.TrimSpace(c.PullPolicy))
		if c.PullPolicy != policy || (policy != "always" && policy != "if-missing" && policy != "never") {
			return nil, errors.New("container pull policy must be always, if-missing, or never")
		}
		// Docker/Podman call the conditional policy "missing" while the
		// configuration/API vocabulary uses "if-missing" for clarity.
		if policy == "if-missing" {
			policy = "missing"
		}
		args = append(args, "--pull", policy)
	}
	if c.Name != "" {
		if err := validateContainerName(c.Name); err != nil {
			return nil, err
		}
		args = append(args, "--name", c.Name)
	}
	if c.Platform != "" {
		if err := validateContainerPlatform(c.Platform); err != nil {
			return nil, err
		}
		args = append(args, "--platform", c.Platform)
	}
	if c.GPUs != "" {
		if err := validateContainerToken(c.GPUs, "container GPUs"); err != nil {
			return nil, err
		}
		args = append(args, "--gpus", c.GPUs)
	}
	if c.ShmSize != "" {
		if err := validateContainerToken(c.ShmSize, "container shmSize"); err != nil {
			return nil, err
		}
		args = append(args, "--shm-size", c.ShmSize)
	}
	if c.StopTimeout < 0 {
		return nil, errors.New("container stop timeout must be >= 0")
	}
	if c.StopTimeout > 0 {
		seconds := int64(c.StopTimeout / time.Second)
		if c.StopTimeout%time.Second != 0 {
			seconds++
		}
		args = append(args, "--stop-timeout", strconv.FormatInt(seconds, 10))
	}
	keys := make([]string, 0, len(c.Env))
	for key := range c.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := validateEnvKey(key); err != nil {
			return nil, err
		}
		if err := validateContainerToken(c.Env[key], "container environment value"); err != nil {
			return nil, err
		}
		args = append(args, "-e", key+"="+c.Env[key])
	}
	for _, mount := range c.Mounts {
		if err := validateMount(mount); err != nil {
			return nil, err
		}
		value := mount.Source + ":" + mount.Target
		if mount.ReadOnly {
			value += ":ro"
		}
		args = append(args, "-v", value)
	}
	for _, port := range c.Ports {
		if port.Host < 1 || port.Host > 65535 || port.Container < 1 || port.Container > 65535 {
			return nil, errors.New("container port must be between 1 and 65535")
		}
		args = append(args, "-p", fmt.Sprintf("%d:%d", port.Host, port.Container))
	}
	if len(c.Entrypoint) > 0 {
		for _, value := range c.Entrypoint {
			if err := validateContainerToken(value, "container entrypoint"); err != nil {
				return nil, err
			}
		}
		args = append(args, "--entrypoint", c.Entrypoint[0])
	}
	args = append(args, image)
	// Docker/Podman accept only the executable after --entrypoint. Preserve
	// any additional configured entrypoint items as the leading container
	// command arguments instead of silently discarding them.
	if len(c.Entrypoint) > 1 {
		args = append(args, c.Entrypoint[1:]...)
	}
	for _, value := range c.Command {
		if err := validateContainerToken(value, "container command"); err != nil {
			return nil, err
		}
		args = append(args, value)
	}
	return args, nil
}

func validateContainerName(value string) error {
	if len(value) > 128 || strings.TrimSpace(value) != value || value == "" {
		return errors.New("container name is invalid")
	}
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || !(unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("-_.", r)) {
			return fmt.Errorf("container name %q contains invalid characters", value)
		}
	}
	return nil
}

func validateContainerToken(value, field string) error {
	if len(value) > 4096 || strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("%s is too long or contains NUL", field)
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return fmt.Errorf("%s contains control characters", field)
		}
	}
	return nil
}

func validateEnvKey(key string) error {
	if key == "" || len(key) > 128 {
		return errors.New("container environment key is invalid")
	}
	for i, r := range key {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r == '_' || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return fmt.Errorf("container environment key %q is invalid", key)
	}
	return nil
}

func validateMount(mount ContainerMount) error {
	if mount.Source == "" || mount.Target == "" {
		return errors.New("container mount source and target are required")
	}
	if strings.ContainsAny(mount.Source+mount.Target, "\x00\r\n") {
		return errors.New("container mount contains control characters")
	}
	return nil
}
