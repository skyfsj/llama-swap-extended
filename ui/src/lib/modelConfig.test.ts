import { describe, expect, it } from "vitest";
import {
  autoServedModelName,
  buildModelPatch,
  buildModelValue,
  draftFromModel,
  emptyLaunchDraft,
  emptyModelDraft,
  extraLaunchArguments,
  hasModelCapabilityOverrides,
  launchToBackend,
  managedLaunchKind,  managedRuntimeEntrypoint,
  runtimeMode,
  managedModelPathFromArguments,
  managedModelTargets,
  rebuildManagedArguments,
  runtimeOptions,
  serializeLaunchArgument,
  summarizeChanges,
  syncCudaEnvLines,
  cudaVisibleDevicesFromEnv,
  syncLaunchOwnedArguments,
  tokenizeArgLine,
  tokenizeLaunchArguments,
  withManagedModelTarget,
} from "./modelConfig";
import type { ModelConfigDraft } from "./modelConfig";

describe("modelConfig", () => {
  it("derives managed runtime entrypoints from runtime definitions", () => {
    expect(runtimeOptions({ runtimes: {
      "vllm-main": { kind: "vLLM" },
      "llama-cuda": { kind: "llamacpp" },
    } })).toEqual([
      { name: "llama-cuda", kind: "llamacpp", mode: "native" },
      { name: "vllm-main", kind: "vllm", mode: "native" },
    ]);
    expect(managedRuntimeEntrypoint("vllm")).toBe("vllm");
    expect(managedRuntimeEntrypoint("llamacpp")).toBe("llama-server");
  });

  it("derives the runtime mode like the server: explicit mode, then images, then native", () => {
    expect(runtimeMode({ kind: "vllm" })).toBe("native");
    expect(runtimeMode({ kind: "vllm", mode: "native" })).toBe("native");
    expect(runtimeMode({ kind: "vllm", mode: "container" })).toBe("container");
    expect(runtimeMode({ kind: "vllm", source: { image: "vllm/vllm-openai:latest" } })).toBe("container");
    expect(runtimeMode({ kind: "vllm", container: { image: "vllm/vllm-openai:latest" } })).toBe("container");
  });

  it("excludes container runtimes from the managed native launch", () => {
    expect(managedLaunchKind("vllm", "native")).toBe("vllm");
    expect(managedLaunchKind("llamacpp", "")).toBe("llamacpp");
    expect(managedLaunchKind("vllm", "container")).toBe("");
    expect(managedLaunchKind("generic", "native")).toBe("");
    expect(managedLaunchKind("", "native")).toBe("");
  });

  it("reads legacy and structured fields into one draft", () => {
    const draft = draftFromModel("qwen", {
      name: "Qwen",
      aliases: ["coder", "chat"],
      ttl: 45,
      backend: {
        type: "vllm",
        runtime: "vllm-cuda",
        args: ["vllm", "serve", "/models/qwen"],
        apis: ["chat", "responses"],
        resources: { vramMiB: 24576, gpuAffinity: ["cuda:0"] },
      },
      capabilities: { in: ["text"], out: ["text"], tools: true, context: 32768 },
      filters: { stripParams: "temperature", setParams: { top_p: 0.9 } },
      timeouts: { connect: 12 },
      compat: { ignoreWebsockets: true },
    });

    expect(draft.launchMode).toBe("backend");
    expect(draft.backendRuntime).toBe("vllm-cuda");
    expect(draft.backendArguments).toBe("vllm\nserve\n/models/qwen");
    expect(draft.backendAPIs).toEqual(["chat", "responses"]);
    expect(draft.capabilitiesTools).toBe(true);
    expect(draft.capabilitiesContext).toBe("32768");
    expect(draft.filtersStripParams).toBe("temperature");
    expect(draft.filtersSetParams).toEqual({ top_p: 0.9 });
    expect(draft.timeoutConnect).toBe("12");
    expect(draft.ignoreWebsockets).toBe("true");
  });

  it("converts backend GPU utilization fractions to form percentages", () => {
    const draft = draftFromModel("qwen", {
      backend: {
        launch: { gpuMemoryUtilization: 0.8 },
      },
    });

    expect(draft.launch.gpuMemoryUtilization).toBe("80");
  });

  it("serializes numbers emitted by native number inputs", () => {
    const draft = emptyModelDraft("numeric-input");
    draft.launchMode = "backend";
    draft.backendArguments = "/models/qwen";
    draft.launch.contextPerRequest = 262144;
    draft.launch.maxConcurrency = 4;
    draft.launch.tensorParallelSize = 4;
    draft.launch.gpuMemoryUtilization = 80;
    draft.ttl = 60;
    draft.capabilitiesContext = 262144;
    draft.timeoutConnect = 10;

    expect(hasModelCapabilityOverrides(draft)).toBe(true);
    expect(buildModelValue(draft)).toEqual({
      ttl: 60,
      backend: {
        args: ["/models/qwen"],
        launch: {
          contextPerRequest: 262144,
          maxConcurrency: 4,
          tensorParallelSize: 4,
          gpuMemoryUtilization: 0.8,
        },
      },
      capabilities: { context: 262144 },
      timeouts: { connect: 10 },
    });
  });

  it("hydrates an explicit TP value left in args by an older hybrid config", () => {
    const draft = draftFromModel("qwen", {
      backend: {
        type: "vllm",
        runtime: "vllm-cuda",
        launch: {
          model: "/models/qwen",
          servedModelName: "qwen",
          contextPerRequest: 262144,
          maxConcurrency: 2,
          gpuMemoryUtilization: 0.8,
        },
        args: ["--model /models/qwen", "--tensor-parallel-size 4", "--dtype float16"],
      },
    });

    expect(draft.launch.tensorParallelSize).toBe("4");
    const backend = buildModelValue(draft).backend as Record<string, unknown>;
    expect((backend.launch as Record<string, unknown>).tensorParallelSize).toBe(4);
  });

  it("builds a new command model without materializing empty optional fields", () => {
    const draft = emptyModelDraft("qwen-local");
    draft.launchMode = "command";
    draft.initialLaunchMode = "command";
    draft.name = "Qwen Local";
    draft.cmd = "llama-server --model /models/qwen.gguf --port ${PORT}";
    draft.proxy = "http://127.0.0.1:${PORT}";
    draft.capabilitiesIn = ["text"];
    draft.capabilitiesOut = ["text"];

    expect(buildModelPatch({ models: {} }, draft)).toEqual([{
      op: "add",
      path: "/models/qwen-local",
      value: {
        name: "Qwen Local",
        cmd: draft.cmd,
        proxy: draft.proxy,
        capabilities: { in: ["text"], out: ["text"] },
      },
    }]);
  });

  it("keeps backend discovery and model capabilities automatic by default", () => {
    const draft = emptyModelDraft("auto-model");
    draft.launchMode = "command";
    draft.initialLaunchMode = "command";
    draft.cmd = "llama-server --model /models/auto.gguf --port ${PORT}";
    draft.proxy = "http://127.0.0.1:${PORT}";

    expect(hasModelCapabilityOverrides(draft)).toBe(false);
    expect(buildModelValue(draft)).toEqual({
      cmd: draft.cmd,
      proxy: draft.proxy,
    });

    draft.backendDiscover = "false";
    expect(hasModelCapabilityOverrides(draft)).toBe(true);
  });

  it("persists a managed vLLM runtime with the operator's own lines", () => {
    const draft = emptyModelDraft("qwen-managed");
    draft.launchMode = "backend";
    draft.backendRuntime = "vllm-managed";
    draft.backendType = "vllm";
    draft.backendArguments = rebuildManagedArguments(
      "vllm serve /models/qwen --host 0.0.0.0 --port ${PORT}",
      "vllm",
      "",
    );

    // The entrypoint, bind host and port are system-owned and never stored;
    // the positional model target is operator content and is kept verbatim.
    expect(draft.backendArguments).toBe("/models/qwen");
    expect(buildModelValue(draft)).toEqual({
      backend: {
        type: "vllm",
        runtime: "vllm-managed",
        args: ["/models/qwen"],
      },
    });
  });

  it("derives engine-specific targets from the model file catalog", () => {
    const files = [
      {
        id: "gguf",
        name: "qwen.gguf",
        path: "/models/qwen.gguf",
        relative_path: "qwen.gguf",
        source_id: "local",
        source_type: "directory",
        format: "gguf",
      },
      {
        id: "shard-1",
        name: "model-00001-of-00002.safetensors",
        path: "/cache/models--Qwen--Qwen3/snapshots/abcdef123456/model-00001-of-00002.safetensors",
        relative_path: "models--Qwen--Qwen3/snapshots/abcdef123456/model-00001-of-00002.safetensors",
        source_id: "hf-cache",
        source_type: "hf_cache",
        repository: "Qwen/Qwen3",
        revision: "abcdef123456",
        format: "safetensors",
      },
      {
        id: "shard-2",
        name: "model-00002-of-00002.safetensors",
        path: "/cache/models--Qwen--Qwen3/snapshots/abcdef123456/model-00002-of-00002.safetensors",
        relative_path: "models--Qwen--Qwen3/snapshots/abcdef123456/model-00002-of-00002.safetensors",
        source_id: "hf-cache",
        source_type: "hf_cache",
        repository: "Qwen/Qwen3",
        revision: "abcdef123456",
        format: "safetensors",
      },
    ];

    expect(managedModelTargets(files, "llamacpp")).toEqual([{
      value: "/models/qwen.gguf",
      label: "qwen.gguf · local",
      path: "/models/qwen.gguf",
      sourceID: "local",
      format: "gguf",
    }]);
    expect(managedModelTargets(files, "vllm")).toHaveLength(2);
    expect(managedModelTargets(files, "vllm")).toContainEqual({
      value: "/cache/models--Qwen--Qwen3/snapshots/abcdef123456",
      label: "Qwen/Qwen3 @ abcdef12 · hf-cache",
      path: "/cache/models--Qwen--Qwen3/snapshots/abcdef123456",
      sourceID: "hf-cache",
      format: "safetensors",
    });
  });

  it("normalizes a pasted single-line launch command into operator lines", () => {
    expect(extraLaunchArguments("vllm serve /models/qwen --port 8001", "vllm")).toBe("/models/qwen");
    expect(extraLaunchArguments("llama-server --model /models/qwen.gguf --port 8001", "llamacpp"))
      .toBe("--model /models/qwen.gguf");
    expect(managedModelPathFromArguments("llama-server --model /models/qwen.gguf --port 8001", "llamacpp"))
      .toBe("/models/qwen.gguf");
    expect(managedModelPathFromArguments("vllm serve /models/qwen --port 8001", "vllm"))
      .toBe("/models/qwen");

    const draft = emptyModelDraft("single-line");
    draft.launchMode = "backend";
    draft.backendRuntime = "vllm-managed";
    draft.backendType = "vllm";
    draft.backendArguments = rebuildManagedArguments("vllm serve /models/qwen --port 8001", "vllm", "");
    expect(buildModelValue(draft)).toEqual({
      backend: {
        type: "vllm",
        runtime: "vllm-managed",
        args: ["/models/qwen"],
      },
    });
  });

  it("normalizes python -m vLLM entrypoints and keeps the real model position", () => {
    const oneLine = "python -m vllm.entrypoints.openai.api_server --host 127.0.0.1 --port 8001 --model /models/Qwen3-8B";
    expect(extraLaunchArguments(oneLine, "vllm")).toBe("--model /models/Qwen3-8B");
    expect(managedModelPathFromArguments(oneLine, "vllm")).toBe("/models/Qwen3-8B");

    const perLine = "python\n-m\nvllm.entrypoints.openai.api_server\n--host\n127.0.0.1\n--port\n8001";
    expect(managedModelPathFromArguments(perLine, "vllm")).toBe("");
    expect(withManagedModelTarget(perLine, "vllm", "/models/Qwen3-8B")).toBe("--model /models/Qwen3-8B");

    const positional = "python -m vllm.entrypoints.openai.api_server /models/Qwen3-8B --port 8001";
    expect(managedModelPathFromArguments(positional, "vllm")).toBe("/models/Qwen3-8B");
    expect(withManagedModelTarget(positional, "vllm", "/models/other")).toBe("--model /models/other");
  });

  it("round-trips arguments containing spaces through the editor text form", () => {
    const draft = draftFromModel("spaced", {
      backend: { type: "generic", args: ["--prompt", "hello world", "vllm"] },
    });
    expect(draft.backendArguments).toBe("--prompt\nhello world\nvllm");

    const value = buildModelValue(draft);
    expect(value.backend).toEqual({ type: "generic", args: ["--prompt", "hello world", "vllm"] });
  });

  it("inserts a selected model target without discarding other launch arguments", () => {
    const llamaArgs = withManagedModelTarget(
      "/old/model\n--ctx-size 32768",
      "llamacpp",
      "/models/qwen.gguf",
    );
    expect(llamaArgs).toBe("--model /models/qwen.gguf\n--ctx-size 32768");
    expect(managedModelPathFromArguments(llamaArgs, "llamacpp")).toBe("/models/qwen.gguf");

    const vllmArgs = withManagedModelTarget(
      llamaArgs,
      "vllm",
      "/cache/qwen/snapshot",
    );
    expect(vllmArgs).toBe("--model /cache/qwen/snapshot\n--ctx-size 32768");
    expect(managedModelPathFromArguments(vllmArgs, "vllm")).toBe("/cache/qwen/snapshot");
  });

  it("keeps structured metadata when editing an existing command model", () => {
    const current = {
      cmd: "llama-server --port 8080",
      backend: { type: "llamacpp", args: ["metadata", "only"], resources: { vramMiB: 4096 } },
    };
    const draft = draftFromModel("llama", current);
    draft.name = "Llama";

    expect(buildModelValue(draft, current)).toMatchObject({
      name: "Llama",
      cmd: current.cmd,
      backend: current.backend,
    });
  });

  it("preserves model-only advanced fields when switching to a structured backend", () => {
    const current = {
      metadata: { owner: "platform" },
      cmd: "old-server",
      cmdStop: "kill-old-server",
      backend: {
        type: "vllm",
        runtime: "vllm-cuda",
        args: ["vllm", "serve", "old"],
        lifecycle: { mode: "sleep", sleepLevel: 1 },
        resources: { vramMiB: 12000 },
      },
    };
    const draft = draftFromModel("managed-qwen", current);
    draft.launchMode = "backend";
    draft.backendRuntime = "";
    draft.backendArguments = "vllm\nserve\nnew";
    draft.backendContainer = {};

    expect(buildModelValue(draft, current)).toEqual({
      metadata: { owner: "platform" },
      backend: {
        type: "vllm",
        args: ["vllm", "serve", "new"],
        lifecycle: { mode: "sleep", sleepLevel: 1 },
        resources: { vramMiB: 12000 },
      },
    });
  });

  it("round-trips the model LMCache block for a vllm backend", () => {
    const current = {
      backend: {
        type: "vllm",
        runtime: "vllm-cuda",
        args: ["vllm", "serve", "model"],
        lmcache: { enabled: true, mode: "mp", role: "kv_consumer", host: "cache-host", port: 6000 },
      },
    };
    const draft = draftFromModel("cached", current);
    expect(draft.lmcacheEnabled).toBe(true);
    expect(draft.lmcacheMode).toBe("mp");
    expect(draft.lmcacheRole).toBe("kv_consumer");
    expect(draft.lmcacheHost).toBe("cache-host");
    expect(draft.lmcachePort).toBe("6000");

    const value = buildModelValue(draft, current);
    expect((value.backend as Record<string, unknown>)["lmcache"]).toEqual({
      enabled: true,
      mode: "mp",
      role: "kv_consumer",
      host: "cache-host",
      port: 6000,
    });
  });

  it("round-trips the per-model runtime version pin", () => {
    const current = {
      backend: {
        type: "vllm",
        runtime: "vllm-cuda",
        runtimeVersion: "v1.2.3",
        args: ["vllm", "serve", "model"],
      },
    };
    const draft = draftFromModel("pinned", current);
    expect(draft.backendRuntimeVersion).toBe("v1.2.3");

    const value = buildModelValue(draft, current);
    expect((value.backend as Record<string, unknown>)["runtimeVersion"]).toBe("v1.2.3");

    // An empty pin follows the runtime's active version and is not persisted.
    draft.backendRuntimeVersion = "";
    const cleared = buildModelValue(draft, current);
    expect((cleared.backend as Record<string, unknown>)["runtimeVersion"]).toBeUndefined();
  });

  it("removes the LMCache block when the model usage is disabled", () => {
    const current = {
      backend: {
        type: "vllm",
        runtime: "vllm-cuda",
        args: ["vllm", "serve", "model"],
        lmcache: { enabled: true, mode: "inProcess", chunkSize: 512 },
      },
    };
    const draft = draftFromModel("cached", current);
    expect(draft.lmcacheMode).toBe("inProcess");
    expect(draft.lmcacheChunkSize).toBe("512");
    draft.lmcacheEnabled = false;

    const value = buildModelValue(draft, current);
    expect((value.backend as Record<string, unknown>)["lmcache"]).toBeUndefined();
  });

  it("requires a local launch definition", () => {
    const draft = emptyModelDraft("broken");
    draft.launchMode = "backend";
    expect(() => buildModelValue(draft)).toThrow("backend.args or backend.container.image");
  });

  it("summarizes nested changes as leaf fields", () => {
    expect(summarizeChanges(
      { name: "Old", backend: { type: "vllm", apis: ["chat"] } },
      { name: "New", backend: { type: "vllm", apis: ["chat", "responses"] }, ttl: 60 },
    )).toEqual([
      { path: "backend.apis", before: ["chat"], after: ["chat", "responses"], kind: "changed" },
      { path: "name", before: "Old", after: "New", kind: "changed" },
      { path: "ttl", after: 60, kind: "added" },
    ]);
  });

  it("expands a newly added object into readable field changes", () => {
    expect(summarizeChanges(undefined, {
      name: "UI model",
      aliases: ["ui-check", "ui-demo"],
      backend: { type: "vllm" },
    })).toEqual([
      { path: "aliases", after: ["ui-check", "ui-demo"], kind: "added" },
      { path: "backend.type", after: "vllm", kind: "added" },
      { path: "name", after: "UI model", kind: "added" },
    ]);
  });
});

