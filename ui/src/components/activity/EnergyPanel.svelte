<script lang="ts">
  import { untrack } from "svelte";
  import type { PowerReportData, PowerSeriesData, PowerSeriesPointData } from "../../lib/types";
  import { getPowerReport } from "../../stores/api";
  import { activityRevision, livePowerSamples } from "../../stores/api";
  import { persistentStore } from "../../stores/persistent";
  import { connectionState } from "../../stores/theme";
  import { translate } from "../../lib/i18n";
  import {
    POWER_SERIES_COLORS,
    POWER_WINDOW_LABEL_KEYS,
    POWER_WINDOW_OPTIONS,
    currentWatts,
    formatCost,
    formatGpuSeconds,
    formatKwh,
    formatWatts,
    hasPowerData,
    isPowerWindowKey,
    monthlyEstimate,
    powerChartData,
    seriesLabel,
    tariffLabel,
    type PowerWindowKey,
  } from "../../lib/energyReport";
  import PerformanceChart from "../PerformanceChart.svelte";
  import SegmentedControl from "../SegmentedControl.svelte";
  import * as Card from "$lib/components/ui/card/index.js";
  import * as Table from "$lib/components/ui/table/index.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import { RefreshCw, Zap } from "@lucide/svelte";

  const storedWindow = persistentStore<PowerWindowKey>("activity-power-window", "day");

  let selectedWindow = $state<PowerWindowKey>(
    isPowerWindowKey($storedWindow) ? $storedWindow : "day",
  );
  let report = $state<PowerReportData | null>(null);
  let loading = $state(true);
  let refreshing = $state(false);
  let error = $state("");
  let requestID = 0;
  let pollTimer: ReturnType<typeof setInterval> | null = null;

  let currency = $derived(report?.currency ?? "");

  // Live samples arrive via SSE between store flushes and are appended per
  // source so the curve grows in real time. They are dropped on each store
  // load: the report's own series then covers the same span authoritatively.
  const LIVE_SAMPLE_CAP = 400;
  let liveBySource = $state<Record<string, PowerSeriesPointData[]>>({});

  $effect(() => {
    const batch = $livePowerSamples;
    untrack(() => {
      const entries: Record<string, PowerSeriesPointData[]> = {
        node: batch.node ?? [],
      };
      // GPU batches carry every card; the server's stat key keeps per-card
      // streams separable, falling back to position when no UUID exists.
      (batch.gpu ?? []).forEach((point, index) => {
        const key = gpuStatKeys[index] ?? String(index);
        entries[key] = [point];
      });
      for (const [key, points] of Object.entries(entries)) {
        if (points.length === 0) continue;
        const bucket = liveBySource[key] ?? [];
        const merged = [...bucket, ...points].slice(-LIVE_SAMPLE_CAP);
        liveBySource = { ...liveBySource, [key]: merged };
      }
    });
  });

  let gpuStatKeys = $derived((report?.series ?? []).filter((entry) => !entry.is_node).map((entry) => entry.source_id));

  let gpuSeries = $derived.by(() => {
    const stored = (report?.series ?? []).filter((entry) => !entry.is_node);
    return stored.map((entry) => fuseLive(entry, liveBySource[entry.source_id] ?? []));
  });
  let nodeSeries = $derived.by(() => {
    const stored = (report?.series ?? []).find((entry) => entry.is_node) ?? null;
    return stored ? fuseLive(stored, liveBySource["node"] ?? []) : null;
  });
  let chartSeries = $derived((nodeSeries ? [nodeSeries] : []).concat(gpuSeries));
  let chartData = $derived(powerChartData(chartSeries, report?.bucket_seconds ?? 0));
  let totals = $derived(report?.totals);
  let estimate = $derived.by(() => {
    if (!totals) return 0;
    return monthlyEstimate(
      totals.month_cost,
      totals.month_coverage_hours,
      currentPower,
      report?.price_per_kwh ?? 0,
    );
  });

  // The headline "now" reading prefers the node meter (whole machine) and
  // falls back to the busiest GPU card.
  let currentPower = $derived.by(() => {
    const node = currentWatts(nodeSeries);
    if (node >= 0) return node;
    let best = -1;
    for (const entry of gpuSeries) {
      const watts = currentWatts(entry);
      if (watts > best) best = watts;
    }
    return best;
  });

  let windowItems = $derived(
    POWER_WINDOW_OPTIONS.map((option) => ({ key: option.key, label: $translate(POWER_WINDOW_LABEL_KEYS[option.key]) })),
  );

  /**
   * fuseLive appends live SSE samples newer than the stored series' last
   * point. The stored series stays untouched; live points only extend it so
   * the curve keeps growing between store flushes.
   */
  function fuseLive(entry: PowerSeriesData, live: PowerSeriesPointData[]): PowerSeriesData {
    if (live.length === 0) return entry;
    const lastTs = entry.points.length > 0 ? entry.points[entry.points.length - 1].timestamp : "";
    const newer = live.filter((point) => point.timestamp > lastTs);
    if (newer.length === 0) return entry;
    return { ...entry, points: [...entry.points, ...newer] };
  }

  async function load(): Promise<void> {
    const id = ++requestID;
    liveBySource = {};
    try {
      const result = await getPowerReport({
        window: POWER_WINDOW_OPTIONS.find((option) => option.key === selectedWindow)?.api,
      });
      if (id !== requestID) return;
      report = result;
      error = "";
    } catch (cause) {
      if (id !== requestID) return;
      error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (id === requestID) {
        loading = false;
        refreshing = false;
      }
    }
  }

  function refreshNow(): void {
    refreshing = true;
    void load();
  }

  $effect(() => {
    selectedWindow;
    untrack(() => {
      void load();
    });
  });

  // Power samples land at most once per flush interval; a slow poll keeps the
  // panel current without hammering the report aggregation.
  $effect(() => {
    if ($connectionState !== "connected") return;
    if (pollTimer !== null) clearInterval(pollTimer);
    pollTimer = setInterval(() => {
      if (typeof document !== "undefined" && document.hidden) return;
      void load();
    }, 60_000);
    return () => {
      if (pollTimer !== null) {
        clearInterval(pollTimer);
        pollTimer = null;
      }
    };
  });

  let seenRevision = $activityRevision;
  $effect(() => {
    if ($connectionState !== "connected") return;
    const revision = $activityRevision;
    untrack(() => {
      if (revision === seenRevision) return;
      seenRevision = revision;
      void load();
    });
  });

  function setWindow(key: PowerWindowKey): void {
    selectedWindow = key;
    storedWindow.set(key);
  }

  function activeWindowIndex(): number {
    const index = POWER_WINDOW_OPTIONS.findIndex((option) => option.key === selectedWindow);
    return index < 0 ? 2 : index;
  }

  function datasetColor(index: number): string {
    return POWER_SERIES_COLORS[index % POWER_SERIES_COLORS.length];
  }

  function kpiLabel(energyWh: number | undefined, cost: number | undefined): string {
    if (!totals) return "—";
    const kwh = formatKwh(energyWh ?? -1);
    if (!currency || !(cost !== undefined && cost > 0)) return kwh;
    return `${kwh} · ${formatCost(cost, currency)}`;
  }

  function inferenceShare(): number {
    if (!totals || totals.energy_wh <= 0) return 0;
    return Math.min(100, (totals.inference_energy_wh / totals.energy_wh) * 100);
  }
