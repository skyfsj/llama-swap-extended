// Summary and markdown-export helpers for the activity table. Kept free of
// Svelte so the arithmetic and rendering rules can be unit tested directly.

import type { ActivityLogEntry } from "./types";
import { formatAbsoluteTime, formatDuration, formatSpeed } from "./format";
import { localeToIntl, translateFor, type Locale } from "./i18n";

/** Format a draft acceptance rate as "p% (accepted/drafted)". */
export function formatDrafted(drafted: number, accepted: number): string {
  return drafted > 0
    ? ((accepted * 100) / drafted).toFixed(1) + "% (" + accepted + "/" + drafted + ")"
    : "-";
}

/** Format cache creation tokens while keeping missing/invalid telemetry clear. */
export function formatCacheCreationTokens(value: number | undefined, locale?: Locale): string {
  if (value === undefined || !Number.isFinite(value) || value <= 0) return "-";
  return value.toLocaleString(locale ? localeToIntl(locale) : undefined);
}

/**
 * Format the cache hit ratio only when the row actually carries cache
 * telemetry. A zero ratio is meaningful for a cache-creation request, while
 * an omitted ratio is indistinguishable from JSON's zero value on the client;
 * the accompanying counters/hash let us make that distinction safely.
 */
export function formatCacheHitRatio(row: ActivityLogEntry): string {
  const ratio = row.cache_hit_ratio;
  const hasTelemetry =
    ratio !== undefined &&
    Number.isFinite(ratio) &&
    (ratio > 0 ||
      (row.tokens.cache_tokens ?? 0) > 0 ||
      (row.cache_creation_tokens ?? 0) > 0 ||
      !!row.prefix_hash ||
      row.repair_applied === true);
  if (!hasTelemetry) return "-";
  const bounded = Math.max(0, Math.min(1, ratio as number));
  return `${(bounded * 100).toFixed(1)}%`;
}

/** Render a short, copy-safe prefix hash as the stability marker. */
export function formatPrefixStability(row: ActivityLogEntry): string {
  const hash = row.prefix_hash?.trim();
  if (!hash) return "-";
  return hash.length > 12 ? `${hash.slice(0, 12)}…` : hash;
}

/** Totals and average speeds across a set of activity rows. */
export interface ActivitySummary {
  durationMs: number;
  cacheTokens: number;
  inputTokens: number;
  outputTokens: number;
  draftTokens: number;
  draftAccTokens: number;
  /** Token-weighted average, or -1 when no row reported a usable speed. */
  promptPerSecond: number;
  /** Token-weighted average, or -1 when no row reported a usable speed. */
  tokensPerSecond: number;
  /** Timestamp of the oldest row, or "" when there are none. */
  startedAt: string;
  /** Timestamp of the newest row, or "" when there are none. */
  endedAt: string;
}

/**
 * Sum a page of activity rows.
 *
 * Speeds are token-weighted (total tokens / total time) rather than a plain
 * mean of the per-request rates, so a handful of tiny requests cannot skew the
 * figure away from actual throughput. Rows without tokens, or whose speed the
 * upstream did not report (a negative value, see formatSpeed), contribute to
 * neither side of the ratio.
 */
export function summarizeActivity(rows: ActivityLogEntry[]): ActivitySummary {
  const summary: ActivitySummary = {
    durationMs: 0,
    cacheTokens: 0,
    inputTokens: 0,
    outputTokens: 0,
    draftTokens: 0,
    draftAccTokens: 0,
    promptPerSecond: -1,
    tokensPerSecond: -1,
    startedAt: "",
    endedAt: "",
  };

  let promptTokens = 0;
  let promptSeconds = 0;
  let genTokens = 0;
  let genSeconds = 0;
  // The rows arrive in whatever order the table is sorted by, so the range is
  // tracked by parsed time rather than by taking the first and last row.
  let startedMs = Infinity;
  let endedMs = -Infinity;

  for (const row of rows) {
    const rowMs = new Date(row.timestamp).getTime();
    if (!Number.isNaN(rowMs)) {
      if (rowMs < startedMs) {
        startedMs = rowMs;
        summary.startedAt = row.timestamp;
      }
      if (rowMs > endedMs) {
        endedMs = rowMs;
        summary.endedAt = row.timestamp;
      }
    }

    if (row.duration_ms >= 0) summary.durationMs += row.duration_ms;
    summary.cacheTokens += row.tokens.cache_tokens;
    summary.inputTokens += row.tokens.input_tokens;
    summary.outputTokens += row.tokens.output_tokens;
    summary.draftTokens += row.tokens.draft_tokens;
    summary.draftAccTokens += row.tokens.draft_acc_tokens;

    if (row.tokens.input_tokens > 0 && row.tokens.prompt_per_second > 0) {
      promptTokens += row.tokens.input_tokens;
      promptSeconds += row.tokens.input_tokens / row.tokens.prompt_per_second;
    }
    if (row.tokens.output_tokens > 0 && row.tokens.tokens_per_second > 0) {
      genTokens += row.tokens.output_tokens;
      genSeconds += row.tokens.output_tokens / row.tokens.tokens_per_second;
    }
  }

  if (promptSeconds > 0) summary.promptPerSecond = promptTokens / promptSeconds;
  if (genSeconds > 0) summary.tokensPerSecond = genTokens / genSeconds;

  return summary;
}

/**
 * Plain-text value for a table column, mirroring what the column's cell
 * renderer shows. The time column is rendered as an absolute timestamp because
 * an exported "5m ago" is meaningless once pasted somewhere else.
 */