describe("managed launch arguments", () => {
  it("round-trips JSON and quoted values losslessly", () => {
    const json = '{"enable_thinking":true,"reasoning_effort":"xhigh"}';
    expect(serializeLaunchArgument(json)).toBe(`'${json}'`);
    expect(tokenizeArgLine(`'${json}'`)).toEqual([json]);

    expect(serializeLaunchArgument("hello world")).toBe("'hello world'");
    expect(tokenizeArgLine("'hello world'")).toEqual(["hello world"]);

    const mixed = `it's a "test"`;
    expect(tokenizeArgLine(serializeLaunchArgument(mixed))).toEqual([mixed]);

    const windowsPath = `C:\\models\\qwen.gguf`;
    expect(tokenizeArgLine(serializeLaunchArgument(windowsPath))).toEqual([windowsPath]);
  });

  it("keeps the operator's own lines for managed runtimes", () => {
    expect(extraLaunchArguments(
      "vllm\nserve\n--model /models/qwen\n--host 127.0.0.1\n--port ${PORT}\n--dtype float16\n--tensor-parallel-size 4",
      "vllm",
    )).toBe("--model /models/qwen\n--dtype float16\n--tensor-parallel-size 4");

    expect(extraLaunchArguments("vllm serve /models/qwen --host 0.0.0.0 --port 8001 --dtype float16", "vllm"))
      .toBe("/models/qwen --dtype float16");

    expect(extraLaunchArguments(
      "llama-server\n--model /models/q.gguf\n--host 0.0.0.0\n--port 8000\n--ctx-size 32768",
      "llamacpp",
    )).toBe("--model /models/q.gguf\n--ctx-size 32768");

    expect(extraLaunchArguments("--prompt\nhello", "generic")).toBe("--prompt\nhello");
  });

  it("rebuilds operator lines and replaces the model target on request", () => {
    expect(rebuildManagedArguments(
      "vllm serve /models/qwen --host 127.0.0.1 --port 8001 --dtype float16",
      "vllm", "",
    )).toBe("/models/qwen --dtype float16");

    expect(rebuildManagedArguments("vllm serve --dtype float16", "vllm", "/models/new"))
      .toBe("--model /models/new\n--dtype float16");

    expect(rebuildManagedArguments("llama-server --model /old.gguf --port 9000", "llamacpp", "/new.gguf"))
      .toBe("--model /new.gguf");
  });

  it("round-trips a real vLLM flag list verbatim", () => {
    // A hand-edited config in the 前名后参 style: the editor must show these
    // lines exactly as written and store them back unchanged.
    const flags = [
      "--model /models/Qwen3.8-27B-FP8",
      "--served-model-name Qwen/Qwen3.8-27B-FP8",
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
      `--default-chat-template-kwargs '{"enable_thinking":true,"reasoning_effort":"xhigh"}'`,
      "--reasoning-parser qwen3",
      "--enable-auto-tool-choice",
      "--tool-call-parser qwen3_coder",
      `--speculative-config '{"method":"dflash","model":"/models/Qwen3.8-27B-DFlash2","num_speculative_tokens":7,"kv_cache_dtype":"auto"}'`,
    ].join("\n");

    expect(extraLaunchArguments(flags, "vllm")).toBe(flags);
    expect(rebuildManagedArguments(flags, "vllm", "")).toBe(flags);

    // Selecting a model file swaps the target and keeps every other line.
    expect(withManagedModelTarget(flags, "vllm", "/models/other"))
      .toBe(`--model /models/other\n${flags.split("\n").slice(1).join("\n")}`);

    // The stored lines still expand to a clean argv with the JSON unquoted.
    const tokens = tokenizeLaunchArguments(flags);
    expect(tokens).toContain("--model");
    expect(tokens).toContain("/models/Qwen3.8-27B-FP8");
    expect(tokens).toContain('--default-chat-template-kwargs');
    expect(tokens).toContain('{"enable_thinking":true,"reasoning_effort":"xhigh"}');
    expect(tokens).toContain("--speculative-config");
    expect(tokens).toContain('{"method":"dflash","model":"/models/Qwen3.8-27B-DFlash2","num_speculative_tokens":7,"kv_cache_dtype":"auto"}');
  });
});

