import { describe, expect, it } from "vitest";
import { adoptLaunchFields, parseLaunchCommand, stripOwnedLaunchArguments } from "./launchParse";

// The exact command operators paste out of terminals: python -m head, one
// flag per line, JSON payloads with doubled '' stray quotes, a flag whose
// value sits on the next line, and boolean flags without values.
const PASTED_VLLM_COMMAND = [
  "python",
  "-m",
  "vllm.entrypoints.openai.api_server",
  "--model /models/Qwen3.8-27B-FP8",
  "--served-model-name Qwen/Qwen3.8-27B-FP8-DFlash2",
  "--host 127.0.0.1",
  "--port ${PORT}",
  "--trust-remote-code",
  "--tensor-parallel-size 4",
  "--dtype float16",
  "--gpu-memory-utilization 0.90",
  "--max-model-len 262144",
  "--max-num-seqs 2",
  "--max-num-batched-tokens 8192",
  "--attention-backend FLASH_ATTN_V100",
  "--kv-cache-dtype fp8_e5m2",
  "--enable-prefix-caching",
  "--enable-prompt-tokens-details",
  "--enable-force-include-usage",
  `--default-chat-template-kwargs '{"enable_thinking":true,"reasoning_effort":"xhigh"}''`,
  "--reasoning-parser qwen3",
  "--enable-auto-tool-choice",
  "--tool-call-parser qwen3_coder",
  `--speculative-config '{"method":"dflash","model":"/models/Qwen3.8-27B-DFlash2","num_speculative_tokens":7,"kv_cache_dtype":"auto"}''`,
  "--mm-encoder-tp-mode weights",
  "--limit-mm-per-prompt ",
  `'{"image":999,"video":999}'`,
  "--numa-bind",
  "--no-enable-log-requests",
].join("\n");

describe("parseLaunchCommand", () => {
  it("fills the structured fields from a pasted python -m vllm command", () => {
    const parsed = parseLaunchCommand("vllm", PASTED_VLLM_COMMAND);
    expect(parsed.model).toBe("/models/Qwen3.8-27B-FP8");
    expect(parsed.servedModelName).toBe("Qwen/Qwen3.8-27B-FP8-DFlash2");
    expect(parsed.contextPerRequest).toBe(262144);
    expect(parsed.maxConcurrency).toBe(2);
    expect(parsed.gpuMemoryUtilization).toBeCloseTo(0.9);
    expect(parsed.tensorParallelSize).toBe(4);
    expect(parsed.warnings).toEqual([]);
  });

  it("keeps unknown flags and quoted JSON payloads verbatim in extraArgs", () => {
    const parsed = parseLaunchCommand("vllm", PASTED_VLLM_COMMAND);
    for (const token of [
      "--trust-remote-code",
      "--dtype",
      "float16",
      "--max-num-batched-tokens",
      "8192",
      "--attention-backend",
      "FLASH_ATTN_V100",
      "--kv-cache-dtype",
      "fp8_e5m2",
      "--enable-prefix-caching",
      "--enable-prompt-tokens-details",
      "--enable-force-include-usage",
      "--default-chat-template-kwargs",
      '{"enable_thinking":true,"reasoning_effort":"xhigh"}',
      "--reasoning-parser",
      "qwen3",
      "--enable-auto-tool-choice",
      "--tool-call-parser",
      "qwen3_coder",
      "--speculative-config",
      '{"method":"dflash","model":"/models/Qwen3.8-27B-DFlash2","num_speculative_tokens":7,"kv_cache_dtype":"auto"}',
      "--mm-encoder-tp-mode",
      "weights",
      "--limit-mm-per-prompt",
      '{"image":999,"video":999}',
      "--numa-bind",
      "--no-enable-log-requests",
    ]) {
      expect(parsed.extraArgs, `missing ${token}`).toContain(token);
    }
  });

  it("drops the entrypoint head, host and port from the extras", () => {
    const parsed = parseLaunchCommand("vllm", PASTED_VLLM_COMMAND);
    for (const token of ["python", "-m", "vllm.entrypoints.openai.api_server", "--host", "127.0.0.1", "--port", "${PORT}"]) {
      expect(parsed.extraArgs).not.toContain(token);
    }
  });

  it("never merges tokens across lines despite the stray quotes", () => {
    const parsed = parseLaunchCommand("vllm", PASTED_VLLM_COMMAND);
    for (const token of parsed.extraArgs) {
      expect(token, `token spans lines: ${token}`).not.toContain("\n");
    }
    // The stray doubled quote drops silently; the JSON it guarded survives.
    expect(parsed.extraArgs).toContain("--no-enable-log-requests");
  });

  it("moves a leading CUDA_VISIBLE_DEVICES assignment into the GPU selection", () => {
    const parsed = parseLaunchCommand("vllm", "CUDA_VISIBLE_DEVICES=0,1 vllm serve /models/qwen --port 8000");
    expect(parsed.cudaVisibleDevices).toEqual(["0", "1"]);
    expect(parsed.model).toBe("/models/qwen");
    expect(parsed.extraArgs).toEqual([]);
  });

  it("parses --flag=value inline forms for owned flags", () => {
    const parsed = parseLaunchCommand(
      "vllm",
      `vllm serve /models/qwen --max-model-len=262144 --max-num-seqs=2 --gpu-memory-utilization=0.8 -tp 4`,
    );
    expect(parsed.model).toBe("/models/qwen");
    expect(parsed.contextPerRequest).toBe(262144);
    expect(parsed.maxConcurrency).toBe(2);
    expect(parsed.gpuMemoryUtilization).toBeCloseTo(0.8);
    expect(parsed.tensorParallelSize).toBe(4);
    expect(parsed.extraArgs).toEqual([]);
  });

  it("treats vLLM --model-path as the managed model target", () => {
    const parsed = parseLaunchCommand("vllm", "vllm serve --model-path /models/Qwen --tensor-parallel-size 4");
    expect(parsed.model).toBe("/models/Qwen");
    expect(parsed.tensorParallelSize).toBe(4);
    expect(parsed.extraArgs).toEqual([]);
  });

  it("splits llama.cpp ctx-size across the parallel slots", () => {
    const parsed = parseLaunchCommand("llamacpp", "llama-server -m /models/qwen.gguf -c 524288 -np 2");
    expect(parsed.model).toBe("/models/qwen.gguf");
    expect(parsed.contextPerRequest).toBe(262144);
    expect(parsed.maxConcurrency).toBe(2);
    expect(parsed.warnings).toEqual([]);
  });

  it("keeps the total ctx-size and warns when it does not divide evenly", () => {
    const parsed = parseLaunchCommand("llamacpp", "llama-server -m /models/qwen.gguf -c 500000 -np 3");
    expect(parsed.contextPerRequest).toBe(500000);
    expect(parsed.warnings).toHaveLength(1);
  });

  it("warns instead of failing when a reserved flag loses its value", () => {
    const parsed = parseLaunchCommand("vllm", "vllm serve /models/qwen --max-model-len");
    expect(parsed.model).toBe("/models/qwen");
    expect(parsed.contextPerRequest).toBe(0);
    expect(parsed.warnings).toHaveLength(1);
    expect(parsed.extraArgs).toContain("--max-model-len");
  });

  it("warns on values the engine would reject and keeps them out of the fields", () => {
    const parsed = parseLaunchCommand("vllm", "vllm serve /models/qwen --gpu-memory-utilization 1.5");
    expect(parsed.gpuMemoryUtilization).toBeNull();
    expect(parsed.warnings).toHaveLength(1);
  });

  it("leaves lists empty instead of null for a minimal command", () => {
    const parsed = parseLaunchCommand("vllm", "vllm serve /models/qwen");
    expect(parsed.model).toBe("/models/qwen");
    expect(parsed.cudaVisibleDevices).toEqual([]);
    expect(parsed.extraArgs).toEqual([]);
    expect(parsed.warnings).toEqual([]);
  });
});

