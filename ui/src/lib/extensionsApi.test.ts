import { afterEach, describe, expect, it, vi } from "vitest";
import { fetchExtensionLogHistory, streamExtensionLogs } from "./extensionsApi";

describe("extension log requests", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("does not request a log path for a draft without an id", async () => {
    // A new extension has an empty manifest id; asking for /api/extensions/logs/
    // produced a 404 on every render of the editor.
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);

    expect(await fetchExtensionLogHistory("")).toBe("");
    expect(await fetchExtensionLogHistory("   ")).toBe("");

    const chunks: string[] = [];
    for await (const chunk of streamExtensionLogs("")) chunks.push(chunk);
    expect(chunks).toEqual([]);

    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("requests the log path once an id exists", async () => {
    const fetchMock = vi.fn(async () => new Response("tail", { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);

    expect(await fetchExtensionLogHistory("demo")).toBe("tail");
    expect(fetchMock).toHaveBeenCalledWith("/api/extensions/logs/demo", expect.anything());
  });
});
