package config

import (
	"strings"

	"github.com/mostlygeek/llama-swap/internal/launchspec"
)

// ModelLaunchConfig is the structured, basic-UI-owned subset of a managed
// engine launch. It is the source of truth for the fields most users set;
// backend.args keeps only the unmanaged "extra" engine arguments. GPU
// selection deliberately remains in the model environment as
// CUDA_VISIBLE_DEVICES: it is the shared, single persistent source for the
// card picker and hand-written launch configuration.
type ModelLaunchConfig struct {
	Model                string   `yaml:"model,omitempty" json:"model,omitempty"`
	ServedModelName      string   `yaml:"servedModelName,omitempty" json:"servedModelName,omitempty"`
	ContextPerRequest    int      `yaml:"contextPerRequest,omitempty" json:"contextPerRequest,omitempty"`
	MaxConcurrency       int      `yaml:"maxConcurrency,omitempty" json:"maxConcurrency,omitempty"`
	GPUs                 []string `yaml:"gpus,omitempty" json:"gpus,omitempty"`
	GPUMemoryUtilization *float64 `yaml:"gpuMemoryUtilization,omitempty" json:"gpuMemoryUtilization,omitempty"`
	TensorParallelSize   int      `yaml:"tensorParallelSize,omitempty" json:"tensorParallelSize,omitempty"`
}

// LaunchManagedModel reports whether the launch block, not backend.args,
// owns the model target. It is safe on a nil receiver.
func (l *ModelLaunchConfig) LaunchManagedModel() bool {
	return l != nil && strings.TrimSpace(l.Model) != ""
}

// Spec converts the structured block into a launchspec for argv building.
func (l *ModelLaunchConfig) Spec() launchspec.Spec {
	spec := launchspec.Spec{}
	if l == nil {
		return spec
	}
	spec.Model = l.Model
	spec.ServedModelName = l.ServedModelName
	spec.ContextPerRequest = l.ContextPerRequest
	spec.MaxConcurrency = l.MaxConcurrency
	spec.GPUMemoryUtilization = l.GPUMemoryUtilization
	spec.TensorParallelSize = l.TensorParallelSize
	spec.CUDAVisibleDevices = l.GPUs
	return spec
}

// MigrateLaunchBlocks folds every structured launch block into backend.args
// so the command text is the single source of truth for a managed model. The
// structured fields (model target, context, concurrency, tensor parallelism,
// memory utilization, served name) are rendered into the exact flags the
// launch builder would have injected, and the GPU selector is normalized into
// model.env. Config validation and config-source loading both call it so the
// running server sees the same normalized configuration the validators
// accepted. The launch block itself is cleared: nothing about the numeric
// fields is stored twice.
func (c Config) MigrateLaunchBlocks() {
	for name, model := range c.Models {
		runtimeName := strings.TrimSpace(model.Backend.Runtime)
		if runtimeName == "" {
			continue
		}
		runtimeConfig, found := c.Runtimes[runtimeName]
		if !found || effectiveRuntimeMode(runtimeConfig) != "native" {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(runtimeConfig.Kind))
		if kind != "vllm" && kind != "llamacpp" {
			continue
		}
		if model.Backend.Launch == nil {
			continue
		}
		// A custom launch head owns its entire command line: folding engine
		// flags into its args would feed flags the head does not understand.
		if strings.TrimSpace(model.Backend.EntryPoint) != "" {
			model.Backend.Launch = nil
			c.Models[name] = model
			continue
		}
		NormalizeModelLaunch(&model, kind)
		c.Models[name] = model
	}
}

// foldLaunchIntoArguments renders a structured launch block into explicit
// engine flags appended to the operator's arguments. With a launch block
// present the validator guarantees args carry none of the reserved flags, so
// rendering the structured values after them is lossless and keeps the
// operator's ordering. Tensor parallelism keeps its previous launch-time
// behavior: an unset value over a multi-GPU selection is materialized as the
// device count so the resulting command reproduces the old argv.
func foldLaunchIntoArguments(kind string, launch *ModelLaunchConfig, args []string, environment []string) []string {
	if launch == nil {
		return args
	}
	spec := launch.Spec()
	if kind == "vllm" && spec.TensorParallelSize == 0 && len(spec.CUDAVisibleDevices) > 1 {
		spec.TensorParallelSize = len(spec.CUDAVisibleDevices)
	}
	if kind == "vllm" && spec.TensorParallelSize == 0 {
		if devices := CUDAVisibleDevicesFromEnvironment(environment); len(devices) > 1 {
			spec.TensorParallelSize = len(devices)
		}
	}
	rendered := spec.BuildArgs(kind, false)
	if len(rendered) == 0 {
		return args
	}
	// An operator's own flag always wins: a legacy config can carry both the
	// structured field and the flag it renders, and appending a second copy
	// would produce a duplicate the engine reads unpredictably.
	present := make(map[string]bool, len(args))
	for _, token := range SplitManagedLaunchTokens(args) {
		if name := flagName(token); name != "" {
			present[name] = true
		}
	}
	folded := make([]string, 0, len(args)+len(rendered))
	folded = append(folded, args...)
	for index := 0; index < len(rendered); index++ {
		token := rendered[index]
		name := flagName(token)
		if name == "" || present[name] {
			continue
		}
		if strings.Contains(token, "=") {
			folded = append(folded, token)
			continue
		}
		// Keep the flag/value pair together.
		if index+1 < len(rendered) {
			folded = append(folded, token, rendered[index+1])
			index++
			continue
		}
		folded = append(folded, token)
	}
	return folded
}

