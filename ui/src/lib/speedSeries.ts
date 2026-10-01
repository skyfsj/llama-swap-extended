// Helpers for the activity page's inference-speed section: request building for
// /api/metrics/speed, aggregation of the returned buckets, and the formatting
// the charts and tables need. Kept free of Svelte imports so the maths is unit
// testable.

import type { ActivityFilters } from "./activityFilters";
import type { ContextBucketData, ContextSeriesData, SpeedPointData, SpeedReportData, SpeedSeriesData } from "./types";

export type SpeedWindowKey = "hour" | "6h" | "day" | "week" | "all";

export interface SpeedWindowOption {
  key: SpeedWindowKey;
  labelKey: string;
  /** Window length in ms; 0 means the whole history. */
  ms: number;
}

export const SPEED_WINDOW_OPTIONS: readonly SpeedWindowOption[] = [
  { key: "hour", labelKey: "activity.speed.windows.hour", ms: 60 * 60 * 1000 },
  { key: "6h", labelKey: "activity.speed.windows.sixHours", ms: 6 * 60 * 60 * 1000 },
  { key: "day", labelKey: "activity.speed.windows.day", ms: 24 * 60 * 60 * 1000 },
  { key: "week", labelKey: "activity.speed.windows.week", ms: 7 * 24 * 60 * 60 * 1000 },
  { key: "all", labelKey: "activity.speed.windows.all", ms: 0 },
];

export const SPEED_WINDOW_LABEL_KEYS: Record<SpeedWindowKey, string> = {
  hour: "activity.speed.windows.hour",
  "6h": "activity.speed.windows.sixHours",
  day: "activity.speed.windows.day",
  week: "activity.speed.windows.week",
  all: "activity.speed.windows.all",
};

export function isSpeedWindowKey(value: string): value is SpeedWindowKey {
  return SPEED_WINDOW_OPTIONS.some((option) => option.key === value);
}

export interface SpeedQuery {
  /** Model the charts are drawn for; empty means the server decides (all models). */
  model: string;
  /** Page-level drawer filters: key/session scope still applies. */
  filters: ActivityFilters;
  /** The section's own time window. */
  window: SpeedWindowKey;
  configuredOnly?: boolean;
}

/**
 * buildSpeedQueryParams writes the /api/metrics/speed query. The section's time
 * window is authoritative over the drawer's range so the quick range control
 * always means what it says; the drawer's model, key and session scope still
 * narrow the rows.
 */
export function buildSpeedQueryParams(query: SpeedQuery, now = new Date()): URLSearchParams {
  const params = new URLSearchParams();
  const model = query.model.trim();
  if (model !== "") params.set("model", model);

  const filters = query.filters;
  const keyID = filters.keyID.trim();
  if (keyID !== "" && !params.has("key_id")) params.set("key_id", keyID);
  const sessionID = filters.sessionID.trim();
  if (sessionID !== "") params.set("session_id", sessionID);

  if (query.window !== "all") {
    const option = SPEED_WINDOW_OPTIONS.find((candidate) => candidate.key === query.window);
    if (option && option.ms > 0) {
      params.set("start", new Date(now.getTime() - option.ms).toISOString());
      params.set("end", now.toISOString());
    }
  }
  if (query.configuredOnly) params.set("configured_only", "true");
  return params;
}

/** Unknown sentinel: the server reports a negative value for "not measured". */
function isKnown(value: number): boolean {
  return Number.isFinite(value) && value >= 0;
}

/**
 * speedWindowStart returns the inclusive start of a quick range, or null for
 * the whole history. Used by the chart to label the window.
 */
export function speedWindowStart(window: SpeedWindowKey, now = new Date()): Date | null {
  if (window === "all") return null;
  const option = SPEED_WINDOW_OPTIONS.find((candidate) => candidate.key === window);
  if (!option || option.ms <= 0) return null;
  return new Date(now.getTime() - option.ms);
}

export interface SpeedAverages {
  requests: number;
  prefillTps: number;
  decodeTps: number;
  ttftSeconds: number;
  /** Average decode time per generated token in ms, derived from the decode rate. */
  perTokenLatencyMs: number;
}

