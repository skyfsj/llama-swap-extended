import { describe, expect, it } from "vitest";
import {
  activeFilterCount,
  appendActivityFilters,
  emptyActivityFilters,
  hasActiveFilters,
  normalizeActivityFilters,
  type ActivityFilters,
} from "./activityFilters";

function build(overrides: Partial<ActivityFilters> = {}): ActivityFilters {
  return { ...emptyActivityFilters(), ...overrides };
}

function paramsFor(filters: ActivityFilters, now?: Date): URLSearchParams {
  const query = new URLSearchParams();
  appendActivityFilters(query, filters, now);
  return query;
}

describe("emptyActivityFilters", () => {
  it("returns a fresh object each call", () => {
    const first = emptyActivityFilters();
    first.minID = "5";
    expect(emptyActivityFilters().minID).toBe("");
  });
});

describe("appendActivityFilters", () => {
  it("adds nothing for empty filters", () => {
    expect(paramsFor(emptyActivityFilters()).toString()).toBe("");
  });

  it("sets id bounds", () => {
    const query = paramsFor(build({ minID: "5", maxID: "10" }));
    expect(query.get("min_id")).toBe("5");
    expect(query.get("max_id")).toBe("10");
  });

  it("drops blank, non-numeric and out-of-range ids", () => {
    for (const value of ["", "   ", "abc", "0", "-3", "1.5"]) {
      expect(paramsFor(build({ minID: value })).has("min_id")).toBe(false);
    }
  });

  it("writes model, key, session and valid time bounds", () => {
    const query = paramsFor(build({ model: "qwen", keyID: "key-1", sessionID: "session-1", start: "2026-08-01T00:00:00Z", end: "2026-08-02T00:00:00Z" }));
    expect(query.get("model")).toBe("qwen");
    expect(query.get("key_id")).toBe("key-1");
    expect(query.get("session_id")).toBe("session-1");
    expect(query.get("start")).toBe("2026-08-01T00:00:00.000Z");
    expect(query.get("end")).toBe("2026-08-02T00:00:00.000Z");
  });

  it("drops invalid time bounds", () => {
    const query = paramsFor(build({ start: "not-a-date", end: "" }));
    expect(query.has("start")).toBe(false);
    expect(query.has("end")).toBe(false);
  });

  it("uses a deterministic relative time range without guessing token data", () => {
    const now = new Date("2026-09-05T12:00:00.000Z");
    const query = paramsFor(build({ range: "week" }), now);
    expect(query.get("start")).toBe("2026-08-29T12:00:00.000Z");
    expect(query.get("end")).toBe("2026-09-05T12:00:00.000Z");
  });

  it("uses explicit bounds for a custom range", () => {
    const query = paramsFor(build({ range: "custom", start: "2026-09-01T00:00:00Z", end: "2026-09-05T00:00:00Z" }));
    expect(query.get("start")).toBe("2026-09-01T00:00:00.000Z");
    expect(query.get("end")).toBe("2026-09-05T00:00:00.000Z");
  });
});

describe("activeFilterCount", () => {
  it("is zero for empty filters", () => {
    expect(activeFilterCount(emptyActivityFilters())).toBe(0);
    expect(hasActiveFilters(emptyActivityFilters())).toBe(false);
  });

  it("counts each set field", () => {
    expect(activeFilterCount(build({ minID: "1" }))).toBe(1);
    const filters = build({ minID: "1", maxID: "9", model: "m", keyID: "k", sessionID: "s", start: "2026-01-01", end: "2026-01-02" });
    expect(activeFilterCount(filters)).toBe(7);
    expect(hasActiveFilters(filters)).toBe(true);
  });

  it("counts a preset or custom range as one time filter", () => {
    expect(activeFilterCount(build({ range: "day" }))).toBe(1);
    expect(activeFilterCount(build({ range: "custom", start: "2026-09-01", end: "2026-09-02" }))).toBe(1);
  });
});

describe("normalizeActivityFilters", () => {
  it("falls back to empty for non-objects", () => {
    for (const value of [null, undefined, 42, "nope", true]) {
      expect(normalizeActivityFilters(value)).toEqual(emptyActivityFilters());
    }
  });

  it("keeps valid fields and drops wrongly typed ones", () => {
    expect(normalizeActivityFilters({ minID: "3", maxID: 5 })).toEqual(build({ minID: "3" }));
    expect(normalizeActivityFilters({ range: "week" })).toEqual(build({ range: "week" }));
    expect(normalizeActivityFilters({ range: "quarter" })).toEqual(build());
  });

  it("ignores unknown keys", () => {
    expect(normalizeActivityFilters({ minID: "3", userAgent: "x" })).toEqual(build({ minID: "3" }));
  });

  it("keeps supported dimensions and ignores unknown keys", () => {
    const restored = normalizeActivityFilters({
      start: "2026-08-01T00:00",
      end: "2026-08-16T00:00",
      models: ["m1", "m2"],
      minID: "2",
      model: "m1",
      keyID: "k1",
    });
    expect(restored).toEqual(build({ minID: "2", start: "2026-08-01T00:00", end: "2026-08-16T00:00", model: "m1", keyID: "k1" }));
    expect(paramsFor(restored).get("model")).toBe("m1");
    expect(paramsFor(restored).get("key_id")).toBe("k1");
  });
});