// flagName returns the flag a token spells, or "" for a positional value. Both
// the paired ("--flag") and inline ("--flag=value") forms are recognized.
func flagName(token string) string {
	trimmed := strings.TrimSpace(token)
	if !strings.HasPrefix(trimmed, "-") {
		return ""
	}
	if equals := strings.Index(trimmed, "="); equals > 0 {
		return trimmed[:equals]
	}
	return trimmed
}

// CUDAVisibleDevicesFromEnvironment reads the portable CUDA selector from a
// model environment. The runtime binder uses it to build a transient launch
// plan without creating a second stored GPU selector.
func CUDAVisibleDevicesFromEnvironment(environment []string) []string {
	for index, assignment := range environment {
		name, value, found := strings.Cut(assignment, "=")
		if !found || strings.TrimSpace(name) != "CUDA_VISIBLE_DEVICES" {
			continue
		}
		devices := make([]string, 0)
		for _, device := range strings.Split(value, ",") {
			if device = strings.TrimSpace(device); device != "" {
				devices = append(devices, device)
			}
		}
		// Older UI saves could let YAML coerce a comma-separated value into
		// one assignment followed by bare numeric scalars (1, 2, 3, 4).
		// Treat only immediately adjacent numeric scalars as continuations;
		// unrelated bare entries remain untouched and are not guessed at.
		for next := index + 1; next < len(environment); next++ {
			if !IsSafeAnonymousEnvironmentValue(environment[next]) {
				break
			}
			for _, device := range strings.Split(strings.TrimSpace(environment[next]), ",") {
				if device != "" {
					devices = append(devices, device)
				}
			}
		}
		return devices
	}
	return nil
}

func normalizeLaunchGPUEnvironment(environment []string, launch *ModelLaunchConfig) []string {
	if launch == nil {
		return environment
	}
	devices := CUDAVisibleDevicesFromEnvironment(environment)
	if len(devices) == 0 {
		devices = append(devices, launch.GPUs...)
	}
	launch.GPUs = nil
	if len(devices) == 0 {
		return environment
	}

	// Keep unrelated values in their original order and replace the first CUDA
	// setting in place. PCI ordering makes numeric IDs match nvidia-smi across
	// restarts without moving unrelated environment variables.
	normalized := make([]string, 0, len(environment)+2)
	continuations := cudaVisibleDeviceContinuationIndexes(environment)
	inserted := false
	for index, assignment := range environment {
		if _, continuation := continuations[index]; continuation {
			continue
		}
		name, _, found := strings.Cut(assignment, "=")
		if found && (strings.TrimSpace(name) == "CUDA_VISIBLE_DEVICES" || strings.TrimSpace(name) == "CUDA_DEVICE_ORDER") {
			if !inserted {
				normalized = append(normalized,
					"CUDA_DEVICE_ORDER=PCI_BUS_ID",
					"CUDA_VISIBLE_DEVICES="+strings.Join(devices, ","),
				)
				inserted = true
			}
			continue
		}
		normalized = append(normalized, assignment)
	}
	if !inserted {
		normalized = append(normalized,
			"CUDA_DEVICE_ORDER=PCI_BUS_ID",
			"CUDA_VISIBLE_DEVICES="+strings.Join(devices, ","),
		)
	}
	return normalized
}

func cudaVisibleDeviceContinuationIndexes(environment []string) map[int]struct{} {
	continuations := make(map[int]struct{})
	for index, assignment := range environment {
		name, _, found := strings.Cut(assignment, "=")
		if !found || strings.TrimSpace(name) != "CUDA_VISIBLE_DEVICES" {
			continue
		}
		for next := index + 1; next < len(environment); next++ {
			if !IsSafeAnonymousEnvironmentValue(environment[next]) {
				break
			}
			continuations[next] = struct{}{}
		}
		break
	}
	return continuations
}

// NormalizeModelLaunch folds a legacy structured launch block into explicit
// engine flags and clears it, leaving backend.args as the single source of
// truth for the command line. It is idempotent: a model without a launch block
// is untouched. The loader calls it for every managed model, and the launch
// binder calls it again so programmatically constructed configs (tests,
// embedders) behave exactly like loaded ones.
func NormalizeModelLaunch(model *ModelConfig, kind string) {
	selections := NormalizeModelLaunchFlags(model, kind)
	if len(selections) == 0 {
		return
	}
	// GPU selection is the sole persisted card choice; fold it into the
	// environment. No hardware snapshot is available on this path, so the
	// vendor filter is the launch binder's job.
	model.Env = normalizeLaunchGPUEnvironment(model.Env, &ModelLaunchConfig{GPUs: selections})
}

// NormalizeModelLaunchFlags folds a legacy launch block into explicit engine
// flags and clears it, returning the block's GPU selections so the caller can
// apply its own vendor filtering before writing CUDA environment. It is
// idempotent: a model without a launch block yields nil and is untouched.
func NormalizeModelLaunchFlags(model *ModelConfig, kind string) []string {
	if model == nil || model.Backend.Launch == nil {
		return nil
	}
	launch := model.Backend.Launch
	selections := append([]string(nil), launch.GPUs...)
	model.Backend.Arguments = foldLaunchIntoArguments(kind, launch, model.Backend.Arguments, model.Env)
	model.Backend.Launch = nil
	return selections
}

// HasTensorParallelFlag reports whether an argument list already pins the
// tensor parallel size, in either the paired or the inline form.
func HasTensorParallelFlag(args []string) bool {
	for _, token := range args {
		if token == "--tensor-parallel-size" || token == "-tp" {
			return true
		}
		if strings.HasPrefix(token, "--tensor-parallel-size=") || strings.HasPrefix(token, "-tp=") {
			return true
		}
	}
	return false
}
