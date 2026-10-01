<script lang="ts">
  import { speedSeries, formatMs, formatTps, type SpeedPoint } from "../../lib/speedTimeline";
  import { translate } from "../../lib/i18n";

  interface Props {
    points: SpeedPoint[];
  }

  let { points }: Props = $props();

  const WIDTH = 560;
  const HEIGHT = 140;
  const PADDING = { left: 8, right: 8, top: 10, bottom: 18 };

  let series = $derived(speedSeries(points));
  let maxTps = $derived(series.reduce((acc, point) => Math.max(acc, point.tps), 0));
  let maxTime = $derived(points.length > 0 ? points[points.length - 1][0] : 0);

  // Gridlines at quarter fractions of the max speed keep the eye calibrated
  // without a full axis system in a detail dialog.
  let gridlines = $derived([0.25, 0.5, 0.75, 1].map((fraction) => ({
    y: PADDING.top + (1 - fraction) * (HEIGHT - PADDING.top - PADDING.bottom),
    label: formatTps(maxTps * fraction),
  })));

  function x(t: number): number {
    if (maxTime <= 0) return PADDING.left;
    return PADDING.left + (t / maxTime) * (WIDTH - PADDING.left - PADDING.right);
  }

  function y(tps: number): number {
    if (maxTps <= 0) return HEIGHT - PADDING.bottom;
    return PADDING.top + (1 - tps / maxTps) * (HEIGHT - PADDING.top - PADDING.bottom);
  }

  let path = $derived(
    series.length > 0
      ? series.map((point, index) => `${index === 0 ? "M" : "L"}${x(point.t).toFixed(1)},${y(point.tps).toFixed(1)}`).join(" ")
      : "",
  );
  let areaPath = $derived(
    path
      ? `${path} L${x(series[series.length - 1].t).toFixed(1)},${HEIGHT - PADDING.bottom} L${x(series[0].t).toFixed(1)},${HEIGHT - PADDING.bottom} Z`
      : "",
  );
</script>

{#if series.length > 1 && maxTime > 0}
  <svg viewBox="0 0 {WIDTH} {HEIGHT}" class="w-full" role="img" aria-label="speed curve">
    {#each gridlines as line (line.y)}
      <line x1={PADDING.left} y1={line.y} x2={WIDTH - PADDING.right} y2={line.y} class="stroke-border" stroke-width="1" stroke-dasharray="3 4" />
      <text x={WIDTH - PADDING.right} y={line.y - 3} text-anchor="end" class="fill-muted-foreground" font-size="9">{line.label}</text>
    {/each}
    <path d={areaPath} class="fill-primary/10" stroke="none" />
    <path d={path} class="stroke-primary" fill="none" stroke-width="1.6" stroke-linejoin="round" />
    <text x={PADDING.left} y={HEIGHT - 5} class="fill-muted-foreground" font-size="9">0 ms</text>
    <text x={WIDTH - PADDING.right} y={HEIGHT - 5} text-anchor="end" class="fill-muted-foreground" font-size="9">{formatMs(maxTime)}</text>
  </svg>
{:else}
  <p class="text-muted-foreground text-xs">{$translate("controlPlane.speedCurveUnavailable")}</p>
{/if}