/**
 * speedAverages averages a model's buckets. Bucket means are weighted by the
 * number of requests in each bucket, otherwise a quiet hour with a handful of
 * requests counts as much as a busy one.
 */
export function speedAverages(points: SpeedPointData[]): SpeedAverages {
  const averages: SpeedAverages = {
    requests: 0,
    prefillTps: -1,
    decodeTps: -1,
    ttftSeconds: -1,
    perTokenLatencyMs: -1,
  };
  let weight = 0;
  for (const point of points) {
    averages.requests += point.requests > 0 ? point.requests : 0;
  }
  for (const point of points) {
    const requests = point.requests > 0 ? point.requests : 0;
    if (requests === 0) continue;
    weight += requests;
    if (isKnown(point.prefill_tps)) {
      averages.prefillTps = averages.prefillTps < 0 ? 0 : averages.prefillTps;
      averages.prefillTps += point.prefill_tps * requests;
    }
    if (isKnown(point.decode_tps)) {
      averages.decodeTps = averages.decodeTps < 0 ? 0 : averages.decodeTps;
      averages.decodeTps += point.decode_tps * requests;
    }
    if (isKnown(point.ttft_ms)) {
      averages.ttftSeconds = averages.ttftSeconds < 0 ? 0 : averages.ttftSeconds;
      averages.ttftSeconds += (point.ttft_ms / 1000) * requests;
    }
  }
  if (weight > 0) {
    if (averages.prefillTps >= 0) averages.prefillTps /= weight;
    if (averages.decodeTps >= 0) averages.decodeTps /= weight;
    if (averages.ttftSeconds >= 0) averages.ttftSeconds /= weight;
  }
  averages.perTokenLatencyMs = averages.decodeTps > 0 ? 1000 / averages.decodeTps : -1;
  return averages;
}

export interface SpeedTrend {
  /** Fractional change of the recent half versus the earlier half; null when
   * either half has no measurement. */
  prefill: number | null;
  decode: number | null;
  ttft: number | null;
}

/**
 * speedTrend compares the recent half of the window with the earlier half,
 * giving each bucket its request weight. It is a trend indicator for the KPI
 * cards, not a statistics-grade period-over-period delta.
 */
export function speedTrend(points: SpeedPointData[]): SpeedTrend {
  const halves = splitPoints(points);
  return {
    prefill: trendFrom(halves.earlier, halves.recent, (point) => point.prefill_tps),
    decode: trendFrom(halves.earlier, halves.recent, (point) => point.decode_tps),
    ttft: trendFrom(halves.earlier, halves.recent, (point) => point.ttft_ms),
  };
}

function splitPoints(points: SpeedPointData[]): { earlier: SpeedPointData[]; recent: SpeedPointData[] } {
  if (points.length < 2) return { earlier: [], recent: points };
  const middle = Math.floor(points.length / 2);
  return { earlier: points.slice(0, middle), recent: points.slice(middle) };
}

function trendFrom(earlier: SpeedPointData[], recent: SpeedPointData[], pick: (point: SpeedPointData) => number): number | null {
  const before = weightedMean(earlier, pick);
  const after = weightedMean(recent, pick);
  if (before === null || after === null || before === 0 || !Number.isFinite(before) || !Number.isFinite(after)) {
    return null;
  }
  return (after - before) / before;
}

function weightedMean(points: SpeedPointData[], pick: (point: SpeedPointData) => number): number | null {
  let sum = 0;
  let weight = 0;
  for (const point of points) {
    if (point.requests <= 0) continue;
    const value = pick(point);
    if (!isKnown(value)) continue;
    sum += value * point.requests;
    weight += point.requests;
  }
  if (weight === 0) return null;
  return sum / weight;
}

export interface SpeedChartData {
  labels: string[];
  prefill: (number | null)[];
  decode: (number | null)[];
  ttftSeconds: (number | null)[];
}

/**
 * speedChartData turns a model's buckets into chart series. Unknown values
 * become null so Chart.js leaves a gap instead of drawing the line to zero.
 * Labels carry a date once the buckets are wide enough that a time of day alone
 * would be ambiguous.
 */
