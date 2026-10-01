export interface PrometheusSample {
  name: string;
  labels: Record<string, string>;
  value: number;
}

export interface LMCacheMetricSummary {
  lookupRequested?: number;
  lookupHit?: number;
  lookupHitRatio?: number;
  l1MemoryUsageBytes?: number;
  l1UsageRatio?: number;
  l2UsageBytes: Array<{ name: string; value: number }>;
  activePrefetchJobs?: number;
  failures?: number;
}

const samplePattern = /^([A-Za-z_:][A-Za-z0-9_.:-]*)(?:\{([^}]*)\})?\s+([-+]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][-+]?\d+)?|NaN|[+-]Inf)(?:\s+\d+)?\s*$/;
const labelPattern = /([A-Za-z_][A-Za-z0-9_]*)\s*=\s*("(?:\\.|[^"\\])*")/g;

function decodeLabel(value: string): string {
  try {
    return JSON.parse(value) as string;
  } catch {
    return value.slice(1, -1);
  }
}

function parseValue(value: string): number | undefined {
  if (value === "NaN") return Number.NaN;
  if (value === "+Inf") return Number.POSITIVE_INFINITY;
  if (value === "-Inf") return Number.NEGATIVE_INFINITY;
  const parsed = Number(value);
  return Number.isFinite(parsed) ? parsed : undefined;
}

function parseLabels(source = ""): Record<string, string> {
  const labels: Record<string, string> = {};
  labelPattern.lastIndex = 0;
  let match: RegExpExecArray | null;
  while ((match = labelPattern.exec(source)) !== null) {
    labels[match[1]] = decodeLabel(match[2]);
  }
  return labels;
}

export function parsePrometheusMetrics(text: string): PrometheusSample[] {
  if (!text.trim()) return [];
  const samples: PrometheusSample[] = [];
  for (const line of text.split(/\r?\n/)) {
    const trimmed = line.trim();
    if (!trimmed || trimmed.startsWith("#")) continue;
    const match = samplePattern.exec(trimmed);
    if (!match) continue;
    const value = parseValue(match[3]);
    if (value === undefined) continue;
    samples.push({ name: match[1], labels: parseLabels(match[2]), value });
  }
  return samples;
}

function metricEndsWith(name: string, suffix: string): boolean {
  const normalized = name.toLowerCase().replace(/[.:]/g, "_");
  return normalized === suffix || normalized.endsWith(`_${suffix}`);
}

function sumMetrics(samples: PrometheusSample[], suffixes: string | string[]): number | undefined {
  const accepted = Array.isArray(suffixes) ? suffixes : [suffixes];
  const values = samples
    .filter((sample) => accepted.some((suffix) => metricEndsWith(sample.name, suffix) || metricEndsWith(sample.name, `${suffix}_total`)))
    .map((sample) => sample.value)
    .filter((value) => Number.isFinite(value));
  if (values.length === 0) return undefined;
  return values.reduce((total, value) => total + value, 0);
}

function oneMetric(samples: PrometheusSample[], suffix: string): number | undefined {
  const matches = samples.filter((sample) => metricEndsWith(sample.name, suffix) && Number.isFinite(sample.value));
  return matches.length > 0 ? matches[matches.length - 1].value : undefined;
}

export function summarizeLMCacheMetrics(samples: PrometheusSample[]): LMCacheMetricSummary {
  const lookupRequested = sumMetrics(samples, ["lookup_requested_tokens", "lookup_requested"]);
  const lookupHit = sumMetrics(samples, ["lookup_hit_tokens", "lookup_hit"]);
  const l1MemoryUsageBytes = oneMetric(samples, "l1_memory_usage_bytes");
  const l1UsageRatio = oneMetric(samples, "l1_usage_ratio");
  const requested = lookupRequested ?? 0;
  const hit = lookupHit ?? 0;
  const lookupHitRatio = requested > 0 ? hit / requested : undefined;
  const l2ByName = new Map<string, number>();
  for (const sample of samples) {
    if (!metricEndsWith(sample.name, "l2_usage_bytes") || !Number.isFinite(sample.value)) continue;
    const name = sample.labels.l2_name || sample.labels.l2Name || sample.labels.adapter || sample.labels.name || "default";
    l2ByName.set(name, (l2ByName.get(name) ?? 0) + sample.value);
  }
  return {
    lookupRequested,
    lookupHit,
    lookupHitRatio,
    l1MemoryUsageBytes,
    l1UsageRatio,
    l2UsageBytes: [...l2ByName.entries()].map(([name, value]) => ({ name, value })).sort((left, right) => left.name.localeCompare(right.name)),
    activePrefetchJobs: oneMetric(samples, "active_prefetch_jobs"),
    failures: sumMetrics(samples, "failure"),
  };
}

export function summarizeLMCacheMetricsText(text: string): LMCacheMetricSummary {
  return summarizeLMCacheMetrics(parsePrometheusMetrics(text));
}
