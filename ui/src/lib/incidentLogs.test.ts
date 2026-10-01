import { afterEach, describe, expect, it, vi } from "vitest";
import { fetchIncidentLog, fetchIncidentLogs, incidentLogURL } from "./incidentLogs";

describe("incident logs API", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("loads retained entries with same-origin credentials", async () => {
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(new Response(JSON.stringify({
      items: [{ name: "request-error__entry.log", kind: "request-error", createdAt: "2026-09-05T00:00:00Z", size: 42 }],
      maxFiles: 7,
    }), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);

    await expect(fetchIncidentLogs()).resolves.toEqual({
      items: [{ name: "request-error__entry.log", kind: "request-error", createdAt: "2026-09-05T00:00:00Z", size: 42 }],
      maxFiles: 7,
    });
    expect(fetchMock).toHaveBeenCalledWith("/api/logs/incidents", { credentials: "same-origin" });
  });

  it("keeps the API error message and escapes snapshot names", async () => {
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(new Response(JSON.stringify({ error: { message: "forbidden" } }), { status: 403 }));
    vi.stubGlobal("fetch", fetchMock);

    await expect(fetchIncidentLogs()).rejects.toThrow("forbidden");
    expect(incidentLogURL("request/error.log")).toBe("/api/logs/incidents/request%2Ferror.log");
  });

  it("loads one snapshot in the current page", async () => {
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(new Response("event: request-error\nstatus: 502\n", { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);

    await expect(fetchIncidentLog("request-error__entry.log")).resolves.toContain("status: 502");
    expect(fetchMock).toHaveBeenCalledWith("/api/logs/incidents/request-error__entry.log", { credentials: "same-origin" });
  });
});
