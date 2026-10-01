package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/hw"
	runtimeManager "github.com/mostlygeek/llama-swap/internal/runtime"
)

const fallbackRuntimeManagerRoot = "/var/lib/llama-swap/runtimes"

// managedRuntimeRoot returns the same default used when Server constructs its
// Runtime Manager. Keeping the path derivation in one helper prevents model
// startup from accidentally using a different current pointer than Runtime
// stage/activate operations.
func managedRuntimeRoot(cfg config.Config) string {
	if root := strings.TrimSpace(cfg.RuntimeManager.Root); root != "" {
		return root
	}
	if cacheRoot, err := os.UserCacheDir(); err == nil && filepath.IsAbs(cacheRoot) {
		return filepath.Join(cacheRoot, "llama-swap", "runtimes")
	}
	return fallbackRuntimeManagerRoot
}

// bindManagedRuntimeLaunchConfig produces the private configuration snapshot
// given to the local process router. The public Server config remains exactly
// as loaded from YAML, so config GET/PATCH never exposes or persists generated
// runtime paths. Only native, structured backend launches are bound; legacy
// cmd strings remain an explicit compatibility path and container launches
// retain their existing typed container contract.
func bindManagedRuntimeLaunchConfig(cfg config.Config, hardware *hw.HardwareSnapshot) (config.Config, error) {
	return bindManagedRuntimeLaunchConfigForVersions(cfg, nil, hardware)
}

