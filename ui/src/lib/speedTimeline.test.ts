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

  // vLLM merges RequestOutputs when the engine runs ahead of the API layer,
  // so the first frame can carry every token produced during prefill with
  // both timestamps on the same millisecond. Dividing by that dt reported
  // ~10-19k tokens/sec and flattened the rest of the curve. Rates are taken
  // between anchors >=100ms apart, so quantized gaps never become a rate.
  it("does not spike on a prefill-burst first frame", () => {
    const series = speedSeries([
      [1996, 0],
      [1997, 19],
      [2028, 20],
      [2051, 21],
      [2151, 25],
    ]);
    // 25 tokens over 1996->2151 (155ms real span)
    expect(series).toEqual([{ t: 2073.5, tps: (25 / 155) * 1000 }]);
  });

  it("does not spike when a stall lands many frames on one millisecond", () => {
    // Production sample 55501: 37 frames share t=26682 after an 11s stall.
    const series = speedSeries([
      [15623, 609],
      [26682, 618.6],
      [26682, 664.6],
      [26682, 723.6],
      [26919, 733.9],
      [27221, 739.5],
      [27502, 744.6],
    ]);
    // The window right after the stall divides the 10.3 tokens of that
    // window by the 237ms real gap — ~43 t/s, not a spike.
    const maxTps = Math.max(...series.slice(1).map((p) => p.tps));
    expect(maxTps).toBeLessThan(100);
    // The first emitted window spans 15623->26682 (the stall gap itself):
    // 9.6 tokens over 11 seconds, honestly reported instead of compressed
    // into the stall-exit frame.
    expect(series[0]).toEqual({ t: 21152.5, tps: ((618.6 - 609) / (26682 - 15623)) * 1000 });
  });

  it("renders a sub-window curve as a single span average", () => {
    const series = speedSeries([
      [0, 0],
      [40, 2],
      [80, 4],
    ]);
    expect(series).toEqual([{ t: 40, tps: 50 }]);
  });

  it("skips backwards intervals without emitting a rate", () => {
    const series = speedSeries([
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
