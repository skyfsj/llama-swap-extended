import { describe, expect, it } from "vitest";
import { emptyActivityFilters } from "./activityFilters";
import {
  SPEED_WINDOW_OPTIONS,
  buildSpeedQueryParams,
  contextBucketFullLabel,
  contextBucketShortLabel,
  contextRows,
  formatTokens,
  rateOrDash,
  secondsOrDash,
  selectedContextSeries,
  selectedSpeedSeries,
  speedAverages,
  speedChartData,
  speedTrend,
  type SpeedWindowKey,
} from "./speedSeries";
import type { ContextBucketData, SpeedPointData, SpeedReportData } from "./types";

function point(overrides: Partial<SpeedPointData> = {}): SpeedPointData {
  return {
    timestamp: "2026-09-28T10:00:00.000Z",
    requests: 1,
    prefill_tps: -1,
    decode_tps: -1,
    ttft_ms: -1,
    ...overrides,
  };
}

function bucket(overrides: Partial<ContextBucketData> = {}): ContextBucketData {
  return {
    label: "4k-8k",
    min_tokens: 4096,
    max_tokens: 8192,
    requests: 1,
    avg_input_tokens: 5000,
    avg_output_tokens: 100,
    prefill_tps: -1,
    decode_tps: -1,
    ttft_ms: -1,
    ...overrides,
  };
}

describe("buildSpeedQueryParams", () => {
  const now = new Date("2026-09-28T12:00:00.000Z");

  it("scopes to one model and keeps the drawer's key and session scope", () => {
    const params = buildSpeedQueryParams({
      model: "Qwen/Qwen3.8-27B-FP8",
      filters: { ...emptyActivityFilters(), keyID: "key-1", sessionID: "session-9" },
      window: "all",
    }, now);
    expect(params.get("model")).toBe("Qwen/Qwen3.8-27B-FP8");
    expect(params.get("key_id")).toBe("key-1");
    expect(params.get("session_id")).toBe("session-9");
    expect(params.has("start")).toBe(false);
    expect(params.has("end")).toBe(false);
  });

  it("lets the section's quick range win over the drawer's range", () => {
    const params = buildSpeedQueryParams({
      model: "m1",
      filters: { ...emptyActivityFilters(), range: "week", start: "2026-01-01T00:00:00.000Z" },
      window: "hour",
    }, now);
    expect(params.get("start")).toBe("2026-09-28T11:00:00.000Z");
    expect(params.get("end")).toBe("2026-09-28T12:00:00.000Z");
  });

  it("omits every range param for the whole history", () => {
    const params = buildSpeedQueryParams({ model: "", filters: emptyActivityFilters(), window: "all" }, now);
    expect([...params.keys()]).toEqual([]);
  });

  it("passes the configured-only scope through", () => {
    const params = buildSpeedQueryParams(
      { model: "m1", filters: emptyActivityFilters(), window: "all", configuredOnly: true },
      now,
    );
    expect(params.get("configured_only")).toBe("true");
  });

  it("covers every selectable window", () => {
    const keys = SPEED_WINDOW_OPTIONS.map((option) => option.key);
    expect(keys).toEqual<SpeedWindowKey[]>(["hour", "6h", "day", "week", "all"]);
  });
});

describe("speedAverages", () => {
  it("weights buckets by their request count", () => {
    const averages = speedAverages([
      point({ requests: 90, prefill_tps: 100, decode_tps: 20, ttft_ms: 1000 }),
      point({ requests: 10, prefill_tps: 500, decode_tps: 100, ttft_ms: 4000 }),
    ]);
    expect(averages.requests).toBe(100);
    expect(averages.prefillTps).toBeCloseTo(140);
    expect(averages.decodeTps).toBeCloseTo(28);
    expect(averages.ttftSeconds).toBeCloseTo(1.3);
    // 28 tok/s is one token every ~35.7ms.
    expect(averages.perTokenLatencyMs).toBeCloseTo(1000 / 28);
  });

  it("treats unreported metrics as unknown instead of zero", () => {
    const averages = speedAverages([point({ requests: 3, decode_tps: 30 })]);
    expect(averages.prefillTps).toBe(-1);
    expect(averages.ttftSeconds).toBe(-1);
    expect(averages.decodeTps).toBe(30);
  });

  it("reports nothing for an empty window", () => {
    expect(speedAverages([])).toEqual({
      requests: 0,
      prefillTps: -1,
      decodeTps: -1,
      ttftSeconds: -1,
      perTokenLatencyMs: -1,
    });
  });
});

describe("speedTrend", () => {
  it("compares the recent half with the earlier half", () => {
    const trend = speedTrend([
      point({ requests: 2, decode_tps: 100 }),
      point({ requests: 2, decode_tps: 100 }),
      point({ requests: 2, decode_tps: 150 }),
      point({ requests: 2, decode_tps: 150 }),
    ]);
    expect(trend.decode).toBeCloseTo(0.5);
  });

  it("has no trend when one half never measured the metric", () => {
    const trend = speedTrend([point({ requests: 1, decode_tps: -1 }), point({ requests: 1, decode_tps: 40 })]);
    expect(trend.decode).toBeNull();
  });

  it("has no trend when the earlier value is zero", () => {
    const trend = speedTrend([point({ requests: 1, decode_tps: 0 }), point({ requests: 1, decode_tps: 10 })]);
    expect(trend.decode).toBeNull();
  });
});

