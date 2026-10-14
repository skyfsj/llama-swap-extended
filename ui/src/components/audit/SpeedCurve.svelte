<script lang="ts">
  import { speedSeries, formatMs, formatTps, type SpeedPoint } from "../../lib/speedTimeline";
  import { translate } from "../../lib/i18n";
  import { Activity, Zap } from "@lucide/svelte";

  interface Props {
    points: SpeedPoint[];
  }

  let { points }: Props = $props();

  const WIDTH = 560;
  const HEIGHT = 140;
  const PADDING = { left: 8, right: 8, top: 12, bottom: 20 };

  let series = $derived(speedSeries(points));
  let maxTps = $derived(series.reduce((acc, point) => Math.max(acc, point.tps), 0));
  let avgTps = $derived(
    series.length > 0
      ? series.reduce((acc, point) => acc + point.tps, 0) / series.length
      : 0
  );
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
  <div class="space-y-2">
    <div class="flex flex-wrap items-center justify-between gap-2 text-xs">
      <div class="flex items-center gap-3">
        <span class="inline-flex items-center gap-1 font-medium text-foreground">
          <Activity class="size-3.5 text-primary" aria-hidden="true" />
          <span>{$translate("controlPlane.auditAvgTps")}:</span>
          <span class="font-mono">{formatTps(avgTps)}</span>
        </span>
        <span class="inline-flex items-center gap-1 text-muted-foreground">
          <Zap class="size-3.5 text-amber-500" aria-hidden="true" />
          <span>{$translate("controlPlane.auditPeakTps")}:</span>
          <span class="font-mono text-foreground">{formatTps(maxTps)}</span>
        </span>
      </div>
      <span class="text-xs text-muted-foreground font-mono">
        {formatMs(maxTime)} · {series.length} samples
      </span>
    </div>

    <div class="rounded-md border bg-muted/10 p-2">
      <svg viewBox="0 0 {WIDTH} {HEIGHT}" class="w-full overflow-visible" role="img" aria-label="speed curve">
        <defs>
          <linearGradient id="speedAreaGrad" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stop-color="currentColor" class="text-primary" stop-opacity="0.25" />
            <stop offset="100%" stop-color="currentColor" class="text-primary" stop-opacity="0.0" />
          </linearGradient>
        </defs>
        {#each gridlines as line (line.y)}
          <line x1={PADDING.left} y1={line.y} x2={WIDTH - PADDING.right} y2={line.y} class="stroke-border/70" stroke-width="1" stroke-dasharray="3 4" />
          <text x={WIDTH - PADDING.right} y={line.y - 3} text-anchor="end" class="fill-muted-foreground/80 font-mono" font-size="9">{line.label}</text>
        {/each}
        <path d={areaPath} fill="url(#speedAreaGrad)" stroke="none" />
        <path d={path} class="stroke-primary" fill="none" stroke-width="1.8" stroke-linejoin="round" stroke-linecap="round" />
        <text x={PADDING.left} y={HEIGHT - 5} class="fill-muted-foreground font-mono" font-size="9">0 ms</text>
        <text x={WIDTH - PADDING.right} y={HEIGHT - 5} text-anchor="end" class="fill-muted-foreground font-mono" font-size="9">{formatMs(maxTime)}</text>
      </svg>
    </div>
  </div>
{:else}
  <p class="text-muted-foreground text-xs py-2">{$translate("controlPlane.speedCurveUnavailable")}</p>
{/if}
