import { describe, expect, it } from "vitest";
import {
  normalizeProvider,
  normalizeProvidersSection,
  providersSectionToConfig,
} from "./providersApi";

describe("normalizeProvider", () => {
  it("returns null for entries without an endpoint", () => {
    expect(normalizeProvider(null)).toBeNull();
    expect(normalizeProvider({ type: "openai" })).toBeNull();
    expect(normalizeProvider("text")).toBeNull();
  });

  it("normalizes a full structured entry", () => {
    const p = normalizeProvider({
      type: "anthropic",
      endpoint: "https://api.anthropic.com/",
      proxy: "socks5://127.0.0.1:1080",
      priority: 5,
      weight: 2,
      sticky: false,
      bareNamePolicy: "provider-first",
      maxRetries: 4,
      disabled: true,
      apiKeys: [
        "scalar-key",
        { value: "structured", weight: 3, priority: 7, maxConcurrent: 2, proxy: "direct", disabled: true },
        { valueEnv: "MY_ENV" },
        { value: "" }, // dropped: no material
      ],
      models: ["solo", { local: "a", remote: "b" }, { local: "" }],
    });
    expect(p).not.toBeNull();
    expect(p!.type).toBe("anthropic");
    expect(p!.endpoint).toBe("https://api.anthropic.com/");
    expect(p!.sticky).toBe(false);
    expect(p!.bareNamePolicy).toBe("provider-first");
    expect(p!.disabled).toBe(true);
    expect(p!.apiKeys).toHaveLength(3);
    expect(p!.apiKeys[0]).toEqual({ value: "scalar-key" });
    expect(p!.apiKeys[1]).toMatchObject({ value: "structured", weight: 3, maxConcurrent: 2, proxy: "direct", disabled: true });
    expect(p!.apiKeys[2]).toEqual({ valueEnv: "MY_ENV" });
    expect(p!.models).toEqual([
      { local: "solo", remote: "solo" },
      { local: "a", remote: "b" },
    ]);
  });

  it("defaults type to openai and sticky to true", () => {
    const p = normalizeProvider({ endpoint: "https://x.com", apiKeys: ["k"], models: ["m"] });
    expect(p!.type).toBe("openai");
    expect(p!.sticky).toBe(true);
    expect(p!.bareNamePolicy).toBe("local-first");
    expect(p!.disabled).toBe(false);
  });
});

describe("normalizeProvidersSection", () => {
  it("normalizes a config snapshot's providers section", () => {
    const section = normalizeProvidersSection({
      providers: {
        a: { endpoint: "https://a.com", apiKeys: ["k"], models: ["m"] },
        b: { endpoint: "" }, // dropped
      },
      models: {},
    });
    expect(Object.keys(section)).toEqual(["a"]);
  });

  it("returns an empty record for malformed input", () => {
    expect(normalizeProvidersSection(null)).toEqual({});
    expect(normalizeProvidersSection({})).toEqual({});
    expect(normalizeProvidersSection({ providers: "oops" })).toEqual({});
  });
});

describe("providersSectionToConfig", () => {
  it("round-trips scalar forms and omits defaults", () => {
    const out = providersSectionToConfig({
      p: {
        type: "openai",
        endpoint: "https://x.com",
        sticky: true,
        bareNamePolicy: "local-first",
        disabled: false,
        apiKeys: [{ value: "k" }],
        models: [{ local: "same", remote: "same" }, { local: "a", remote: "b" }],
      },
    });
    const p = out.p as Record<string, unknown>;
    expect(p.type).toBe("openai");
    expect(p.endpoint).toBe("https://x.com");
    expect(p.sticky).toBeUndefined(); // default true is omitted
    expect(p.bareNamePolicy).toBeUndefined(); // default local-first omitted
    expect(p.disabled).toBeUndefined();
    expect(p.apiKeys).toEqual([{ value: "k" }]);
    expect(p.models).toEqual(["same", { local: "a", remote: "b" }]);
  });

  it("keeps non-defaults explicit", () => {
    const out = providersSectionToConfig({
      p: {
        type: "anthropic",
        endpoint: "https://x.com",
        proxy: "direct",
        priority: 3,
        weight: 2,
        sticky: false,
        bareNamePolicy: "provider-first",
        maxRetries: 5,
        disabled: true,
        apiKeys: [{ valueEnv: "K", weight: 1, priority: 2, maxConcurrent: 4, proxy: "http://p", disabled: true }],
        models: [],
      },
    });
    const p = out.p as Record<string, unknown>;
    expect(p.proxy).toBe("direct");
    expect(p.priority).toBe(3);
    expect(p.sticky).toBe(false);
    expect(p.bareNamePolicy).toBe("provider-first");
    expect(p.maxRetries).toBe(5);
    expect(p.disabled).toBe(true);
    expect(p.apiKeys).toEqual([
      { valueEnv: "K", weight: 1, priority: 2, maxConcurrent: 4, proxy: "http://p", disabled: true },
    ]);
  });
});
