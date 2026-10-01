package server

import (
	"fmt"
	"net/url"
	"reflect"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/config"
	runtimeManager "github.com/mostlygeek/llama-swap/internal/runtime"
)

func managedRuntimeDefinitionsChanged(active, desired config.Config) bool {
	return active.RuntimeManager.BuildWhileBusy != desired.RuntimeManager.BuildWhileBusy ||
		!reflect.DeepEqual(active.RuntimeManager.SourceAllowlist, desired.RuntimeManager.SourceAllowlist) ||
		!reflect.DeepEqual(active.RuntimeManager.ContainerRegistries, desired.RuntimeManager.ContainerRegistries) ||
		!reflect.DeepEqual(active.Runtimes, desired.Runtimes) ||
		active.LMCache.Enabled != desired.LMCache.Enabled ||
		active.LMCache.EffectiveVersion() != desired.LMCache.EffectiveVersion() ||
		active.LMCache.PackageName() != desired.LMCache.PackageName() ||
		active.LMCache.IndexURL != desired.LMCache.IndexURL ||
		active.LMCache.PythonVersion != desired.LMCache.PythonVersion ||
		!reflect.DeepEqual(active.LMCache.Update, desired.LMCache.Update)
}

// runtimeProvidersForConfig builds the provider set from the live control
// plane policy. The manager keeps these providers in memory, so configuration
// reloads can tighten source/registry allowlists without restarting the
// daemon or leaving the automatic update loop pointed at stale settings.
func runtimeProvidersForConfig(cfg config.Config) map[string]runtimeManager.Provider {
	allowlist := append([]string(nil), cfg.RuntimeManager.SourceAllowlist...)
	registries := append([]string(nil), cfg.RuntimeManager.ContainerRegistries...)
	if len(registries) == 0 {
		registries = []string{"docker.io", "ghcr.io"}
	}
	return map[string]runtimeManager.Provider{
		"llamacpp": runtimeManager.ProviderMux{
			Native:    runtimeManager.LlamaCPPProvider{SourceAllowlist: append([]string(nil), allowlist...)},
			Container: runtimeManager.ContainerProvider{Engine: "docker", AllowedRegistries: append([]string(nil), registries...)},
		},
		"vllm": runtimeManager.ProviderMux{
			Native: runtimeManager.VLLMProvider{
				SourceAllowlist:  append([]string(nil), allowlist...),
				AllowPipFallback: true,
			},
			Container: runtimeManager.ContainerProvider{Engine: "docker", AllowedRegistries: append([]string(nil), registries...)},
		},
		// The standalone LMCache server runs from its own version-managed
		// virtualenv, isolated from every vLLM venv. It is a derived runtime
		// (registered from the top-level lmcache module, never from user
		// runtimes) and is always native.
		"lmcache": runtimeManager.LMCacheProvider{
			IndexURL:        strings.TrimSpace(cfg.LMCache.IndexURL),
			UpdateURL:       lmcacheUpdateURL(cfg.LMCache.IndexURL, cfg.LMCache.PackageName()),
			SourceAllowlist: append([]string(nil), allowlist...),
		},
	}
}

// lmcacheUpdateURL derives the PyPI JSON endpoint from the configured package
// index. A simple-index mirror is not the same resource as its JSON API, and
// silently falling back to public PyPI would make an air-gapped or policy
// constrained deployment check the wrong source.
func lmcacheUpdateURL(indexURL, packageName string) string {
	indexURL = strings.TrimSpace(indexURL)
	if indexURL == "" {
		return ""
	}
	u, err := url.Parse(indexURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return indexURL
	}
	path := strings.TrimRight(u.Path, "/")
	if strings.HasSuffix(strings.ToLower(path), "/simple") {
		path = strings.TrimSuffix(path, "/simple")
	}
	path = strings.TrimRight(path, "/") + "/pypi/" + url.PathEscape(strings.TrimSpace(packageName)) + "/json"
	u.Path = path
	u.RawPath = ""
	return u.String()
}

