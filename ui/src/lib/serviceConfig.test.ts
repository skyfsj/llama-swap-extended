import { describe, expect, it } from "vitest";
import { buildServicePatch, draftFromServiceConfig } from "./serviceConfig";

describe("serviceConfig", () => {
  it("reads service-level settings without borrowing model fields", () => {
    const draft = draftFromServiceConfig({
      healthCheckTimeout: 120,
      logRequests: true,
      globalTTL: 60,
      store: { path: "/var/lib/llama-swap/activity.db" },
      performance: { disabled: false, every: "15s" },
      ui: { activity: { session_id: ["X-Session-ID"] } },
      models: { qwen: { ttl: 5 } },
    });

    expect(draft.healthCheckTimeout).toBe("120");
    expect(draft.logRequests).toBe("true");
    expect(draft.globalTTL).toBe("60");
    expect(draft.storePath).toBe("/var/lib/llama-swap/activity.db");
    expect(draft.uiSessionHeaders).toEqual(["X-Session-ID"]);
  });

  it("emits only changed service roots and preserves nested fields", () => {
    const config = {
      logLevel: "info",
      globalTTL: 0,
      performance: { disabled: false, every: "5s", future: "kept" },
    };
    const draft = draftFromServiceConfig(config);
    draft.logLevel = "debug";
    draft.globalTTL = "30";
    draft.performanceEvery = "15s";

    expect(buildServicePatch(config, draft)).toEqual([
      { op: "replace", path: "/logLevel", value: "debug" },
      { op: "replace", path: "/globalTTL", value: 30 },
      { op: "replace", path: "/performance", value: { disabled: false, every: "15s", future: "kept" } },
    ]);
  });

  it("supports removing inherited settings and explicitly disabling session lookup", () => {
    const config = { logRequests: true, ui: { activity: { session_id: ["X-Session-ID"] } } };
    const draft = draftFromServiceConfig(config);
    draft.logRequests = "";
    draft.uiSessionHeadersEnabled = true;
    draft.uiSessionHeaders = [];

    expect(buildServicePatch(config, draft)).toEqual([
      { op: "remove", path: "/logRequests" },
      { op: "replace", path: "/ui", value: { activity: { session_id: [] } } },
    ]);
  });

  it("treats an absent rollbackOnModelStartFailure as enabled and writes only an opt-out", () => {
    // The historical behaviour restores the last configuration that started,
    // so an absent key means enabled.
    expect(draftFromServiceConfig({}).rollbackOnModelStartFailure).toBe(true);
    expect(draftFromServiceConfig({ rollbackOnModelStartFailure: false }).rollbackOnModelStartFailure).toBe(false);
    expect(draftFromServiceConfig({ rollbackOnModelStartFailure: true }).rollbackOnModelStartFailure).toBe(true);

    // Leaving it enabled writes nothing, so an unrelated save does not drift
    // the file toward an explicit true.
    const base = { logLevel: "info", rollbackOnModelStartFailure: true };
    const changed = draftFromServiceConfig(base);
    changed.logLevel = "debug";
    expect(buildServicePatch(base, changed)).toEqual([
      { op: "replace", path: "/logLevel", value: "debug" },
    ]);

    // Opting out records the explicit false.
    const optedOut = draftFromServiceConfig({});
    optedOut.rollbackOnModelStartFailure = false;
    expect(buildServicePatch({}, optedOut)).toEqual([
      { op: "add", path: "/rollbackOnModelStartFailure", value: false },
    ]);
  });
});
