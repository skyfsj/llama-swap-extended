import { afterEach, describe, expect, it, vi } from "vitest";
import { deletePricingPrice, getPricingCatalog, getPricingPrices, savePricingPrice, syncPricing } from "./pricing";

describe("pricing catalog API", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("loads provider and model suggestions from the local models.dev catalog", async () => {
    const fetchMock = vi.fn<typeof fetch>()
      .mockResolvedValueOnce(new Response(JSON.stringify({ providers: ["openai"], models: [] })))
      .mockResolvedValueOnce(new Response(JSON.stringify({ providers: [], models: ["gpt-5"] })));
    vi.stubGlobal("fetch", fetchMock);

    await expect(getPricingCatalog()).resolves.toEqual({ providers: ["openai"], models: [] });
    await expect(getPricingCatalog({ provider: "openai" })).resolves.toEqual({ providers: [], models: ["gpt-5"] });
    expect(fetchMock).toHaveBeenNthCalledWith(1, "/api/pricing/catalog?limit=200");
    expect(fetchMock).toHaveBeenNthCalledWith(2, "/api/pricing/catalog?provider=openai&limit=200");
  });

  it("loads paged rates and supports pricing row mutations", async () => {
    const fetchMock = vi.fn<typeof fetch>()
      .mockResolvedValueOnce(new Response(JSON.stringify({
        data: [{ provider: "openai", model: "gpt-5", input: 1, output: 5, cacheRead: 0.1, cacheWrite: 0.2, reasoning: 2 }],
        page: 2, limit: 10, total: 11, total_pages: 2, syncedAt: "2026-09-01T00:00:00Z",
      })))
      .mockResolvedValueOnce(new Response(JSON.stringify({ provider: "openai", model: "gpt-5", input: 1, output: 5, cacheRead: 0.1, cacheWrite: 0.2, reasoning: 2 })))
      .mockResolvedValueOnce(new Response(JSON.stringify({ deleted: true })))
      .mockResolvedValueOnce(new Response(JSON.stringify({ changed: true, count: 1, syncedAt: "2026-09-01T00:00:00Z" })));
    vi.stubGlobal("fetch", fetchMock);

    await expect(getPricingPrices({ query: "gpt", page: 2, limit: 10 })).resolves.toMatchObject({ total: 11, page: 2, data: [{ model: "gpt-5" }] });
    await expect(savePricingPrice({ provider: "openai", model: "gpt-5", input: 1, output: 5, cacheRead: 0.1, cacheWrite: 0.2, reasoning: 2 })).resolves.toMatchObject({ model: "gpt-5" });
    await expect(deletePricingPrice("openai", "gpt-5")).resolves.toBeUndefined();
    await expect(syncPricing()).resolves.toMatchObject({ changed: true, count: 1 });

    expect(fetchMock).toHaveBeenNthCalledWith(1, "/api/pricing/prices?q=gpt&page=2&limit=10");
    expect(fetchMock).toHaveBeenNthCalledWith(2, "/api/pricing/prices", expect.objectContaining({ method: "PUT" }));
    expect(fetchMock).toHaveBeenNthCalledWith(3, "/api/pricing/prices?provider=openai&model=gpt-5", { method: "DELETE" });
    expect(fetchMock).toHaveBeenNthCalledWith(4, "/api/pricing/sync", { method: "POST" });
  });
});
