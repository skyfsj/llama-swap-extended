import { describe, it, expect } from "vitest";
import { uiPrefix, apiUrl } from "./apiPath";

describe("apiPath", () => {
  it("returns no prefix for the default root mount", () => {
    expect(uiPrefix("/ui/")).toBe("");
    expect(uiPrefix("/ui")).toBe("");
  });

  it("returns the prefix the proxy mounted the UI under", () => {
    expect(uiPrefix("/asubpath/llama-swap/ui/")).toBe("/asubpath/llama-swap");
    expect(uiPrefix("/asubpath/llama-swap/ui")).toBe("/asubpath/llama-swap");
    expect(uiPrefix("/llama-swap/ui/")).toBe("/llama-swap");
  });

  it("returns no prefix when the path is not the UI mount", () => {
    expect(uiPrefix("/")).toBe("");
    expect(uiPrefix("/ui/models/foo")).toBe("");
  });

  it("leaves paths unchanged outside the browser", () => {
    // Vitest runs in a Node environment: no window, rooted paths pass through.
    expect(apiUrl("/api/mcp")).toBe("/api/mcp");
  });

  it("applies the mount prefix to rooted paths", () => {
    const original = globalThis.window;
    // @ts-expect-error -- minimal browser stand-in for the prefix lookup
    globalThis.window = { location: { pathname: "/asubpath/llama-swap/ui/" } };
    try {
      expect(apiUrl("/api/version")).toBe("/asubpath/llama-swap/api/version");
      expect(apiUrl("/v1/chat/completions")).toBe("/asubpath/llama-swap/v1/chat/completions");
    } finally {
      globalThis.window = original;
    }
  });
});
