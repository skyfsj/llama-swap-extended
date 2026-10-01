export type BenchmarkMode = "concurrency" | "long-context" | "async-prefill";
export type BenchmarkRequestKind = "request" | "prefill" | "probe";
export type BenchmarkRunStatus = "waiting" | "streaming" | "done" | "error" | "cancelled";
export type BenchmarkPhase = "waiting" | "loading" | "reasoning" | "content";

export interface BenchmarkQueueEntry {
  id: string;
  model: string;
}

export interface BenchmarkSettings {
  mode: BenchmarkMode;
  queue: readonly BenchmarkQueueEntry[];
  prompt: string;
  maxTokens?: number;
  contextTokens?: number;
  prefillRequests?: number;
}

export interface BenchmarkPlan {
  id: string;
  entryId: string;
  model: string;
  kind: BenchmarkRequestKind;
  prompt: string;
  maxTokens: number;
  estimatedPromptTokens: number;
}

export interface BenchmarkRunSample {
  status: BenchmarkRunStatus;
  elapsedMs: number;
  firstTokenMs: number | null;
  promptTokens: number;
  outputTokens: number;
}

export interface BenchmarkRunView extends BenchmarkRunSample {
  kind: BenchmarkRequestKind;
  loadingText: string;
  reasoningContent: string;
  content: string;
  loadingDone: boolean;
  waitingMs: number;
  loadingMs: number;
  reasoningMs: number;
  contentMs: number;
  phase: BenchmarkPhase;
  error?: string;
}

export interface BenchmarkSummary {
  total: number;
  completed: number;
  failed: number;
  cancelled: number;
  running: number;
  pending: number;
  totalPromptTokens: number;
  totalOutputTokens: number;
  averageFirstTokenMs: number | null;
  p50FirstTokenMs: number | null;
  p95FirstTokenMs: number | null;
  wallTimeMs: number;
  outputTokensPerSecond: number;
}

export const DEFAULT_CONTEXT_TOKENS = 8_192;
export const DEFAULT_PREFILL_REQUESTS = 2;
export const MAX_CONTEXT_TOKENS = 1_000_000;

const DEFAULT_MAX_TOKENS = 256;
const MAX_OUTPUT_TOKENS = 65_536;
const CHARS_PER_ESTIMATED_TOKEN = 4;
const LONG_CONTEXT_FILLER =
  "This paragraph is generated for a repeatable long context benchmark. It provides neutral text so prompt processing can be measured without relying on a particular document. ";
const DEFAULT_PROBE_PROMPT = "Reply with one short word.";

function integerInRange(value: number | undefined, fallback: number, min: number, max: number): number {
  if (value === undefined || !Number.isFinite(value)) return fallback;
  return Math.min(max, Math.max(min, Math.trunc(value)));
}

/**
 * Estimates tokens for synthetic benchmark text. The result is deliberately
 * labelled as an estimate in the UI: the actual tokenizer belongs to the
 * selected model and is not available in the browser.
 */
export function estimateTokens(text: string): number {
  const characters = Array.from(text).length;
  return characters === 0 ? 0 : Math.max(1, Math.ceil(characters / CHARS_PER_ESTIMATED_TOKEN));
}

/**
 * Builds a deterministic prompt of roughly targetTokens tokens and appends
 * the user's query at the end, where the model will attend to it naturally.
 */
export function buildLongContextPrompt(targetTokens: number, query = ""): string {
  const tokens = integerInRange(targetTokens, DEFAULT_CONTEXT_TOKENS, 1, MAX_CONTEXT_TOKENS);
  const targetCharacters = tokens * CHARS_PER_ESTIMATED_TOKEN;
  const suffix = query.trim();
  const separator = suffix ? "\n\n" : "";
  const fillerCharacters = Math.max(0, targetCharacters - separator.length - suffix.length);

  let filler = "";
  while (filler.length < fillerCharacters) filler += LONG_CONTEXT_FILLER;
  return filler.slice(0, fillerCharacters) + separator + suffix;
}

function makePlan(
  entry: BenchmarkQueueEntry,
  id: string,
  kind: BenchmarkRequestKind,
  prompt: string,
  maxTokens: number,
): BenchmarkPlan {
  return {
    id,
    entryId: entry.id,
    model: entry.model,
    kind,
    prompt,
    maxTokens,
    estimatedPromptTokens: estimateTokens(prompt),
  };
}