// bindManagedRuntimeLaunchConfigForVersions is the activation-time variant of
// the normal private launch binding. A non-empty version entry selects an
// immutable staged version instead of current, which lets the router recreate
// the old process correctly if a candidate process fails after current has
// already been switched.
func bindManagedRuntimeLaunchConfigForVersions(cfg config.Config, versions map[string]string, hardware *hw.HardwareSnapshot) (config.Config, error) {
	result := cfg
	var models map[string]config.ModelConfig

	for modelID, model := range cfg.Models {
		runtimeName := strings.TrimSpace(model.Backend.Runtime)
		if runtimeName == "" {
			continue
		}
		runtimeConfig, ok := cfg.Runtimes[runtimeName]
		if !ok {
			return config.Config{}, fmt.Errorf("model %q references unknown backend.runtime %q", modelID, runtimeName)
		}
		kind := strings.ToLower(strings.TrimSpace(runtimeConfig.Kind))
		if kind != "vllm" && kind != "llamacpp" {
			return config.Config{}, fmt.Errorf("model %q references runtime %q with unsupported kind %q", modelID, runtimeName, runtimeConfig.Kind)
		}
		backendType := strings.ToLower(strings.TrimSpace(model.Backend.Type))
		if backendType != kind {
			return config.Config{}, fmt.Errorf("model %q backend.type %q must match runtime %q kind %q", modelID, model.Backend.Type, runtimeName, kind)
		}

		version := strings.TrimSpace(versions[runtimeName])
		// A per-model pin wins over the runtime-level switch target: the model
		// keeps serving its installed version even while the runtime's current
		// pointer moves. A pin to an uninstalled version fails closed below.
		if pinned := strings.TrimSpace(model.Backend.RuntimeVersion); pinned != "" {
			version = pinned
		}
		if configuredRuntimeMode(runtimeConfig) == "container" {
			bound, err := bindManagedContainerRuntimeModel(managedRuntimeRoot(cfg), runtimeName, kind, runtimeConfig, model, version)
			if err != nil {
				return config.Config{}, fmt.Errorf("model %q managed container runtime %q: %w", modelID, runtimeName, err)
			}
			if models == nil {
				models = make(map[string]config.ModelConfig, len(cfg.Models))
				for id, candidate := range cfg.Models {
					models[id] = candidate
				}
				result.Models = models
			}
			models[modelID] = bound
			continue
		}
		if strings.TrimSpace(model.Cmd) != "" {
			return config.Config{}, fmt.Errorf("model %q uses backend.runtime %q but also sets legacy cmd; use backend.args for a native managed runtime", modelID, runtimeName)
		}
		if strings.TrimSpace(model.Backend.Container.Image) != "" {
			return config.Config{}, fmt.Errorf("model %q uses native backend.runtime %q with backend.container; use either native backend.args or a container runtime", modelID, runtimeName)
		}
		// The configuration contract (ValidateExtensionConfig) accepts a
		// launch-only managed model whose args are empty, and
		// MigrateLaunchBlocks produces exactly that shape from legacy
		// args-only configs. Rejecting it here would fail configs that the
		// loader itself validated and migrated.
		if len(model.Backend.Arguments) == 0 && !model.Backend.Launch.LaunchManagedModel() {
			return config.Config{}, fmt.Errorf("model %q uses native backend.runtime %q without backend.args or a launch block", modelID, runtimeName)
		}

		// The launch binding always resolves the kind's canonical entrypoint,
		// never operator-written argv[0]: whatever the configuration names,
		// the process executes the runtime-owned executable.
		entrypoint := config.ManagedLaunchEntrypoint(kind)
		var binding runtimeManager.LaunchBinding
		var err error
		if version != "" {
			binding, err = runtimeManager.LaunchBindingForVersion(managedRuntimeRoot(cfg), runtimeName, kind, entrypoint, version)
		} else {
			var found bool
			binding, found, err = runtimeManager.CurrentVersionLaunchBinding(managedRuntimeRoot(cfg), runtimeName, kind, entrypoint)
			if err == nil && !found {
				// Before the first activation keep an absolute current path. It can
				// never resolve to an ambient host executable, yet lets the server
				// start its control plane and stage the runtime later.
				binding, err = runtimeManager.CurrentLaunchBinding(managedRuntimeRoot(cfg), runtimeName, kind, entrypoint)
			}
		}
		if err != nil {
			return config.Config{}, fmt.Errorf("model %q managed runtime %q: %w", modelID, runtimeName, err)
		}
		launch := model.Backend.Launch
		if launch != nil {
			// CUDA_VISIBLE_DEVICES is the sole persisted GPU selection. Copy it
			// into this short-lived launch plan only so the managed builder can
			// derive tensor parallelism and normalize the process environment.
			selections := launch.GPUs
			if len(selections) == 0 {
				selections = config.CUDAVisibleDevicesFromEnvironment(model.Env)
			}
			if len(selections) > 0 {
				// CUDA_VISIBLE_DEVICES only reaches a CUDA runtime: the launch
				// binding keeps the selections it can resolve to an NVIDIA card,
				// so on Apple or AMD hosts the selection stays affinity metadata
				// without leaking CUDA variables into the environment.
				boundLaunch := *launch
				boundLaunch.GPUs = nvidiaLaunchDevices(hardware, selections)
				launch = &boundLaunch
			}
		}
		rebuilt, launchEnv, err := config.BuildManagedLaunchArguments(model.Backend.Arguments, launch, kind, model.Proxy)
		if err != nil {
			return config.Config{}, fmt.Errorf("model %q managed runtime %q: %w", modelID, runtimeName, err)
		}
		rebuilt = config.EnsureVLLMSleepModeArgument(rebuilt, model.Backend.Type, model.UnloadAfter)
		rebuilt[0] = binding.Executable
		bound := model
		bound.Backend = model.Backend
		bound.Backend.Arguments = rebuilt
		bound.Env = prependManagedRuntimePath(model.Env, binding.BinDir)
		bound.Env = prependManagedRuntimeLibraryPath(bound.Env, binding.LibraryDirs)
		bound.Env = appendManagedLaunchEnv(bound.Env, launchEnv)
		if kind == "vllm" {
			// LMCache launch overrides are generated, not persisted: the
			// private snapshot gets the kv-transfer flag while the public
			// config keeps the operator-written backend block.
			if err := applyLMCacheLaunchConfig(cfg, modelID, runtimeName, version, &bound); err != nil {
				return config.Config{}, fmt.Errorf("model %q lmcache: %w", modelID, err)
			}
		}
		if models == nil {
			models = make(map[string]config.ModelConfig, len(cfg.Models))
			for id, candidate := range cfg.Models {
				models[id] = candidate
			}
			result.Models = models
		}
		models[modelID] = bound
	}
	return result, nil
}