export function speedChartData(points: SpeedPointData[], bucketSeconds = 0): SpeedChartData {
  return {
    labels: points.map((point) => formatBucketTime(point.timestamp, bucketSeconds)),
    prefill: points.map((point) => (isKnown(point.prefill_tps) ? point.prefill_tps : null)),
    decode: points.map((point) => (isKnown(point.decode_tps) ? point.decode_tps : null)),
    ttftSeconds: points.map((point) => (isKnown(point.ttft_ms) ? point.ttft_ms / 1000 : null)),
  };
}

export function formatBucketTime(timestamp: string, bucketSeconds = 0): string {
  const date = new Date(timestamp);
  if (Number.isNaN(date.getTime())) return "";
  const pad = (value: number) => String(value).padStart(2, "0");
  const time = `${pad(date.getHours())}:${pad(date.getMinutes())}`;
  // A day-wide bucket repeats the same time of day every day, so the label needs
  // the date to identify the bucket at all.
  if (bucketSeconds >= 24 * 60 * 60) return `${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
  if (bucketSeconds >= 60 * 60) return `${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${time}`;
  return time;
}

export function formatBucketTimeFull(timestamp: string): string {
  const date = new Date(timestamp);
  if (Number.isNaN(date.getTime())) return timestamp;
  const pad = (value: number) => String(value).padStart(2, "0");
  return `${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

export function selectedSpeedSeries(report: SpeedReportData | null, model: string): SpeedSeriesData | null {
  if (!report || report.series.length === 0) return null;
  const wanted = model.trim();
  if (wanted === "") return report.series[0];
  return report.series.find((series) => series.model === wanted) ?? report.series[0];
}

export function selectedContextSeries(report: SpeedReportData | null, model: string): ContextSeriesData | null {
  if (!report || report.context.length === 0) return null;
  const wanted = model.trim();
  if (wanted === "") return report.context[0];
  return report.context.find((series) => series.model === wanted) ?? report.context[0];
}

/** formatTokens renders a token count as the compact bucket label (4K, 1M). */
export function formatTokens(tokens: number): string {
  if (!Number.isFinite(tokens) || tokens <= 0) return "0";
  if (tokens >= 1024 * 1024) {
    const millions = tokens / (1024 * 1024);
    return `${roundTokenValue(millions)}M`;
  }
  if (tokens >= 1024) {
    return `${roundTokenValue(tokens / 1024)}K`;
  }
  return String(tokens);
}

function roundTokenValue(value: number): string {
  return Number.isInteger(value) ? String(value) : value.toFixed(1);
}

/** Chart axis label for a bucket: the upper bound, or ">1M" when open ended. */
export function contextBucketShortLabel(bucket: ContextBucketData): string {
  if (bucket.max_tokens === 0) return `>${formatTokens(bucket.min_tokens)}`;
  return formatTokens(bucket.max_tokens);
}

/** Table label for a bucket: the upper bound with the exact token count. */
export function contextBucketFullLabel(bucket: ContextBucketData): string {
  const exact = bucket.max_tokens === 0 ? bucket.min_tokens : bucket.max_tokens;
  if (bucket.max_tokens === 0) return `>${formatTokens(exact)} (${new Intl.NumberFormat("en-US").format(exact)}+)`;
  return `${formatTokens(exact)} (${new Intl.NumberFormat("en-US").format(exact)})`;
}

/** contextBucketsForModel flattens a report into one row per model and bucket. */
export function contextRows(report: SpeedReportData | null): { model: string; bucket: ContextBucketData }[] {
  if (!report) return [];
  const rows: { model: string; bucket: ContextBucketData }[] = [];
  for (const series of report.context) {
    for (const bucket of series.buckets) {
      rows.push({ model: series.model, bucket });
    }
  }
  return rows;
}

/** A metric the backend never reported renders as a dash, not as zero. */
export function rateOrDash(value: number): string {
  return isKnown(value) && value > 0 ? value.toFixed(1) : "—";
}

export function secondsOrDash(value: number): string {
  return isKnown(value) && value >= 0 ? value.toFixed(2) : "—";
}
