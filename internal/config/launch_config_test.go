package config

import (
	"reflect"
	"testing"
)

func TestLaunchConfig_MigrateLaunchBlocks_PreservesEnvironmentGPUSelection(t *testing.T) {
	backend := BackendConfig{Arguments: []string{
		"vllm", "serve", "--model", "/models/qwen", "--tensor-parallel-size", "4",
	}}
	migrateLaunchFromArguments("qwen", &backend, "vllm", []string{
		"CUDA_DEVICE_ORDER=PCI_BUS_ID",
		"CUDA_VISIBLE_DEVICES=1,2,3,4",
	})

	if backend.Launch == nil {
		t.Fatal("launch was not migrated")
	}
	if got, want := backend.Launch.GPUs, []string{"1", "2", "3", "4"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("GPUs = %v, want %v", got, want)
	}
	if got, want := backend.Launch.TensorParallelSize, 4; got != want {
		t.Fatalf("tensor parallel size = %d, want %d", got, want)
	}
}

func TestLaunchConfig_MigrateLaunchBlocks_InfersGPUSelectionWithoutEnvironment(t *testing.T) {
	backend := BackendConfig{Arguments: []string{
		"vllm", "serve", "--model", "/models/qwen", "--tensor-parallel-size", "4",
	}}
	migrateLaunchFromArguments("qwen", &backend, "vllm", nil)

	if backend.Launch == nil {
		t.Fatal("launch was not migrated")
	}
	if got, want := backend.Launch.GPUs, []string{"0", "1", "2", "3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("GPUs = %v, want %v", got, want)
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
