import { describe, expect, it } from "vitest";
import { buildExtensionRequest, chooseExtensionEndpoint, changedRequestKeys, summarizeRuns } from "./extensionWorkbench";
const fields = { model: " test ", prompt: "Hello", system: "Be brief", temperature: 0.5, maxTokens: 32 };
describe("extension workbench", () => {
  it("prefers a supported extension endpoint and safely defaults without protocol metadata", () => {
    expect(chooseExtensionEndpoint(["embeddings", "anthropic.messages", "responses"])).toBe("anthropic.messages");
    expect(chooseExtensionEndpoint(["responses"])).toBe("responses");
    expect(chooseExtensionEndpoint(["embeddings"])).toBe("chat.completions");
    expect(chooseExtensionEndpoint([])).toBe("chat.completions");
  });
  it("builds each endpoint's native request shape", () => {
    expect(buildExtensionRequest("chat.completions", fields)).toEqual({ model: "test", temperature: .5, max_tokens: 32, messages: [{ role: "system", content: "Be brief" }, { role: "user", content: "Hello" }] });
    expect(buildExtensionRequest("responses", fields)).toEqual({ model: "test", temperature: .5, max_output_tokens: 32, instructions: "Be brief", input: "Hello" });
    expect(buildExtensionRequest("anthropic.messages", fields)).toEqual({ model: "test", temperature: .5, max_tokens: 32, system: "Be brief", messages: [{ role: "user", content: "Hello" }] });
  });
  it("does not count skipped or failed requests as successful latency samples", () => {
    const result = summarizeRuns([{ duration: 10, outcome: "success" }, { duration: 20, outcome: "success" }, { duration: 1, outcome: "unmatched" }, { duration: 1000, outcome: "error" }]);
    expect(result).toEqual({ total: 4, success: 2, errors: 1, unmatched: 1, min: 10, max: 20, mean: 15, p50: 10, p95: 20 });
    expect(summarizeRuns([]).mean).toBeNull();
    expect(summarizeRuns([{ duration: 1, outcome: "unmatched" }]).p95).toBeNull();
  });
  it("keeps successful requests with missing or invalid timing out of latency statistics", () => {
    const result = summarizeRuns([
      { duration: null, outcome: "success" },
      { duration: Number.NaN, outcome: "success" },
      { duration: Number.POSITIVE_INFINITY, outcome: "success" },
      { duration: -1, outcome: "success" },
      { duration: 0, outcome: "success" },
    ]);
    expect(result.success).toBe(5);
    expect(result.mean).toBe(0);
    expect(result.p95).toBe(0);
    expect(summarizeRuns([{ duration: null, outcome: "success" }]).min).toBeNull();
  });
  it("uses nearest-rank percentiles across a complete repetition set", () => {
    const result = summarizeRuns(Array.from({ length: 100 }, (_, i) => ({ duration: 100 - i, outcome: "success" as const })));
    expect(result.p50).toBe(50);
    expect(result.p95).toBe(95);
    expect(result.mean).toBe(50.5);
  });
  it("detects added, removed and modified request fields", () => {
    expect(changedRequestKeys({ same: 1, removed: true, nested: { a: 1 } }, { same: 1, added: [], nested: { a: 2 } })).toEqual(["removed", "nested", "added"]);
  });
});
