import { describe, expect, it } from "vitest";
import {
  emptyUsageFilters,
  formatTokenCount,
  usageExportUrl,
  usageFiltersToQuery,
} from "./usage";

describe("usageFiltersToQuery", () => {
  it("omits empty list filters and always emits a granularity", () => {
    const query = usageFiltersToQuery(emptyUsageFilters());
    expect(query).toBe("?granularity=day");
  });

  it("repeats list filters so several values can be selected at once", () => {
    const filters = emptyUsageFilters();
    filters.keys = ["key-a", "key-b"];
    filters.models = ["gpt-4"];
    const query = usageFiltersToQuery(filters);
    expect(query).toContain("key=key-a");
    expect(query).toContain("key=key-b");
    expect(query).toContain("model=gpt-4");
    expect(query).toContain("granularity=day");
  });

  it("encodes time bounds", () => {
    const filters = emptyUsageFilters();
    filters.start = "2026-09-01T00:00:00.000Z";
    filters.end = "2026-09-30T23:59:59.000Z";
    filters.granularity = "hour";
    const query = usageFiltersToQuery(filters);
    expect(query).toContain("start=2026-09-01T00%3A00%3A00.000Z");
    expect(query).toContain("end=2026-09-30T23%3A59%3A59.000Z");
    expect(query).toContain("granularity=hour");
  });
});

describe("usageExportUrl", () => {
  it("reuses the same query builder as the JSON endpoints", () => {
    const filters = emptyUsageFilters();
    filters.models = ["gpt-4"];
    expect(usageExportUrl(filters)).toBe(
      `/api/usage/export.csv${usageFiltersToQuery(filters)}`,
    );
  });
});

describe("formatTokenCount", () => {
  it("keeps small counts exact and abbreviates large ones", () => {
    expect(formatTokenCount(0)).toBe("0");
    expect(formatTokenCount(842)).toBe("842");
    expect(formatTokenCount(1500)).toBe("1.50K");
    expect(formatTokenCount(208_880_000)).toBe("208.9M");
    expect(formatTokenCount(4_132_125_477)).toBe("4.13B");
    expect(formatTokenCount(1_500_000_000)).toBe("1.50B");
    expect(formatTokenCount(Number.NaN)).toBe("0");
    expect(formatTokenCount(-5)).toBe("0");
  });
});
