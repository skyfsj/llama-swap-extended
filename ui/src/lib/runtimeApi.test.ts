import { afterEach, describe, expect, it, vi } from "vitest";
import { fetchRuntimes, postRuntimeAction } from "./runtimeApi";

describe("runtime API errors", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("preserves the model reference reason in an OpenAI error envelope", async () => {
    const message = "runtime version is referenced by model qwen";
    vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockResolvedValue(new Response(JSON.stringify({
      src: "llama-swap",
      error: { message, type: "invalid_request_error", param: null, code: "conflict" },
    }), { status: 409 })));

    await expect(postRuntimeAction("vllm-cuda", "activate/v1.0")).rejects.toThrow(message);
  });

  it("supports a legacy string error envelope", async () => {
    vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockResolvedValue(new Response(JSON.stringify({
      error: "forbidden: runtime-admin permission is required",
    }), { status: 403 })));

    await expect(postRuntimeAction("vllm-cuda", "check")).rejects.toThrow("runtime-admin permission is required");
  });

  it.each(["upstream unavailable", "null"])("falls back to the HTTP status for an unrecognized body: %s", async (body) => {
    vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockResolvedValue(new Response(body, { status: 502 })));

    await expect(fetchRuntimes()).rejects.toThrow("HTTP 502");
  });
});
