import { describe, expect, it } from "vitest";
import { routingBlockNamesFor } from "./routerReferences";

describe("routingBlockNamesFor", () => {
  it("reports the matrix that names the model through a var", () => {
    const config = {
      routing: {
        router: {
          use: "matrix",
          settings: {
            matrix: { vars: { big: "qwen", small: "hy" }, sets: { pair: "big & small" } },
          },
        },
      },
    };
    expect(routingBlockNamesFor(config, "qwen")).toEqual(["matrix"]);
    expect(routingBlockNamesFor(config, "hy")).toEqual(["matrix"]);
    expect(routingBlockNamesFor(config, "other")).toEqual([]);
  });

  it("reports the matrix that names the model only in a set expression", () => {
    const config = {
      routing: { router: { use: "matrix", settings: { matrix: { sets: { solo: "hy" } } } } },
    };
    expect(routingBlockNamesFor(config, "hy")).toEqual(["matrix"]);
  });

  it("ignores a set reference to another set", () => {
    const config = {
      routing: { router: { use: "matrix", settings: { matrix: { sets: { uses: "+base & hy" } } } } },
    };
    // "+base" is a set reference, not a model name.
    expect(routingBlockNamesFor(config, "base")).toEqual([]);
    expect(routingBlockNamesFor(config, "hy")).toEqual(["matrix"]);
  });

  it("reads the legacy top-level matrix key", () => {
    const config = { matrix: { vars: { big: "qwen" }, sets: { solo: "big" } } };
    expect(routingBlockNamesFor(config, "qwen")).toEqual(["matrix"]);
  });

  it("reports group members", () => {
    const config = {
      routing: { router: { settings: { groups: { first: { members: ["a", "b"] }, second: { members: ["c"] } } } } },
    };
    expect(routingBlockNamesFor(config, "a")).toEqual(["groups"]);
    expect(routingBlockNamesFor(config, "c")).toEqual(["groups"]);
    expect(routingBlockNamesFor(config, "d")).toEqual([]);
  });

  it("reports gpus card lists", () => {
    const config = { routing: { router: { use: "gpus", settings: { gpus: { "0": ["a"], "1": ["b"] } } } } };
    expect(routingBlockNamesFor(config, "b")).toEqual(["gpus"]);
  });

  it("reports the scheduler priority map", () => {
    const config = { routing: { scheduler: { settings: { fifo: { priority: { a: 5 } } } } } };
    expect(routingBlockNamesFor(config, "a")).toEqual(["priority"]);
  });

  it("reports selector targets", () => {
    const config = { selectors: { coding: { targets: ["a", "b"] }, solo: { targets: ["a"] } } };
    expect(routingBlockNamesFor(config, "b")).toEqual(["selectors"]);
  });

  it("reports profile pins that rewrite to the model", () => {
    const config = { profiles: { fast: { pins: { a: "b" } } } };
    expect(routingBlockNamesFor(config, "b")).toEqual(["profiles"]);
  });

  it("reports every block that names the model in a stable order", () => {
    const config = {
      matrix: { vars: { big: "a" } },
      routing: { router: { settings: { groups: { first: { members: ["a"] } } } } },
      selectors: { coding: { targets: ["a"] } },
    };
    expect(routingBlockNamesFor(config, "a")).toEqual(["matrix", "groups", "selectors"]);
  });

  it("tolerates a missing or malformed configuration", () => {
    expect(routingBlockNamesFor(undefined, "a")).toEqual([]);
    expect(routingBlockNamesFor({}, "a")).toEqual([]);
    expect(routingBlockNamesFor({ routing: { router: { settings: { groups: { broken: "nope" } } } } }, "a")).toEqual([]);
    expect(routingBlockNamesFor({ selectors: "nope" }, "a")).toEqual([]);
  });

  it("reports nothing for an empty model id", () => {
    expect(routingBlockNamesFor({ matrix: { vars: { a: "b" } } }, "")).toEqual([]);
  });
});