/** Converts the selected benchmark mode into immutable request descriptions. */
export function createBenchmarkPlans(settings: BenchmarkSettings): BenchmarkPlan[] {
  const maxTokens = integerInRange(settings.maxTokens, DEFAULT_MAX_TOKENS, 1, MAX_OUTPUT_TOKENS);
  const requestPrompt = settings.prompt.trim() || DEFAULT_PROBE_PROMPT;

  if (settings.mode === "concurrency") {
    return settings.queue.map((entry) => makePlan(entry, entry.id, "request", requestPrompt, maxTokens));
  }

  const contextPrompt = buildLongContextPrompt(settings.contextTokens ?? DEFAULT_CONTEXT_TOKENS, requestPrompt);
  if (settings.mode === "long-context") {
    return settings.queue.map((entry) => makePlan(entry, entry.id, "request", contextPrompt, maxTokens));
  }

  const prefillRequests = integerInRange(
    settings.prefillRequests,
    DEFAULT_PREFILL_REQUESTS,
    1,
    32,
  );
  const plans: BenchmarkPlan[] = [];
  for (const entry of settings.queue) {
    for (let index = 0; index < prefillRequests; index++) {
      plans.push(
        makePlan(
          entry,
          `${entry.id}:prefill:${index + 1}`,
          "prefill",
          contextPrompt,
          1,
        ),
      );
    }
    plans.push(makePlan(entry, `${entry.id}:probe`, "probe", requestPrompt, maxTokens));
  }
  return plans;
}

/** Runs a list with a bounded number of active workers; zero means unlimited. */
export async function runWithConcurrency<T>(
  items: readonly T[],
  limit: number,
  worker: (item: T, index: number) => Promise<void>,
): Promise<void> {
  if (items.length === 0) return;

  const requestedWorkers = Number.isFinite(limit) && limit > 0 ? Math.floor(limit) : items.length;
  const workerCount = Math.max(1, Math.min(items.length, requestedWorkers));
  let nextIndex = 0;

  const runWorker = async (): Promise<void> => {
    while (true) {
      const index = nextIndex++;
      if (index >= items.length) return;
      await worker(items[index], index);
    }
  };

  await Promise.all(Array.from({ length: workerCount }, () => runWorker()));
}

/** Waits before starting a probe, while still allowing the run to be aborted. */
export function delayWithAbort(ms: number, signal?: AbortSignal): Promise<void> {
  if (ms <= 0) return Promise.resolve();

  return new Promise((resolve, reject) => {
    const timeout = globalThis.setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    const onAbort = () => {
      globalThis.clearTimeout(timeout);
      signal?.removeEventListener("abort", onAbort);
      reject(new DOMException("The operation was aborted", "AbortError"));
    };
    if (signal?.aborted) {
      onAbort();
      return;
    }
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}

export function summarizeBenchmarkRuns(samples: readonly BenchmarkRunSample[]): BenchmarkSummary {
  const firstTokenSamples = samples
    .map((sample) => sample.firstTokenMs)
    .filter((value): value is number => value !== null && Number.isFinite(value))
    .sort((left, right) => left - right);
  const wallTimeMs = Math.max(0, ...samples.map((sample) => sample.elapsedMs));
  const totalOutputTokens = samples.reduce((total, sample) => total + Math.max(0, sample.outputTokens), 0);

  const percentile = (fraction: number): number | null => {
    if (firstTokenSamples.length === 0) return null;
    if (firstTokenSamples.length === 1) return firstTokenSamples[0];
    const position = (firstTokenSamples.length - 1) * fraction;
    const lowerIndex = Math.floor(position);
    const upperIndex = Math.ceil(position);
    const weight = position - lowerIndex;
    return firstTokenSamples[lowerIndex] * (1 - weight) + firstTokenSamples[upperIndex] * weight;
  };

  return {
    total: samples.length,
    completed: samples.filter((sample) => sample.status === "done").length,
    failed: samples.filter((sample) => sample.status === "error").length,
    cancelled: samples.filter((sample) => sample.status === "cancelled").length,
    running: samples.filter((sample) => sample.status === "streaming").length,
    pending: samples.filter((sample) => sample.status === "waiting").length,
    totalPromptTokens: samples.reduce((total, sample) => total + Math.max(0, sample.promptTokens), 0),
    totalOutputTokens,
    averageFirstTokenMs: firstTokenSamples.length > 0
      ? firstTokenSamples.reduce((total, value) => total + value, 0) / firstTokenSamples.length
      : null,
    p50FirstTokenMs: percentile(0.5),
    p95FirstTokenMs: percentile(0.95),
    wallTimeMs,
    outputTokensPerSecond: wallTimeMs > 0 ? totalOutputTokens / (wallTimeMs / 1_000) : 0,
  };
}
