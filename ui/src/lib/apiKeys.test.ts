import { describe, expect, it } from "vitest";
import { normalizeAPIKey, type APIKey } from "./apiKeys";

const baseRecord = {
  id: "key-1",
  name: "client",
  kind: "access",
  parentId: "key-0",
  scopes: ["inference"],
  models: ["gpt-4"],
  allowedIps: ["203.0.113.0/24"],
  maxConcurrency: 3,
  group: "team-a",
  allowManagementLogin: false,
  createdAt: "2026-09-01T00:00:00Z",
};

describe("normalizeAPIKey", () => {
  it("reads the split key fields", () => {
    const key = normalizeAPIKey(baseRecord) as APIKey;
    expect(key.id).toBe("key-1");
    expect(key.kind).toBe("access");
    expect(key.parentId).toBe("key-0");
    expect(key.allowedIps).toEqual(["203.0.113.0/24"]);
    expect(key.maxConcurrency).toBe(3);
    expect(key.group).toBe("team-a");
  });

  it("defaults a missing kind to management so legacy rows stay usable", () => {
    const legacy = normalizeAPIKey({ id: "legacy", name: "legacy" }) as APIKey;
    expect(legacy.kind).toBe("management");
    expect(legacy.parentId).toBeUndefined();
    expect(legacy.allowedIps).toEqual([]);
    expect(legacy.maxConcurrency).toBe(0);
  });

  it("drops rows without an id rather than rendering partial data", () => {
    expect(normalizeAPIKey({ name: "no id" })).toBeNull();
    expect(normalizeAPIKey(null)).toBeNull();
  });

  it("never surfaces a secret even if a proxy adds one", () => {
    const key = normalizeAPIKey({ ...baseRecord, key: "sk-should-not-appear" }) as APIKey;
    expect(Object.keys(key)).not.toContain("key");
    expect((key as unknown as Record<string, unknown>).keySecret).toBeUndefined();
  });
});
