import { describe, expect, it } from "vitest";
import {
  combinationsToDsl,
  dslToCombinations,
  matrixFromConfig,
  matrixToConfig,
} from "./matrixDsl";

describe("dslToCombinations", () => {
  it("reads a plain AND combination", () => {
    expect(dslToCombinations("a & b")).toEqual([{ members: ["a", "b"] }]);
  });

  it("splits OR alternatives into separate rows", () => {
    expect(dslToCombinations("a & b | c")).toEqual([{ members: ["a", "b"] }, { members: ["c"] }]);
  });

  it("distributes AND over OR so a table row list is total", () => {
    expect(dslToCombinations("(a | b) & c")).toEqual([{ members: ["a", "c"] }, { members: ["b", "c"] }]);
  });

  it("keeps a single leaf as one row", () => {
    expect(dslToCombinations("a")).toEqual([{ members: ["a"] }]);
  });

  it("treats an empty expression as no rows", () => {
    expect(dslToCombinations("   ")).toEqual([]);
  });

  it("refuses a reference to another set so the DSL editor stays authoritative", () => {
    expect(dslToCombinations("+base & b")).toBeNull();
  });

  it("refuses an expression it cannot parse", () => {
    expect(dslToCombinations("a &")).toBeNull();
    expect(dslToCombinations("(a")).toBeNull();
    expect(dslToCombinations("a b")).toBeNull();
  });
});

describe("combinationsToDsl", () => {
  it("renders one combination per row and joins the alternatives", () => {
    expect(combinationsToDsl([{ members: ["a", "b"] }, { members: ["c"] }])).toBe("(a & b) | c");
  });

  it("renders a single member without a group", () => {
    expect(combinationsToDsl([{ members: ["a"] }])).toBe("a");
  });

  it("renders an empty table as an empty expression", () => {
    expect(combinationsToDsl([])).toBe("");
    expect(combinationsToDsl([{ members: [] }])).toBe("");
  });
});

describe("round trip", () => {
  it("keeps the same combinations when the table is re-read", () => {
    const dsl = "(a | b) & c";
    const rows = dslToCombinations(dsl);
    expect(rows).not.toBeNull();
    expect(dslToCombinations(combinationsToDsl(rows!))).toEqual(rows);
  });
});

describe("matrixFromConfig / matrixToConfig", () => {
  it("reads vars, evict costs and sets into editor rows", () => {
    const draft = matrixFromConfig({
      vars: { small: "qwen", mid: "llama" },
      evict_costs: { small: 5, llama: 1 },
      sets: { pair: "small & mid", alternative: "small | mid" },
    });
    expect(draft.vars).toEqual([
      { name: "small", model: "qwen" },
      { name: "mid", model: "llama" },
    ]);
    expect(draft.evictCosts).toEqual([
      { key: "small", cost: "5" },
      { key: "llama", cost: "1" },
    ]);
    expect(draft.sets).toEqual([
      { name: "pair", combinations: [{ members: ["small", "mid"] }], rawDSL: "small & mid", unrepresentable: false },
      { name: "alternative", combinations: [{ members: ["small"] }, { members: ["mid"] }], rawDSL: "small | mid", unrepresentable: false },
    ]);
  });

  it("keeps an unrepresentable set in its DSL form", () => {
    const draft = matrixFromConfig({ vars: { a: "x" }, sets: { base: "a", uses: "+base & y" } });
    const uses = draft.sets.find((set) => set.name === "uses");
    expect(uses?.unrepresentable).toBe(true);
    expect(uses?.rawDSL).toBe("+base & y");
    // Writing it back must not lose the reference.
    expect(matrixToConfig(draft).sets?.uses).toBe("+base & y");
  });

  it("drops entries the loader would reject", () => {
    const config = matrixToConfig({
      vars: [{ name: "  ", model: "x" }, { name: "ok", model: "  " }, { name: "good", model: "model" }],
      evictCosts: [{ key: "good", cost: "3" }, { key: "bad", cost: "0" }, { key: "zero", cost: "abc" }],
      sets: [{ name: "empty", combinations: [], rawDSL: "", unrepresentable: false }, { name: "  ", combinations: [{ members: ["a"] }], rawDSL: "", unrepresentable: false }],
    });
    expect(config.vars).toEqual({ good: "model" });
    expect(config.evict_costs).toEqual({ good: 3 });
    expect(config.sets).toBeUndefined();
  });

  it("keeps the definition order of sets", () => {
    const draft = matrixFromConfig({ sets: { z: "a", a: "b", m: "c" } });
    const rendered = matrixToConfig(draft);
    expect(Object.keys(rendered.sets ?? {})).toEqual(["z", "a", "m"]);
  });

  it("omits empty blocks so the loader never sees an empty map", () => {
    expect(matrixToConfig({ vars: [], evictCosts: [], sets: [] })).toEqual({});
  });
});
