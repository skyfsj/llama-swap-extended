import { describe, expect, it } from "vitest";
import { fieldEditorVariant } from "./settingsFieldVariant";
import type { SettingsField } from "./settingsApi";

function field(overrides: Partial<SettingsField>): SettingsField {
  return {
    path: "/test",
    label: "Test",
    component: "text",
    ...overrides,
  } as SettingsField;
}

describe("fieldEditorVariant", () => {
  // Regression: the provider-list check must resolve before the plain
  // provider select. When the single-value select shadowed it, editing
  // hooks.on_startup.preload submitted a scalar model ID and the server
  // rejected the save with "cannot unmarshal !!str into []string".
  it("renders a provider-backed list as a multi-select, not a single select", () => {
    const preload = field({ component: "list", provider: "models" });
    expect(fieldEditorVariant(preload)).toBe("provider-list");
  });

  it("renders a plain provider-backed scalar as a single select", () => {
    const scalar = field({ component: "text", provider: "models" });
    expect(fieldEditorVariant(scalar)).toBe("select");
  });

  it("renders enum fields as a single select", () => {
    expect(fieldEditorVariant(field({ component: "select", enum: ["a", "b"] }))).toBe("select");
  });

  it("keeps sensitive scalar fields on the password input", () => {
    expect(fieldEditorVariant(field({ component: "text", sensitive: true }))).toBe("sensitive-input");
  });

  it("falls back to the list editor for non-provider lists", () => {
    expect(fieldEditorVariant(field({ component: "list" }))).toBe("list");
  });
});
