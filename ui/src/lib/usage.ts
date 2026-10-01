import { requestJson } from "./http";

const request = requestJson;

export type UsageGranularity = "minute" | "hour" | "day";

export interface UsageTotals {
  requests: number;
  inputTokens: number;
  outputTokens: number;
  cachedTokens: number;
  cacheCreationTokens: number;
  reasoningTokens: number;
  totalTokens: number;
  estimatedCost: number;
  costEstimated: boolean;
  averageDurationMs: number;
  averageFirstTokenMs: number;
  cacheHitRatio: number;
  cacheCreationRatio: number;
  promptsPerSecond: number;
  generationPerSecond: number;
}

export interface UsageDimensionRow {
  name: string;
  requests: number;
  inputTokens: number;
  outputTokens: number;
  cachedTokens: number;
  cacheCreationTokens: number;
  estimatedCost: number;
}

export interface UsageTrendBucket {
  bucketStart: string;
  requests: number;
  inputTokens: number;
  outputTokens: number;
  cachedTokens: number;
  cacheCreationTokens: number;
  cacheHitRatio: number;
}

export interface UsageAnalytics {
  start: string;
  end: string;
  granularity: UsageGranularity;
  totals: UsageTotals;
  byModel: UsageDimensionRow[];
  byGroup: UsageDimensionRow[];
  byEndpoint: UsageDimensionRow[];
  byKey: UsageDimensionRow[];
  trend: UsageTrendBucket[];
}

export interface UsageRecord {
  id: number;
  timestamp: string;
  model: string;
  keyId: string;
  keyName: string;
  keyKind: string;
  keyGroup: string;
  endpoint: string;
  clientIp: string;
  respStatusCode: number;
  inputTokens: number;
  outputTokens: number;
  cachedTokens: number;
  cacheCreationTokens: number;
  reasoningTokens: number;
  estimatedCost: number;
  costEstimated: boolean;
  durationMs: number;
  firstTokenMs: number;
}

export interface UsageRecordPage {
  data: UsageRecord[];
  total: number;
  limit: number;
  offset: number;
  totalPages: number;
}

export interface UsageKeyOption {
  id: string;
  name: string;
  kind: string;
  group: string;
}

export interface UsageFilterOptions {
  keys: UsageKeyOption[];
  models: string[];
  groups: string[];
  endpoints: string[];
}

/** The filter selection shared by every usage request. */
export interface UsageFilters {
  start?: string;
  end?: string;
  keys: string[];
  models: string[];
  groups: string[];
  endpoints: string[];
  granularity: UsageGranularity;
}

export const emptyUsageFilters = (): UsageFilters => ({
  keys: [],
  models: [],
  groups: [],
  endpoints: [],
  granularity: "day",
});

/**
 * Builds the query string for a usage request. List filters repeat, which the
 * server accepts alongside comma-separated single values.
 */
export function usageFiltersToQuery(filters: UsageFilters, extra: Record<string, string> = {}): string {
  const params = new URLSearchParams();
  for (const [field, values] of [
    ["key", filters.keys],
    ["model", filters.models],
    ["group", filters.groups],
    ["endpoint", filters.endpoints],
  ] as const) {
    for (const value of values) params.append(field, value);
  }
  if (filters.start) params.set("start", filters.start);
  if (filters.end) params.set("end", filters.end);
  params.set("granularity", filters.granularity);
  for (const [key, value] of Object.entries(extra)) {
    if (value) params.set(key, value);
  }
  const query = params.toString();
  return query ? `?${query}` : "";
}

export async function fetchUsageAnalytics(filters: UsageFilters): Promise<UsageAnalytics> {
  return request<UsageAnalytics>(`/api/usage/analytics${usageFiltersToQuery(filters)}`);
}

export async function fetchUsageFilterOptions(filters: UsageFilters): Promise<UsageFilterOptions> {
  return request<UsageFilterOptions>(`/api/usage/options${usageFiltersToQuery(filters)}`);
}

export async function fetchUsageRecords(filters: UsageFilters, limit = 100, offset = 0, sort = "time", order = "desc"): Promise<UsageRecordPage> {
  return request<UsageRecordPage>(
    `/api/usage/records${usageFiltersToQuery(filters, { limit: String(limit), offset: String(offset), sort, order })}`,
  );
}

/** Downloads the filtered selection as CSV without a round-trip through the page. */
export function usageExportUrl(filters: UsageFilters): string {
  return `/api/usage/export.csv${usageFiltersToQuery(filters)}`;
}

/**
 * Compact token counts: 1.2M / 340.5K / 812. Kept in one place so the cards,
 * charts and detail table never disagree about rounding.
 */
export function formatTokenCount(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return "0";
  if (value >= 1_000_000_000) return `${(value / 1_000_000_000).toFixed(2)}B`;
  if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(value >= 10_000_000 ? 1 : 2)}M`;
  if (value >= 1_000) return `${(value / 1_000).toFixed(value >= 10_000 ? 1 : 2)}K`;
  return String(Math.round(value));
}

/** Named presets for the time-range picker, resolved against the local clock. */
export interface UsageRangePreset {
  id: string;
  labelKey: string;
  start: () => Date;
  end?: () => Date;
}

export const usageRangePresets: UsageRangePreset[] = [
  { id: "today", labelKey: "usage.rangeToday", start: () => startOfDay(new Date()) },
  { id: "yesterday", labelKey: "usage.rangeYesterday", start: () => startOfDay(addDays(new Date(), -1)), end: () => startOfDay(new Date()) },
  { id: "7d", labelKey: "usage.range7d", start: () => addDays(new Date(), -6) },
  { id: "30d", labelKey: "usage.range30d", start: () => addDays(new Date(), -29) },
  { id: "month", labelKey: "usage.rangeMonth", start: () => new Date(new Date().getFullYear(), new Date().getMonth(), 1) },
];

function startOfDay(date: Date): Date {
  return new Date(date.getFullYear(), date.getMonth(), date.getDate());
}

function addDays(date: Date, days: number): Date {
  const next = new Date(date);
  next.setDate(next.getDate() + days);
  return next;
}