describe("stripOwnedLaunchArguments", () => {
  it("removes the launch-owned flags and keeps the extras verbatim", () => {
    const text = [
      "--model /models/unsloth/Qwen3.8-27B-NVFP4",
      "--served-model-name Qwen/Qwen3.8-27B-FP8",
      "--trust-remote-code",
      "--tensor-parallel-size 4",
      "--dtype float16",
      "--max-model-len 262144",
      "--kv-cache-dtype float16",
      "--max-num-seqs 1",
      "--gpu-memory-utilization 0.9",
    ].join("\n");
    expect(stripOwnedLaunchArguments("vllm", text)).toBe(
      ["--trust-remote-code", "--dtype float16", "--kv-cache-dtype float16"].join("\n"),
    );
  });

  it("drops the llama-swap-owned host and port pairs", () => {
    const text = "--host 127.0.0.1\n--port 5801\n--enable-prefix-caching";
    expect(stripOwnedLaunchArguments("vllm", text)).toBe("--enable-prefix-caching");
  });

  it("handles inline --flag=value forms and short aliases", () => {
    const text = "--model=/models/qwen.gguf\n-tp 2\n--enable-chunked-prefill";
    expect(stripOwnedLaunchArguments("vllm", text)).toBe("--enable-chunked-prefill");
  });

  it("keeps lines that lose nothing byte for byte", () => {
    const text = '--limit-mm-per-prompt {"image":999,"video":999}\n--dtype float16';
    expect(stripOwnedLaunchArguments("vllm", text)).toBe(text);
  });

  it("uses the llamacpp reserved set", () => {
    const text = "-m /models/qwen.gguf\n--ctx-size 8192\n--parallel 2\n--flash-attn";
    expect(stripOwnedLaunchArguments("llamacpp", text)).toBe("--flash-attn");
  });

  it("returns an empty string when every token is owned", () => {
    expect(stripOwnedLaunchArguments("vllm", "--model /models/qwen.gguf")).toBe("");
  });
});

