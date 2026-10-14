package config

import (
	"reflect"
	"testing"
)

func runtimeMetaConfig(metadata RuntimeMetadataConfig) (ModelConfig, RuntimeConfig) {
	model := ModelConfig{
		Backend: BackendConfig{
			Type:      "vllm",
			Runtime:   "rt",
			Arguments: []string{"--model", "/models/qwen"},
		},
	}
	runtime := RuntimeConfig{Kind: "vllm", Mode: "native", Metadata: metadata}
	return model, runtime
}

func TestEffectiveLaunchMetadata_ModelEntryPointWinsOverRuntime(t *testing.T) {
	model, runtime := runtimeMetaConfig(RuntimeMetadataConfig{EntryPoint: "runtime-head"})
	model.Backend.EntryPoint = "model-head"
	head, defaults := EffectiveLaunchMetadata(model, runtime, "vllm")
	if head != "model-head" {
		t.Fatalf("head = %q, want the model override", head)
	}
	if len(defaults) != 0 {
		t.Fatalf("defaults = %v, want none for a custom head", defaults)
	}
}

func TestEffectiveLaunchMetadata_RuntimeEntryPointApplies(t *testing.T) {
	model, runtime := runtimeMetaConfig(RuntimeMetadataConfig{
		EntryPoint:        "python3 /scripts/server.py",
		ContextPerRequest: 32768,
	})
	head, defaults := EffectiveLaunchMetadata(model, runtime, "vllm")
	if head != "python3 /scripts/server.py" {
		t.Fatalf("head = %q", head)
	}
	// A custom head owns its command line: no engine flags may be injected.
	if len(defaults) != 0 {
		t.Fatalf("defaults = %v, want none", defaults)
	}
}

func TestEffectiveLaunchMetadata_RendersDefaultsSkippingPinnedFlags(t *testing.T) {
	utilization := 0.9
	model, runtime := runtimeMetaConfig(RuntimeMetadataConfig{
		ContextPerRequest:    32768,
		MaxConcurrency:       2,
		TensorParallelSize:   4,
		GPUMemoryUtilization: &utilization,
	})
	model.Backend.Arguments = []string{"--model", "/models/qwen", "--max-num-seqs", "8"}
	head, defaults := EffectiveLaunchMetadata(model, runtime, "vllm")
	if head != "" {
		t.Fatalf("head = %q, want the kind default", head)
	}
	want := []string{"--max-model-len", "32768", "--tensor-parallel-size", "4", "--gpu-memory-utilization", "0.9"}
	if !reflect.DeepEqual(defaults, want) {
		t.Fatalf("defaults = %v, want %v (pinned --max-num-seqs must be skipped)", defaults, want)
	}
}

func TestRuntimeMetadataConfig_Validate(t *testing.T) {
	tooBig := 1.5
	if err := (RuntimeMetadataConfig{GPUMemoryUtilization: &tooBig}).Validate("rt", "native"); err == nil {
		t.Fatal("utilization > 1 must be rejected")
	}
	if err := (RuntimeMetadataConfig{EntryPoint: "x"}).Validate("rt", "container"); err == nil {
		t.Fatal("entrypoint on a container runtime must be rejected")
	}
	if err := (RuntimeMetadataConfig{ContextPerRequest: -1}).Validate("rt", "native"); err == nil {
		t.Fatal("negative context must be rejected")
	}
	if err := (RuntimeMetadataConfig{MaxConcurrency: 2}).Validate("rt", "native"); err != nil {
		t.Fatalf("valid metadata rejected: %v", err)
	}
}
