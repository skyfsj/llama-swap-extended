import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { deleteModelConfig, ModelConfigRequestError } from "./modelConfigApi";

describe("model configuration API", () => {
  const fetchMock = vi.fn<typeof fetch>();

  beforeEach(() => {
    vi.stubGlobal("fetch", fetchMock);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    fetchMock.mockReset();
  });

  it("sends the optimistic concurrency token and file cleanup choice", async () => {
    fetchMock.mockResolvedValue(new Response(JSON.stringify({ deleted: "Qwen/Qwen3" }), { status: 200 }));

    await expect(deleteModelConfig("Qwen/Qwen3", "etag-1", true)).resolves.toMatchObject({ deleted: "Qwen/Qwen3" });
    expect(fetchMock).toHaveBeenCalledWith("/api/config/models/Qwen%2FQwen3", {
      method: "DELETE",
      headers: { "Content-Type": "application/json", "If-Match": "etag-1" },
      body: JSON.stringify({ delete_model_file: true }),
    });
  });

  it("preserves shared-file conflicts", async () => {
    fetchMock.mockResolvedValue(new Response(JSON.stringify({ error: "shared", registered: ["other"] }), { status: 409 }));

    await expect(deleteModelConfig("model", "etag-1", true)).rejects.toEqual(
      expect.objectContaining({
        name: "ModelConfigRequestError",
        status: 409,
        payload: { error: "shared", registered: ["other"] },
      } satisfies Partial<ModelConfigRequestError>),
    );
  });

  it("reads the standard object-valued error envelope", async () => {
    fetchMock.mockResolvedValue(new Response(JSON.stringify({ error: { message: "If-Match is required" } }), { status: 428 }));

    await expect(deleteModelConfig("model", "", false)).rejects.toMatchObject({
      name: "ModelConfigRequestError",
      message: "If-Match is required",
      status: 428,
    });
  });
});
