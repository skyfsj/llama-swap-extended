<script lang="ts">
  import { ArrowDownRight, ArrowUpRight } from "@lucide/svelte";
  import { translate, localeToIntl, locale } from "../../lib/i18n";
  import { formatCompactNumber } from "../../lib/format";
  import Sparkline from "./Sparkline.svelte";
  import { rateOrDash, secondsOrDash, type SpeedAverages, type SpeedTrend } from "../../lib/speedSeries";
  import type { InflightRequestEntry } from "../../lib/types";
  import * as Card from "$lib/components/ui/card/index.js";

  interface Props {
    averages: SpeedAverages;
    trend: SpeedTrend;
    points: { prefill_tps: number; decode_tps: number; ttft_ms: number; requests: number }[];
    inflightRequests: InflightRequestEntry[];
    model: string;
  }

  let { averages, trend, points, inflightRequests, model }: Props = $props();

  let nf = $derived(new Intl.NumberFormat(localeToIntl($locale)));

  function full(value: number): string {
    if (!Number.isFinite(value) || value < 0) return "—";
    return nf.format(value);
  }

  function trendLabel(value: number | null): string {
    if (value === null || !Number.isFinite(value)) return "";
    const percent = (value * 100).toFixed(1);
    return value >= 0 ? `+${percent}%` : `${percent}%`;
  }

  function trendIsImprovement(value: number | null): boolean | null {
    if (value === null || !Number.isFinite(value) || value === 0) return null;
    return value > 0;
  }

  /** Latency trends read the other way round: down is an improvement. */
  function trendIsImprovementInverted(value: number | null): boolean | null {
    const improvement = trendIsImprovement(value);
    if (improvement === null) return null;
    return !improvement;
  }
</script>

