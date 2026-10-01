<script lang="ts">
  import { onMount, onDestroy } from "svelte";
  import {
    CategoryScale,
    Chart,
    Filler,
    Legend,
    LineController,
    LineElement,
    LinearScale,
    PointElement,
    Tooltip,
    type ChartDataset,
  } from "chart.js";
  import { isDarkMode } from "../../stores/theme";
  import { localeToIntl, locale } from "../../lib/i18n";
  import { translate } from "$lib/i18n";
  import * as Card from "$lib/components/ui/card/index.js";
  import type { UsageTrendBucket } from "../../lib/usage";

  // Only the line/area pieces this chart uses.
  Chart.register(
    LineController,
    LineElement,
    PointElement,
    LinearScale,
    CategoryScale,
    Filler,
    Legend,
    Tooltip,
  );

  interface Props {
    buckets: UsageTrendBucket[];
    granularity: string;
  }

  let { buckets, granularity }: Props = $props();

  let canvas: HTMLCanvasElement | undefined = $state();
  let chart: Chart | undefined;

  const series = [
    { key: "inputTokens", labelKey: "trendInput", color: "#3b82f6" },
    { key: "outputTokens", labelKey: "trendOutput", color: "#22d3ee" },
    { key: "cacheCreationTokens", labelKey: "trendCacheCreation", color: "#f59e0b" },
    { key: "cachedTokens", labelKey: "trendCacheRead", color: "#a78bfa" },
  ] as const;

  function formatBucket(start: string, granularity: string): string {
    const parsed = new Date(start);
    if (Number.isNaN(parsed.getTime())) return start;
    if (granularity === "minute") {
      return parsed.toLocaleTimeString(localeToIntl($locale), { hour: "2-digit", minute: "2-digit" });
    }
    if (granularity === "hour") {
      return parsed.toLocaleTimeString(localeToIntl($locale), { hour: "2-digit", minute: "2-digit" });
    }
    return parsed.toLocaleDateString(localeToIntl($locale), { month: "short", day: "numeric" });
  }

  function buildChart(): void {
    if (!canvas) return;
    chart?.destroy();
    const dark = $isDarkMode;
    const labels = buckets.map((bucket) => formatBucket(bucket.bucketStart, granularity));
    // Chart.js accepts nulls in a line dataset, so the hit-rate series can skip
    // buckets where no cache read was recorded instead of plotting a false 0.
    const datasets: ChartDataset<"line", (number | null)[]>[] = series.map((entry) => ({
      label: $translate(`usageRecords.${entry.labelKey}`),
      data: buckets.map((bucket) => bucket[entry.key] as number),
      borderColor: entry.color,
      backgroundColor: entry.color + "22",
      fill: entry.key === "inputTokens",
      tension: 0.35,
      pointRadius: 0,
      borderWidth: 2,
      yAxisID: "y",
    }));
    // The cache hit rate shares the x-axis but needs its own 0-100% scale.
    datasets.push({
      label: $translate("usageRecords.trendCacheHitRate"),
      data: buckets.map((bucket) => (bucket.cacheHitRatio > 0 ? Math.round(bucket.cacheHitRatio * 1000) / 10 : null)),
      borderColor: dark ? "#e5e7eb" : "#111827",
      borderDash: [4, 4],
      fill: false,
      tension: 0.35,
      pointRadius: 0,
      borderWidth: 1,
      yAxisID: "hitRate",
    });

    chart = new Chart(canvas, {
      type: "line",
      data: { labels, datasets },
      options: {
        responsive: true,
        maintainAspectRatio: false,
        animation: false,
        interaction: { mode: "index", intersect: false },
        plugins: {
          legend: {
            position: "top",
            labels: {
              color: dark ? "#d1d5db" : "#374151",
              usePointStyle: true,
              pointStyle: "circle",
              padding: 12,
              font: { size: 11 },
            },
          },
          tooltip: {
            backgroundColor: dark ? "#1f2937" : "#ffffff",
            titleColor: dark ? "#f3f4f6" : "#111827",
            bodyColor: dark ? "#d1d5db" : "#374151",
            borderColor: dark ? "#374151" : "#e5e7eb",
            borderWidth: 1,
          },
        },
        scales: {
          y: {
            beginAtZero: true,
            ticks: {
              color: dark ? "#9ca3af" : "#6b7280",
              callback: (value) => formatCompact(value as number),
            },
            grid: { color: dark ? "rgba(255,255,255,0.08)" : "rgba(0,0,0,0.08)" },
          },
          hitRate: {
            position: "right",
            beginAtZero: true,
            max: 100,
            ticks: {
              color: dark ? "#9ca3af" : "#6b7280",
              callback: (value) => `${value}%`,
            },
            grid: { display: false },
          },
          x: {
            ticks: { color: dark ? "#9ca3af" : "#6b7280", maxRotation: 0, autoSkip: true, maxTicksLimit: 12 },
            grid: { display: false },
          },
        },
      },
    });
  }

  function formatCompact(value: number): string {
    if (Math.abs(value) >= 1_000_000) return `${(value / 1_000_000).toFixed(1)}M`;
    if (Math.abs(value) >= 1_000) return `${(value / 1_000).toFixed(0)}K`;
    return String(value);
  }

  $effect(() => {
    void buckets;
    void $isDarkMode;
    buildChart();
  });

  onMount(buildChart);
  onDestroy(() => chart?.destroy());
</script>

<Card.Root class="p-4">
  <h4 class="text-sm font-semibold">{$translate("usageRecords.tokenTrend")}</h4>
  <div class="mt-3 h-56">
    {#if buckets.length > 0}
      <canvas bind:this={canvas} aria-label={$translate("usageRecords.tokenTrend")}></canvas>
    {:else}
      <div class="text-muted-foreground flex h-full items-center justify-center text-sm">
        {$translate("usageRecords.noData")}
      </div>
    {/if}
  </div>
</Card.Root>
