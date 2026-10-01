<script lang="ts">
  import { untrack } from "svelte";
  import type { ActivityFilters } from "../../lib/activityFilters";
  import type { ContextBucketData, InflightRequestEntry, SpeedReportData } from "../../lib/types";
  import { getSpeedReport } from "../../stores/api";
  import { activityRevision } from "../../stores/api";
  import { persistentStore } from "../../stores/persistent";
  import { connectionState } from "../../stores/theme";
  import { translate, localeToIntl, locale } from "../../lib/i18n";
  import { formatCompactNumber } from "../../lib/format";
  import {
    SPEED_WINDOW_LABEL_KEYS,
    contextBucketFullLabel,
    contextBucketShortLabel,
    contextRows,
    isSpeedWindowKey,
    rateOrDash,
    secondsOrDash,
    selectedContextSeries,
    selectedSpeedSeries,
    speedAverages,
    speedChartData,
    speedTrend,
    type SpeedWindowKey,
  } from "../../lib/speedSeries";
  import PerformanceChart from "../PerformanceChart.svelte";
  import SegmentedControl from "../SegmentedControl.svelte";
  import SpeedKpiCards from "./SpeedKpiCards.svelte";
  import * as Card from "$lib/components/ui/card/index.js";
  import * as Select from "$lib/components/ui/select/index.js";
  import * as Table from "$lib/components/ui/table/index.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import { RefreshCw } from "@lucide/svelte";

  interface Props {
    filters: ActivityFilters;
    inflightRequests: InflightRequestEntry[];
  }

  let { filters, inflightRequests }: Props = $props();

  const WINDOW_ORDER: SpeedWindowKey[] = ["hour", "6h", "day", "week", "all"];
  // The curves follow one model at a time because rates are only comparable
  // within an engine; the comparison table below lists every model.
  const storedModel = persistentStore<string>("activity-speed-model", "");
  const storedWindow = persistentStore<SpeedWindowKey>("activity-speed-window", "hour");

  let selectedModel = $state($storedModel);
  let selectedWindow = $state<SpeedWindowKey>(
    isSpeedWindowKey($storedWindow) ? $storedWindow : "hour",
  );
  let report = $state<SpeedReportData | null>(null);
  let loading = $state(true);
  let refreshing = $state(false);
  let error = $state("");
  let requestID = 0;
  let pollTimer: ReturnType<typeof setInterval> | null = null;
  let revisionRefreshTimer: ReturnType<typeof setTimeout> | null = null;

  let series = $derived(selectedSpeedSeries(report, selectedModel));
  let contextSeries = $derived(selectedContextSeries(report, selectedModel));
  let chartData = $derived(speedChartData(series?.points ?? [], report?.bucket_seconds ?? 0));
  let averages = $derived(speedAverages(series?.points ?? []));
  let trend = $derived(speedTrend(series?.points ?? []));
  let tableRows = $derived(contextRows(report));

  let windowItems = $derived(
    WINDOW_ORDER.map((key) => ({ key, label: $translate(SPEED_WINDOW_LABEL_KEYS[key]) })),
  );
  let modelOptions = $derived(
    (report?.series ?? []).map((entry) => ({ value: entry.model, label: entry.model })),
  );

  async function load(): Promise<void> {
    const id = ++requestID;
    try {
      const result = await getSpeedReport({
        model: selectedModel,
        filters,
        window: selectedWindow,
        configuredOnly: true,
      });
      if (id !== requestID) return;
      report = result;
      error = "";
      // A remembered model can disappear from the report (deleted or filtered
      // out). Fall back to the busiest model so the charts keep something.
      if (selectedModel !== "" && !result.series.some((entry) => entry.model === selectedModel)) {
        selectedModel = "";
        storedModel.set("");
      }
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

  // Reload when the model, window or drawer filters change. The fetch is
  // untracked so the state it writes cannot re-trigger the effect.
  $effect(() => {
    selectedModel;
    selectedWindow;
    filters;
    untrack(() => {
      void load();
    });
  });

  // Live traffic should show up without a manual refresh, but a busy window
  // must not turn into one request per activity event.
  let seenRevision = $activityRevision;
  $effect(() => {
    if ($connectionState !== "connected") return;
    const revision = $activityRevision;
    untrack(() => {
      if (revision === seenRevision) return;
      seenRevision = revision;
      if (revisionRefreshTimer !== null) clearTimeout(revisionRefreshTimer);
      revisionRefreshTimer = setTimeout(() => {
        revisionRefreshTimer = null;
        void load();
      }, 10_000);
    });
  });

  $effect(() => {
    if ($connectionState !== "connected") return;
    if (pollTimer !== null) clearInterval(pollTimer);
    pollTimer = setInterval(() => {
      if (typeof document !== "undefined" && document.hidden) return;
      void load();
    }, 30_000);
    return () => {
      if (pollTimer !== null) {
        clearInterval(pollTimer);
        pollTimer = null;
      }
    };
  });

  $effect(() => {
    return () => {
      if (revisionRefreshTimer !== null) clearTimeout(revisionRefreshTimer);
    };
  });

  function setModel(next: string): void {
    selectedModel = next;
    storedModel.set(next);
  }

  function setWindow(key: SpeedWindowKey): void {
    selectedWindow = key;
    storedWindow.set(key);
  }

  function activeWindowIndex(): number {
    const index = WINDOW_ORDER.indexOf(selectedWindow);
    return index < 0 ? 0 : index;
  }

  function perTokenLatency(bucket: ContextBucketData): number {
    return bucket.decode_tps > 0 ? 1000 / bucket.decode_tps : -1;
  }

  function compact(value: number): string {
    return formatCompactNumber(value, $locale);
  }

  function full(value: number): string {
    if (!Number.isFinite(value) || value < 0) return "—";
    return new Intl.NumberFormat(localeToIntl($locale)).format(value);
  }
</script>

<Card.Root class="p-3">
  <div class="flex flex-wrap items-center justify-between gap-3">
    <div class="min-w-0">
      <div class="text-sm font-semibold">{$translate("activity.speed.title")}</div>
      <div class="text-muted-foreground text-xs">{$translate("activity.speed.subtitle")}</div>
    </div>
    <div class="flex flex-wrap items-center gap-2">
      {#if modelOptions.length > 0}
        <Select.Root type="single" value={selectedModel} onValueChange={(next) => setModel(next || "")}>
          <Select.Trigger class="max-w-[22rem]" aria-label={$translate("activity.speed.model")}>
            <span class="truncate">{series?.model ?? $translate("activity.speed.modelFallback")}</span>
          </Select.Trigger>
          <Select.Content>
            {#each modelOptions as option (option.value)}
              <Select.Item value={option.value}>{option.label}</Select.Item>
            {/each}
          </Select.Content>
        </Select.Root>
      {/if}
      <SegmentedControl
        items={windowItems}
        selected={activeWindowIndex()}
        onSelect={(index) => setWindow(WINDOW_ORDER[index] ?? "hour")}
        label={$translate("activity.speed.windowLabel")}
      />
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
  {:else if !series || series.points.length === 0}
    <div class="text-muted-foreground py-10 text-center text-sm">{$translate("activity.speed.empty")}</div>
  {:else}
    <div class="mt-3">
      <SpeedKpiCards
        {averages}
        {trend}
        points={series.points}
        {inflightRequests}
        model={series.model}
      />
    </div>

    <div class="mt-3">
      <PerformanceChart
        title={`${$translate("activity.speed.curveTitle")} · ${series.model}`}
        labels={chartData.labels}
        datasets={[
          {
            label: $translate("activity.speed.series.prefill"),
            data: chartData.prefill,
            borderColor: "#8b5cf6",
            axis: "y",
          },
          {
            label: $translate("activity.speed.series.decode"),
            data: chartData.decode,
            borderColor: "#3b82f6",
            axis: "y",
          },
          {
            label: $translate("activity.speed.series.ttft"),
            data: chartData.ttftSeconds,
            borderColor: "#10b981",
            axis: "y1",
          },
        ]}
        yMin={0}
        yLabel="tok/s"
        y2Min={0}
        y2Label="s"
      />
    </div>

    {#if contextSeries && contextSeries.buckets.length > 0}
      <div class="mt-3">
        <PerformanceChart
          title={`${$translate("activity.speed.contextTitle")} · ${contextSeries.model}`}
          labels={contextSeries.buckets.map(contextBucketShortLabel)}
          datasets={[
            {
              label: $translate("activity.speed.series.prefill"),
              data: contextSeries.buckets.map((bucket: ContextBucketData) =>
                bucket.prefill_tps >= 0 ? bucket.prefill_tps : null,
              ),
              borderColor: "#8b5cf6",
              pointLabels: true,
            },
            {
              label: $translate("activity.speed.series.decode"),
              data: contextSeries.buckets.map((bucket: ContextBucketData) =>
                bucket.decode_tps >= 0 ? bucket.decode_tps : null,
              ),
              borderColor: "#3b82f6",
              pointLabels: true,
            },
          ]}
          yMin={0}
          yLabel="tok/s"
        />
      </div>
    {/if}
  {/if}

  {#if !loading && tableRows.length > 0}
    <Card.Root class="mt-3 overflow-hidden p-0">
      <Card.Header class="flex items-center justify-between border-b px-4 py-2">
        <Card.Title class="text-sm font-semibold">{$translate("activity.speed.contextTable")}</Card.Title>
      </Card.Header>
      <div class="overflow-x-auto">
        <Table.Root class="min-w-[72rem]">
          <Table.Header>
            <Table.Row>
              <Table.Head>{$translate("activity.speed.table.model")}</Table.Head>
              <Table.Head>{$translate("activity.speed.table.context")}</Table.Head>
              <Table.Head>{$translate("activity.speed.table.requests")}</Table.Head>
              <Table.Head>{$translate("activity.speed.table.avgInput")}</Table.Head>
              <Table.Head>{$translate("activity.speed.table.avgOutput")}</Table.Head>
              <Table.Head>{$translate("activity.speed.table.prefill")}</Table.Head>
              <Table.Head>{$translate("activity.speed.table.decode")}</Table.Head>
              <Table.Head>{$translate("activity.speed.table.ttft")}</Table.Head>
              <Table.Head>{$translate("activity.speed.table.perToken")}</Table.Head>
            </Table.Row>
          </Table.Header>
          <Table.Body>
            {#each tableRows as row (row.model + row.bucket.label)}
              <Table.Row>
                <Table.Cell class="max-w-[18rem] truncate font-medium" title={row.model}>{row.model}</Table.Cell>
                <Table.Cell title={full(row.bucket.max_tokens || row.bucket.min_tokens)}>
                  {contextBucketFullLabel(row.bucket)}
                </Table.Cell>
                <Table.Cell title={full(row.bucket.requests)}>{compact(row.bucket.requests)}</Table.Cell>
                <Table.Cell title={`${full(row.bucket.avg_input_tokens)} Token`}>
                  {compact(row.bucket.avg_input_tokens)} Token
                </Table.Cell>
                <Table.Cell title={`${full(row.bucket.avg_output_tokens)} Token`}>
                  {compact(row.bucket.avg_output_tokens)} Token
                </Table.Cell>
                <Table.Cell title={full(row.bucket.prefill_tps)}>{rateOrDash(row.bucket.prefill_tps)} tok/s</Table.Cell>
                <Table.Cell title={full(row.bucket.decode_tps)}>{rateOrDash(row.bucket.decode_tps)} tok/s</Table.Cell>
                <Table.Cell title={full(row.bucket.ttft_ms / 1000)}>{secondsOrDash(row.bucket.ttft_ms / 1000)} s</Table.Cell>
                <Table.Cell title={full(perTokenLatency(row.bucket))}>
                  {rateOrDash(perTokenLatency(row.bucket))} ms
                </Table.Cell>
              </Table.Row>
            {/each}
          </Table.Body>
        </Table.Root>
      </div>
    </Card.Root>
  {/if}
</Card.Root>