<div class="grid grid-cols-2 gap-2 sm:grid-cols-3 xl:grid-cols-6">
  <Card.Root class="gap-1 p-3">
    <div class="text-muted-foreground text-xs font-medium">{$translate("activity.speed.kpi.prefill")}</div>
    <div class="text-lg font-semibold" title={full(averages.prefillTps)}>
      {rateOrDash(averages.prefillTps)}
      {#if averages.prefillTps >= 0}<span class="text-muted-foreground text-xs font-normal">tok/s</span>{/if}
    </div>
    <Sparkline
      values={points.map((point) => (point.prefill_tps >= 0 ? point.prefill_tps : null))}
      label={$translate("activity.speed.kpi.prefill")}
      colorClass="text-violet-500 dark:text-violet-400"
    />
    {#if trend.prefill !== null}
      <div class="text-muted-foreground flex items-center gap-1 text-xs">
        {#if trendIsImprovement(trend.prefill) === true}
          <ArrowUpRight class="text-emerald-500" size={14} />
        {:else if trendIsImprovement(trend.prefill) === false}
          <ArrowDownRight class="text-rose-500" size={14} />
        {/if}
        <span>{trendLabel(trend.prefill)}</span>
        <span class="opacity-70">{$translate("activity.speed.kpi.trend")}</span>
      </div>
    {/if}
  </Card.Root>

  <Card.Root class="gap-1 p-3">
    <div class="text-muted-foreground text-xs font-medium">{$translate("activity.speed.kpi.decode")}</div>
    <div class="text-lg font-semibold" title={full(averages.decodeTps)}>
      {rateOrDash(averages.decodeTps)}
      {#if averages.decodeTps >= 0}<span class="text-muted-foreground text-xs font-normal">tok/s</span>{/if}
    </div>
    <Sparkline
      values={points.map((point) => (point.decode_tps >= 0 ? point.decode_tps : null))}
      label={$translate("activity.speed.kpi.decode")}
      colorClass="text-blue-500 dark:text-blue-400"
    />
    {#if trend.decode !== null}
      <div class="text-muted-foreground flex items-center gap-1 text-xs">
        {#if trendIsImprovement(trend.decode) === true}
          <ArrowUpRight class="text-emerald-500" size={14} />
        {:else if trendIsImprovement(trend.decode) === false}
          <ArrowDownRight class="text-rose-500" size={14} />
        {/if}
        <span>{trendLabel(trend.decode)}</span>
        <span class="opacity-70">{$translate("activity.speed.kpi.trend")}</span>
      </div>
    {/if}
  </Card.Root>

  <Card.Root class="gap-1 p-3">
    <div class="text-muted-foreground text-xs font-medium">{$translate("activity.speed.kpi.ttft")}</div>
    <div class="text-lg font-semibold" title={full(averages.ttftSeconds)}>
      {secondsOrDash(averages.ttftSeconds)}
      {#if averages.ttftSeconds >= 0}<span class="text-muted-foreground text-xs font-normal">s</span>{/if}
    </div>
    <Sparkline
      values={points.map((point) => (point.ttft_ms >= 0 ? point.ttft_ms / 1000 : null))}
      label={$translate("activity.speed.kpi.ttft")}
      colorClass="text-emerald-500 dark:text-emerald-400"
    />
    {#if trend.ttft !== null}
      <div class="text-muted-foreground flex items-center gap-1 text-xs">
        {#if trendIsImprovementInverted(trend.ttft) === true}
          <ArrowUpRight class="text-emerald-500" size={14} />
        {:else if trendIsImprovementInverted(trend.ttft) === false}
          <ArrowDownRight class="text-rose-500" size={14} />
        {/if}
        <span>{trendLabel(trend.ttft)}</span>
        <span class="opacity-70">{$translate("activity.speed.kpi.trend")}</span>
      </div>
    {/if}
  </Card.Root>

  <Card.Root class="gap-1 p-3">
    <div class="text-muted-foreground text-xs font-medium">{$translate("activity.speed.kpi.perToken")}</div>
    <div class="text-lg font-semibold" title={full(averages.perTokenLatencyMs)}>
      {rateOrDash(averages.perTokenLatencyMs)}
      {#if averages.perTokenLatencyMs >= 0}<span class="text-muted-foreground text-xs font-normal">ms</span>{/if}
    </div>
    <Sparkline
      values={points.map((point) =>
        point.decode_tps > 0 ? 1000 / point.decode_tps : null,
      )}
      label={$translate("activity.speed.kpi.perToken")}
      colorClass="text-amber-500 dark:text-amber-400"
    />
    {#if trend.decode !== null}
      <div class="text-muted-foreground flex items-center gap-1 text-xs">
        {#if trendIsImprovementInverted(trend.decode) === true}
          <ArrowUpRight class="text-emerald-500" size={14} />
        {:else if trendIsImprovementInverted(trend.decode) === false}
          <ArrowDownRight class="text-rose-500" size={14} />
        {/if}
        <span>{trendLabel(trend.decode)}</span>
        <span class="opacity-70">{$translate("activity.speed.kpi.trend")}</span>
      </div>
    {/if}
  </Card.Root>

  <Card.Root class="gap-1 p-3">
    <div class="text-muted-foreground text-xs font-medium">{$translate("activity.speed.kpi.requests")}</div>
    <div class="text-lg font-semibold" title={full(averages.requests)}>
      {formatCompactNumber(averages.requests, $locale)}
      <span class="text-muted-foreground text-xs font-normal">{$translate("activity.speed.kpi.requestsInWindow")}</span>
    </div>
    <Sparkline
      values={points.map((point) => point.requests)}
      label={$translate("activity.speed.kpi.requests")}
      colorClass="text-foreground/60"
    />
  </Card.Root>

  <Card.Root class="gap-1 p-3">
    <div class="text-muted-foreground text-xs font-medium">{$translate("activity.speed.kpi.concurrency")}</div>
    <div class="text-lg font-semibold" title={model}>{inflightRequests.length}</div>
    <div class="text-muted-foreground text-xs">
      {$translate("activity.speed.kpi.concurrencyHint")}
    </div>
  </Card.Root>
</div>
