import { afterEach, describe, expect, it, vi } from "vitest";
import { commitSettings, fetchSettings, previewSettings, type SettingsSnapshot } from "./settingsApi";

describe("settings API contract", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("loads the source-aware snapshot with same-origin credentials", async () => {
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(new Response(JSON.stringify({ etag: "\"x\"", config: {}, yaml: "", sources: [], writable: false }), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    await fetchSettings();
    expect(fetchMock).toHaveBeenCalledWith("/api/settings/config", expect.objectContaining({ credentials: "same-origin" }));
  });

  it("keeps preview hash and If-Match on commit", async () => {
    const fetchMock = vi.fn<typeof fetch>()
      .mockResolvedValueOnce(new Response(JSON.stringify({ valid: true, baseEtag: "\"x\"", previewHash: "hash" }), { status: 200 }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ snapshot: {}, revision: {}, preview: {} }), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    const snapshot = { etag: "\"x\"" } as SettingsSnapshot;
    const draft = { mode: "structured" as const, changes: [{ op: "replace" as const, path: "/startPort", value: 5900 }] };
    const preview = await previewSettings(draft);
    await commitSettings(snapshot, draft, preview.previewHash);
    expect(fetchMock.mock.calls[1][1]).toEqual(expect.objectContaining({
      headers: expect.objectContaining({ "If-Match": "\"x\"" }),
      body: JSON.stringify({ draft, previewHash: "hash" }),
    }));
  });
});