// bindManagedContainerRuntimeModel combines the runtime-owned launch defaults
// with optional per-model settings and replaces the image with the immutable
// digest selected by the current manifest. The public configuration remains
// untouched; this function only prepares the private snapshot consumed by the
// process router.
func bindManagedContainerRuntimeModel(root, runtimeName, kind string, runtimeConfig config.RuntimeConfig, model config.ModelConfig, version string) (config.ModelConfig, error) {
	if strings.TrimSpace(model.Cmd) != "" {
		return config.ModelConfig{}, errors.New("container managed runtimes cannot use legacy cmd")
	}
	launch := cloneRuntimeContainerConfig(runtimeConfig.Container)
	// source.image is the provider's provenance input and therefore wins over a
	// duplicate container.image value. The latter remains a valid fallback for
	// concise runtime definitions.
	if strings.TrimSpace(runtimeConfig.Source.Image) != "" {
		launch.Image = strings.TrimSpace(runtimeConfig.Source.Image)
	}
	if launch.Platform == "" {
		launch.Platform = strings.TrimSpace(runtimeConfig.Source.Platform)
	}
	if launch.PullPolicy == "" {
		launch.PullPolicy = strings.TrimSpace(runtimeConfig.Source.PullPolicy)
	}
	overlayRuntimeContainer(&launch, model.Backend.Container)

	var binding runtimeManager.ContainerBinding
	var found bool
	var err error
	if version != "" {
		binding, err = runtimeManager.ContainerBindingForVersion(root, runtimeName, kind, version)
		found = err == nil
	} else {
		binding, found, err = runtimeManager.CurrentContainerBinding(root, runtimeName, kind)
	}
	if err != nil {
		return config.ModelConfig{}, err
	}
	if found {
		// The digest and engine are coupled: an image pulled into Podman's store
		// cannot be launched through Docker (and vice versa). The durable manifest
		// is authoritative after activation, even if the declarative config still
		// names a mutable tag.
		launch.Image = binding.Image
		launch.Engine = binding.Engine
		if binding.Platform != "" {
			launch.Platform = binding.Platform
		}
	} else {
		// A runtime-only container declaration is valid before the first Stage,
		// but it is not a permission to launch a mutable configured tag. Leave
		// the private model snapshot intentionally commandless so loading it
		// fails with the normal clear "empty command" process error until an
		// explicit Stage/Activate has established a manifest digest.
		bound := model
		bound.Cmd = ""
		bound.Backend = model.Backend
		bound.Backend.Arguments = nil
		bound.Backend.Container = config.RuntimeContainerConfig{}
		return bound, nil
	}
	// Runtime Manager owns pull/update decisions. A process launch must never
	// turn a mutable tag into an untracked update, including before the first
	// version has been staged.
	launch.PullPolicy = "never"
	launch.Command = config.EnsureVLLMSleepModeArgument(launch.Command, model.Backend.Type, model.UnloadAfter)

	bound := model
	bound.Cmd = ""
	bound.Backend = model.Backend
	bound.Backend.Arguments = config.EnsureVLLMSleepModeArgument(model.Backend.Arguments, model.Backend.Type, model.UnloadAfter)
	bound.Backend.Container = launch
	return bound, nil
}

