package config

import (
	"reflect"
	"testing"
)

// Whole-line argument entries ("--flag value" as one list item, the hand-edited
// config form) must be split into paired tokens with the model target hoisted
// into the canonical launch prefix.
func TestBuildManagedLaunchArgumentsConsumesWholeLineManagedPairs(t *testing.T) {
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

	got, _, err := BuildManagedLaunchArguments(args, "", "vllm", "http://127.0.0.1:5801")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"vllm", "serve", "--model", "/models/Qwen3.8-27B-FP8",
		"--host", "127.0.0.1", "--port", "5801",
		"--served-model-name", "Qwen/Qwen3.8-27B-FP8",
		"--trust-remote-code",
		"--tensor-parallel-size", "4", "--dtype", "float16",
		"--gpu-memory-utilization", "0.8",
		"--max-model-len", "262144", "--max-num-seqs", "2",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %v, want %v", got, want)
	}
}

// An operator-written tensor-parallel-size must stay a paired flag: its value
// may never be emitted as a bare positional token after the launch prefix.
func TestBuildManagedLaunchArgumentsKeepsTensorParallelPairing(t *testing.T) {
	got, _, err := BuildManagedLaunchArguments([]string{
		"--model /models/qwen",
		"--tensor-parallel-size 4",
		"--max-num-seqs 2",
	}, "", "vllm", "http://127.0.0.1:5801")
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

// Operator-written host/port duplicates are dropped: the bind address belongs
// to the model's resolved proxy, even when the default head is used.
func TestBuildManagedLaunchArgumentsDropsOperatorBindAddress(t *testing.T) {
	got, _, err := BuildManagedLaunchArguments([]string{
		"--model", "/models/qwen",
		"--host", "0.0.0.0", "--port", "1234",
	}, "", "vllm", "http://127.0.0.1:5801")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"vllm", "serve", "--model", "/models/qwen", "--host", "127.0.0.1", "--port", "5801"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %v, want %v", got, want)
	}
}