export function activityCellText(
  row: ActivityLogEntry,
  columnId: string,
  locale?: Locale,
): string {
  const numberLocale = locale ? localeToIntl(locale) : undefined;
  const formatNumber = (value: number): string =>
    numberLocale ? value.toLocaleString(numberLocale) : value.toLocaleString();
  const unknown = locale ? translateFor(locale, "common.unknown") : "unknown";

  switch (columnId) {
    case "id":
      return String(row.id);
    case "time":
      return formatAbsoluteTime(row.timestamp, locale);
    case "model":
      return row.model;
    case "req_path":
      return row.req_path || "-";
    case "resp_status_code":
      return String(row.resp_status_code || "-");
    case "resp_content_type":
      return row.resp_content_type || "-";
    case "cached":
      return row.tokens.cache_tokens > 0 ? formatNumber(row.tokens.cache_tokens) : "-";
    case "cache_creation":
      return formatCacheCreationTokens(row.cache_creation_tokens, locale);
    case "cache_hit_ratio":
      return formatCacheHitRatio(row);
    case "repair_applied":
      return row.repair_applied ? (locale ? translateFor(locale, "activity.table.values.applied") : "Applied") : "-";
    case "prefix_stability":
      return formatPrefixStability(row);
    case "prompt":
      return formatNumber(row.tokens.input_tokens);
    case "generated":
      return formatNumber(row.tokens.output_tokens);
    case "drafted":
      return formatDrafted(row.tokens.draft_tokens, row.tokens.draft_acc_tokens);
    case "prompt_speed":
      return formatSpeed(row.tokens.prompt_per_second, unknown);
    case "gen_speed":
      return formatSpeed(row.tokens.tokens_per_second, unknown);
    case "duration": {
      const first = row.first_token_ms > 0 ? formatDuration(row.first_token_ms) : "-";
      const total = row.duration_ms >= 0 ? formatDuration(row.duration_ms) : "-";
      return `${first} / ${total}`;
    }
    case "meta": {
      const entries = Object.entries(row.metadata || {});
      return entries.length > 0
        ? entries.map(([key, value]) => `${key}=${value}`).join("; ")
        : "-";
    }
    default:
      return "";
  }
}

/**
 * Render the page totals as a two-column markdown table. Speeds are annotated
 * in parentheses next to the token counts they describe.
 */
export function summaryMarkdown(summary: ActivitySummary, locale?: Locale): string {
  const message = (key: string, fallback: string): string =>
    locale ? translateFor(locale, key) : fallback;
  const numberLocale = locale ? localeToIntl(locale) : undefined;
  const formatNumber = (value: number): string =>
    numberLocale ? value.toLocaleString(numberLocale) : value.toLocaleString();
  const unknown = locale ? translateFor(locale, "common.unknown") : "unknown";
  const drafted =
    summary.draftTokens > 0
      ? `${((summary.draftAccTokens * 100) / summary.draftTokens).toFixed(1)}% ${
          summary.draftAccTokens
        }/${summary.draftTokens}`
      : "-";

  const range =
    summary.startedAt !== "" && summary.endedAt !== ""
      ? `${formatAbsoluteTime(summary.startedAt, locale)} → ${formatAbsoluteTime(summary.endedAt, locale)}`
      : "-";

  const rows: [string, string][] = [
    [message("activity.export.range", "Range"), range],
    [message("activity.export.duration", "Duration"), formatDuration(summary.durationMs)],
    [message("activity.export.cached", "Cached"), formatNumber(summary.cacheTokens)],
    [
      message("activity.export.prompt", "Prompt"),
      `${formatNumber(summary.inputTokens)} (${formatSpeed(summary.promptPerSecond, unknown)})`,
    ],
    [
      message("activity.export.generated", "Generated"),
      `${formatNumber(summary.outputTokens)} (${formatSpeed(summary.tokensPerSecond, unknown)})`,
    ],
    [message("activity.export.drafted", "Drafted"), drafted],
  ];

  return [
    `| ${message("activity.export.summary", "Summary")} | |`,
    "| --- | --- |",
    ...rows.map(([label, value]) => `| ${label} | ${escapeCell(value)} |`),
  ].join("\n");
}

export interface MarkdownColumn {
  id: string;
  label: string;
}

// Cell values are free-form (metadata especially), so pipes are escaped and
// any line break is flattened to keep every row on one line.
function escapeCell(value: string): string {
  return value.replace(/\|/g, "\\|").replace(/[\r\n\t]+/g, " ");
}

/** Render rows as GitHub-flavored markdown table source, columns in order. */
export function toMarkdownTable(
  rows: ActivityLogEntry[],
  columns: MarkdownColumn[],
  locale?: Locale,
): string {
  if (columns.length === 0) return "";
  const lines = [
    `| ${columns.map((column) => escapeCell(column.label)).join(" | ")} |`,
    `| ${columns.map(() => "---").join(" | ")} |`,
  ];
  for (const row of rows) {
    lines.push(
      `| ${columns.map((column) => escapeCell(activityCellText(row, column.id, locale))).join(" | ")} |`
    );
  }
  return lines.join("\n");
}

/**
 * The full export: a summary table for the rows, the rows themselves, and an
 * attribution line. generatedAt is injectable so the output is testable.
 */
export function buildActivityMarkdown(
  rows: ActivityLogEntry[],
  columns: MarkdownColumn[],
  generatedAt: Date = new Date(),
  locale?: Locale,
): string {
  const attribution = locale
    ? translateFor(locale, "activity.export.attribution", {
        timestamp: formatAbsoluteTime(generatedAt.toISOString(), locale),
      })
    : "Exported from [llama-swap](https://github.com/mostlygeek/llama-swap) at " +
      formatAbsoluteTime(generatedAt.toISOString());

  return [summaryMarkdown(summarizeActivity(rows), locale), toMarkdownTable(rows, columns, locale), attribution]
    .filter((section) => section !== "")
    .join("\n\n");
}