// syncManagedRuntimeDefinitions publishes the desired runtime definitions to
// the long-lived manager during an in-process config reload. Removing a YAML
// definition only disables automatic checks; durable versions and pointers
// remain available to the explicit runtime API for rollback and pruning.
//
// The candidate is validated synchronously so a malformed definition rejects
// the whole reload, but the manager state change is queued instead of applied
// inline: the manager applies the update in its own loop while holding its
// lock, and an in-flight activation may hold that lock while its switch hook
// waits for this apply to finish. Applying inline would deadlock the pair, so
// this path must never wait on the manager lock.
func (s *Server) syncManagedRuntimeDefinitions(active, desired config.Config) error {
	if s == nil {
		return nil
	}
	if !managedRuntimeDefinitionsChanged(active, desired) {
		return nil
	}
	if s.runtime == nil {
		return fmt.Errorf("runtime manager is not running; restart the daemon to initialize it")
	}

	definitions := make(map[string]runtimeManager.Definition, len(desired.Runtimes)+1)
	registered := make([]string, 0, len(desired.Runtimes))
	removed := make([]string, 0, len(active.Runtimes)+1)
	for name, runtimeConfig := range desired.Runtimes {
		if strings.TrimSpace(runtimeConfig.Kind) == "" {
			// An incomplete definition still claims the runtime name so its
			// durable state stays addressable, but it configures nothing.
			registered = append(registered, name)
			removed = append(removed, name)
			continue
		}
		definitions[name] = runtimeManager.Definition{Spec: runtimeSpecFromConfig(name, runtimeConfig), Policy: runtimePolicyFromConfig(runtimeConfig.Update)}
	}
	for name := range active.Runtimes {
		if _, exists := desired.Runtimes[name]; exists {
			continue
		}
		removed = append(removed, name)
	}

	// The derived LMCache server runtime follows the top-level module, never
	// the user's runtimes map (the name is reserved in config validation).
	if desired.LMCache.Enabled {
		definitions[config.LMCacheRuntimeName] = runtimeManager.Definition{Spec: lmcacheRuntimeSpec(desired), Policy: lmcacheRuntimePolicy(desired.LMCache.Update)}
	} else {
		removed = append(removed, config.LMCacheRuntimeName)
	}

	buildWhileBusy := desired.RuntimeManager.BuildWhileBusy
	return s.runtime.QueueControlUpdate(runtimeManager.ControlUpdate{
		Definitions:    definitions,
		Registered:     registered,
		Removed:        removed,
		Providers:      runtimeProvidersForConfig(desired),
		BuildWhileBusy: &buildWhileBusy,
	})
}

// lmcacheRuntimeSpec converts the top-level lmcache module into the
// provider-neutral definition of the derived server runtime. An empty
// version is legal: the explicit enable/stage flow resolves the newest
// published release and records it in the manifest.
func lmcacheRuntimeSpec(cfg config.Config) runtimeManager.Spec {
	module := cfg.LMCache
	build := map[string]string{
		"package": module.PackageName(),
		"python":  module.EffectivePythonVersion(),
	}
	if indexURL := strings.TrimSpace(module.IndexURL); indexURL != "" {
		build["indexURL"] = indexURL
	}
	return runtimeManager.Spec{
		Name:       config.LMCacheRuntimeName,
		Kind:       "lmcache",
		Mode:       runtimeManager.RuntimeModeNative,
		Version:    strings.TrimSpace(module.EffectiveVersion()),
		SourceType: "pypi",
		Build:      build,
	}
}

// lmcacheRuntimePolicy derives the update policy of the derived server
// runtime from lmcache.update. An absent or empty-policy block stays on
// "disabled": the server is a dependency of running models, so automatic
// updates require an explicit opt-in. When enabled, the manager's update
// loop drives check/stage/activate; the activation hooks (before =
// dependency guard + stop, after = restart on the new version) keep the
// server lifecycle correct for both automatic and explicit operations.
func lmcacheRuntimePolicy(update *config.RuntimeUpdateConfig) runtimeManager.UpdatePolicy {
	if update == nil {
		return runtimeManager.UpdatePolicy{Policy: "disabled"}
	}
	policy := runtimePolicyFromConfig(*update)
	if strings.TrimSpace(update.Policy) == "" {
		policy.Policy = "disabled"
	}
	return policy
}

