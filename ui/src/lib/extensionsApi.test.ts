import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  blankExtension,
  checkExtension,
  deleteExtension,
  duplicateExtension,
  getExtension,
  listExtensions,
  saveExtension,
  settingDiagnosticsFrom,
  testExtension,
} from "./extensionsApi";
import { HttpApiError } from "./http";

describe("extensions API", () => {
  const fetchMock = vi.fn<typeof fetch>();

  beforeEach(() => {
    vi.stubGlobal("fetch", fetchMock);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    fetchMock.mockReset();
  });

  it("starts a new extension with a single entrypoint", () => {
    const blank = blankExtension();
    expect(Object.keys(blank.files)).toEqual(["index.js"]);
    expect(blank.files["index.js"]).toContain("async onRequest");
    expect(blank.manifest.timeout).toBe("10s");
  });

  it("creates and updates with the optimistic concurrency token", async () => {
    fetchMock.mockResolvedValue(new Response(JSON.stringify({ manifest: { id: "a" }, files: { "index.js": "x" }, etag: `"e1"` }), { status: 201 }));

    await saveExtension(blankExtension());
    expect(fetchMock.mock.calls[0]?.[1]?.method).toBe("POST");

    const draft = blankExtension();
    draft.etag = `"e1"`;
    await saveExtension(draft);
    const [, init] = fetchMock.mock.calls[1] ?? [];
    expect(init?.method).toBe("PUT");
    expect(new Headers(init?.headers).get("If-Match")).toBe(`"e1"`);
    expect(JSON.parse(String(init?.body))).toMatchObject({ files: { "index.js": draft.files["index.js"] } });
  });

  it("reads, reloads, deletes and duplicates", async () => {
    fetchMock.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url === "/api/extensions") return Promise.resolve(new Response(JSON.stringify({ data: [{ manifest: { id: "a" }, files: { "index.js": "x" }, etag: `"e1"` }] }), { status: 200 }));
      if (url.endsWith("/duplicate")) return Promise.resolve(new Response(JSON.stringify({ manifest: { id: "a-copy" }, files: { "index.js": "x" }, etag: `"e2"` }), { status: 201 }));
      if (url.endsWith("/reload")) return Promise.resolve(new Response(JSON.stringify({ data: [] }), { status: 200 }));
      if (url === "/api/extensions/a" && init?.method === "DELETE") return Promise.resolve(new Response(null, { status: 204 }));
      return Promise.resolve(new Response(JSON.stringify({ manifest: { id: "a" }, files: { "index.js": "x" }, etag: `"e1"` }), { status: 200 }));
    });

    await expect(listExtensions()).resolves.toHaveLength(1);
    await expect(getExtension("a")).resolves.toMatchObject({ etag: `"e1"` });
    await expect(duplicateExtension("a", "a-copy")).resolves.toMatchObject({ manifest: { id: "a-copy" } });
    await expect(deleteExtension("a", `"e1"`)).resolves.toBeUndefined();
  });

  it("checks a whole tree in one request", async () => {
    fetchMock.mockResolvedValue(new Response(JSON.stringify({ diagnostics: [] }), { status: 200 }));

    await expect(checkExtension({ files: { "index.js": "export default {}" } })).resolves.toEqual([]);
    const body = JSON.parse(String(fetchMock.mock.calls[0]?.[1]?.body));
    expect(body.files).toEqual({ "index.js": "export default {}" });
  });

  it("surfaces ctx.log output from a test run", async () => {
    fetchMock.mockResolvedValue(new Response(JSON.stringify({ matched: true, hooks: ["onRequest"], logs: ["info: hi"], injectedTools: [] }), { status: 200 }));

    const result = await testExtension("a", { model: "m", messages: [] }, "chat.completions");
    expect(result.logs).toEqual(["info: hi"]);
    const body = JSON.parse(String(fetchMock.mock.calls[0]?.[1]?.body));
    expect(body).toMatchObject({ context: { resolvedModel: "m", endpoint: "chat.completions" } });
  });

  it("maps rejected settings values back to their field", () => {
    const error = new HttpApiError("invalid", 422, {
      error: "extension settings are invalid",
      diagnostics: [{ path: "apiKey", severity: "error", message: "is required" }],
    });
    expect(settingDiagnosticsFrom(error)).toEqual({ apiKey: "is required" });
    expect(settingDiagnosticsFrom(new Error("plain"))).toEqual({});
    expect(settingDiagnosticsFrom(new HttpApiError("invalid", 400, { error: "boom" }))).toEqual({});
  });
});