describe("speedChartData", () => {
  const hourly = "2026-09-28T10:05:00.000Z";

  it("maps unknown values to null so the line breaks instead of diving to zero", () => {
    const chart = speedChartData([
      point({ timestamp: hourly, requests: 2, prefill_tps: 800, decode_tps: 30, ttft_ms: 1200 }),
      point({ timestamp: "2026-09-28T10:10:00.000Z", requests: 0 }),
    ], 3600);
    expect(chart.prefill).toEqual([800, null]);
    expect(chart.decode).toEqual([30, null]);
    // TTFT is charted in seconds.
    expect(chart.ttftSeconds).toEqual([1.2, null]);
  });

  it("labels short buckets with the local time of day", () => {
    const chart = speedChartData([point({ timestamp: hourly })], 300);
    const expected = (() => {
      const date = new Date(hourly);
      const pad = (value: number) => String(value).padStart(2, "0");
      return `${pad(date.getHours())}:${pad(date.getMinutes())}`;
    })();
    expect(chart.labels).toEqual([expected]);
  });

  it("labels daily buckets with the date, since the time of day would repeat", () => {
    const chart = speedChartData([point({ timestamp: hourly })], 86400);
    const date = new Date(hourly);
    const pad = (value: number) => String(value).padStart(2, "0");
    expect(chart.labels).toEqual([`${pad(date.getMonth() + 1)}-${pad(date.getDate())}`]);
  });

  it("labels multi-hour buckets with date and time", () => {
    const chart = speedChartData([point({ timestamp: hourly })], 21600);
    const date = new Date(hourly);
    const pad = (value: number) => String(value).padStart(2, "0");
    expect(chart.labels).toEqual([
      `${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}`,
    ]);
  });
});

describe("context bucket labels", () => {
  it("labels the axis by the bucket's upper bound", () => {
    expect(contextBucketShortLabel(bucket({ min_tokens: 0, max_tokens: 4096 }))).toBe("4K");
    expect(contextBucketShortLabel(bucket({ min_tokens: 65536, max_tokens: 131072 }))).toBe("128K");
    expect(contextBucketShortLabel(bucket({ min_tokens: 524288, max_tokens: 1048576 }))).toBe("1M");
    expect(contextBucketShortLabel(bucket({ min_tokens: 1048576, max_tokens: 0 }))).toBe(">1M");
  });

  it("labels the table with the exact token count", () => {
    expect(contextBucketFullLabel(bucket({ min_tokens: 4096, max_tokens: 8192 }))).toBe("8K (8,192)");
    expect(contextBucketFullLabel(bucket({ min_tokens: 1048576, max_tokens: 0 }))).toBe(">1M (1,048,576+)");
  });
});

describe("formatTokens", () => {
  it("renders token counts compactly", () => {
    expect(formatTokens(4096)).toBe("4K");
    expect(formatTokens(4 * 1024 + 2048)).toBe("6K");
    expect(formatTokens(1024 * 1024)).toBe("1M");
    expect(formatTokens(1024 * 1024 + 512 * 1024)).toBe("1.5M");
    expect(formatTokens(512)).toBe("512");
    expect(formatTokens(0)).toBe("0");
  });
});

describe("report selection helpers", () => {
  const report: SpeedReportData = {
    bucket_seconds: 3600,
    series: [
      { model: "busiest", points: [point({ requests: 4 })] },
      { model: "quiet", points: [point({ requests: 1 })] },
    ],
    context: [
      { model: "busiest", buckets: [bucket()] },
      { model: "quiet", buckets: [bucket({ label: "16k-32k" })] },
    ],
  };

  it("falls back to the busiest model", () => {
    expect(selectedSpeedSeries(null, "busiest")?.model).toBeUndefined();
    expect(selectedSpeedSeries(report, "")?.model).toBe("busiest");
    expect(selectedSpeedSeries(report, "deleted")?.model).toBe("busiest");
    expect(selectedSpeedSeries(report, "quiet")?.model).toBe("quiet");
  });

  it("keeps context selection aligned with the curve selection", () => {
    expect(selectedContextSeries(report, "quiet")?.model).toBe("quiet");
    expect(selectedContextSeries(report, "missing")?.model).toBe("busiest");
  });

  it("flattens context rows without losing the model", () => {
    const rows = contextRows(report);
    expect(rows.map((row) => `${row.model}:${row.bucket.label}`)).toEqual(["busiest:4k-8k", "quiet:16k-32k"]);
    expect(contextRows(null)).toEqual([]);
  });
});

describe("dash formatting", () => {
  it("renders unreported metrics as a dash", () => {
    expect(rateOrDash(-1)).toBe("—");
    expect(rateOrDash(812.34)).toBe("812.3");
    expect(secondsOrDash(-1)).toBe("—");
    expect(secondsOrDash(1.24)).toBe("1.24");
  });
});
