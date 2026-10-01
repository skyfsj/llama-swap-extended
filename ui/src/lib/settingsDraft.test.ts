import { describe, it, expect } from "vitest";
import {
  cloneConfig,
  configPathParts,
  draftChanges,
  updateAtConfig,
  validateSettingsField,
  valueAtConfig,
} from "./settingsDraft";
import type { SettingsField } from "./settingsApi";
import { translateFor, type Translate } from "./i18n";

const t: Translate = (key, params) => translateFor("en", key, params);

function field(overrides: Partial<SettingsField>): SettingsField {
  return {
    path: "/test",
    label: "Test",
    component: "text",
    ...overrides,
  } as SettingsField;
}

describe("configPathParts", () => {
  it("splits pointer paths and restores escaped tokens", () => {
    expect(configPathParts("/a/b/c")).toEqual(["a", "b", "c"]);
    expect(configPathParts("a~1b")).toEqual(["a/b"]);
    expect(configPathParts("x~0y")).toEqual(["x~y"]);
  });
});

describe("updateAtConfig / valueAtConfig", () => {
  it("creates intermediate objects without mutating the source", () => {
    const source = { existing: 1 };
    const next = updateAtConfig(source, "/a/deep/path", "value");
    const branch = next.a as { deep: { path: string } };
    expect(branch.deep.path).toBe("value");
    expect(source).toEqual({ existing: 1 });
    expect(valueAtConfig(next, "/a/deep/path")).toBe("value");
  });

  it("overwrites an existing scalar branch", () => {
    const next = updateAtConfig({ a: "scalar" }, "/a/b", 2);
    expect(next.a).toEqual({ b: 2 });
  });
});

describe("validateSettingsField", () => {
  it("requires missing values only when required", () => {
    expect(validateSettingsField(t, field({ required: true }), "")).toBeTruthy();
    expect(validateSettingsField(t, field({}), "")).toBeUndefined();
  });

  it("checks number range", () => {
    const f = field({ component: "number", min: 1, max: 10 });
    expect(validateSettingsField(t, f, 0)).toBeTruthy();
    expect(validateSettingsField(t, f, 11)).toBeTruthy();
    expect(validateSettingsField(t, f, 5)).toBeUndefined();
  });

  it("checks Go duration literals", () => {
    const f = field({ component: "duration" });
    expect(validateSettingsField(t, f, "1h30m")).toBeUndefined();
    expect(validateSettingsField(t, f, "500")).toBeTruthy();
  });

  it("rejects URLs with credentials or query strings for mirror base URLs", () => {
    const f = field({ path: "/providers/hf/hfBaseURL" });
    expect(validateSettingsField(t, f, "https://mirror.example")).toBeUndefined();
    expect(validateSettingsField(t, f, "https://user@mirror.example")).toBeTruthy();
    expect(validateSettingsField(t, f, "https://mirror.example?q=1")).toBeTruthy();
  });

  it("checks env variable names for token env fields", () => {
    const f = field({ path: "/providers/hf/hfTokenEnv" });
    expect(validateSettingsField(t, f, "HF_TOKEN")).toBeUndefined();
    expect(validateSettingsField(t, f, "9BAD")).toBeTruthy();
  });
});

describe("draftChanges", () => {
  it("emits replace operations only for changed top-level keys", () => {
    const baseline = JSON.stringify({ keep: 1, change: "old", removed: undefined });
    const changes = draftChanges(baseline, { keep: 1, change: "new", added: true });
    expect(changes).toEqual([
      { op: "replace", path: "/change", value: "new" },
      { op: "replace", path: "/added", value: true },
    ]);
  });
});

describe("cloneConfig", () => {
  it("deep-copies via JSON", () => {
    const source = { nested: { value: 1 } };
    const copy = cloneConfig(source) as { nested: { value: number } };
    copy.nested.value = 2;
    expect(source.nested.value).toBe(1);
  });
});