</script>

<Card.Root class="p-3">
  <div class="flex flex-wrap items-center justify-between gap-3">
    <div class="min-w-0">
      <div class="text-sm font-semibold">{$translate("activity.energy.title")}</div>
      <div class="text-muted-foreground text-xs">{$translate("activity.energy.subtitle")}</div>
    </div>
    <div class="flex flex-wrap items-center gap-2">
      <Button
        variant="outline"
        size="sm"
        onclick={refreshNow}
        disabled={refreshing}
        title={$translate("activity.speed.refresh")}
      >
        <RefreshCw class={refreshing ? "animate-spin" : ""} size={14} />
      </Button>
    </div>
  </div>

  {#if error}
    <div class="text-destructive mt-3 text-sm">{error}</div>
  {/if}

  {#if loading}
    <div class="text-muted-foreground py-10 text-center text-sm">{$translate("common.loading")}</div>
  {:else if !hasPowerData(report)}
    <div class="text-muted-foreground py-10 text-center text-sm">
      {$translate("activity.energy.unavailable")}
    </div>
  {:else}
    <div class="mt-3 grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
      <Card.Root class="p-3">
        <div class="flex items-center gap-1.5 text-muted-foreground text-xs">
          <Zap size={12} />
          {$translate("activity.energy.kpi.current")}
        </div>
        <div class="mt-1 text-lg font-semibold">{formatWatts(currentPower)}</div>
        <div class="text-muted-foreground text-xs">
          {#if report?.sources.ipmi}
            {$translate("activity.energy.kpi.currentNode")}
          {:else if gpuSeries.length > 0}
            {$translate("activity.energy.kpi.currentGpu")}
          {:else}
            —
          {/if}
        </div>
      </Card.Root>
      <Card.Root class="p-3">
        <div class="text-muted-foreground text-xs">{$translate("activity.energy.kpi.today")}</div>
        <div class="mt-1 text-lg font-semibold">{kpiLabel(totals?.today_energy_wh, totals?.today_cost)}</div>
        <div class="text-muted-foreground text-xs">{$translate("activity.energy.kpi.todayHint")}</div>
      </Card.Root>
      <Card.Root class="p-3">
        <div class="text-muted-foreground text-xs">{$translate("activity.energy.kpi.month")}</div>
        <div class="mt-1 text-lg font-semibold">{kpiLabel(totals?.month_energy_wh, totals?.month_cost)}</div>
        <div class="text-muted-foreground text-xs">{$translate("activity.energy.kpi.monthEstimate", { value: formatCost(estimate, currency) })}</div>
      </Card.Root>
    </div>

    <div class="mt-3">
      <div class="mb-2 flex flex-wrap items-center justify-between gap-3">
        <div class="text-sm font-semibold">{$translate("activity.energy.chartTitle")}</div>
        <SegmentedControl
          items={windowItems}
          selected={activeWindowIndex()}
          onSelect={(index) => setWindow(POWER_WINDOW_OPTIONS[index]?.key ?? "day")}
          label={$translate("activity.speed.windowLabel")}
        />
      </div>
      <PerformanceChart
        title=""
        labels={chartData.labels}
        datasets={chartSeries.map((entry: PowerSeriesData, index: number) => ({
          label: seriesLabel(entry),
          data: chartData.values[index] ?? [],
          borderColor: datasetColor(index),
        }))}
        yMin={0}
        yLabel="W"
      />
    </div>

    {#if (report?.models?.length ?? 0) > 0}
      <Card.Root class="mt-3 overflow-hidden p-0">
        <Card.Header class="flex items-center justify-between border-b px-4 py-2">
          <Card.Title class="text-sm font-semibold">{$translate("activity.energy.modelsTable")}</Card.Title>
          <span class="text-muted-foreground text-xs">
            {$translate("activity.energy.kpi.inferenceShare", { value: inferenceShare().toFixed(0) })}
          </span>
        </Card.Header>
        <div class="overflow-x-auto">
          <Table.Root class="min-w-[48rem]">
            <Table.Header>
              <Table.Row>
                <Table.Head>{$translate("activity.energy.table.model")}</Table.Head>
                <Table.Head>{$translate("activity.energy.table.energy")}</Table.Head>
                <Table.Head>{$translate("activity.energy.table.gross")}</Table.Head>
                <Table.Head>{$translate("activity.energy.table.cost")}</Table.Head>
                <Table.Head>{$translate("activity.energy.table.gpuTime")}</Table.Head>
                <Table.Head>{$translate("activity.energy.table.requests")}</Table.Head>
              </Table.Row>
            </Table.Header>
            <Table.Body>
              {#each report?.models ?? [] as model (model.model_id)}
                <Table.Row>
                  <Table.Cell class="max-w-[18rem] truncate font-medium" title={model.model_id}>{model.model_id}</Table.Cell>
                  <Table.Cell>{formatKwh(model.energy_wh)}</Table.Cell>
                  <Table.Cell class="text-muted-foreground">{formatKwh(model.gross_energy_wh)}</Table.Cell>
                  <Table.Cell>{formatCost(model.cost, currency)}</Table.Cell>
                  <Table.Cell>{formatGpuSeconds(model.gpu_seconds)}</Table.Cell>
                  <Table.Cell>{model.requests}</Table.Cell>
                </Table.Row>
              {/each}
            </Table.Body>
          </Table.Root>
        </div>
        <div class="text-muted-foreground border-t px-4 py-2 text-xs">
          {$translate("activity.energy.attributionNote")}
        </div>
      </Card.Root>
    {/if}

    {#if (report?.tariffs?.length ?? 0) > 0 && (report?.totals?.by_tariff?.length ?? 0) > 0}
      <Card.Root class="mt-3 overflow-hidden p-0">
        <Card.Header class="flex items-center justify-between border-b px-4 py-2">
          <Card.Title class="text-sm font-semibold">{$translate("activity.energy.tariffTable")}</Card.Title>
        </Card.Header>
        <div class="overflow-x-auto">
          <Table.Root class="min-w-[32rem]">
            <Table.Header>
              <Table.Row>
                <Table.Head>{$translate("activity.energy.table.tariff")}</Table.Head>
                <Table.Head>{$translate("activity.energy.table.energy")}</Table.Head>
                <Table.Head>{$translate("activity.energy.table.cost")}</Table.Head>
              </Table.Row>
            </Table.Header>
            <Table.Body>
              {#each report?.totals?.by_tariff ?? [] as tariff (tariff.name)}
                <Table.Row>
                  <Table.Cell class="font-medium">{tariffLabel(tariff.name)}</Table.Cell>
                  <Table.Cell>{formatKwh(tariff.energy_wh)}</Table.Cell>
                  <Table.Cell>{formatCost(tariff.cost, currency)}</Table.Cell>
                </Table.Row>
              {/each}
            </Table.Body>
          </Table.Root>
        </div>
      </Card.Root>
    {/if}

    {#if report && !report.sources.ipmi}
      <div class="text-muted-foreground mt-2 text-xs">{$translate("activity.energy.noIpmi")}</div>
    {/if}
  {/if}
</Card.Root>
