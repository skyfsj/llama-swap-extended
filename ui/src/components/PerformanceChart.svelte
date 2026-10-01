<script lang="ts">
  import { onMount } from "svelte";
  import {
    Chart,
    LineController,
    LineElement,
    PointElement,
    LinearScale,
    CategoryScale,
    Legend,
    Title,
    Tooltip,
  } from "chart.js";
  import { isDarkMode } from "../stores/theme";
  import * as Card from "$lib/components/ui/card/index.js";

  // Only the line-chart pieces this component actually uses, instead of
  // Chart.js's full `registerables` (every chart type/scale/plugin).
  Chart.register(
    LineController,
    LineElement,
    PointElement,
    LinearScale,
    CategoryScale,
    Legend,
    Title,
    Tooltip,
  );

  interface Dataset {
    label: string;
    data: (number | null)[];
    borderColor: string;
    /** Axis this series is measured on. Defaults to the primary (left) axis. */
    axis?: "y" | "y1";
    /** Label every point with its value instead of only on hover. */
    pointLabels?: boolean;
  }

  interface Props {
    title: string;
    labels: string[];
    datasets: Dataset[];
    yMin?: number;
    yMax?: number;
    yLabel?: string;
    /** Right-hand axis, for a series measured in different units. */
    y2Label?: string;
    y2Min?: number;
    y2Max?: number;
    showLegend?: boolean;
  }

  let { title, labels, datasets, yMin, yMax, yLabel, y2Label, y2Min, y2Max, showLegend = true }: Props = $props();

  let canvas: HTMLCanvasElement;
  let chart: Chart;

  function getChartColors(dark: boolean) {
    return {
      grid: dark ? "rgba(255,255,255,0.08)" : "rgba(0,0,0,0.08)",
      tick: dark ? "#9ca3af" : "#6b7280",
      legend: dark ? "#d1d5db" : "#374151",
      tooltipBg: dark ? "#1f2937" : "#ffffff",
      tooltipText: dark ? "#f3f4f6" : "#111827",
      tooltipBorder: dark ? "#374151" : "#e5e7eb",
    };
  }

  // Point labels keep sparse category charts readable without hovering, which
  // matters for context-length tables where each bucket is a handful of
  // requests. They are opt-in because a dense time series would become noise.
  const valueLabelPlugin = {
    id: "valueLabels",
    afterDatasetsDraw(chartInstance: Chart) {
      const { ctx } = chartInstance;
      ctx.save();
      ctx.font = "10px ui-sans-serif, system-ui, sans-serif";
      ctx.textAlign = "center";
      ctx.textBaseline = "bottom";
      chartInstance.data.datasets.forEach((dataset, datasetIndex) => {
        if (!(dataset as Dataset).pointLabels) return;
        const meta = chartInstance.getDatasetMeta(datasetIndex);
        if (meta.hidden) return;
        const color = dataset.borderColor;
        ctx.fillStyle = typeof color === "string" ? color : "#6b7280";
        meta.data.forEach((point, index) => {
          const value = dataset.data?.[index];
          if (value === null || value === undefined) return;
          // Chart.js types a data point as number | [x, y] | Point; only the
          // scalar form carries a value worth labelling.
          const numeric = typeof value === "number" ? value : null;
          if (numeric === null || !Number.isFinite(numeric)) return;
          const text = Number.isInteger(numeric) ? String(numeric) : numeric.toFixed(1);
          ctx.fillText(text, point.x, point.y - 5);
        });
      });
      ctx.restore();
    },
  };

  function buildOptions(dark: boolean) {
    const colors = getChartColors(dark);
    const usesSecondaryAxis = datasets.some((dataset) => dataset.axis === "y1");
    return {
      responsive: true,
      maintainAspectRatio: false,
      animation: false as const,
      interaction: {
        mode: "index" as const,
        intersect: false,
      },
      plugins: {
        legend: {
          display: showLegend,
          position: "top" as const,
          labels: {
            color: colors.legend,
            usePointStyle: true,
            pointStyle: "circle" as const,
            padding: 12,
            font: { size: 11 },
          },
        },
        title: {
          display: true,
          text: title,
          color: colors.legend,
          font: { size: 14, weight: "bold" as const },
        },
        tooltip: {
          backgroundColor: colors.tooltipBg,
          titleColor: colors.tooltipText,
          bodyColor: colors.tooltipText,
          borderColor: colors.tooltipBorder,
          borderWidth: 1,
        },
      },
      scales: {
        x: {
          bounds: "data" as const,
          offset: false,
          ticks: { color: colors.tick, maxRotation: 0, font: { size: 10 }, maxTicksLimit: 10 },
          grid: { color: colors.grid },
        },
        y: {
          min: yMin,
          max: yMax,
          ticks: { color: colors.tick, font: { size: 10 } },
          grid: { color: colors.grid },
          title: yLabel
            ? { display: true, text: yLabel, color: colors.tick }
            : undefined,
        },
        ...(usesSecondaryAxis
          ? {
              y1: {
                position: "right" as const,
                min: y2Min,
                max: y2Max,
                // A right axis must not paint over the primary grid.
                grid: { drawOnChartArea: false, color: colors.grid },
                ticks: { color: colors.tick, font: { size: 10 } },
                title: y2Label
                  ? { display: true, text: y2Label, color: colors.tick }
                  : undefined,
              },
            }
          : {}),
      },
    };
  }

  function toChartDatasets(source: Dataset[]) {
    return source.map((ds) => ({
      label: ds.label,
      data: [...ds.data],
      borderColor: ds.borderColor,
      backgroundColor: ds.borderColor + "20",
      borderWidth: 1.5,
      // Labelled points stay visible; an unlabelled series is only a curve.
      pointRadius: ds.pointLabels ? 3 : 0,
      tension: 0.4,
      fill: false,
      yAxisID: ds.axis === "y1" ? "y1" : "y",
    }));
  }

  onMount(() => {
    chart = new Chart(canvas, {
      type: "line",
      data: {
        labels: [...labels],
        datasets: toChartDatasets(datasets),
      },
      options: buildOptions($isDarkMode),
      plugins: [valueLabelPlugin],
    });

    return () => {
      chart.destroy();
    };
  });

  $effect(() => {
    if (!chart) return;
    const _dark = $isDarkMode;
    const _title = title;
    const _yLabel = yLabel;
    const _y2Label = y2Label;
    chart.options = buildOptions(_dark);
    void _title;
    void _yLabel;
    void _y2Label;
    chart.update("none");
  });

  $effect(() => {
    if (!chart) return;
    const _l = labels;
    const _d = datasets;
    chart.data.labels = [..._l];
    chart.data.datasets = toChartDatasets(_d);
    chart.update("none");
  });
</script>

<Card.Root class="h-[300px] py-0">
  <Card.Content class="h-full p-4">
    <canvas bind:this={canvas}></canvas>
  </Card.Content>
</Card.Root>