describe("launch-flag editing safety", () => {
  it("keeps the following flag when a reserved flag loses its value", () => {
    // The editor keeps one flag per line: an operator clearing a value must
    // not have the next line's flag swallowed as this flag's value.
    const parsed = parseLaunchCommand("vllm", "--model /models/q\n--max-num-seqs\n--trust-remote-code\n--dtype float16");
    expect(parsed.maxConcurrency).toBe(0);
    expect(parsed.pendingFields).toContain("maxConcurrency");
    expect(parsed.warnings).toEqual([]);
    expect(parsed.extraArgs).toEqual([
      "--max-num-seqs",
      "--trust-remote-code",
      "--dtype", "float16",
    ]);
  });

  it("still takes a value that sits on the following line", () => {
    const parsed = parseLaunchCommand("vllm", "--max-model-len\n327680\n--trust-remote-code");
    expect(parsed.contextPerRequest).toBe(327680);
    expect(parsed.pendingFields).toEqual([]);
    expect(parsed.extraArgs).toEqual(["--trust-remote-code"]);
  });

  it("reports a field as pending when its value is not usable", () => {
    const parsed = parseLaunchCommand("vllm", "--max-num-seqs abc\n--gpu-memory-utilization 1.5");
    expect(parsed.pendingFields).toEqual(["maxConcurrency", "gpuMemoryUtilization"]);
    expect(parsed.warnings).toHaveLength(2);
  });

  it("does not strip a flag that follows a valueless managed flag", () => {
    expect(stripOwnedLaunchArguments("vllm", "--model /models/q\n--max-num-seqs\n--trust-remote-code"))
      .toBe("--trust-remote-code");
  });

  it("does not strip a flag that follows a valueless host flag", () => {
    expect(stripOwnedLaunchArguments("vllm", "--host\n--trust-remote-code")).toBe("--trust-remote-code");
  });
});

describe("adoptLaunchFields", () => {
  const launch = () => ({
    model: "", servedModelName: "", contextPerRequest: "", maxConcurrency: "",
    tensorParallelSize: "", gpus: [] as string[], gpuMemoryUtilization: "",
  });

  it("adopts the model target from the args text", () => {
    // An edit to the --model line must reach the managed launch: the save
    // strips the flag from the stored extras, so a value the field never
    // reads is silently lost.
    const adopted = adoptLaunchFields("vllm", "--model /models/Qwen\n--dtype fp8", launch());
    expect(adopted.model).toBe("/models/Qwen");
  });

  it("keeps the model target when the args text has none", () => {
    const current = { ...launch(), model: "/models/Qwen" };
    const adopted = adoptLaunchFields("vllm", "--dtype fp8", current);
    expect(adopted.model).toBe("/models/Qwen");
  });

  it("adopts a leading positional model target", () => {
    const adopted = adoptLaunchFields("vllm", "/models/Qwen\n--dtype fp8", launch());
    expect(adopted.model).toBe("/models/Qwen");
  });

  it("fills the numeric fields and the utilization percentage", () => {
    const adopted = adoptLaunchFields("vllm", [
      "--model /models/q", "--max-model-len 327680", "--max-num-seqs 4",
      "--tensor-parallel-size 2", "--gpu-memory-utilization 0.9",
    ].join("\n"), launch());
    expect(adopted.contextPerRequest).toBe("327680");
    expect(adopted.maxConcurrency).toBe("4");
    expect(adopted.tensorParallelSize).toBe("2");
    expect(adopted.gpuMemoryUtilization).toBe("90");
  });

  it("keeps a field whose flag is still being typed", () => {
    const current = { ...launch(), maxConcurrency: "1" };
    const adopted = adoptLaunchFields("vllm", "--max-num-seqs\n--dtype fp8", current);
    expect(adopted.maxConcurrency).toBe("1");
  });

  it("keeps a field whose flag holds a value the engine would reject", () => {
    const current = { ...launch(), maxConcurrency: "1" };
    const adopted = adoptLaunchFields("vllm", "--max-num-seqs abc", current);
    expect(adopted.maxConcurrency).toBe("1");
  });

  it("clears a field whose flag line is gone", () => {
    const current = { ...launch(), maxConcurrency: "1", contextPerRequest: "327680" };
    const adopted = adoptLaunchFields("vllm", "--max-model-len 4096\n--dtype fp8", current);
    expect(adopted.maxConcurrency).toBe("");
    expect(adopted.contextPerRequest).toBe("4096");
  });

  it("preserves the GPU selection", () => {
    const current = { ...launch(), gpus: ["uuid:GPU-1"] };
    const adopted = adoptLaunchFields("vllm", "--max-num-seqs 2", current);
    expect(adopted.gpus).toEqual(["uuid:GPU-1"]);
  });
});
