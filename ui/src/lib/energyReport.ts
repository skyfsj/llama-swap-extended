/**
 * Pure helpers behind the activity page's energy panel. The server report
 * (/api/metrics/power) provides per-source power series and per-model energy
 * attribution; these helpers shape that payload for the chart and tables.
 */
import type { PowerReportData, PowerSeriesData } from "./types";
import { formatBucketTime } from "./speedSeries";

export const POWER_WINDOW_OPTIONS = [
  { key: "hour", api: "1h" },
  { key: "6h", api: "6h" },
  { key: "day", api: "24h" },
  { key: "week", api: "7d" },
  { key: "30d", api: "30d" },
] as const;

export type PowerWindowKey = (typeof POWER_WINDOW_OPTIONS)[number]["key"];

export const POWER_WINDOW_LABEL_KEYS: Record<PowerWindowKey, string> = {
  hour: "activity.speed.windows.hour",
  "6h": "activity.speed.windows.sixHours",
  day: "activity.speed.windows.day",
  week: "activity.speed.windows.week",
  "30d": "activity.energy.windows.thirtyDays",
};

export function isPowerWindowKey(value: string): value is PowerWindowKey {
  return POWER_WINDOW_OPTIONS.some((option) => option.key === value);
}

/** Chart.js line colors, mirroring the palette the speed panel uses. */
export const POWER_SERIES_COLORS = ["#8b5cf6", "#3b82f6", "#10b981", "#f59e0b", "#ec4899", "#06b6d4"];

export interface PowerChartData {
  labels: string[];
  values: (number | null)[][];
}

/**
 * powerChartData builds one aligned value array per series: every series
 * shares the union of timestamps so lines start and end together, and gaps
 * stay null instead of diving to zero.
 */
export function powerChartData(series: PowerSeriesData[], bucketSeconds = 0): PowerChartData {
  const labelToIndex = new Map<string, number>();
  const labels: string[] = [];
  const ensureLabel = (timestamp: string): number => {
    let index = labelToIndex.get(timestamp);
    if (index === undefined) {
      index = labels.length;
      labelToIndex.set(timestamp, index);
      labels.push(formatBucketTime(timestamp, bucketSeconds));
    }
    return index;
  };

  for (const entry of series) {
    for (const point of entry.points) ensureLabel(point.timestamp);
  }

  const values = series.map((entry) => {
    const column = new Array<null | number>(labels.length).fill(null);
    for (const point of entry.points) {
      const index = labelToIndex.get(point.timestamp);
      if (index !== undefined) column[index] = point.avg_watts;
    }
    return column;
  });

  return { labels, values };
}

/** currentWatts returns the newest reading of a series, or -1 when unknown. */
export function currentWatts(series: PowerSeriesData | null | undefined): number {
  if (!series || series.points.length === 0) return -1;
  return series.points[series.points.length - 1].avg_watts;
}

/** seriesLabel picks the display name of a power source. */
export function seriesLabel(series: PowerSeriesData): string {
  if (series.is_node) return series.source_id;
  return series.source_name || series.source_id;
}

/** formatWatts renders an average wattage, negative = unknown. */
export function formatWatts(watts: number): string {
  if (!Number.isFinite(watts) || watts < 0) return "—";
  const digits = watts >= 100 ? 0 : 1;
  return `${watts.toFixed(digits)} W`;
}

/** formatKwh renders watt-hours as kWh with a sensible precision. */
export function formatKwh(wattHours: number): string {
  if (!Number.isFinite(wattHours) || wattHours < 0) return "—";
  const kwh = wattHours / 1000;
  if (kwh >= 100) return `${kwh.toFixed(1)} kWh`;
  if (kwh >= 1) return `${kwh.toFixed(2)} kWh`;
  return `${(wattHours / 1000).toFixed(3)} kWh`;
}

/** formatCost renders a cost with the configured currency label. */
export function formatCost(cost: number, currency: string): string {
  if (!Number.isFinite(cost) || cost < 0) return "—";
  const digits = cost >= 100 ? 2 : cost >= 1 ? 2 : 3;
  return `${cost.toFixed(digits)} ${currency}`;
}

/** formatGpuSeconds renders occupancy time as e.g. "2h 05m" or "45m". */
export function formatGpuSeconds(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return "—";
  const totalMinutes = Math.round(seconds / 60);
  const days = Math.floor(totalMinutes / (60 * 24));
  const hours = Math.floor((totalMinutes % (60 * 24)) / 60);
  const minutes = totalMinutes % 60;
  if (days > 0) return `${days}d ${hours}h`;
  if (hours > 0) return `${hours}h ${String(minutes).padStart(2, "0")}m`;
  return `${minutes}m`;
}

/**
 * monthlyEstimate extrapolates the month-to-date cost across the whole
 * calendar month. Returns 0 while there is nothing to extrapolate from.
 */
export function monthlyEstimate(monthToDate: number, now: Date = new Date()): number {
  if (!Number.isFinite(monthToDate) || monthToDate <= 0) return 0;
  const daysInMonth = new Date(now.getFullYear(), now.getMonth() + 1, 0).getDate();
  const elapsedDays = now.getDate() - 1 + now.getHours() / 24;
  if (elapsedDays <= 0) return monthToDate;
  return (monthToDate / elapsedDays) * daysInMonth;
}

/** tariffLabel names a tariff bucket; the empty name is the base price. */
export function tariffLabel(name: string): string {
  return name === "" ? "base" : name;
}

/** hasPowerData reports whether the report carries any usable readings. */
export function hasPowerData(report: PowerReportData | null): boolean {
  if (!report) return false;
  if (report.totals.energy_wh > 0 || report.totals.node_energy_wh > 0) return true;
  return report.series.some((entry) => entry.points.some((point) => point.avg_watts > 0));
}
