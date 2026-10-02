import { describe, expect, it } from "vitest";
import {
  currentWatts,
  formatCost,
  formatGpuSeconds,
  formatKwh,
  formatWatts,
  hasPowerData,
  isPowerWindowKey,
  monthlyEstimate,
  powerChartData,
  seriesLabel,
  tariffLabel,
} from "./energyReport";
import type { PowerReportData, PowerSeriesData } from "./types";

function series(sourceId: string, points: { t: string; watts: number }[], isNode = false): PowerSeriesData {
  return {
    source_id: sourceId,
    source_name: isNode ? "" : `Card ${sourceId}`,
    is_node: isNode,
    points: points.map((point) => ({
      timestamp: point.t,
      avg_watts: point.watts,
      min_watts: point.watts,
      max_watts: point.watts,
    })),
  };
}

describe("powerChartData", () => {
  it("aligns series on the union of timestamps with nulls for gaps", () => {
    const a = series("a", [{ t: "2026-10-02T01:00:00Z", watts: 100 }, { t: "2026-10-02T01:05:00Z", watts: 120 }]);
    const b = series("b", [{ t: "2026-10-02T01:05:00Z", watts: 300 }]);
    const chart = powerChartData([a, b], 300);
    expect(chart.labels).toHaveLength(2);
    expect(chart.values[0]).toEqual([100, 120]);
    expect(chart.values[1]).toEqual([null, 300]);
  });

  it("returns empty structures without series", () => {
    expect(powerChartData([])).toEqual({ labels: [], values: [] });
  });
});

describe("formatting helpers", () => {
  it("renders unknown readings as dashes", () => {
    expect(formatWatts(-1)).toBe("—");
    expect(formatKwh(-1)).toBe("—");
    expect(formatCost(-1, "CNY")).toBe("—");
    expect(formatGpuSeconds(0)).toBe("—");
  });

  it("formats wattages and energy", () => {
    expect(formatWatts(320.4)).toBe("320 W");
    expect(formatWatts(45.6)).toBe("45.6 W");
    expect(formatKwh(1500)).toBe("1.50 kWh");
    expect(formatKwh(500)).toBe("0.500 kWh");
    expect(formatCost(0.42, "CNY")).toBe("0.420 CNY");
  });

  it("formats occupancy durations", () => {
    expect(formatGpuSeconds(45 * 60)).toBe("45m");
    expect(formatGpuSeconds(125 * 60)).toBe("2h 05m");
    expect(formatGpuSeconds(26 * 3600)).toBe("1d 2h");
  });
});

describe("currentWatts", () => {
  it("prefers the newest reading and reports unknown as -1", () => {
    const entry = series("a", [
      { t: "2026-10-02T01:00:00Z", watts: 100 },
      { t: "2026-10-02T01:05:00Z", watts: 150 },
    ]);
    expect(currentWatts(entry)).toBe(150);
    expect(currentWatts(null)).toBe(-1);
    expect(currentWatts(series("a", []))).toBe(-1);
  });
});

describe("seriesLabel and tariffLabel", () => {
  it("uses the node id, the card name, and marks the base tariff", () => {
    expect(seriesLabel(series("node", [], true)).length).toBeGreaterThan(0);
    expect(seriesLabel(series("GPU-x", []))).toBe("Card GPU-x");
    expect(tariffLabel("")).toBe("base");
    expect(tariffLabel("peak")).toBe("peak");
  });
});

describe("isPowerWindowKey", () => {
  it("accepts only known windows", () => {
    expect(isPowerWindowKey("day")).toBe(true);
    expect(isPowerWindowKey("30d")).toBe(true);
    expect(isPowerWindowKey("all")).toBe(false);
  });
});

describe("monthlyEstimate", () => {
  it("extrapolates by the covered span, not the wall clock", () => {
    // 1.6 h of samples costing 0.24 in a 31-day month: the projection must
    // follow the data span, not the hours since midnight (the old formula
    // understated this case by an order of magnitude).
    const estimate = monthlyEstimate(0.24, 1.6, 381, 0.55, new Date(2026, 9, 2, 13, 0, 0));
    expect(estimate).toBeGreaterThan(105);
    expect(estimate).toBeLessThan(115);
  });

  it("falls back to the live power projection with little history", () => {
    const estimate = monthlyEstimate(0.05, 0.4, 381, 0.55, new Date(2026, 9, 2, 13, 0, 0));
    // 381 W × 0.55 CNY/kWh × 744 h ≈ 155.9
    expect(estimate).toBeGreaterThan(150);
    expect(estimate).toBeLessThan(160);
  });

  it("handles empty and boundary inputs", () => {
    expect(monthlyEstimate(0, 5, 381, 0.55)).toBe(0);
    // No coverage and no live power to project from.
    expect(monthlyEstimate(5, 0, 0, 0.55)).toBe(0);
  });
});

describe("hasPowerData", () => {
  it("detects usable readings", () => {
    const empty = {
      currency: "CNY",
      price_per_kwh: 0,
      tariffs: [],
      sources: { gpu: false, ipmi: false },
      start: "",
      end: "",
      bucket_seconds: 60,
      series: [],
      models: [],
      totals: {
        energy_wh: 0,
        inference_energy_wh: 0,
        idle_energy_wh: 0,
        cost: 0,
        node_energy_wh: -1,
        node_cost: 0,
        today_energy_wh: 0,
        today_cost: 0,
        month_energy_wh: 0,
        month_cost: 0,
        month_coverage_hours: 0,
        by_tariff: [],
      },
      baselines: {},
    } as PowerReportData;
    expect(hasPowerData(empty)).toBe(false);
    expect(hasPowerData(null)).toBe(false);
    const withPower = {
      ...empty,
      series: [series("a", [{ t: "2026-10-02T01:00:00Z", watts: 120 }])],
    };
    expect(hasPowerData(withPower)).toBe(true);
  });
});
