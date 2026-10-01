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
  for (let index = 1; index < points.length; index++) {
    const [t0, tok0] = points[index - 1];
    const [t1, tok1] = points[index];
    const dt = t1 - t0;
    const dTok = tok1 - tok0;
    if (dt < 1 || dTok < 0) continue;
    out.push({ t: (t0 + t1) / 2, tps: (dTok / dt) * 1000 });
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
