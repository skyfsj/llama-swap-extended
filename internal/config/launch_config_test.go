package config

import (
	"reflect"
	"slices"
	"testing"
)

// foldLaunchConfig builds a config with a legacy structured launch block and
// runs the normalization the loader applies.
func foldLaunchConfig(t *testing.T, backend BackendConfig, environment []string) ModelConfig {
	t.Helper()
	cfg := Config{
		Runtimes: map[string]RuntimeConfig{"vllm-1cat": {Kind: "vllm", Mode: "native"}},
		Models: map[string]ModelConfig{
			"qwen": {Backend: backend, Env: environment},
		},
	}
	cfg.MigrateLaunchBlocks()
	return cfg.Models["qwen"]
}

func TestLaunchConfig_MigrateLaunchBlocksFoldsStructuredFieldsIntoArguments(t *testing.T) {
	utilization := 0.9
	model := foldLaunchConfig(t, BackendConfig{
		Runtime: "vllm-1cat",
		Arguments: []string{
			"--trust-remote-code",
		},
		Launch: &ModelLaunchConfig{
			Model:                "/models/qwen",
			ServedModelName:      "Qwen/Qwen3.8-27B-FP8",
			ContextPerRequest:    327680,
			MaxConcurrency:       1,
			TensorParallelSize:   4,
			GPUMemoryUtilization: &utilization,
		},
	}, []string{
		"CUDA_DEVICE_ORDER=PCI_BUS_ID",
		"CUDA_VISIBLE_DEVICES=1,2,3,4",
	})

	if model.Backend.Launch != nil {
		t.Fatalf("launch block was retained after migration: %+v", model.Backend.Launch)
	}
	want := []string{
		"--trust-remote-code",
		"--model", "/models/qwen",
		"--served-model-name", "Qwen/Qwen3.8-27B-FP8",
		"--max-model-len", "327680",
		"--max-num-seqs", "1",
		"--tensor-parallel-size", "4",
		"--gpu-memory-utilization", "0.9",
	}
	if !reflect.DeepEqual(model.Backend.Arguments, want) {
		t.Fatalf("arguments = %v, want %v", model.Backend.Arguments, want)
	}
	if got, wantEnv := model.Env, []string{"CUDA_DEVICE_ORDER=PCI_BUS_ID", "CUDA_VISIBLE_DEVICES=1,2,3,4"}; !reflect.DeepEqual(got, wantEnv) {
		t.Fatalf("environment = %v, want %v", got, wantEnv)
	}
}

func TestLaunchConfig_MigrateLaunchBlocksMaterializesTensorParallelFromSelection(t *testing.T) {
	model := foldLaunchConfig(t, BackendConfig{
		Runtime:   "vllm-1cat",
		Arguments: []string{"--trust-remote-code"},
		Launch: &ModelLaunchConfig{
			Model: "/models/qwen",
		},
	}, []string{
		"CUDA_VISIBLE_DEVICES=1,2,3,4",
	})

	if !slices.Contains(model.Backend.Arguments, "--tensor-parallel-size") {
		t.Fatalf("tensor parallel size was not materialized: %v", model.Backend.Arguments)
	}
	if got := model.Backend.Arguments[len(model.Backend.Arguments)-1]; got != "4" {
		t.Fatalf("tensor parallel size = %q, want %q", got, "4")
	}
}

func TestLaunchConfig_MigrateLaunchBlocksLeavesArgumentsOnlyModelsAlone(t *testing.T) {
	args := []string{"--model", "/models/qwen", "--max-model-len", "8192"}
	model := foldLaunchConfig(t, BackendConfig{Runtime: "vllm-1cat", Arguments: args}, nil)
	if !reflect.DeepEqual(model.Backend.Arguments, args) {
		t.Fatalf("arguments changed: %v", model.Backend.Arguments)
	}
	if model.Backend.Launch != nil {
		t.Fatalf("launch block created from args-only model: %+v", model.Backend.Launch)
	}
}

func TestLaunchConfig_NormalizeLaunchGPUEnvironmentKeepsEnvironmentAsSource(t *testing.T) {
	launch := &ModelLaunchConfig{GPUs: []string{"0", "1"}}
	environment := normalizeLaunchGPUEnvironment([]string{
		"KEEP=value",
		"CUDA_VISIBLE_DEVICES=1,2,3,4",
	}, launch)

	if len(launch.GPUs) != 0 {
		t.Fatalf("legacy launch GPUs were retained: %v", launch.GPUs)
	}
	want := []string{
		"KEEP=value",
		"CUDA_DEVICE_ORDER=PCI_BUS_ID",
		"CUDA_VISIBLE_DEVICES=1,2,3,4",
	}
	if !reflect.DeepEqual(environment, want) {
		t.Fatalf("environment = %v, want %v", environment, want)
	}
}

