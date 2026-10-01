import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { deleteModelFile, getModelFiles, groupModelFiles, modelFileDisplayPath, ModelFileRequestError } from "./modelFiles";
import type { ModelDownload, ModelFile } from "./types";

describe("model file API", () => {
  const fetchMock = vi.fn<typeof fetch>();

  beforeEach(() => {
    vi.stubGlobal("fetch", fetchMock);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    fetchMock.mockReset();
  });

  it("encodes source and search filters", async () => {
    const response = {
      data: [],
      sources: [],
      total: 0,
      limit: 1000,
      offset: 0,
      truncated: false,
      scanned_at: "2026-08-28T00:00:00Z",
    };
    fetchMock.mockResolvedValue(new Response(JSON.stringify(response), { status: 200 }));

    await expect(getModelFiles({ source: "hf-cache", query: "Qwen 3" })).resolves.toEqual(response);
    expect(fetchMock).toHaveBeenCalledWith("/api/model-files?limit=1000&source=hf-cache&query=Qwen+3");
  });

  it("preserves conflict details when deletion is rejected", async () => {
    fetchMock.mockResolvedValue(
      new Response(JSON.stringify({ error: "blocked", registered: ["qwen"], in_use: [] }), { status: 409 }),
    );

    const promise = deleteModelFile({ id: "file-id", source_id: "local", path: "/models/qwen.gguf" });
    await expect(promise).rejects.toBeInstanceOf(ModelFileRequestError);
    await expect(promise).rejects.toMatchObject({ status: 409, conflict: { registered: ["qwen"] } });
  });

  it("extracts the message from object-shaped error envelopes", async () => {
    // swaputil.SendResponse envelopes: {error: {message, type, code}} — the
    // message must surface, not "[object Object]".
    fetchMock.mockResolvedValue(
      new Response(JSON.stringify({ error: { message: "model file not found", type: "invalid_request_error", code: "not_found" } }), { status: 404 }),
    );

    const promise = deleteModelFile({ id: "file-id", source_id: "local", path: "/models/qwen.gguf" });
    await expect(promise).rejects.toMatchObject({ status: 404, message: "model file not found" });
  });
});

