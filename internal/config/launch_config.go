package config

import (
	"fmt"
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

// launchspecReservedFlags exposes the per-kind reserved flag set for config
// validation.
func launchspecReservedFlags(kind string) map[string]bool {
	return launchspec.ReservedFlags(kind)
}

// MigrateLaunchBlocks upgrades every legacy args-only managed model to the
// structured launch block in place. Config validation and config-source
// loading both call it so the running server sees the same normalized
// configuration the validators accepted.
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
		migrateLaunchFromArguments(name, &model.Backend, kind, model.Env)
		// Older editor versions duplicated this setting in backend.launch.gpus.
		// Fold that legacy field into model.env and clear it so the selector has
		// one persisted source of truth going forward.
		model.Env = normalizeLaunchGPUEnvironment(model.Env, model.Backend.Launch)
		c.Models[name] = model
	}
}

// migrateLaunchFromArguments upgrades a legacy args-only managed model: the
// structured fields are extracted from the operator's command text and the
// args list is reduced to the unmanaged extras. Migration is skipped when it
// cannot be proven lossless (for example a ctx-size that does not divide
// evenly across parallel slots), keeping the legacy text untouched so the
// operator can confirm by hand.
func migrateLaunchFromArguments(modelName string, backend *BackendConfig, kind string, environment []string) {
	if backend == nil || backend.Launch != nil || len(backend.Arguments) == 0 {
		return
	}
	command := strings.Join(backend.Arguments, "\n")
	parsed, err := launchspec.Parse(kind, command)
	if err != nil {
		return
	}
	// A warning means semantics were ambiguous; keep the legacy text whole.
	if len(parsed.Warnings) > 0 {
		return
	}
	launch := &ModelLaunchConfig{
		Model:                parsed.Model,
		ServedModelName:      parsed.ServedModelName,
		ContextPerRequest:    parsed.ContextPerRequest,
		MaxConcurrency:       parsed.MaxConcurrency,
		GPUMemoryUtilization: parsed.GPUMemoryUtilization,
		TensorParallelSize:   parsed.TensorParallelSize,
		GPUs:                 parsed.CUDAVisibleDevices,
	}
	if len(launch.GPUs) == 0 {
		// CUDA_VISIBLE_DEVICES is commonly declared in the model environment,
		// not inline with backend.args. Preserve that explicit operator choice
		// before falling back to inferred 0..N tensor-parallel ordinals.
		launch.GPUs = CUDAVisibleDevicesFromEnvironment(environment)
	}
	if parsed.TensorParallelSize > 0 {
		// Tensor parallelism is derived from the GPU selection at start; a
		// pasted explicit value only seeds the GPU count when GPUs are unset.
		if len(launch.GPUs) == 0 && parsed.TensorParallelSize > 1 {
			for index := 0; index < parsed.TensorParallelSize; index++ {
				launch.GPUs = append(launch.GPUs, fmt.Sprintf("%d", index))
			}
		}
	}
	if launchEmpty(launch) {
		return
	}
	backend.Launch = launch
	backend.Arguments = parsed.ExtraArgs
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

// launchEmpty reports a structured block that would add no managed fields.
func launchEmpty(launch *ModelLaunchConfig) bool {
	return launch.Model == "" && launch.ServedModelName == "" &&
		launch.ContextPerRequest == 0 && launch.MaxConcurrency == 0 &&
		launch.GPUMemoryUtilization == nil && launch.TensorParallelSize == 0 &&
		len(launch.GPUs) == 0
}
