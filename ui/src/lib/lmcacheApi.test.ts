import { afterEach, describe, expect, it, vi } from "vitest";
import {
  fetchLMCacheDashboard,
  patchLMCacheConfig,
  postLMCacheAction,
  type LMCacheConfigSnapshot,
} from "./lmcacheApi";

describe("LMCache API contract", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("loads the dashboard through same-origin credentials", async () => {
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(new Response(JSON.stringify({
      available: false,
      reason: "stopped",
      checkedAt: "2026-09-05T00:00:00Z",
      health: {},
      status: {},
      adapters: [],
      versions: {},
      metrics: {},
      periodicHealth: {},
      errors: {},
    }), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);

    await fetchLMCacheDashboard();

    expect(fetchMock).toHaveBeenCalledWith(
      "/api/lmcache/dashboard",
      expect.objectContaining({ credentials: "same-origin" }),
    );
  });

  it("patches only /lmcache and keeps the config ETag", async () => {
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(new Response(JSON.stringify({
      etag: "\"next\"",
      config: { lmcache: { package: "lmcache" } },
      writable: true,
    }), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);

    const snapshot: LMCacheConfigSnapshot = {
      etag: "\"current\"",
      config: { startPort: 8080, lmcache: { package: "lmcache" } },
      writable: true,
    };
    const lmcache = { package: "lmcache", update: { version: "0.3.11" } };

    await patchLMCacheConfig(snapshot, lmcache);

    expect(fetchMock).toHaveBeenCalledWith("/api/config", expect.objectContaining({
      credentials: "same-origin",
      method: "PATCH",
      headers: expect.objectContaining({
        "Content-Type": "application/json",
        "If-Match": "\"current\"",
      }),
      body: JSON.stringify([{
        op: "replace",
        path: "/lmcache",
        value: lmcache,
      }]),
    }));
  });

  it("rejects a validation failure returned as HTTP 200", async () => {
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(new Response(JSON.stringify({
      valid: false,
      issues: [{ message: "rollbackOnFailure must be true" }],
    }), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);

    await expect(patchLMCacheConfig({ etag: "\"current\"", config: {}, writable: true }, {
      update: { rollbackOnFailure: false },
    })).rejects.toThrow("rollbackOnFailure must be true");
  });

  it("preserves the model reference reason and status from an OpenAI error envelope", async () => {
    const message = "LMCache is still referenced by model qwen";
    vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockResolvedValue(new Response(JSON.stringify({
      src: "llama-swap",
      error: { message, type: "invalid_request_error", param: null, code: "conflict" },
    }), { status: 409 })));

    await expect(postLMCacheAction("server/stop")).rejects.toMatchObject({
      name: "LMCacheApiError", message, status: 409,
    });
  });

  it("preserves a legacy string error envelope and status", async () => {
    const message = "forbidden: runtime-admin permission is required";
    vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockResolvedValue(new Response(JSON.stringify({
      error: message,
    }), { status: 403 })));

    await expect(postLMCacheAction("check")).rejects.toMatchObject({
      name: "LMCacheApiError", message, status: 403,
    });
  });

  it("falls back to the HTTP status for a non-JSON error response", async () => {
    vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockResolvedValue(new Response("unavailable", { status: 502 })));

    await expect(fetchLMCacheDashboard()).rejects.toMatchObject({
      name: "LMCacheApiError", message: "HTTP 502", status: 502,
    });
  });
});
