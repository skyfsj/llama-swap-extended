package launchspec

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func TestLaunchSpec_ParseVLLMFullCommand(t *testing.T) {
	command := strings.Join([]string{
		`CUDA_VISIBLE_DEVICES=0,1,2,3 \`,
		`python -m vllm.entrypoints.openai.api_server \`,
		`  --model /models/qwen \`,
		`  --served-model-name Qwen/Test \`,
		`  --max-model-len 262144 \`,
		`  --max-num-seqs 2 \`,
		`  --gpu-memory-utilization 0.9 \`,
		`  --enable-prefix-caching`,
	}, "\n")

	spec, err := Parse("vllm", command)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Model != "/models/qwen" || spec.ServedModelName != "Qwen/Test" {
		t.Fatalf("model=%q served=%q", spec.Model, spec.ServedModelName)
	}
	if spec.ContextPerRequest != 262144 || spec.MaxConcurrency != 2 {
		t.Fatalf("ctx=%d concurrency=%d", spec.ContextPerRequest, spec.MaxConcurrency)
	}
	if spec.GPUMemoryUtilization == nil || *spec.GPUMemoryUtilization != 0.9 {
		t.Fatalf("gpu-mem=%v", spec.GPUMemoryUtilization)
	}
	if strings.Join(spec.CUDAVisibleDevices, ",") != "0,1,2,3" {
		t.Fatalf("gpus=%v", spec.CUDAVisibleDevices)
	}
	if strings.Join(spec.ExtraArgs, " ") != "--enable-prefix-caching" {
		t.Fatalf("extras=%v", spec.ExtraArgs)
	}
}

func TestLaunchSpec_ParseVLLMServeEqualsAndSpaces(t *testing.T) {
	spec, err := Parse("vllm", `vllm serve "/models/Qwen 27B" --max-model-len=262144 --max-num-seqs=2 -tp 4`)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Model != "/models/Qwen 27B" {
		t.Fatalf("model=%q", spec.Model)
	}
	if spec.ContextPerRequest != 262144 || spec.MaxConcurrency != 2 || spec.TensorParallelSize != 4 {
		t.Fatalf("ctx=%d conc=%d tp=%d", spec.ContextPerRequest, spec.MaxConcurrency, spec.TensorParallelSize)
	}
	if len(spec.ExtraArgs) != 0 {
		t.Fatalf("extras=%v", spec.ExtraArgs)
	}
}

func TestLaunchSpec_ParseTokensKeepsSpacedModelPathAndManagedPairs(t *testing.T) {
	spec, err := ParseTokens("vllm", []string{
		"vllm", "serve", "--model-path", "/models/Qwen 27B", "--tensor-parallel-size", "4",
	})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Model != "/models/Qwen 27B" || spec.TensorParallelSize != 4 {
		t.Fatalf("model=%q tp=%d", spec.Model, spec.TensorParallelSize)
	}
	if len(spec.ExtraArgs) != 0 {
		t.Fatalf("managed args leaked into extras: %v", spec.ExtraArgs)
	}
}

func TestLaunchSpec_ParseTokensStripsAbsoluteLauncherPath(t *testing.T) {
	spec, err := ParseTokens("vllm", []string{
		"/opt/venv/bin/vllm", "serve", "--model", "/models/qwen",
	})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Model != "/models/qwen" || len(spec.ExtraArgs) != 0 {
		t.Fatalf("model=%q extras=%v", spec.Model, spec.ExtraArgs)
	}
}

func TestLaunchSpec_ParseLlamaLegacyCtxDivision(t *testing.T) {
	spec, err := Parse("llamacpp", "./llama-server -m /models/qwen.gguf -c 524288 -np 2 --cache-type-k q8_0")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Model != "/models/qwen.gguf" || spec.ContextPerRequest != 262144 || spec.MaxConcurrency != 2 {
		t.Fatalf("model=%q ctx=%d conc=%d", spec.Model, spec.ContextPerRequest, spec.MaxConcurrency)
	}
	if strings.Join(spec.ExtraArgs, " ") != "--cache-type-k q8_0" {
		t.Fatalf("extras=%v", spec.ExtraArgs)
	}
}

func TestLaunchSpec_ParseLlamaUnevenCtxKeepsWarning(t *testing.T) {
	spec, err := Parse("llamacpp", "llama-server -m /m.gguf --ctx-size 500000 --parallel 3")
	if err != nil {
		t.Fatal(err)
	}
	if spec.ContextPerRequest != 500000 {
		t.Fatalf("ctx=%d", spec.ContextPerRequest)
	}
	if len(spec.Warnings) == 0 {
		t.Fatal("expected a division warning")
	}
}

func TestLaunchSpec_ParseLlamaKVUnified(t *testing.T) {
	spec, err := Parse("llamacpp", "llama-server --model /models/qwen.gguf --parallel 2 --kv-unified-per-slot 262144")
	if err != nil {
		t.Fatal(err)
	}
	if spec.ContextPerRequest != 262144 || spec.MaxConcurrency != 2 {
		t.Fatalf("ctx=%d conc=%d", spec.ContextPerRequest, spec.MaxConcurrency)
	}
}

func TestLaunchSpec_UnknownArgsPreserved(t *testing.T) {
	spec, err := Parse("vllm", "vllm serve /models/qwen --future-vllm-feature foo --enable-prefix-caching")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(spec.ExtraArgs, " ")
	if !strings.Contains(joined, "--future-vllm-feature foo") {
		t.Fatalf("extras lost unknown flag: %v", spec.ExtraArgs)
	}
}

func TestLaunchSpec_QuotedPathIsOneValue(t *testing.T) {
	spec, err := Parse("vllm", `vllm serve /models/qwen --chat-template-file "/models/templates/a b.jinja"`)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(spec.ExtraArgs, "\n")
	if !strings.Contains(joined, "/models/templates/a b.jinja") {
		t.Fatalf("quoted value split: %v", spec.ExtraArgs)
	}
}

// A doubled ” is what terminals and chat transcripts leave behind when
// single quotes are escaped. The stray quote may only affect its own line:
// the following flags must still parse as separate arguments.
func TestLaunchSpec_StrayQuoteDoesNotSwallowLines(t *testing.T) {
	command := strings.Join([]string{
		"python",
		"-m",
		"vllm.entrypoints.openai.api_server",
		"--model /models/Qwen3.8-27B-FP8",
		`--default-chat-template-kwargs '{"enable_thinking":true,"reasoning_effort":"xhigh"}''`,
		"--reasoning-parser qwen3",
		`--speculative-config '{"method":"dflash","num_speculative_tokens":7}''`,
		"--limit-mm-per-prompt",
		`'{"image":999,"video":999}'`,
		"--numa-bind",
		"--no-enable-log-requests",
	}, "\n")

	spec, err := Parse("vllm", command)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Model != "/models/Qwen3.8-27B-FP8" {
		t.Fatalf("model=%q", spec.Model)
	}
	for _, want := range []string{
		`--default-chat-template-kwargs`, `{"enable_thinking":true,"reasoning_effort":"xhigh"}`,
		"--reasoning-parser", "qwen3",
		`--speculative-config`, `{"method":"dflash","num_speculative_tokens":7}`,
		"--limit-mm-per-prompt", `{"image":999,"video":999}`,
		"--numa-bind", "--no-enable-log-requests",
	} {
		if !slices.Contains(spec.ExtraArgs, want) {
			t.Fatalf("extra %q missing: %v", want, spec.ExtraArgs)
		}
	}
	for _, token := range spec.ExtraArgs {
		if strings.Contains(token, "\n") {
			t.Fatalf("token merged across lines: %q", token)
		}
	}
}