describe("syncLaunchOwnedArguments", () => {
  it("replaces owned flag lines in place and appends missing ones (vllm)", () => {
    const launch = emptyLaunchDraft();
    launch.servedModelName = "qwen38";
    launch.contextPerRequest = "262144";
    launch.maxConcurrency = "8";
    launch.gpuMemoryUtilization = "90";
    const input = "--model /models/q\n--max-num-seqs 2\n--trust-remote-code";
    expect(syncLaunchOwnedArguments(input, "vllm", launch)).toBe(
      "--model /models/q\n--max-num-seqs 8\n--trust-remote-code\n--served-model-name qwen38\n--max-model-len 262144\n--gpu-memory-utilization 0.9",
    );
  });

  it("drops a line whose structured field is unset (vllm)", () => {
    const launch = emptyLaunchDraft();
    launch.servedModelName = "m";
    expect(syncLaunchOwnedArguments("--max-num-seqs 4\n--max-model-len 9\n--dtype fp8", "vllm", launch))
      .toBe("--dtype fp8\n--served-model-name m");
  });

  it("is idempotent and keeps non-owned lines verbatim", () => {
    const launch = emptyLaunchDraft();
    launch.servedModelName = "m";
    launch.maxConcurrency = "2";
    const first = syncLaunchOwnedArguments("--speculative-config '{\"method\":\"x\"}'\n--max-num-seqs 2", "vllm", launch);
    expect(syncLaunchOwnedArguments(first, "vllm", launch)).toBe(first);
    expect(first).toContain("--speculative-config '{\"method\":\"x\"}'");
  });

  it("maps llamacpp fields to --ctx-size and --parallel", () => {
    const launch = emptyLaunchDraft();
    launch.servedModelName = "alias1";
    launch.contextPerRequest = "16384";
    launch.maxConcurrency = "2";
    expect(syncLaunchOwnedArguments("--ctx-size 32768\n--flash-attn auto", "llamacpp", launch))
      .toBe("--ctx-size 32768\n--flash-attn auto\n--alias alias1\n--parallel 2");
  });

  it("renders the inline --flag=value form", () => {
    const launch = emptyLaunchDraft();
    launch.servedModelName = "m";
    expect(syncLaunchOwnedArguments("--max-num-seqs=4 --dtype fp8", "vllm", launch))
      .toBe("--dtype fp8\n--served-model-name m");
  });

  it("auto-sets tensor parallel only from NVIDIA selections", () => {
    const launch = emptyLaunchDraft();
    launch.servedModelName = "m";
    launch.gpus = ["index:0", "index:1"];
    expect(syncLaunchOwnedArguments("", "vllm", launch, 1)).not.toContain("tensor-parallel-size");
    expect(syncLaunchOwnedArguments("", "vllm", launch, 2)).toContain("--tensor-parallel-size 2");
  });

  it("keeps an explicit tensor parallel size instead of replacing it with GPU count", () => {
    const launch = emptyLaunchDraft();
    launch.tensorParallelSize = "4";
    launch.gpus = ["index:0", "index:1"];
    expect(syncLaunchOwnedArguments("--tensor-parallel-size 2", "vllm", launch, 2))
      .toBe("--tensor-parallel-size 4");
  });

  it("leaves an owned line whose value is still being typed", () => {
    // The field keeps its value while the operator clears and retypes the
    // line's value, so the renderer sees a pending line and must not drop or
    // rewrite it: either would discard the keystroke that produced it.
    const launch = emptyLaunchDraft();
    launch.maxConcurrency = "1";
    const input = "--trust-remote-code\n--max-num-seqs\n--dtype float16";
    expect(syncLaunchOwnedArguments(input, "vllm", launch)).toBe(input);
  });

  it("leaves an owned line whose value only differs in formatting", () => {
    // 0.90 and 08 are the same values the structured fields hold; rewriting
    // them would move the cursor for no semantic gain.
    const launch = emptyLaunchDraft();
    launch.gpuMemoryUtilization = "90";
    launch.maxConcurrency = "8";
    const input = "--gpu-memory-utilization 0.90\n--max-num-seqs 08\n--dtype fp8";
    expect(syncLaunchOwnedArguments(input, "vllm", launch)).toBe(input);
  });

  it("still replaces an owned line that holds a different value", () => {
    const launch = emptyLaunchDraft();
    launch.maxConcurrency = "8";
    expect(syncLaunchOwnedArguments("--max-num-seqs 2\n--dtype fp8", "vllm", launch))
      .toBe("--max-num-seqs 8\n--dtype fp8");
  });

  it("drops only the valueless flag when a line mixes owned and extra tokens", () => {
    const launch = emptyLaunchDraft();
    expect(syncLaunchOwnedArguments("--max-num-seqs --dtype fp8", "vllm", launch))
      .toBe("--dtype fp8");
  });
});

