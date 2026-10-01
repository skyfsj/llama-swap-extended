import { describe, expect, it } from "vitest";
import { childSchema, defaultForSchema, modelFieldSchema, resolveSchema, topLevelSchema } from "./configSchema";

describe("configSchema", () => {
  it("uses the repository schema for top-level configuration domains", () => {
    expect(resolveSchema(topLevelSchema("peers")).type).toBe("object");
    expect(resolveSchema(topLevelSchema("resourceBudget")).properties).toMatchObject({
      vramMiB: expect.any(Object),
      ramMiB: expect.any(Object),
      autoEvict: expect.any(Object),
    });
  });

  it("resolves model-only nested fields", () => {
    const filters = resolveSchema(modelFieldSchema("filters"));
    expect(filters.properties).toMatchObject({
      stripParams: expect.any(Object),
      setParams: expect.any(Object),
      setParamsByID: expect.any(Object),
    });
    expect(resolveSchema(childSchema(modelFieldSchema("backend"), "container")).properties).toMatchObject({
      image: expect.any(Object),
      engine: expect.any(Object),
      mounts: expect.any(Object),
    });
  });

  it("merges variant object fields so oneOf definitions remain form-editable", () => {
    const runtimes = resolveSchema(topLevelSchema("runtimes"));
    const runtime = resolveSchema(runtimes.additionalProperties as Record<string, unknown>);
    const source = resolveSchema(childSchema(runtime, "source"));
    expect(source.properties).toMatchObject({
      type: expect.any(Object),
      repository: expect.any(Object),
      image: expect.any(Object),
    });
  });

  it("initializes required object fields without requiring raw JSON", () => {
    expect(defaultForSchema({
      type: "object",
      required: ["proxy", "models"],
      properties: {
        proxy: { type: "string" },
        models: { type: "array", items: { type: "string" } },
        optional: { type: "boolean" },
      },
    })).toEqual({ proxy: "", models: [] });
  });

  it("materializes declared defaults for a structured form", () => {
    expect(defaultForSchema({
      type: "object",
      default: {},
      properties: {
        maxFiles: { type: "integer", default: 2000 },
        maxDepth: { type: "integer", default: 8 },
        downloads: { type: "object", properties: { enabled: { type: "boolean", default: true } } },
      },
    })).toEqual({ maxFiles: 2000, maxDepth: 8 });
  });
});
