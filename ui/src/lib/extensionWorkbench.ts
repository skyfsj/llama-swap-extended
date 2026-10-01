export type RequestEndpoint = "chat.completions" | "responses" | "anthropic.messages";
export function chooseExtensionEndpoint(preferred: string[]): RequestEndpoint {
  return preferred.find((value): value is RequestEndpoint => value === "chat.completions" || value === "responses" || value === "anthropic.messages") ?? "chat.completions";
}
export interface RequestFields {
  model: string;
  prompt: string;
  system: string;
  temperature: number;
  maxTokens: number;
}
export function buildExtensionRequest(endpoint: RequestEndpoint, fields: RequestFields): Record<string, unknown> {
  const common = { model: fields.model.trim(), temperature: fields.temperature };
  if (endpoint === "responses") return { ...common, input: fields.prompt, ...(fields.system ? { instructions: fields.system } : {}), max_output_tokens: fields.maxTokens };
  const messages = [{ role: "user", content: fields.prompt }];
  if (endpoint === "anthropic.messages") return { ...common, messages, ...(fields.system ? { system: fields.system } : {}), max_tokens: fields.maxTokens };
  return { ...common, messages: fields.system ? [{ role: "system", content: fields.system }, ...messages] : messages, max_tokens: fields.maxTokens };
}
export interface RunSample { duration: number | null; roundTrip?: number; outcome: "success" | "error" | "unmatched"; error?: string }
export function summarizeRuns(samples: RunSample[]) {
  const times = samples.filter((sample) => sample.outcome === "success").map((sample) => sample.duration).filter((value): value is number => value !== null && Number.isFinite(value) && value >= 0).sort((a, b) => a - b);
  const percentile = (fraction: number) => times.length ? times[Math.max(0, Math.ceil(times.length * fraction) - 1)] : null;
  return { total: samples.length, success: samples.filter((s) => s.outcome === "success").length, errors: samples.filter((s) => s.outcome === "error").length, unmatched: samples.filter((s) => s.outcome === "unmatched").length, min: times[0] ?? null, max: times.at(-1) ?? null, mean: times.length ? times.reduce((a, b) => a + b, 0) / times.length : null, p50: percentile(.5), p95: percentile(.95) };
}
export function changedRequestKeys(before: Record<string, unknown>, after: Record<string, unknown>): string[] {
  return [...new Set([...Object.keys(before), ...Object.keys(after)])].filter((key) => JSON.stringify(before[key]) !== JSON.stringify(after[key]));
}