func cloneRuntimeContainerConfig(input config.RuntimeContainerConfig) config.RuntimeContainerConfig {
	output := input
	output.Entrypoint = append([]string(nil), input.Entrypoint...)
	output.Command = append([]string(nil), input.Command...)
	if len(input.Env) > 0 {
		output.Env = make(map[string]string, len(input.Env))
		for key, value := range input.Env {
			output.Env[key] = value
		}
	}
	output.Mounts = append([]config.RuntimeMount(nil), input.Mounts...)
	output.Ports = append([]config.RuntimePort(nil), input.Ports...)
	return output
}

// overlayRuntimeContainer applies model-scoped launch details without allowing
// the model to replace the runtime's image provenance. Non-empty slices replace
// the runtime defaults; environment entries are overlaid by key.
func overlayRuntimeContainer(target *config.RuntimeContainerConfig, model config.RuntimeContainerConfig) {
	if target == nil {
		return
	}
	if model.Engine != "" {
		target.Engine = model.Engine
	}
	if model.Name != "" {
		target.Name = model.Name
	}
	if model.Platform != "" {
		target.Platform = model.Platform
	}
	if model.Entrypoint != nil {
		target.Entrypoint = append([]string(nil), model.Entrypoint...)
	}
	if model.Command != nil {
		target.Command = append([]string(nil), model.Command...)
	}
	if len(model.Env) > 0 {
		if target.Env == nil {
			target.Env = make(map[string]string, len(model.Env))
		}
		for key, value := range model.Env {
			target.Env[key] = value
		}
	}
	if model.Mounts != nil {
		target.Mounts = append([]config.RuntimeMount(nil), model.Mounts...)
	}
	if model.Ports != nil {
		target.Ports = append([]config.RuntimePort(nil), model.Ports...)
	}
	if model.GPUs != "" {
		target.GPUs = model.GPUs
	}
	if model.ShmSize != "" {
		target.ShmSize = model.ShmSize
	}
	if model.StopTimeout != 0 {
		target.StopTimeout = model.StopTimeout
	}
}

// prependManagedRuntimeLibraryPath keeps native managed runtimes independent
// of the daemon's host loader configuration. llama.cpp currently emits shared
// libraries in current/build/bin while the stable executable lives in
// current/bin; both paths are needed after activation. Existing operator
// values remain available after the managed directories, and the input slice
// is never mutated.
func prependManagedRuntimeLibraryPath(environment []string, libraryDirs []string) []string {
	if len(libraryDirs) == 0 {
		return append([]string(nil), environment...)
	}
	var existing string
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		key, value, hasValue := strings.Cut(entry, "=")
		if hasValue && strings.EqualFold(key, "LD_LIBRARY_PATH") {
			existing = value
			continue
		}
		result = append(result, entry)
	}
	paths := make([]string, 0, len(libraryDirs)+1)
	for _, directory := range libraryDirs {
		directory = filepath.Clean(strings.TrimSpace(directory))
		if directory == "." || directory == "" {
			continue
		}
		duplicate := false
		for _, prior := range paths {
			if prior == directory {
				duplicate = true
				break
			}
		}
		if !duplicate {
			paths = append(paths, directory)
		}
	}
	if existing != "" {
		paths = append(paths, existing)
	}
	if len(paths) > 0 {
		result = append(result, "LD_LIBRARY_PATH="+strings.Join(paths, string(os.PathListSeparator)))
	}
	return result
}

func configuredRuntimeMode(value config.RuntimeConfig) string {
	mode := strings.ToLower(strings.TrimSpace(value.Mode))
	if mode == "" && (strings.TrimSpace(value.Source.Image) != "" || strings.TrimSpace(value.Container.Image) != "") {
		return "container"
	}
	if mode == "" {
		return "native"
	}
	return mode
}