const emptyCudaEnvState = () => ({ visible: { managed: "", stashed: "" }, order: { managed: "", stashed: "" } });

describe("syncCudaEnvLines", () => {
  it("appends the device-order pin and visible-devices line, updates and removes them", () => {
    const state = emptyCudaEnvState();
    expect(syncCudaEnvLines("", ["0"], state)).toBe("CUDA_DEVICE_ORDER=PCI_BUS_ID\nCUDA_VISIBLE_DEVICES=0");
    expect(syncCudaEnvLines("CUDA_DEVICE_ORDER=PCI_BUS_ID\nCUDA_VISIBLE_DEVICES=0", ["0", "1"], state))
      .toBe("CUDA_DEVICE_ORDER=PCI_BUS_ID\nCUDA_VISIBLE_DEVICES=0,1");
    expect(syncCudaEnvLines("CUDA_DEVICE_ORDER=PCI_BUS_ID\nCUDA_VISIBLE_DEVICES=0,1", [], state)).toBe("");
    expect(state.visible.managed).toBe("");
    expect(state.order.managed).toBe("");
  });

  it("restores operator lines that the selection replaced", () => {
    const state = emptyCudaEnvState();
    const opCvd = "CUDA_VISIBLE_DEVICES=3,5";
    const opOrder = "CUDA_DEVICE_ORDER=LEGACY";
    const first = syncCudaEnvLines(`${opOrder}\n${opCvd}`, ["0"], state);
    expect(first).toBe("CUDA_DEVICE_ORDER=PCI_BUS_ID\nCUDA_VISIBLE_DEVICES=0");
    expect(state.visible.stashed).toBe(opCvd);
    expect(state.order.stashed).toBe(opOrder);
    expect(syncCudaEnvLines(first, [], state)).toBe(`${opOrder}\n${opCvd}`);
    expect(state.visible.stashed).toBe("");
  });

  it("leaves operator lines untouched while the selection is empty", () => {
    const state = emptyCudaEnvState();
    const op = "OTHER=1\nCUDA_DEVICE_ORDER=LEGACY\nCUDA_VISIBLE_DEVICES=7";
    expect(syncCudaEnvLines(op, [], state)).toBe(op);
  });

  it("keeps other env lines and their order", () => {
    const state = emptyCudaEnvState();
    expect(syncCudaEnvLines("OMP_NUM_THREADS=8", ["1", "2"], state))
      .toBe("OMP_NUM_THREADS=8\nCUDA_DEVICE_ORDER=PCI_BUS_ID\nCUDA_VISIBLE_DEVICES=1,2");
  });
});

