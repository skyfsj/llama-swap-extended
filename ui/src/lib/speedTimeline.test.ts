import { describe, expect, it } from "vitest";
import { parseSpeedTimeline, phaseShare, speedSeries, formatMs, formatTps } from "./speedTimeline";

describe("parseSpeedTimeline", () => {
  it("parses a valid [t, tokens] array", () => {
    const points = parseSpeedTimeline("[[0,0],[100,12],[250,40]]");
    expect(points).toEqual([
      [0, 0],
      [100, 12],
      [250, 40],
    ]);
  });

  it("returns empty for empty/absent/invalid input", () => {
    expect(parseSpeedTimeline("")).toEqual([]);
    expect(parseSpeedTimeline(undefined)).toEqual([]);
    expect(parseSpeedTimeline(null)).toEqual([]);
    expect(parseSpeedTimeline("not json")).toEqual([]);
    expect(parseSpeedTimeline("{}")).toEqual([]);
    expect(parseSpeedTimeline("[[1]]")).toEqual([]);
    expect(parseSpeedTimeline('[["x","y"]]')).toEqual([]);
  });

  it("sorts by time so a reordered row still renders sanely", () => {
    const points = parseSpeedTimeline("[[200,10],[0,0],[100,5]]");
    expect(points.map((point) => point[0])).toEqual([0, 100, 200]);
  });
});

describe("speedSeries", () => {
  it("computes per-interval tokens/sec at interval midpoints", () => {
    const series = speedSeries([
      [0, 0],
      [1000, 50],
      [4000, 110],
    ]);
    expect(series).toEqual([
      { t: 500, tps: 50 },
      { t: 2500, tps: 20 },
    ]);
  });

  it("skips zero-duration and backwards intervals", () => {
    const series = speedSeries([
      [0, 0],
      [0, 5],
      [100, 3],
    ]);
    expect(series).toEqual([]);
  });
});

describe("phaseShare", () => {
  it("splits wall time into ttft, decode and other", () => {
    const share = phaseShare(2000, 7000, 10000);
    expect(share).toEqual({ ttftMs: 2000, decodeMs: 7000, otherMs: 1000, totalMs: 10000 });
  });

  it("treats missing measurements as zero without going negative", () => {
    expect(phaseShare(-1, -1, 500)).toEqual({ ttftMs: 0, decodeMs: 0, otherMs: 500, totalMs: 500 });
  });

  it("never reports a negative other segment when decode outlasts the wall clock", () => {
    const share = phaseShare(1000, 9000, 5000);
    expect(share.otherMs).toBe(0);
    expect(share.totalMs).toBe(10000);
  });
});

describe("formatting", () => {
  it("formats durations and rates", () => {
    expect(formatMs(850)).toBe("850 ms");
    expect(formatMs(1500)).toBe("1.50 s");
    expect(formatMs(12400)).toBe("12.4 s");
    expect(formatTps(45.6)).toBe("45.6 t/s");
    expect(formatTps(1234)).toBe("1.2k t/s");
  });
});