func TestLaunchSpec_BuildParseRoundTrip(t *testing.T) {
	spec := Spec{
		Model: "/models/Qwen 27B", ServedModelName: "Qwen/Test",
		ContextPerRequest: 262144, MaxConcurrency: 2,
		TensorParallelSize: 4, CUDAVisibleDevices: []string{"0", "1", "2", "3"},
		ExtraArgs: []string{"--enable-prefix-caching", "--kv-cache-dtype", "fp8_e5m2"},
	}
	args := append(spec.BuildArgs("vllm", false), spec.ExtraArgs...)
	quoted := make([]string, 0, len(args))
	for _, token := range args {
		if strings.ContainsAny(token, " \t") {
			quoted = append(quoted, `"`+token+`"`)
			continue
		}
		quoted = append(quoted, token)
	}
	command := "CUDA_VISIBLE_DEVICES=" + strings.Join(spec.CUDAVisibleDevices, ",") + " vllm serve " + strings.Join(quoted, " ")
	parsed, err := Parse("vllm", command)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Model != spec.Model || parsed.ServedModelName != spec.ServedModelName ||
		parsed.ContextPerRequest != spec.ContextPerRequest || parsed.MaxConcurrency != spec.MaxConcurrency ||
		parsed.TensorParallelSize != spec.TensorParallelSize {
		t.Fatalf("round trip mismatch: %+v", parsed)
	}
	if strings.Join(parsed.ExtraArgs, " ") != strings.Join(spec.ExtraArgs, " ") {
		t.Fatalf("extras mismatch: %v", parsed.ExtraArgs)
	}
}

func TestLaunchSpec_LlamaBuildLegacyExpandsContext(t *testing.T) {
	spec := Spec{Model: "/m.gguf", ContextPerRequest: 262144, MaxConcurrency: 2}
	got := strings.Join(spec.BuildArgs("llamacpp", false), " ")
	if !strings.Contains(got, "--ctx-size 524288") || !strings.Contains(got, "--parallel 2") {
		t.Fatalf("legacy build: %s", got)
	}
	unified := strings.Join(spec.BuildArgs("llamacpp", true), " ")
	if !strings.Contains(unified, "--kv-unified-per-slot 262144") {
		t.Fatalf("unified build: %s", unified)
	}
}

func TestLaunchSpec_ParseNeverExecutes(t *testing.T) {
	marker := "/tmp/llama-swap-parser-test-must-not-exist"
	_ = os.Remove(marker)
	payloads := []string{
		"$(touch " + marker + ")",
		"`touch " + marker + "`",
		"; touch " + marker,
		"&& touch " + marker,
	}
	for _, payload := range payloads {
		if _, err := Parse("vllm", "vllm serve /models/qwen "+payload); err != nil {
			t.Fatalf("parse rejected input: %v", err)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("parser executed pasted content: %s exists", marker)
	}
}

func TestLaunchSpec_BuildEnvPinsDeviceOrderWithVisibleDevices(t *testing.T) {
	got := (Spec{CUDAVisibleDevices: []string{"0", "1"}}).BuildEnv()
	want := []string{"CUDA_DEVICE_ORDER=PCI_BUS_ID", "CUDA_VISIBLE_DEVICES=0,1"}
	if !slices.Equal(got, want) {
		t.Fatalf("BuildEnv = %v, want %v", got, want)
	}
	if got := (Spec{}).BuildEnv(); len(got) != 0 {
		t.Fatalf("empty spec env = %v, want none", got)
	}
}
