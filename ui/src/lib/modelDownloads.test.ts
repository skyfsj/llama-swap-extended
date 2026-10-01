import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { defaultDownloadSourceID, deleteModelDownload, downloadSourcesForProvider, enqueueModelDownload, getModelDownloadCredentials, getModelDownloads, retryModelDownload, updateModelDownloadCredentials } from "./modelDownloads";
import type { ModelFileSource } from "./types";

describe("model download API", () => {
  const fetchMock = vi.fn<typeof fetch>();

  beforeEach(() => vi.stubGlobal("fetch", fetchMock));
  afterEach(() => {
    vi.unstubAllGlobals();
    fetchMock.mockReset();
  });

  it("queues a repository with file filters", async () => {
    const task = { id: "d1", repo_id: "acme/demo", status: "queued" };
    fetchMock.mockResolvedValue(new Response(JSON.stringify({ task, duplicate: false }), { status: 202 }));

    await expect(enqueueModelDownload({ provider: "modelscope", repo_id: "acme/demo", source_id: "models", include: ["*.gguf"] })).resolves.toEqual({ task, duplicate: false });
    expect(fetchMock).toHaveBeenCalledWith("/api/model-downloads", expect.objectContaining({ method: "POST" }));
    expect(JSON.parse(fetchMock.mock.calls[0][1]?.body as string)).toMatchObject({ provider: "modelscope", repo_id: "acme/demo", source_id: "models", include: ["*.gguf"] });
  });

  it("surfaces the duplicate flag when the repository is already queued", async () => {
    const task = { id: "d2", repo_id: "acme/demo", status: "downloading" };
    fetchMock.mockResolvedValue(new Response(JSON.stringify({ task, duplicate: true }), { status: 200 }));

    await expect(enqueueModelDownload({ provider: "huggingface", repo_id: "acme/demo", source_id: "models" })).resolves.toEqual({ task, duplicate: true });
  });

  it("filters destinations by provider and prefers its cache by default", () => {
    const sources: ModelFileSource[] = [
      { id: "models", name: "Models", type: "directory", path: "/models", configured: true, available: true, file_count: 0 },
      { id: "hf-cache", name: "HF cache", type: "hf_cache", path: "/cache/hf", configured: false, available: false, file_count: 0 },
      { id: "ms-cache", name: "MS cache", type: "modelscope_cache", path: "/cache/ms", configured: false, available: false, file_count: 0 },
      { id: "single", name: "Single file", type: "file", path: "/model.gguf", configured: true, available: true, file_count: 1 },
    ];

    expect(downloadSourcesForProvider(sources, "huggingface").map((source) => source.id)).toEqual(["models", "hf-cache"]);
    expect(downloadSourcesForProvider(sources, "modelscope").map((source) => source.id)).toEqual(["models", "ms-cache"]);
    expect(defaultDownloadSourceID(sources, "huggingface")).toBe("hf-cache");
    expect(defaultDownloadSourceID(sources, "modelscope")).toBe("ms-cache");
  });

  it("loads queue state and targets retry action", async () => {
    fetchMock
      .mockResolvedValueOnce(new Response(JSON.stringify({ data: [], limit: 100, offset: 0 }), { status: 200 }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ task: { id: "d1", status: "queued" } }), { status: 200 }));

    await expect(getModelDownloads()).resolves.toMatchObject({ data: [] });
    await expect(retryModelDownload("d1")).resolves.toMatchObject({ id: "d1", status: "queued" });
    expect(fetchMock.mock.calls[1][0]).toBe("/api/model-downloads/d1/retry");
  });

  it("deletes a download task", async () => {
    fetchMock.mockResolvedValue(new Response(null, { status: 204 }));

    await expect(deleteModelDownload("d/1")).resolves.toBeUndefined();
    expect(fetchMock).toHaveBeenCalledWith("/api/model-downloads/d%2F1", { method: "DELETE" });
  });

  it("loads credential presence without receiving a secret", async () => {
    const credentials = {
      etag: "etag-1",
      writable: true,
      huggingface: { configured: true, environmentConfigured: false, environmentVariable: "HF_TOKEN" },
      modelscope: { configured: false, environmentConfigured: true, environmentVariable: "MODELSCOPE_API_TOKEN" },
    };
    fetchMock.mockResolvedValue(new Response(JSON.stringify(credentials), { status: 200 }));

    await expect(getModelDownloadCredentials()).resolves.toEqual(credentials);
    expect(fetchMock).toHaveBeenCalledWith("/api/model-download-credentials");
  });

  it("extracts the message from an OpenAI-style object error envelope", async () => {
    fetchMock.mockResolvedValue(
      new Response(JSON.stringify({ src: "llama-swap", error: { message: "repo_id must be a namespace/model identifier", type: "invalid_request_error", param: null, code: "invalid_request" } }), { status: 400 }),
    );

    await expect(enqueueModelDownload({ provider: "huggingface", repo_id: "1" })).rejects.toThrow("repo_id must be a namespace/model identifier");
  });

  it("keeps the legacy string error shape and status fallback", async () => {
    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify({ error: "plain string error" }), { status: 409 }));
    await expect(enqueueModelDownload({ provider: "huggingface", repo_id: "a/b" })).rejects.toThrow("plain string error");

    fetchMock.mockResolvedValueOnce(new Response("no json body", { status: 500 }));
    await expect(getModelDownloads()).rejects.toThrow("Unable to load model downloads (HTTP 500)");
  });

  it("updates credentials with an etag", async () => {
    const credentials = { etag: "etag-2", writable: true };
    fetchMock.mockResolvedValue(new Response(JSON.stringify(credentials), { status: 200 }));

    await expect(updateModelDownloadCredentials({ hfToken: "secret" }, "etag-1")).resolves.toEqual(credentials);
    expect(fetchMock).toHaveBeenCalledWith("/api/model-download-credentials", expect.objectContaining({ method: "PATCH" }));
    const options = fetchMock.mock.calls[0][1];
    expect(options?.headers).toEqual({ "Content-Type": "application/json", "If-Match": "etag-1" });
    expect(JSON.parse(options?.body as string)).toEqual({ hfToken: "secret" });
  });
});
