package config

import (
	"reflect"
	"testing"
)

func TestBuildManagedLaunchArgumentsConsumesWholeLineManagedPairs(t *testing.T) {
	utilization := 0.8
	launch := &ModelLaunchConfig{
		ServedModelName:      "Qwen/Qwen3.8-27B-FP8",
		ContextPerRequest:    262144,
		MaxConcurrency:       2,
		GPUMemoryUtilization: &utilization,
	}
	args := []string{
		"--model /models/Qwen3.8-27B-FP8",
		"--served-model-name Qwen/Qwen3.8-27B-FP8",
		"--trust-remote-code",
		"--tensor-parallel-size 4",
		"--dtype float16",
		"--gpu-memory-utilization 0.8",
		"--max-model-len 262144",
		"--max-num-seqs 2",
	}

	got, _, err := BuildManagedLaunchArguments(args, launch, "vllm", "http://127.0.0.1:5801")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"vllm", "serve", "--model", "/models/Qwen3.8-27B-FP8",
		"--host", "127.0.0.1", "--port", "5801",
		"--served-model-name", "Qwen/Qwen3.8-27B-FP8",
		"--max-model-len", "262144",
		"--max-num-seqs", "2",
		"--tensor-parallel-size", "4",
		"--gpu-memory-utilization", "0.8",
		"--trust-remote-code", "--dtype", "float16",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %v, want %v", got, want)
	}
}

func TestBuildManagedLaunchArgumentsRecoversExplicitTensorParallelSize(t *testing.T) {
	launch := &ModelLaunchConfig{Model: "/models/qwen", ContextPerRequest: 32768}
	got, _, err := BuildManagedLaunchArguments([]string{
		"--tensor-parallel-size 4",
		"--max-num-seqs 2",
	}, launch, "vllm", "http://127.0.0.1:5801")
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < len(got); index++ {
		if got[index] == "--tensor-parallel-size" {
			if index+1 >= len(got) || got[index+1] != "4" {
				t.Fatalf("tensor parallel flag was not paired: %v", got)
			}
			return
		}
	}
	t.Fatalf("tensor parallel flag was lost: %v", got)
}
