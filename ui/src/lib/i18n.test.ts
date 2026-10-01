import { describe, expect, it } from "vitest";
import en from "../locales/en.json";
import zhCN from "../locales/zh-CN.json";
import zhTW from "../locales/zh-TW.json";
import { translateFor } from "./i18n";

function leafPaths(value: unknown, prefix = ""): string[] {
  if (!value || typeof value !== "object") return [prefix];

  return Object.entries(value)
    .flatMap(([key, child]) => leafPaths(child, prefix ? `${prefix}.${key}` : key))
    .sort();
}

function placeholders(value: unknown, prefix = ""): Record<string, string[]> {
  if (typeof value === "string") {
    return {
      [prefix]: [...value.matchAll(/\{(\w+)\}/g)].map((match) => match[1]).sort(),
    };
  }
  if (!value || typeof value !== "object") return {};

  return Object.entries(value).reduce<Record<string, string[]>>((all, [key, child]) => {
    return { ...all, ...placeholders(child, prefix ? `${prefix}.${key}` : key) };
  }, {});
}

describe("i18n catalogs", () => {
  it("keeps every locale key in parity with English", () => {
    expect(leafPaths(zhCN)).toEqual(leafPaths(en));
    expect(leafPaths(zhTW)).toEqual(leafPaths(en));
  });

  it("keeps interpolation placeholders in parity with English", () => {
    expect(placeholders(zhCN)).toEqual(placeholders(en));
    expect(placeholders(zhTW)).toEqual(placeholders(en));
  });

  it("resolves dotted keys and interpolates parameters", () => {
    expect(translateFor("zh-CN", "navigation.activity")).toBe("活动");
    expect(translateFor("zh-TW", "navigation.activity")).toBe("活動");
    expect(translateFor("en", "common.page.info", { page: 2, pages: 4, total: 12 })).toBe(
      "Page 2 of 4 · 12 total"
    );
  });
});