// prependManagedRuntimePath makes dependency lookups deterministic without
// relying on duplicate PATH entries in exec.Cmd. The first backend-provided
// PATH is retained as its fallback; otherwise inherit the daemon PATH. The
// managed bin directory always wins, and the caller's slice is never mutated.
// nvidiaLaunchDevices resolves persisted GPU selections (a card UUID, or an
// index written as "index:N" or "N") against the machine's accelerator
// inventory, keeping only the cards a CUDA runtime can actually see. A
// selection that does not resolve to an NVIDIA accelerator is dropped: on
// Apple or AMD hosts CUDA_VISIBLE_DEVICES has no effect, and a stale UUID
// from a different machine must not leak into the environment. A nil
// snapshot (hardware capture unavailable) keeps every selection verbatim.
func nvidiaLaunchDevices(hardware *hw.HardwareSnapshot, selections []string) []string {
	if hardware == nil {
		return selections
	}
	devices := make([]string, 0, len(selections))
	seen := make(map[int]bool, len(selections))
	for _, selection := range selections {
		trimmed := strings.TrimSpace(selection)
		if trimmed == "" {
			continue
		}
		accelerator := findLaunchAccelerator(hardware, trimmed)
		if accelerator == nil || accelerator.Vendor == nil || !strings.EqualFold(*accelerator.Vendor, "NVIDIA") {
			continue
		}
		if seen[accelerator.Index] {
			// The same card reached through both its UUID and its index:
			// keep the first form and drop the duplicate.
			continue
		}
		seen[accelerator.Index] = true
		if accelerator.UUID != nil && *accelerator.UUID == trimmed {
			devices = append(devices, trimmed)
			continue
		}
		devices = append(devices, strconv.Itoa(accelerator.Index))
	}
	return devices
}

func findLaunchAccelerator(hardware *hw.HardwareSnapshot, selection string) *hw.Accelerator {
	for index := range hardware.Accelerators {
		accelerator := &hardware.Accelerators[index]
		if accelerator.UUID != nil && *accelerator.UUID == selection {
			return accelerator
		}
	}
	if index, err := strconv.Atoi(strings.TrimPrefix(selection, "index:")); err == nil {
		for i := range hardware.Accelerators {
			if hardware.Accelerators[i].Index == index {
				return &hardware.Accelerators[i]
			}
		}
	}
	return nil
}

// appendManagedLaunchEnv merges the structured launch environment
// (CUDA_VISIBLE_DEVICES and friends) into the process environment, replacing
// any operator-written assignment of the same variable.
func appendManagedLaunchEnv(environment []string, launchEnv []string) []string {
	if len(launchEnv) == 0 {
		return environment
	}
	owned := make(map[string]bool, len(launchEnv))
	for _, assignment := range launchEnv {
		if equals := strings.Index(assignment, "="); equals > 0 {
			owned[assignment[:equals]] = true
		}
	}
	merged := make([]string, 0, len(environment)+len(launchEnv))
	for _, assignment := range environment {
		equals := strings.Index(assignment, "=")
		if !strings.Contains(assignment, "=") && owned["CUDA_VISIBLE_DEVICES"] && config.IsSafeAnonymousEnvironmentValue(assignment) {
			// A legacy YAML save may leave numeric CUDA device continuations
			// as bare environment entries. The structured CUDA assignment below
			// is authoritative; bare entries are not valid process variables.
			continue
		}
		if equals > 0 && owned[assignment[:equals]] {
			continue
		}
		merged = append(merged, assignment)
	}
	return append(merged, launchEnv...)
}

func prependManagedRuntimePath(environment []string, binDir string) []string {
	pathValue := os.Getenv("PATH")
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		key, value, hasValue := strings.Cut(entry, "=")
		if hasValue && strings.EqualFold(key, "PATH") {
			pathValue = value
			continue
		}
		result = append(result, entry)
	}
	binDir = filepath.Clean(binDir)
	if pathValue != "" {
		result = append(result, "PATH="+binDir+string(os.PathListSeparator)+pathValue)
	} else {
		result = append(result, "PATH="+binDir)
	}
	return result
}
