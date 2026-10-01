import { describe, expect, it } from "vitest";
import {
  buildLongContextPrompt,
  createBenchmarkPlans,
  estimateTokens,
  runWithConcurrency,
  summarizeBenchmarkRuns,
  type BenchmarkQueueEntry,
  type BenchmarkRunSample,
} from "./playgroundBenchmark";

const queue: BenchmarkQueueEntry[] = [
  { id: "entry-a", model: "model-a" },
  { id: "entry-b", model: "model-b" },
];

describe("playground benchmark plans", () => {
  it("creates one request per queued entry for the concurrency test", () => {
    const plans = createBenchmarkPlans({
      mode: "concurrency",
      queue,
      prompt: "hello",
      maxTokens: 32,
    });

    expect(plans).toHaveLength(2);
    expect(plans.map((plan) => ({ id: plan.id, model: plan.model, kind: plan.kind }))).toEqual([
      { id: "entry-a", model: "model-a", kind: "request" },
      { id: "entry-b", model: "model-b", kind: "request" },
    ]);
    expect(plans[0].prompt).toBe("hello");
    expect(plans[0].maxTokens).toBe(32);
  });

  it("builds a measured long prompt and keeps the user query at the end", () => {
    const prompt = buildLongContextPrompt(2_000, "What is the answer?");
    expect(prompt.endsWith("What is the answer?")).toBe(true);
    expect(estimateTokens(prompt)).toBeGreaterThanOrEqual(2_000);

    const plans = createBenchmarkPlans({
      mode: "long-context",
      queue: [queue[0]],
      prompt: "What is the answer?",
      maxTokens: 4,
      contextTokens: 2_000,
    });
    expect(plans).toHaveLength(1);
    expect(plans[0].kind).toBe("request");
    expect(plans[0].estimatedPromptTokens).toBeGreaterThanOrEqual(2_000);
    expect(plans[0].prompt.endsWith("What is the answer?")).toBe(true);
  });

  it("creates prefill requests plus a short decode probe for async prefill", () => {
    const plans = createBenchmarkPlans({
      mode: "async-prefill",
      queue: [queue[0]],
      prompt: "Return OK.",
      maxTokens: 16,
      contextTokens: 4_000,
      prefillRequests: 3,
    });

    expect(plans.map((plan) => plan.kind)).toEqual(["prefill", "prefill", "prefill", "probe"]);
    expect(plans.slice(0, 3).every((plan) => plan.maxTokens === 1)).toBe(true);
    expect(plans[3]).toMatchObject({ kind: "probe", maxTokens: 16, prompt: "Return OK." });
    expect(new Set(plans.map((plan) => plan.id)).size).toBe(plans.length);
  });
});

describe("runWithConcurrency", () => {
  it("never exceeds the requested number of active tasks", async () => {
    let active = 0;
    let peak = 0;
    const seen: number[] = [];

    await runWithConcurrency([1, 2, 3, 4, 5], 2, async (value) => {
      active++;
      peak = Math.max(peak, active);
      seen.push(value);
      await new Promise((resolve) => setTimeout(resolve, 0));
      active--;
    });

    expect(peak).toBe(2);
    expect(seen.sort((a, b) => a - b)).toEqual([1, 2, 3, 4, 5]);
  });
});

describe("summarizeBenchmarkRuns", () => {
  it("reports completed, failed and cancelled runs separately", () => {
    const samples: BenchmarkRunSample[] = [
      { status: "done", elapsedMs: 1_000, firstTokenMs: 200, promptTokens: 1_000, outputTokens: 100 },
      { status: "done", elapsedMs: 2_000, firstTokenMs: 400, promptTokens: 2_000, outputTokens: 200 },
      { status: "error", elapsedMs: 500, firstTokenMs: null, promptTokens: 3_000, outputTokens: 0 },
      { status: "cancelled", elapsedMs: 300, firstTokenMs: null, promptTokens: 4_000, outputTokens: 0 },
    ];

    expect(summarizeBenchmarkRuns(samples)).toEqual({
      total: 4,
      completed: 2,
      failed: 1,
      cancelled: 1,
      running: 0,
      pending: 0,
      totalPromptTokens: 10_000,
      totalOutputTokens: 300,
      averageFirstTokenMs: 300,
      p50FirstTokenMs: 300,
      p95FirstTokenMs: 390,
      wallTimeMs: 2_000,
      outputTokensPerSecond: 150,
    });
  });

  it("tracks active work and ignores missing first-token samples in percentiles", () => {
    const samples: BenchmarkRunSample[] = [
      { status: "waiting", elapsedMs: 0, firstTokenMs: null, promptTokens: 100, outputTokens: 0 },
      { status: "streaming", elapsedMs: 500, firstTokenMs: 250, promptTokens: 100, outputTokens: 10 },
      { status: "done", elapsedMs: 800, firstTokenMs: 100, promptTokens: 100, outputTokens: 20 },
    ];

    expect(summarizeBenchmarkRuns(samples)).toMatchObject({
      running: 1,
      pending: 1,
      p50FirstTokenMs: 175,
      p95FirstTokenMs: 242.5,
    });
  });
});
