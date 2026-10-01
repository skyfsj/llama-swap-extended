<script lang="ts">
  import { onMount, onDestroy } from "svelte";
  import {
    ArcElement,
    Chart,
    DoughnutController,
    Legend,
    Tooltip,
  } from "chart.js";
  import { isDarkMode } from "../../stores/theme";
  import { locale, localeToIntl, translate } from "../../lib/i18n";
  import { formatTokenCount } from "../../lib/usage";
  import * as Card from "$lib/components/ui/card/index.js";

  // Only the doughnut pieces these charts use, instead of Chart.js's full
  // registerables (every chart type, scale and plugin).
  Chart.register(DoughnutController, ArcElement, Legend, Tooltip);

  interface DonutRow {
    name: string;
    /** Primary chart value: request count. */
    value: number;
    requests: number;
    cost: number;
    inputTokens: number;
    outputTokens: number;
  }

  interface Props {
    title: string;
    rows: DonutRow[];
    valueLabel: string;
    emptyLabel: string;
  }

  let { title, rows, valueLabel, emptyLabel }: Props = $props();

  let canvas: HTMLCanvasElement | undefined = $state();
  let chart: Chart | undefined;

  // Screenshot-like palette: one accent blue that repeats, so a long tail of
  // models still reads as one series instead of a colour explosion.
  const palette = [
    "#3b82f6",
    "#60a5fa",
    "#1d4ed8",
    "#93c5fd",
    "#2563eb",
    "#bfdbfe",
    "#1e40af",
  ];

  function paletteColor(index: number, dark: boolean): string {
    const base = palette[index % palette.length];
    if (!dark) return base;
    // Shift the same hues brighter on a dark background.
    const bright = [
      "#60a5fa",
      "#93c5fd",
      "#3b82f6",
      "#bfdbfe",
      "#2563eb",
      "#dbeafe",
      "#1d4ed8",
    ];
    return bright[index % bright.length];
  }

  function buildChart(): void {
    if (!canvas) return;
    chart?.destroy();
    const labels = rows.map((row) => row.name);
    const values = rows.map((row) => row.value);
    const dark = $isDarkMode;
    chart = new Chart(canvas, {
      type: "doughnut",
      data: {
        labels,
        datasets: [
          {
            data: values,
            backgroundColor: labels.map((_, index) => paletteColor(index, dark)),
            borderColor: dark ? "#0f172a" : "#ffffff",
            borderWidth: 2,
          },
        ],
      },
      options: {
        responsive: true,
        maintainAspectRatio: false,
        cutout: "62%",
        animation: false,
        plugins: {
          legend: {
            position: "right",
            labels: {
              color: dark ? "#d1d5db" : "#374151",
              usePointStyle: true,
              pointStyle: "circle",
              padding: 12,
              boxWidth: 8,
              font: { size: 11 },
            },
          },
          tooltip: {
            backgroundColor: dark ? "#1f2937" : "#ffffff",
            titleColor: dark ? "#f3f4f6" : "#111827",
            bodyColor: dark ? "#d1d5db" : "#374151",
            borderColor: dark ? "#374151" : "#e5e7eb",
            borderWidth: 1,
            callbacks: {
              label: (context) => {
                const row = rows[context.dataIndex];
                if (!row) return "";
                const total = values.reduce((sum, value) => sum + value, 0) || 1;
                const share = ((row.value / total) * 100).toFixed(1);
                return `${row.value.toLocaleString(localeToIntl($locale))} ${valueLabel} (${share}%) · ${formatTokenCount(row.inputTokens + row.outputTokens)} tokens`;
              },
            },
          },
        },
      },
    });
  }

  $effect(() => {
    // Re-render when the data or the theme changes.
    void rows;
    void $isDarkMode;
    buildChart();
  });

  onMount(buildChart);
  onDestroy(() => chart?.destroy());
</script>

<Card.Root class="p-4">
  <div class="flex flex-wrap items-center justify-between gap-2">
    <h4 class="text-sm font-semibold">{title}</h4>
    <span class="text-muted-foreground text-xs">{$translate("usageRecords.distributionByToken")}</span>
  </div>
  <div class="mt-3 grid gap-4 sm:grid-cols-[minmax(0,220px)_1fr]">
    <div class="relative h-44">
      {#if rows.length > 0}
        <canvas bind:this={canvas} aria-label={title}></canvas>
      {:else}
        <div class="text-muted-foreground flex h-full items-center justify-center text-sm">{emptyLabel}</div>
      {/if}
    </div>
    <div class="overflow-x-auto">
      <table class="w-full text-xs">
        <thead class="text-muted-foreground text-left">
          <tr>
            <th class="py-1.5 font-medium">{title}</th>
            <th class="py-1.5 text-right font-medium">{valueLabel}</th>
            <th class="py-1.5 text-right font-medium">{$translate("usageRecords.tokens")}</th>
            <th class="py-1.5 text-right font-medium">{$translate("usageRecords.cost")}</th>
          </tr>
        </thead>
        <tbody>
          {#each rows as row (row.name)}
            <tr class="border-t">
              <td class="max-w-[220px] truncate py-1.5" title={row.name}>{row.name}</td>
              <td class="py-1.5 text-right tabular-nums">{row.requests.toLocaleString(localeToIntl($locale))}</td>
              <td class="py-1.5 text-right tabular-nums" title={(row.inputTokens + row.outputTokens).toLocaleString(localeToIntl($locale))}>{formatTokenCount(row.inputTokens + row.outputTokens)}</td>
              <td class="py-1.5 text-right tabular-nums">${row.cost.toFixed(4)}</td>
            </tr>
          {/each}
          {#if rows.length === 0}
            <tr>
              <td colspan={4} class="text-muted-foreground py-3 text-center">{emptyLabel}</td>
            </tr>
          {/if}
        </tbody>
      </table>
    </div>
  </div>
</Card.Root>