func TestLaunchConfig_CUDAVisibleDevicesFromEnvironmentRecoversYAMLNumericContinuations(t *testing.T) {
	environment := []string{
		"CUDA_DEVICE_ORDER=PCI_BUS_ID",
		"CUDA_VISIBLE_DEVICES=1",
		"2",
		"3",
		"4",
		"NCCL_P2P_LEVEL=PHB",
	}
	if got, want := CUDAVisibleDevicesFromEnvironment(environment), []string{"1", "2", "3", "4"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("GPU selection = %v, want %v", got, want)
	}
	launch := &ModelLaunchConfig{}
	got := normalizeLaunchGPUEnvironment(environment, launch)
	want := []string{
		"CUDA_DEVICE_ORDER=PCI_BUS_ID",
		"CUDA_VISIBLE_DEVICES=1,2,3,4",
		"NCCL_P2P_LEVEL=PHB",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized environment = %v, want %v", got, want)
	}
}

// A custom launch head runs verbatim with the operator's own port macro: the
// builder must not inject engine flags that the head knows nothing about.
func TestBuildManagedLaunchArguments_CustomEntryPointRunsVerbatim(t *testing.T) {
	argv, env, err := BuildManagedLaunchArguments(
		[]string{"--port 5811", "--model /models/Qwen/Qwen3-ASR-1.7B"},
		"python3 /var/lib/llama-swap/scripts/qwen-asr-server.py",
		"vllm",
		"http://127.0.0.1:5811",
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// The operator owns the whole line for a custom head: the port that
	// carries the allocated ${PORT} must reach the server untouched, and no
	// engine flags may be injected.
	want := []string{
		"python3", "/var/lib/llama-swap/scripts/qwen-asr-server.py",
		"--port", "5811", "--model", "/models/Qwen/Qwen3-ASR-1.7B",
	}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
	if len(env) != 0 {
		t.Fatalf("env = %v, want none", env)
	}
}

// The default head keeps requiring a model target and injecting the proxy bind
// address, since that grammar is owned by the runtime kind.
func TestBuildManagedLaunchArguments_DefaultHeadRequiresModel(t *testing.T) {
	if _, _, err := BuildManagedLaunchArguments([]string{"--trust-remote-code"}, "", "vllm", "http://127.0.0.1:9000"); err == nil {
		t.Fatal("expected an error when the args carry no model target")
	}
	argv, _, err := BuildManagedLaunchArguments([]string{"--model", "/models/qwen"}, "", "vllm", "http://127.0.0.1:9000")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	want := []string{"vllm", "serve", "--model", "/models/qwen", "--host", "127.0.0.1", "--port", "9000"}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
}

// A legacy config can carry both the structured field and the flag it renders
// (the production shape): folding must keep the operator's own flag and never
// append a duplicate pair.
func TestLaunchConfig_MigrateLaunchBlocksDoesNotDuplicateOperatorFlags(t *testing.T) {
	utilization := 0.9
	model := foldLaunchConfig(t, BackendConfig{
		Runtime: "vllm-1cat",
		Arguments: []string{
			"--model", "/models/qwen",
			"--max-model-len", "327680",
			"--gpu-memory-utilization", "0.9",
			"--trust-remote-code",
		},
		Launch: &ModelLaunchConfig{
			ServedModelName:      "Qwen/Qwen3.8-27B-FP8",
			ContextPerRequest:    327680,
			MaxConcurrency:       1,
			TensorParallelSize:   4,
			GPUMemoryUtilization: &utilization,
		},
	}, nil)

	want := []string{
		"--model", "/models/qwen",
		"--max-model-len", "327680",
		"--gpu-memory-utilization", "0.9",
		"--trust-remote-code",
		"--served-model-name", "Qwen/Qwen3.8-27B-FP8",
		"--max-num-seqs", "1",
		"--tensor-parallel-size", "4",
	}
	if !reflect.DeepEqual(model.Backend.Arguments, want) {
		t.Fatalf("arguments = %v, want %v", model.Backend.Arguments, want)
	}
}
