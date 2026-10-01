import { describe, expect, it } from "vitest";
import {
  macroOptions,
  parseEnvLines,
  readTtl,
  readUnload,
  serializeEnvLines,
  writeTtl,
  writeUnload,
} from "./modelLifecycle";

describe("modelLifecycle: ttl mapping", () => {
  it("maps raw sentinels to UI modes", () => {
    expect(readTtl(undefined)).toEqual({ mode: "inherit", seconds: "" });
    expect(readTtl(-1)).toEqual({ mode: "inherit", seconds: "" });
    expect(readTtl(0)).toEqual({ mode: "never", seconds: "" });
    expect(readTtl(60)).toEqual({ mode: "custom", seconds: "60" });
  });

  it("maps UI modes back to raw sentinels (round trip)", () => {
    expect(writeTtl({ mode: "inherit", seconds: "" })).toBe(-1);
    expect(writeTtl({ mode: "never", seconds: "" })).toBe(0);
    expect(writeTtl({ mode: "custom", seconds: "45" })).toBe(45);
  });

  it("round trips a custom value through mode+value", () => {
    const state = readTtl(120);
    expect(state.mode).toBe("custom");
    expect(writeTtl(state)).toBe(120);
  });

  it("rejects a non-positive custom ttl (falls back to never)", () => {
    expect(writeTtl({ mode: "custom", seconds: "0" })).toBe(0);
    expect(writeTtl({ mode: "custom", seconds: "" })).toBe(0);
    expect(writeTtl({ mode: "custom", seconds: "-5" })).toBe(0);
  });
});

describe("modelLifecycle: unload mapping", () => {
  it("treats 0 / absence as the global default", () => {
    expect(readUnload(undefined)).toEqual({ mode: "global", seconds: "" });
    expect(readUnload(0)).toEqual({ mode: "global", seconds: "" });
    expect(readUnload(10)).toEqual({ mode: "custom", seconds: "10" });
  });

  it("maps UI modes back to raw values", () => {
    expect(writeUnload({ mode: "global", seconds: "" })).toBe(0);
    expect(writeUnload({ mode: "custom", seconds: "15" })).toBe(15);
  });
});

describe("modelLifecycle: env block", () => {
  it("parses and re-serializes preserving order", () => {
    const entries = parseEnvLines("CUDA_VISIBLE_DEVICES=0,1,2\n\nHF_TOKEN=${env.HF_TOKEN}\n");
    expect(entries).toEqual([
      { key: "CUDA_VISIBLE_DEVICES", value: "0,1,2" },
      { key: "HF_TOKEN", value: "${env.HF_TOKEN}" },
    ]);
    expect(serializeEnvLines(entries)).toBe("CUDA_VISIBLE_DEVICES=0,1,2\nHF_TOKEN=${env.HF_TOKEN}");
  });

  it("ignores blank lines and lines without an assignment", () => {
    expect(parseEnvLines("\nnotanassignment\n=oops\nA=1\n")).toEqual([{ key: "A", value: "1" }]);
  });

  it("round trips through parse+serialize", () => {
    const block = "A=1\nB=two words\n";
    expect(serializeEnvLines(parseEnvLines(block))).toBe("A=1\nB=two words");
  });
});

describe("modelLifecycle: macro options", () => {
  it("lists built-ins, dedupes model over global, and includes env suggestions", () => {
    const options = macroOptions({ default_ctx: "32000" }, { default_ctx: "64000", MY: "x" });
    const tokens = options.map((option) => option.token);
    expect(tokens).toContain("${PORT}");
    expect(tokens).toContain("${MODEL_ID}");
    expect(tokens).toContain("${PID}");
    expect(tokens).toContain("default_ctx");
    expect(tokens).toContain("MY");
    expect(tokens).toContain("env.HF_TOKEN");
    // default_ctx appears exactly once (model overrides global).
    expect(options.filter((option) => option.token === "default_ctx")).toHaveLength(1);
  });
});