describe("model file projects", () => {
  const base: ModelFile = {
    id: "file-1",
    name: "model-00001-of-00002.safetensors",
    path: "/cache/models--Qwen--Qwen3/snapshots/commit123/model-00001-of-00002.safetensors",
    relative_path: "models--Qwen--Qwen3/snapshots/commit123/model-00001-of-00002.safetensors",
    source_id: "hf-cache",
    source_type: "hf_cache",
    repository: "Qwen/Qwen3",
    revision: "commit123",
    format: "safetensors",
    size: 10,
    modified_at: "2026-08-31T00:00:00Z",
  };

  it("groups repository shards and keeps revisions separate", () => {
    const groups = groupModelFiles(
      [
        base,
        { ...base, id: "file-2", name: "model-00002-of-00002.safetensors", relative_path: "models--Qwen--Qwen3/snapshots/commit123/model-00002-of-00002.safetensors", size: 12 },
        { ...base, id: "file-3", revision: "commit456", relative_path: "models--Qwen--Qwen3/snapshots/commit456/model.safetensors", name: "model.safetensors" },
      ],
      { "hf-cache": "Hugging Face cache" },
    );

    expect(groups).toHaveLength(2);
    expect(groups[0]).toMatchObject({ name: "Qwen/Qwen3", revision: "commit123", totalSize: 22 });
    expect(groups[0].files).toHaveLength(2);
    expect(groups[1]).toMatchObject({ revision: "commit456" });
  });

  it("shows paths relative to snapshots and ordinary project directories", () => {
    expect(modelFileDisplayPath(base)).toBe("model-00001-of-00002.safetensors");
    expect(modelFileDisplayPath({ ...base, source_type: "directory", repository: undefined, revision: undefined, relative_path: "project-a/weights/model.gguf" })).toBe("weights/model.gguf");
  });

  it("keeps flat model weights as separate model entries instead of grouping them under the source", () => {
    const gguf: ModelFile = {
      ...base,
      id: "flat-gguf",
      name: "Qwen3-8B-Instruct-Q4_K_M.gguf",
      path: "/models/Qwen3-8B-Instruct-Q4_K_M.gguf",
      relative_path: "Qwen3-8B-Instruct-Q4_K_M.gguf",
      source_id: "local-models",
      source_type: "directory",
      repository: undefined,
      revision: undefined,
      format: "gguf",
    };

    const groups = groupModelFiles([gguf], { "local-models": "llm-models" });

    expect(groups).toEqual([expect.objectContaining({
      name: "Qwen3-8B-Instruct-Q4_K_M",
      presentation: "file",
      sourceName: "llm-models",
      files: [gguf],
    })]);
  });

  it("uses the nearest model directory instead of a vendor container", () => {
    const nested: ModelFile = {
      ...base,
      id: "nested-model",
      name: "layers-0.safetensors",
      path: "/models/Qwen/Qwen3.6-35B-A3B-FP8/layers-0.safetensors",
      relative_path: "Qwen/Qwen3.6-35B-A3B-FP8/layers-0.safetensors",
      source_id: "local-models",
      source_type: "directory",
      repository: undefined,
      revision: undefined,
    };

    const groups = groupModelFiles([nested], { "local-models": "llm-models" });

    expect(groups).toEqual([expect.objectContaining({
      name: "Qwen3.6-35B-A3B-FP8",
      presentation: "directory",
      files: [nested],
    })]);
  });

  it("lists model directories before flat model files", () => {
    const flat: ModelFile = {
      ...base,
      id: "flat-alpha",
      name: "Alpha-Q4_K_M.gguf",
      path: "/models/Alpha-Q4_K_M.gguf",
      relative_path: "Alpha-Q4_K_M.gguf",
      source_id: "local-models",
      source_type: "directory",
      repository: undefined,
      revision: undefined,
      format: "gguf",
    };
    const directory: ModelFile = {
      ...base,
      id: "directory-zeta",
      name: "layers-0.safetensors",
      path: "/models/Zeta-30B/layers-0.safetensors",
      relative_path: "Zeta-30B/layers-0.safetensors",
      source_id: "local-models",
      source_type: "directory",
      repository: undefined,
      revision: undefined,
    };

    const groups = groupModelFiles([flat, directory], { "local-models": "llm-models" });

    expect(groups.map(({ presentation, name }) => ({ presentation, name }))).toEqual([
      { presentation: "directory", name: "Zeta-30B" },
      { presentation: "file", name: "Alpha-Q4_K_M" },
    ]);
  });

  it("adds active downloads as projects and merges progress into existing projects", () => {
    const task: ModelDownload = {
      id: "download-1",
      provider: "huggingface",
      repo_id: "Qwen/Qwen3",
      revision: "main",
      source_id: "hf-cache",
      status: "downloading",
      total_files: 4,
      completed_files: 1,
      total_bytes: 100,
      downloaded_bytes: 25,
      attempts: 1,
      created_at: "2026-08-31T00:00:00Z",
      updated_at: "2026-08-31T00:01:00Z",
    };
    const groups = groupModelFiles([base], { "hf-cache": "Hugging Face" }, [
      task,
      { ...task, id: "download-2", repo_id: "Qwen/New", status: "queued" },
      { ...task, id: "download-3", repo_id: "Qwen/Done", status: "completed" },
    ]);

    expect(groups).toHaveLength(2);
    expect(groups.find((group) => group.name === "Qwen/Qwen3")?.download?.id).toBe("download-1");
    expect(groups.find((group) => group.name === "Qwen/New")).toMatchObject({ files: [], revision: "main" });
    expect(groups.some((group) => group.name === "Qwen/Done")).toBe(false);
  });
});