// runtimeSpecFromConfig converts the source/build portion of the public YAML
// definition into the provider-neutral runtime contract. It intentionally
// does not invent a version for mutable sources: providers derive versions
// from an explicit release channel, tracked Git ref, or container digest,
// while an exact update.version remains authoritative when configured. A
// bundled image asset is the one deterministic exception and uses the stable
// `bundled` seed version when no explicit version is supplied.
func runtimeSpecFromConfig(name string, value config.RuntimeConfig) runtimeManager.Spec {
	mode := strings.ToLower(strings.TrimSpace(value.Mode))
	if mode == "" && (strings.TrimSpace(value.Source.Image) != "" || strings.TrimSpace(value.Container.Image) != "") {
		mode = runtimeManager.RuntimeModeContainer
	}
	if mode == "" {
		mode = runtimeManager.RuntimeModeNative
	}
	sourceType := strings.TrimSpace(value.Source.Type)
	if sourceType == "" && strings.TrimSpace(value.Source.Repository) != "" {
		// A repository is unambiguously a Git source.  This keeps the concise
		// `source.repository` form equivalent to `source.type: git` while still
		// allowing explicit source.type to override inference.
		sourceType = "git"
	}
	if sourceType == "" && mode == runtimeManager.RuntimeModeContainer {
		sourceType = "image"
	}
	source := strings.TrimSpace(value.Source.Image)
	if source == "" {
		source = strings.TrimSpace(value.Container.Image)
	}
	if source == "" {
		source = strings.TrimSpace(value.Source.Path)
	}
	if source == "" {
		source = strings.TrimSpace(value.Source.URL)
	}
	if source == "" {
		source = strings.TrimSpace(value.Source.Repository)
	}
	if source == "" {
		source = strings.TrimSpace(value.Source.Type)
	}
	build := map[string]string{}
	if value.Build.Driver != "" {
		build["driver"] = value.Build.Driver
	}
	if value.Build.WorkDir != "" {
		build["workDir"] = value.Build.WorkDir
	}
	if value.Build.Backend != "" {
		build["backend"] = value.Build.Backend
	}
	if value.Build.Compiler != "" {
		build["compiler"] = value.Build.Compiler
	}
	if value.Build.Python != "" {
		build["python"] = value.Build.Python
	}
	if value.Build.IndexURL != "" {
		build["indexURL"] = value.Build.IndexURL
	}
	if len(value.Build.CUDAArchitectures) > 0 {
		build["cudaArchitectures"] = strings.Join(value.Build.CUDAArchitectures, ",")
	}
	if len(value.Build.CMake) > 0 {
		build["cmake"] = strings.Join(value.Build.CMake, " ")
	}
	if len(value.Build.Extras) > 0 {
		build["extras"] = strings.Join(value.Build.Extras, " ")
	}
	if len(build) == 0 {
		build = nil
	}
	metadata := map[string]string{"channel": strings.TrimSpace(value.Update.Channel), "mode": mode}
	if sourceType != "" {
		metadata["sourceType"] = sourceType
	}
	if trackRef := strings.TrimSpace(value.Source.TrackRef); trackRef != "" {
		metadata["trackRef"] = trackRef
	}
	image := strings.TrimSpace(value.Source.Image)
	if image == "" {
		image = strings.TrimSpace(value.Container.Image)
	}
	if image != "" {
		metadata["image"] = image
	}
	if platform := strings.TrimSpace(value.Source.Platform); platform != "" {
		metadata["platform"] = platform
	}
	if pullPolicy := strings.TrimSpace(value.Source.PullPolicy); pullPolicy != "" {
		metadata["pullPolicy"] = pullPolicy
	}
	if engine := strings.TrimSpace(value.Container.Engine); engine != "" {
		metadata["engine"] = engine
	}
	if verify := strings.TrimSpace(value.Verify.SHA256); verify != "" {
		metadata["verify.sha256"] = verify
	}
	if healthPath := strings.TrimSpace(value.Verify.HealthPath); healthPath != "" {
		metadata["verify.healthPath"] = healthPath
	}
	if smokeCommand := strings.TrimSpace(value.Verify.SmokeCommand); smokeCommand != "" {
		metadata["verify.smokeCommand"] = smokeCommand
	}
	for key, value := range metadata {
		if value == "" {
			delete(metadata, key)
		}
	}
	buildArgs := map[string][]string{
		"cudaArchitectures": append([]string(nil), value.Build.CUDAArchitectures...),
		"cmake":             append([]string(nil), value.Build.CMake...),
		"extras":            append([]string(nil), value.Build.Extras...),
		"args":              append([]string(nil), value.Build.Args...),
	}
	for key, args := range buildArgs {
		if len(args) == 0 {
			delete(buildArgs, key)
		}
	}
	steps := make([]runtimeManager.BuildStep, 0, len(value.Build.Steps))
	for _, step := range value.Build.Steps {
		steps = append(steps, runtimeManager.BuildStep{WorkDir: step.WorkDir, Command: step.Command, Args: append([]string(nil), step.Args...), Env: cloneStringMap(step.Env)})
	}
	artifacts := make([]runtimeManager.Artifact, 0, len(value.Build.Artifacts))
	for _, artifact := range value.Build.Artifacts {
		artifacts = append(artifacts, runtimeManager.Artifact{From: artifact.From, To: artifact.To})
	}
	var container *runtimeManager.ContainerLaunch
	if value.Container.Engine != "" || value.Container.Name != "" || value.Container.Image != "" || value.Container.Platform != "" || value.Container.PullPolicy != "" || value.Container.StopTimeout != 0 || len(value.Container.Command) > 0 || len(value.Container.Entrypoint) > 0 || len(value.Container.Env) > 0 || len(value.Container.Mounts) > 0 || len(value.Container.Ports) > 0 || value.Container.GPUs != "" || value.Container.ShmSize != "" {
		platform := value.Container.Platform
		if platform == "" {
			platform = value.Source.Platform
		}
		container = &runtimeManager.ContainerLaunch{
			Engine: value.Container.Engine, Name: value.Container.Name, Image: value.Container.Image, Platform: platform, PullPolicy: value.Container.PullPolicy,
			Entrypoint: append([]string(nil), value.Container.Entrypoint...), Command: append([]string(nil), value.Container.Command...),
			Env: cloneStringMap(value.Container.Env), GPUs: value.Container.GPUs, ShmSize: value.Container.ShmSize,
			StopTimeout: value.Container.StopTimeout,
		}
		for _, mount := range value.Container.Mounts {
			container.Mounts = append(container.Mounts, runtimeManager.ContainerMount{Source: mount.Source, Target: mount.Target, ReadOnly: mount.ReadOnly})
		}
		for _, port := range value.Container.Ports {
			container.Ports = append(container.Ports, runtimeManager.ContainerPort{Host: port.Host, Container: port.Container})
		}
	}
	ref := strings.TrimSpace(value.Source.Ref)
	if ref == "" {
		ref = strings.TrimSpace(value.Source.TrackRef)
	}
	version := strings.TrimSpace(value.Update.Version)
	if version == "" && strings.EqualFold(sourceType, "bundled") {
		version = "bundled"
	}
	return runtimeManager.Spec{
		Name:       name,
		Kind:       value.Kind,
		Mode:       mode,
		Version:    version,
		SourceType: sourceType,
		Source:     source,
		Ref:        ref,
		Commit:     strings.TrimSpace(value.Update.Commit),
		Checksum:   strings.TrimSpace(value.Source.Checksum),
		Build:      build, BuildArgs: buildArgs, BuildEnv: cloneStringMap(value.Build.Env), BuildSteps: steps,
		InstallArgs: append([]string(nil), value.Build.InstallArgs...), Artifacts: artifacts, Container: container, Metadata: metadata,
	}
}

func cloneStringMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func runtimePolicyFromConfig(value config.RuntimeUpdateConfig) runtimeManager.UpdatePolicy {
	checkEverySet := value.CheckEveryConfigured()
	minIdleSet := value.MinIdleConfigured()
	value = value.Effective()
	return runtimeManager.UpdatePolicy{
		Policy:               value.Policy,
		Channel:              value.Channel,
		CheckEvery:           value.CheckEvery,
		CheckEverySet:        checkEverySet,
		MinIdle:              value.MinIdle,
		MinIdleSet:           minIdleSet,
		ActivateOnlyIdle:     value.ActivateOnlyIdle,
		ActivateOnlyIdleSet:  true,
		KeepVersions:         value.KeepVersions,
		KeepVersionsSet:      true,
		RollbackOnFailure:    value.RollbackOnFailure,
		RollbackOnFailureSet: true,
	}
}
