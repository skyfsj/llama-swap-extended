import { describe, expect, it } from "vitest";
import { isInternalRoute, isSameSettingsTarget } from "./navGuard";

describe("navGuard", () => {
  it("treats absolute same-origin SPA paths as internal navigation", () => {
    expect(isInternalRoute("/")).toBe(true);
    expect(isInternalRoute("/models")).toBe(true);
    expect(isInternalRoute("/settings")).toBe(true);
    expect(isInternalRoute("/playground?tab=chat")).toBe(true);
  });

  it("treats scheme links, hash links, and relative files as non-internal", () => {
    expect(isInternalRoute("https://github.com/mostlygeek/llama-swap")).toBe(false);
    expect(isInternalRoute("mailto:support@example.com")).toBe(false);
    expect(isInternalRoute("#section")).toBe(false);
    expect(isInternalRoute("page.html")).toBe(false);
    expect(isInternalRoute(null)).toBe(false);
  });

  it("recognises the settings route (and its subpaths) as the same target", () => {
    expect(isSameSettingsTarget("/settings")).toBe(true);
    expect(isSameSettingsTarget("/settings/models")).toBe(true);
    expect(isSameSettingsTarget("/settings?x=1")).toBe(true);
    expect(isSameSettingsTarget("/")).toBe(false);
    expect(isSameSettingsTarget("/models")).toBe(false);
  });
});
