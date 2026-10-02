/**
 * Client-side decoding of the per-request speed timeline recorded with an
 * activity row: a bounded JSON array of [msSinceStart, cumulativeTokens]
 * pairs sampled from the streamed response. Kept as pure functions so the
 * curve math is unit-testable without a component.
 */

export type SpeedPoint = [number, number];

export function parseSpeedTimeline(raw: string | undefined | null): SpeedPoint[] {
  if (!raw) return [];
  try {
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    const points: SpeedPoint[] = [];
    for (const entry of parsed) {
      if (!Array.isArray(entry) || entry.length < 2) continue;
      const t = Number(entry[0]);
      const tokens = Number(entry[1]);
      if (!Number.isFinite(t) || !Number.isFinite(tokens)) continue;
      points.push([t, tokens]);
    }
    // The curve must be monotonic in time; a truncated or hand-edited row
    // could violate that and would render a nonsensical chart.
    points.sort((a, b) => a[0] - b[0]);
    return points;
  } catch {
    return [];
  }
}

/** Per-interval decode speed (tokens/sec) between consecutive timeline points. */
export function speedSeries(points: SpeedPoint[]): { t: number; tps: number }[] {
  const out: { t: number; tps: number }[] = [];
  if (points.length < 2) return out;
  // vLLM merges RequestOutputs when the engine runs ahead of the API layer
  // (RequestOutputCollector.put): the first frame can carry every token
  // produced during prefill, and after a stall dozens of frames can land on
  // one millisecond. Those tokens are real, but their per-frame timestamps
  // are quantized — dividing tokens by the 0-1ms gap between neighbors
  // reports tens of thousands of tokens/sec and flattens the whole curve.
  //
  // Compute the rate over sliding anchors at least one window apart instead:
  // tokens are divided by a real elapsed span, so merged frames and stalls
  // both average out honestly. 100ms keeps ~10 samples/second of curve.
  const minWindowMs = 100;
  let anchorIndex = 0;
  let lastEmitStart = -1;
  for (let index = 1; index < points.length; index++) {
    const [t0, tok0] = points[anchorIndex];
    const [t1, tok1] = points[index];
    const dt = t1 - t0;
    // Frames sharing the anchor's millisecond (merged/stalled output) advance
    // the anchor so their tokens belong to the next real window, never to a
    // 0ms denominator.
    if (t1 <= t0) {
      anchorIndex = index;
      continue;
    }
    if (dt < minWindowMs) continue;
    const dTok = tok1 - tok0;
    if (dTok >= 0) {
      out.push({ t: (t0 + t1) / 2, tps: (dTok / dt) * 1000 });
      lastEmitStart = anchorIndex;
    }
    anchorIndex = index;
  }
  // The tail between the last anchor and the final point is shorter than one
  // window; folding it into the previous interval keeps every token counted
  // without dividing by a quantized gap. A curve that never reached one full
  // window renders as a single honest average over its whole span.
  const last = points[points.length - 1];
  if (lastEmitStart >= 0) {
    const start = points[lastEmitStart];
    const span = last[0] - start[0];
    if (span > 0 && last[1] >= start[1]) {
      out[out.length - 1] = {
        t: (start[0] + last[0]) / 2,
        tps: ((last[1] - start[1]) / span) * 1000,
      };
    }
  } else if (last[0] > points[0][0] && last[1] >= points[0][1]) {
    out.push({
      t: (points[0][0] + last[0]) / 2,
      tps: ((last[1] - points[0][1]) / (last[0] - points[0][0])) * 1000,
    });
  }
  return out;
}

export interface PhaseShare {
  ttftMs: number;
  decodeMs: number;
  otherMs: number;
  totalMs: number;
}

/**
 * Phase durations for the request: TTFT (queue + prompt processing, up to the
 * first visible token), decode (first to last visible token) and whatever
 * remains of the measured wall time (response buffering, serialization).
 * Absent measurements (-1) fall back to 0 so the bar still renders.
 */
export function phaseShare(firstTokenMs: number | undefined, decodeMs: number | undefined, durationMs: number | undefined): PhaseShare {
  const ttft = Math.max(0, firstTokenMs ?? 0);
  const decode = Math.max(0, decodeMs ?? 0);
  const total = Math.max(0, durationMs ?? 0);
  // The decode window can legitimately extend past a wall-clock duration that
  // was recorded before the stream's final bytes were flushed; never report a
  // negative "other" segment.
  const other = Math.max(0, total - ttft - decode);
  return { ttftMs: ttft, decodeMs: decode, otherMs: other, totalMs: Math.max(total, ttft + decode) };
}

export function formatMs(value: number): string {
  if (value >= 10_000) return `${(value / 1000).toFixed(1)} s`;
  if (value >= 1000) return `${(value / 1000).toFixed(2)} s`;
  return `${Math.round(value)} ms`;
}

export function formatTps(value: number): string {
  if (value >= 1000) return `${(value / 1000).toFixed(1)}k t/s`;
  return `${value.toFixed(value >= 100 ? 0 : 1)} t/s`;
}