describe("launchToBackend GPU persistence", () => {
  it("keeps card-picker selections out of the structured launch block", () => {
    const draft = emptyModelDraft("qwen");
    draft.launch.model = "/models/qwen";
    draft.launch.gpus = ["1", "2", "3", "4"];
    expect(launchToBackend(draft)).toEqual({ model: "/models/qwen" });
  });
});

describe("cudaVisibleDevicesFromEnv", () => {
  it("reads numeric and UUID selections from the existing environment block", () => {
    expect(cudaVisibleDevicesFromEnv("OMP_NUM_THREADS=8\nCUDA_VISIBLE_DEVICES=1, 2,GPU-deadbeef\nX=1"))
      .toEqual(["1", "2", "GPU-deadbeef"]);
  });

  it("recovers numeric YAML continuation lines from a legacy CUDA environment", () => {
    expect(cudaVisibleDevicesFromEnv("CUDA_VISIBLE_DEVICES=1\n2\n3\n4\nNCCL_P2P_LEVEL=PHB"))
      .toEqual(["1", "2", "3", "4"]);
  });

  it("leaves an environment block without a CUDA selection alone", () => {
    expect(cudaVisibleDevicesFromEnv("CUDA_DEVICE_ORDER=PCI_BUS_ID\nOMP_NUM_THREADS=8")).toEqual([]);
  });
});

