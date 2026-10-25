import { describe, expect, it, vi } from "vitest";
import { activityRecordsKey, fetchActivityRecords } from "./activityRecords";
import { emptyActivityFilters } from "./activityFilters";
import type { ActivityPage } from "./types";

function page(): ActivityPage {
  return { data: [], page: 1, limit: 25, total: 0, total_pages: 0 };
}

// A deferred transport so the test controls exactly when a request settles.
function deferred() {
  let resolve!: (value: ActivityPage) => void;
  const promise = new Promise<ActivityPage>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

describe("activityRecordsKey", () => {
  it("is stable for the same request", () => {
    const key = { modelId: "m", page: 1, limit: 25, sort: "id", order: "desc" as const, filters: emptyActivityFilters() };
    expect(activityRecordsKey(key)).toBe(activityRecordsKey({ ...key }));
  });

  it("separates the values the server answer depends on", () => {
    const base = { modelId: "m", page: 1, limit: 25, sort: "id", order: "desc" as const, filters: emptyActivityFilters() };
    const keys = new Set([
      activityRecordsKey(base),
      activityRecordsKey({ ...base, page: 2 }),
      activityRecordsKey({ ...base, limit: 50 }),
      activityRecordsKey({ ...base, sort: "duration" }),
      activityRecordsKey({ ...base, order: "asc" }),
      activityRecordsKey({ ...base, modelId: "other" }),
    ]);
    expect(keys.size).toBe(6);
  });
});

describe("fetchActivityRecords", () => {
  it("shares one request between panels asking for the same page", () => {
    const transport = vi.fn(() => deferred().promise);
    const first = fetchActivityRecords("same", { page: 1 }, transport);
    const second = fetchActivityRecords("same", { page: 1 }, transport);
    expect(second).toBe(first);
    expect(transport).toHaveBeenCalledTimes(1);
    first.catch(() => {});
  });

  it("does not share requests for different keys", () => {
    const transport = vi.fn(() => deferred().promise);
    const first = fetchActivityRecords("k1", { page: 1 }, transport);
    const second = fetchActivityRecords("k2", { page: 1 }, transport);
    expect(second).not.toBe(first);
    expect(transport).toHaveBeenCalledTimes(2);
    first.catch(() => {});
    second.catch(() => {});
  });

  it("drops the shared entry once the request settles, so the next load is fresh", async () => {
    const gate = deferred();
    const transport = vi.fn(() => gate.promise);
    const first = fetchActivityRecords("settles", { page: 1 }, transport);
    // A second panel mounting mid-flight still shares the first request.
    const second = fetchActivityRecords("settles", { page: 1 }, transport);
    expect(second).toBe(first);
    gate.resolve(page());
    await first;
    // The entry is removed in a finally callback, which runs as a microtask
    // after the promise resolves; letting the queue drain makes the assertion
    // about the map's state rather than about a race.
    await Promise.resolve();
    // After it settled the map is empty, so a later refresh starts a new request
    // instead of handing back a page that is already stale.
    const third = fetchActivityRecords("settles", { page: 1 }, transport);
    expect(third).not.toBe(first);
    expect(transport).toHaveBeenCalledTimes(2);
    await third;
  });
});
