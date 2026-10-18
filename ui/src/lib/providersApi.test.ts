import { describe, expect, it } from "vitest";
import {
  cooldownPolicyOf,
  normalizeProvider,
  normalizeProviderType,
  normalizeProvidersSection,
  providersServingModel,
  providersSectionToConfig,
  normalizeProviderModels,
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

  it("keeps the gemini type and session headers", () => {
    const p = normalizeProvider({
      type: "gemini",
      endpoint: "https://generativelanguage.googleapis.com",
      sessionHeaders: ["X-Session-Id", "  ", "X-Litellm-Session-Id"],
      apiKeys: ["k"],
      models: ["gemini-2.5-flash"],
    });
    expect(p!.type).toBe("gemini");
    expect(p!.sessionHeaders).toEqual(["X-Session-Id", "X-Litellm-Session-Id"]);
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

  it("emits sessionHeaders only when configured", () => {
    const out = providersSectionToConfig({
      p: {
        type: "gemini",
        endpoint: "https://x.com",
        sessionHeaders: ["X-Session-Id"],
        apiKeys: [{ value: "k" }],
        models: [{ local: "m", remote: "m" }],
      },
      q: {
        type: "openai",
        endpoint: "https://y.com",
        sessionHeaders: [],
        apiKeys: [{ value: "k" }],
        models: [{ local: "m", remote: "m" }],
      },
    });
    expect((out.p as Record<string, unknown>).sessionHeaders).toEqual(["X-Session-Id"]);
    expect((out.q as Record<string, unknown>).sessionHeaders).toBeUndefined();
  });
});

describe("provider adapters", () => {
  it("materializes the flat shorthand into one adapter", () => {
    const p = normalizeProvider({
      type: "gemini",
      endpoint: "https://generativelanguage.googleapis.com",
      apiKeys: ["k"],
      models: ["gemini-2.5-flash", { local: "a", remote: "b" }],
    });
    expect(p!.adapters).toEqual([
      {
        type: "gemini",
        endpoint: "https://generativelanguage.googleapis.com",
        models: [
          { local: "gemini-2.5-flash", remote: "gemini-2.5-flash" },
          { local: "a", remote: "b" },
        ],
      },
    ]);
  });

  it("normalizes an explicit multi-adapter provider", () => {
    const p = normalizeProvider({
      endpoint: "",
      apiKeys: ["k"],
      adapters: [
        {
          name: "Chat",
          type: "openai",
          endpoint: "https://opencode.ai/inference/go",
          modelMatch: ["kimi-*", ""],
          models: ["kimi-k3"],
        },
        {
          name: "Google",
          type: "gemini",
          endpoint: "https://opencode.ai/inference/go/google",
          pathMap: { "/v1beta/models/": "/v1/models/" },
          default: true,
          apiKeys: [{ valueEnv: "BACKUP" }],
          models: [],
        },
      ],
      sessionHeaders: ["x-opencode-session"],
      headers: { "user-agent": "llama-swap" },
      template: "opencode-go",
    });
    expect(p).not.toBeNull();
    expect(p!.adapters).toHaveLength(2);
    expect(p!.adapters[0].modelMatch).toEqual(["kimi-*"]);
    expect(p!.adapters[1].pathMap).toEqual({ "/v1beta/models/": "/v1/models/" });
    expect(p!.adapters[1].default).toBe(true);
    expect(p!.adapters[1].apiKeys).toEqual([{ valueEnv: "BACKUP" }]);
    expect(p!.template).toBe("opencode-go");
    expect(p!.sessionHeaders).toEqual(["x-opencode-session"]);
    // The primary adapter feeds the display fields.
    expect(p!.type).toBe("openai");
    expect(p!.endpoint).toBe("https://opencode.ai/inference/go");
  });

  it("normalizes one provider entry whose endpoint comes from adapters only", () => {
    // The config JSON of an adapter-style provider has no top-level endpoint;
    // normalization must not drop the entry.
    const section = normalizeProvidersSection({
      providers: {
        go: {
          apiKeys: ["k"],
          adapters: [{ type: "openai", endpoint: "https://x.com", models: ["m"] }],
        },
        broken: { apiKeys: ["k"], adapters: [] },
      },
    });
    expect(Object.keys(section)).toEqual(["go"]);
  });

  it("writes the flat shorthand for a plain single adapter", () => {
    const out = providersSectionToConfig({
      p: {
        type: "anthropic",
        endpoint: "https://api.anthropic.com",
        models: [{ local: "claude", remote: "claude-4" }],
        adapters: [
          {
            type: "anthropic",
            endpoint: "https://api.anthropic.com",
            models: [{ local: "claude", remote: "claude-4" }],
          },
        ],
        apiKeys: [{ value: "k" }],
      },
    });
    const entry = out.p as Record<string, unknown>;
    expect(entry.type).toBe("anthropic");
    expect(entry.endpoint).toBe("https://api.anthropic.com");
    expect(entry.adapters).toBeUndefined();
  });

  it("writes adapters when there are several or adapter-only fields", () => {
    const out = providersSectionToConfig({
      multi: {
        type: "openai",
        endpoint: "https://x.com",
        models: [],
        adapters: [
          { type: "openai", endpoint: "https://x.com", models: [{ local: "m1", remote: "m1" }] },
          { type: "gemini", endpoint: "https://x.com/google", models: [{ local: "m2", remote: "m2" }] },
        ],
        sessionHeaders: ["x-opencode-session"],
        template: "opencode-go",
        apiKeys: [{ value: "k" }],
      },
      pathed: {
        type: "gemini",
        endpoint: "https://y.com",
        models: [],
        adapters: [
          {
            type: "gemini",
            endpoint: "https://y.com",
            pathMap: { "/v1beta/models/": "/v1/models/" },
            models: [{ local: "m3", remote: "m3" }],
          },
        ],
        apiKeys: [{ value: "k" }],
      },
    });
    const multi = out.multi as Record<string, unknown>;
    expect(multi.type).toBeUndefined();
    expect(Array.isArray(multi.adapters)).toBe(true);
    expect(multi.template).toBe("opencode-go");
    expect(multi.sessionHeaders).toEqual(["x-opencode-session"]);
    // A single adapter with pathMap keeps the adapter form so the rewrite is
    // not silently dropped.
    const pathed = out.pathed as Record<string, unknown>;
    expect(pathed.adapters).toEqual([
      {
        type: "gemini",
        endpoint: "https://y.com",
        pathMap: { "/v1beta/models/": "/v1/models/" },
        models: ["m3"],
      },
    ]);
  });
});

describe("provider quota config", () => {
  it("normalizes and round-trips the quota block", () => {
    const p = normalizeProvider({
      type: "openai",
      endpoint: "https://x.com",
      apiKeys: ["k"],
      models: ["m"],
      quota: {
        preset: "opencode-go",
        refreshInterval: "5m",
        enforce: { enabled: true, threshold: 90, fallbackCooldown: "10m" },
      },
    });
    expect(p!.quota).toEqual({
      preset: "opencode-go",
      refreshInterval: "5m",
      enforce: { enabled: true, threshold: 90, fallbackCooldown: "10m" },
    });
    const out = providersSectionToConfig({ p: p! });
    expect((out.p as Record<string, unknown>).quota).toEqual(p!.quota);
  });

  it("drops an empty quota block from the serialized config", () => {
    const p = normalizeProvider({
      type: "openai",
      endpoint: "https://x.com",
      apiKeys: ["k"],
      models: ["m"],
      quota: {},
    });
    expect(p!.quota).toBeUndefined();
    const out = providersSectionToConfig({ p: p! });
    expect((out.p as Record<string, unknown>).quota).toBeUndefined();
  });
});

describe("provider egress proxies", () => {
  it("round-trips adapter and model level proxies", () => {
    const p = normalizeProvider({
      apiKeys: ["k"],
      adapters: [
        {
          name: "LAN",
          type: "openai",
          endpoint: "http://192.168.1.28:7863",
          proxy: "direct",
          models: [{ local: "lan-model", remote: "lan-model" }],
        },
        {
          name: "Internet",
          type: "openai",
          endpoint: "https://api.deepseek.com",
          proxy: "socks5://127.0.0.1:1080",
          models: [
            { local: "proxied", remote: "proxied", proxy: "http://127.0.0.1:7890" },
            { local: "inherits", remote: "inherits" },
          ],
        },
      ],
    });
    expect(p!.adapters[0].proxy).toBe("direct");
    expect(p!.adapters[1].proxy).toBe("socks5://127.0.0.1:1080");
    expect(p!.adapters[1].models[0].proxy).toBe("http://127.0.0.1:7890");
    expect(p!.adapters[1].models[1].proxy).toBeUndefined();

    const out = providersSectionToConfig({ p: p! });
    const adapters = (out.p as Record<string, unknown>).adapters as Array<Record<string, unknown>>;
    expect(adapters[0].proxy).toBe("direct");
    expect(adapters[1].proxy).toBe("socks5://127.0.0.1:1080");
    // A model carrying a proxy keeps the object form even when local==remote.
    expect(adapters[1].models).toEqual([
      { local: "proxied", remote: "proxied", proxy: "http://127.0.0.1:7890" },
      "inherits",
    ]);
  });

  it("writes the adapter form when a single adapter carries a proxy", () => {
    const p = normalizeProvider({
      apiKeys: ["k"],
      adapters: [
        {
          type: "openai",
          endpoint: "https://x.com",
          proxy: "direct",
          models: ["m"],
        },
      ],
    })!;
    const out = providersSectionToConfig({ p });
    const entry = out.p as Record<string, unknown>;
    expect(entry.adapters).toEqual([{ type: "openai", endpoint: "https://x.com", proxy: "direct", models: ["m"] }]);
  });
});

describe("provider model prefix", () => {
  it("round-trips modelPrefix and omits it when empty", () => {
    const withPrefix = normalizeProvider({
      modelPrefix: "go-",
      apiKeys: ["k"],
      adapters: [{ type: "openai", endpoint: "https://x.com", models: ["glm-5.1"] }],
    })!;
    expect(withPrefix.modelPrefix).toBe("go-");
    const out = providersSectionToConfig({ p: withPrefix });
    expect((out.p as Record<string, unknown>).modelPrefix).toBe("go-");

    const bare = normalizeProvider({
      modelPrefix: "",
      endpoint: "https://x.com",
      apiKeys: ["k"],
      models: ["m"],
    })!;
    expect(bare.modelPrefix).toBeUndefined();
    const out2 = providersSectionToConfig({ p: bare });
    expect((out2.p as Record<string, unknown>).modelPrefix).toBeUndefined();
  });
});

describe("provider cooldown policy", () => {
  it("normalizes scalar and object forms and round-trips", () => {
    const scalar = normalizeProvider({
      cooldown: "disabled",
      endpoint: "https://x.com",
      apiKeys: ["k"],
      models: ["m"],
    })!;
    expect(cooldownPolicyOf(scalar.cooldown)).toBe("disabled");
    expect((providersSectionToConfig({ p: scalar }).p as Record<string, unknown>).cooldown).toBe("disabled");

    const object = normalizeProvider({
      cooldown: { policy: "enabled", auth: "30s" },
      endpoint: "https://y.com",
      apiKeys: ["k"],
      models: ["m"],
    })!;
    expect(cooldownPolicyOf(object.cooldown)).toBe("enabled");
    expect((providersSectionToConfig({ p: object }).p as Record<string, unknown>).cooldown).toEqual({
      policy: "enabled",
      auth: "30s",
    });

    const inherit = normalizeProvider({
      endpoint: "https://z.com",
      apiKeys: ["k"],
      models: ["m"],
    })!;
    expect(cooldownPolicyOf(inherit.cooldown)).toBe("inherit");
    expect((providersSectionToConfig({ p: inherit }).p as Record<string, unknown>).cooldown).toBeUndefined();
  });
});

describe("model aliases", () => {
  it("normalizes aliases and keeps the object form when they exist", () => {
    const p = normalizeProvider({
      endpoint: "https://x.com",
      apiKeys: ["k"],
      models: [
        { remote: "gemini-4-argon-contributor", local: "gemini-4-argon", aliases: ["argon", "gemini-4-argon", ""] },
        "plain",
      ],
    })!;
    expect(p.models[0]).toEqual({
      local: "gemini-4-argon",
      remote: "gemini-4-argon-contributor",
      aliases: ["argon"],
    });
    const out = providersSectionToConfig({ p }) as Record<string, unknown>;
    expect((out.p as Record<string, unknown>).models).toEqual([
      { local: "gemini-4-argon", remote: "gemini-4-argon-contributor", aliases: ["argon"] },
      "plain",
    ]);
  });
});

describe("adapter surfaces", () => {
  it("keeps the declared API surface and defaults to the Completions one", () => {
    const p = normalizeProvider({
      apiKeys: ["k"],
      adapters: [
        { name: "Responses", type: "openai-responses", endpoint: "https://x.com", models: ["m"] },
        { name: "Plain", type: "openai", endpoint: "https://y.com", models: ["n"] },
      ],
    })!;
    expect(p.adapters[0].type).toBe("openai-responses");
    expect(p.adapters[1].type).toBe("openai");
    const out = providersSectionToConfig({ p }) as Record<string, unknown>;
    const adapters = (out.p as Record<string, unknown>).adapters as Array<Record<string, unknown>>;
    expect(adapters[0].type).toBe("openai-responses");
    expect(adapters[1].type).toBe("openai");
    // An unknown surface falls back to Completions rather than being dropped.
    expect(normalizeProviderType("responses")).toBe("openai");
    expect(normalizeProviderType(undefined)).toBe("openai");
  });
});


describe("duplicate client-facing names", () => {
  // A name may be declared twice: across adapters it is one model exposed by
  // two API surfaces, and inside one adapter the model that declares it as its
  // own name keeps it (see the gateway's route registration).
  it("keeps duplicates the user declared", () => {
    const section = providersSectionToConfig({
      p: {
        type: "openai",
        endpoint: "https://x.com",
        apiKeys: [{ value: "k" }],
        models: [],
        adapters: [
          { name: "Responses", type: "openai-responses", endpoint: "https://x.com", models: [{ local: "shared", remote: "shared", aliases: ["alias"] }] },
          { name: "Messages", type: "anthropic", endpoint: "https://x.com", models: [{ local: "shared", remote: "shared" }, { local: "other", remote: "other", aliases: ["alias"] }] },
        ],
      },
    }) as Record<string, unknown>;
    const adapters = (section.p as Record<string, unknown>).adapters as Array<Record<string, unknown>>;
    expect(adapters).toHaveLength(2);
    expect((adapters[1].models as unknown[]).length).toBe(2);
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

  it("emits sessionHeaders only when configured", () => {
    const out = providersSectionToConfig({
      p: {
        type: "gemini",
        endpoint: "https://x.com",
        sessionHeaders: ["X-Session-Id"],
        apiKeys: [{ value: "k" }],
        models: [{ local: "m", remote: "m" }],
      },
      q: {
        type: "openai",
        endpoint: "https://y.com",
        sessionHeaders: [],
        apiKeys: [{ value: "k" }],
        models: [{ local: "m", remote: "m" }],
      },
    });
    expect((out.p as Record<string, unknown>).sessionHeaders).toEqual(["X-Session-Id"]);
    expect((out.q as Record<string, unknown>).sessionHeaders).toBeUndefined();
  });
});

describe("provider adapters", () => {
  it("materializes the flat shorthand into one adapter", () => {
    const p = normalizeProvider({
      type: "gemini",
      endpoint: "https://generativelanguage.googleapis.com",
      apiKeys: ["k"],
      models: ["gemini-2.5-flash", { local: "a", remote: "b" }],
    });
    expect(p!.adapters).toEqual([
      {
        type: "gemini",
        endpoint: "https://generativelanguage.googleapis.com",
        models: [
          { local: "gemini-2.5-flash", remote: "gemini-2.5-flash" },
          { local: "a", remote: "b" },
        ],
      },
    ]);
  });

  it("normalizes an explicit multi-adapter provider", () => {
    const p = normalizeProvider({
      endpoint: "",
      apiKeys: ["k"],
      adapters: [
        {
          name: "Chat",
          type: "openai",
          endpoint: "https://opencode.ai/inference/go",
          modelMatch: ["kimi-*", ""],
          models: ["kimi-k3"],
        },
        {
          name: "Google",
          type: "gemini",
          endpoint: "https://opencode.ai/inference/go/google",
          pathMap: { "/v1beta/models/": "/v1/models/" },
          default: true,
          apiKeys: [{ valueEnv: "BACKUP" }],
          models: [],
        },
      ],
      sessionHeaders: ["x-opencode-session"],
      headers: { "user-agent": "llama-swap" },
      template: "opencode-go",
    });
    expect(p).not.toBeNull();
    expect(p!.adapters).toHaveLength(2);
    expect(p!.adapters[0].modelMatch).toEqual(["kimi-*"]);
    expect(p!.adapters[1].pathMap).toEqual({ "/v1beta/models/": "/v1/models/" });
    expect(p!.adapters[1].default).toBe(true);
    expect(p!.adapters[1].apiKeys).toEqual([{ valueEnv: "BACKUP" }]);
    expect(p!.template).toBe("opencode-go");
    expect(p!.sessionHeaders).toEqual(["x-opencode-session"]);
    // The primary adapter feeds the display fields.
    expect(p!.type).toBe("openai");
    expect(p!.endpoint).toBe("https://opencode.ai/inference/go");
  });

  it("normalizes one provider entry whose endpoint comes from adapters only", () => {
    // The config JSON of an adapter-style provider has no top-level endpoint;
    // normalization must not drop the entry.
    const section = normalizeProvidersSection({
      providers: {
        go: {
          apiKeys: ["k"],
          adapters: [{ type: "openai", endpoint: "https://x.com", models: ["m"] }],
        },
        broken: { apiKeys: ["k"], adapters: [] },
      },
    });
    expect(Object.keys(section)).toEqual(["go"]);
  });

  it("writes the flat shorthand for a plain single adapter", () => {
    const out = providersSectionToConfig({
      p: {
        type: "anthropic",
        endpoint: "https://api.anthropic.com",
        models: [{ local: "claude", remote: "claude-4" }],
        adapters: [
          {
            type: "anthropic",
            endpoint: "https://api.anthropic.com",
            models: [{ local: "claude", remote: "claude-4" }],
          },
        ],
        apiKeys: [{ value: "k" }],
      },
    });
    const entry = out.p as Record<string, unknown>;
    expect(entry.type).toBe("anthropic");
    expect(entry.endpoint).toBe("https://api.anthropic.com");
    expect(entry.adapters).toBeUndefined();
  });

  it("writes adapters when there are several or adapter-only fields", () => {
    const out = providersSectionToConfig({
      multi: {
        type: "openai",
        endpoint: "https://x.com",
        models: [],
        adapters: [
          { type: "openai", endpoint: "https://x.com", models: [{ local: "m1", remote: "m1" }] },
          { type: "gemini", endpoint: "https://x.com/google", models: [{ local: "m2", remote: "m2" }] },
        ],
        sessionHeaders: ["x-opencode-session"],
        template: "opencode-go",
        apiKeys: [{ value: "k" }],
      },
      pathed: {
        type: "gemini",
        endpoint: "https://y.com",
        models: [],
        adapters: [
          {
            type: "gemini",
            endpoint: "https://y.com",
            pathMap: { "/v1beta/models/": "/v1/models/" },
            models: [{ local: "m3", remote: "m3" }],
          },
        ],
        apiKeys: [{ value: "k" }],
      },
    });
    const multi = out.multi as Record<string, unknown>;
    expect(multi.type).toBeUndefined();
    expect(Array.isArray(multi.adapters)).toBe(true);
    expect(multi.template).toBe("opencode-go");
    expect(multi.sessionHeaders).toEqual(["x-opencode-session"]);
    // A single adapter with pathMap keeps the adapter form so the rewrite is
    // not silently dropped.
    const pathed = out.pathed as Record<string, unknown>;
    expect(pathed.adapters).toEqual([
      {
        type: "gemini",
        endpoint: "https://y.com",
        pathMap: { "/v1beta/models/": "/v1/models/" },
        models: ["m3"],
      },
    ]);
  });
});

describe("provider quota config", () => {
  it("normalizes and round-trips the quota block", () => {
    const p = normalizeProvider({
      type: "openai",
      endpoint: "https://x.com",
      apiKeys: ["k"],
      models: ["m"],
      quota: {
        preset: "opencode-go",
        refreshInterval: "5m",
        enforce: { enabled: true, threshold: 90, fallbackCooldown: "10m" },
      },
    });
    expect(p!.quota).toEqual({
      preset: "opencode-go",
      refreshInterval: "5m",
      enforce: { enabled: true, threshold: 90, fallbackCooldown: "10m" },
    });
    const out = providersSectionToConfig({ p: p! });
    expect((out.p as Record<string, unknown>).quota).toEqual(p!.quota);
  });

  it("drops an empty quota block from the serialized config", () => {
    const p = normalizeProvider({
      type: "openai",
      endpoint: "https://x.com",
      apiKeys: ["k"],
      models: ["m"],
      quota: {},
    });
    expect(p!.quota).toBeUndefined();
    const out = providersSectionToConfig({ p: p! });
    expect((out.p as Record<string, unknown>).quota).toBeUndefined();
  });
});

describe("provider egress proxies", () => {
  it("round-trips adapter and model level proxies", () => {
    const p = normalizeProvider({
      apiKeys: ["k"],
      adapters: [
        {
          name: "LAN",
          type: "openai",
          endpoint: "http://192.168.1.28:7863",
          proxy: "direct",
          models: [{ local: "lan-model", remote: "lan-model" }],
        },
        {
          name: "Internet",
          type: "openai",
          endpoint: "https://api.deepseek.com",
          proxy: "socks5://127.0.0.1:1080",
          models: [
            { local: "proxied", remote: "proxied", proxy: "http://127.0.0.1:7890" },
            { local: "inherits", remote: "inherits" },
          ],
        },
      ],
    });
    expect(p!.adapters[0].proxy).toBe("direct");
    expect(p!.adapters[1].proxy).toBe("socks5://127.0.0.1:1080");
    expect(p!.adapters[1].models[0].proxy).toBe("http://127.0.0.1:7890");
    expect(p!.adapters[1].models[1].proxy).toBeUndefined();

    const out = providersSectionToConfig({ p: p! });
    const adapters = (out.p as Record<string, unknown>).adapters as Array<Record<string, unknown>>;
    expect(adapters[0].proxy).toBe("direct");
    expect(adapters[1].proxy).toBe("socks5://127.0.0.1:1080");
    // A model carrying a proxy keeps the object form even when local==remote.
    expect(adapters[1].models).toEqual([
      { local: "proxied", remote: "proxied", proxy: "http://127.0.0.1:7890" },
      "inherits",
    ]);
  });

  it("writes the adapter form when a single adapter carries a proxy", () => {
    const p = normalizeProvider({
      apiKeys: ["k"],
      adapters: [
        {
          type: "openai",
          endpoint: "https://x.com",
          proxy: "direct",
          models: ["m"],
        },
      ],
    })!;
    const out = providersSectionToConfig({ p });
    const entry = out.p as Record<string, unknown>;
    expect(entry.adapters).toEqual([{ type: "openai", endpoint: "https://x.com", proxy: "direct", models: ["m"] }]);
  });
});

describe("provider model prefix", () => {
  it("round-trips modelPrefix and omits it when empty", () => {
    const withPrefix = normalizeProvider({
      modelPrefix: "go-",
      apiKeys: ["k"],
      adapters: [{ type: "openai", endpoint: "https://x.com", models: ["glm-5.1"] }],
    })!;
    expect(withPrefix.modelPrefix).toBe("go-");
    const out = providersSectionToConfig({ p: withPrefix });
    expect((out.p as Record<string, unknown>).modelPrefix).toBe("go-");

    const bare = normalizeProvider({
      modelPrefix: "",
      endpoint: "https://x.com",
      apiKeys: ["k"],
      models: ["m"],
    })!;
    expect(bare.modelPrefix).toBeUndefined();
    const out2 = providersSectionToConfig({ p: bare });
    expect((out2.p as Record<string, unknown>).modelPrefix).toBeUndefined();
  });
});

describe("provider cooldown policy", () => {
  it("normalizes scalar and object forms and round-trips", () => {
    const scalar = normalizeProvider({
      cooldown: "disabled",
      endpoint: "https://x.com",
      apiKeys: ["k"],
      models: ["m"],
    })!;
    expect(cooldownPolicyOf(scalar.cooldown)).toBe("disabled");
    expect((providersSectionToConfig({ p: scalar }).p as Record<string, unknown>).cooldown).toBe("disabled");

    const object = normalizeProvider({
      cooldown: { policy: "enabled", auth: "30s" },
      endpoint: "https://y.com",
      apiKeys: ["k"],
      models: ["m"],
    })!;
    expect(cooldownPolicyOf(object.cooldown)).toBe("enabled");
    expect((providersSectionToConfig({ p: object }).p as Record<string, unknown>).cooldown).toEqual({
      policy: "enabled",
      auth: "30s",
    });

    const inherit = normalizeProvider({
      endpoint: "https://z.com",
      apiKeys: ["k"],
      models: ["m"],
    })!;
    expect(cooldownPolicyOf(inherit.cooldown)).toBe("inherit");
    expect((providersSectionToConfig({ p: inherit }).p as Record<string, unknown>).cooldown).toBeUndefined();
  });
});

describe("model aliases", () => {
  it("normalizes aliases and keeps the object form when they exist", () => {
    const p = normalizeProvider({
      endpoint: "https://x.com",
      apiKeys: ["k"],
      models: [
        { remote: "gemini-4-argon-contributor", local: "gemini-4-argon", aliases: ["argon", "gemini-4-argon", ""] },
        "plain",
      ],
    })!;
    expect(p.models[0]).toEqual({
      local: "gemini-4-argon",
      remote: "gemini-4-argon-contributor",
      aliases: ["argon"],
    });
    const out = providersSectionToConfig({ p }) as Record<string, unknown>;
    expect((out.p as Record<string, unknown>).models).toEqual([
      { local: "gemini-4-argon", remote: "gemini-4-argon-contributor", aliases: ["argon"] },
      "plain",
    ]);
  });
});

describe("adapter surfaces", () => {
  it("keeps the declared API surface and defaults to the Completions one", () => {
    const p = normalizeProvider({
      apiKeys: ["k"],
      adapters: [
        { name: "Responses", type: "openai-responses", endpoint: "https://x.com", models: ["m"] },
        { name: "Plain", type: "openai", endpoint: "https://y.com", models: ["n"] },
      ],
    })!;
    expect(p.adapters[0].type).toBe("openai-responses");
    expect(p.adapters[1].type).toBe("openai");
    const out = providersSectionToConfig({ p }) as Record<string, unknown>;
    const adapters = (out.p as Record<string, unknown>).adapters as Array<Record<string, unknown>>;
    expect(adapters[0].type).toBe("openai-responses");
    expect(adapters[1].type).toBe("openai");
    // An unknown surface falls back to Completions rather than being dropped.
    expect(normalizeProviderType("responses")).toBe("openai");
    expect(normalizeProviderType(undefined)).toBe("openai");
  });
});



describe("provider model capabilities", () => {
  it("round-trips a capability declaration", () => {
    const section = normalizeProvidersSection({
      providers: {
        img: {
          type: "openai",
          endpoint: "https://api.example.com",
          apiKeys: ["k"],
          models: [{ local: "gpt-image-2", remote: "gpt-image-2", capabilities: { in: ["text"], out: ["image"] } }],
        },
      },
      models: {},
    });
    expect(section.img.adapters[0].models[0].capabilities).toEqual({ in: ["text"], out: ["image"] });

    const config = providersSectionToConfig(section) as Record<string, Record<string, unknown>>;
    const models = (config.img.models as Array<Record<string, unknown>>) ?? [];
    expect(models[0]).toMatchObject({ local: "gpt-image-2", capabilities: { in: ["text"], out: ["image"] } });
  });

  it("keeps the scalar form when nothing extra is declared", () => {
    const section = normalizeProvidersSection({
      providers: { p: { type: "openai", endpoint: "https://x.com", apiKeys: ["k"], models: ["glm-5.1"] } },
      models: {},
    });
    const config = providersSectionToConfig(section) as Record<string, Record<string, unknown>>;
    expect(config.p.models).toEqual(["glm-5.1"]);
  });
});

describe("providersServingModel", () => {
  // normalizeProvidersSection 的入参是完整配置快照（取其中的 providers 段），
  // 这也是页面里真正的调用方式。
  const providers = normalizeProvidersSection({
    providers: {
      multi: {
        apiKeys: ["k"],
        adapters: [
          { name: "Chat", type: "openai", endpoint: "https://a.example", models: [{ local: "m1", aliases: ["alias-1"] }] },
          { name: "Messages", type: "anthropic", endpoint: "https://b.example", models: ["m1"] },
        ],
      },
      single: { type: "openai", endpoint: "https://c.example", apiKeys: ["k"], models: ["m2"] },
      off: { type: "openai", endpoint: "https://d.example", apiKeys: ["k"], models: ["m1"], disabled: true },
    },
  });

  it("returns every provider and adapter serving the model", () => {
    const found = providersServingModel(providers, "m1");
    expect(found.map((entry) => `${entry.providerID}/${entry.adapter}`)).toEqual([
      "multi/Chat",
      "multi/Messages",
    ]);
  });

  it("matches aliases like the backend does", () => {
    expect(providersServingModel(providers, "alias-1").map((entry) => entry.providerID)).toEqual(["multi"]);
  });

  it("skips disabled providers and answers with an empty list for unknown names", () => {
    expect(providersServingModel(providers, "m2").map((entry) => entry.providerID)).toEqual(["single"]);
    expect(providersServingModel(providers, "nope")).toEqual([]);
    expect(providersServingModel(providers, "  ")).toEqual([]);
  });
});

describe("normalizeProviderModels", () => {
  it("takes the upstream name as the local one when only it was typed", () => {
    expect(normalizeProviderModels([{ local: "", remote: "gpt-6-luna" }])).toEqual([
      { local: "gpt-6-luna", remote: "gpt-6-luna" },
    ]);
  });

  it("drops a row that names nothing at all", () => {
    expect(normalizeProviderModels([{ local: "  ", remote: "" }, { local: "glm-5.1", remote: "" }])).toEqual([
      { local: "glm-5.1", remote: "" },
    ]);
  });

  it("keeps an explicit local name and trims both sides", () => {
    expect(normalizeProviderModels([{ local: " luna ", remote: " gpt-6-luna " }])).toEqual([
      { local: "luna", remote: "gpt-6-luna" },
    ]);
  });
});