describe("environment persistence", () => {
  it("keeps comma-separated CUDA devices in one environment entry", () => {
    const draft = emptyModelDraft("qwen");
    draft.launchMode = "command";
    draft.cmd = "server";
    draft.env = "CUDA_VISIBLE_DEVICES=1,2,3,4\nNCCL_P2P_LEVEL=PHB";

    expect(buildModelValue(draft).env).toEqual(["CUDA_VISIBLE_DEVICES=1,2,3,4", "NCCL_P2P_LEVEL=PHB"]);
  });
});

describe("lmcache block", () => {
  const configured = (backend: Record<string, unknown>): ModelConfigDraft => {
    const draft = draftFromModel("qwen", {
      backend: { args: ["--dtype", "fp8"], ...backend },
    });
    return draft;
  };

  it("round-trips the standalone (mp) attachment with its host and port", () => {
    const draft = configured({
      lmcache: { enabled: true, mode: "mp", role: "kv_producer", host: "10.0.0.9", port: 6100 },
    });
    expect(draft.lmcacheEnabled).toBe(true);
    expect(draft.lmcacheMode).toBe("mp");
    expect(draft.lmcacheRole).toBe("kv_producer");
    expect(draft.lmcacheHost).toBe("10.0.0.9");
    expect(draft.lmcachePort).toBe("6100");

    const value = buildModelValue(draft);
    expect((value.backend as Record<string, unknown>).lmcache).toEqual({
      enabled: true, mode: "mp", role: "kv_producer", host: "10.0.0.9", port: 6100,
    });
  });

  it("round-trips the non-standalone (inProcess) attachment with its chunk size", () => {
    const draft = configured({
      lmcache: { enabled: true, mode: "inProcess", chunkSize: 512 },
    });
    expect(draft.lmcacheMode).toBe("inProcess");
    expect(draft.lmcacheChunkSize).toBe("512");

    // An explicit zero chunk size is meaningful (library default) and must
    // survive the round trip rather than collapsing to the omitted form.
    draft.lmcacheChunkSize = "0";
    const value = buildModelValue(draft);
    expect((value.backend as Record<string, unknown>).lmcache).toEqual({
      enabled: true, mode: "inProcess", chunkSize: 0,
    });
  });

  it("defaults an enabled attachment without a mode to standalone", () => {
    const draft = configured({ lmcache: { enabled: true } });
    expect(draft.lmcacheMode).toBe("mp");
    expect(buildModelValue(draft)).toMatchObject({
      backend: { lmcache: { enabled: true, mode: "mp" } },
    });
  });

  it("omits the block entirely when the toggle is off", () => {
    const draft = configured({ lmcache: { enabled: true, mode: "inProcess" } });
    draft.lmcacheEnabled = false;
    expect((buildModelValue(draft).backend as Record<string, unknown>).lmcache).toBeUndefined();
  });
});

describe("autoServedModelName", () => {
  it("prefers the upstream model name when set", () => {
    expect(autoServedModelName("Qwen/Qwen3.8-27B-FP8", "qwen38")).toBe("Qwen/Qwen3.8-27B-FP8");
  });

  it("falls back to the model id when the upstream name is empty", () => {
    expect(autoServedModelName("", "qwen38")).toBe("qwen38");
    expect(autoServedModelName("   ", "qwen38")).toBe("qwen38");
  });

  it("trims surrounding whitespace from both inputs", () => {
    expect(autoServedModelName("  upstream-name  ", " qwen38 ")).toBe("upstream-name");
  });
});
