<script lang="ts">
  import { onMount } from "svelte";
  import { Download, LoaderCircle, RefreshCw, RotateCcw } from "@lucide/svelte";
  import * as Card from "$lib/components/ui/card/index.js";
  import * as Select from "$lib/components/ui/select/index.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import { translate } from "../lib/i18n";
  import { formatTokenCount } from "../lib/usage";
  import {
    fetchUsageAnalytics,
    fetchUsageFilterOptions,
    fetchUsageRecords,
    usageExportUrl,
    type UsageAnalytics,
    type UsageFilters,
    type UsageGranularity,
    type UsageRecord,
  } from "../lib/usage";
  import UsageDonut from "../components/usage/UsageDonut.svelte";
  import UsageTrend from "../components/usage/UsageTrend.svelte";

  const granularityOptions: { value: UsageGranularity; labelKey: string }[] = [
    { value: "minute", labelKey: "usageRecords.granularityMinute" },
    { value: "hour", labelKey: "usageRecords.granularityHour" },
    { value: "day", labelKey: "usageRecords.granularityDay" },
  ];

  // Mirrors usageRangePresets in lib/usage.ts: the label comes from the
  // catalogue so the select stays localized, the resolution stays in one place.
  const rangePresets: { id: string; labelKey: string; days: number }[] = [
    { id: "today", labelKey: "usageRecords.rangeToday", days: 0 },
    { id: "7d", labelKey: "usageRecords.range7d", days: 7 },
    { id: "30d", labelKey: "usageRecords.range30d", days: 30 },
    { id: "month", labelKey: "usageRecords.rangeMonth", days: -1 },
  ];

  function startOfDay(date: Date): Date {
    return new Date(date.getFullYear(), date.getMonth(), date.getDate());
  }

  function addDays(date: Date, days: number): Date {
    const next = new Date(date);
    next.setDate(next.getDate() + days);
    return next;
  }

  let filters = $state<UsageFilters>({
    keys: [],
    models: [],
    groups: [],
    endpoints: [],
    granularity: "day",
    start: startOfDay(addDays(new Date(), -29)).toISOString(),
  });
  let rangePreset = $state<string>("30d");
  let analytics = $state<UsageAnalytics | null>(null);
  let options = $state<{ keys: { id: string; name: string; kind: string; group: string }[]; models: string[]; groups: string[]; endpoints: string[] }>({
    keys: [],
    models: [],
    groups: [],
    endpoints: [],
  });
  let records = $state<UsageRecord[]>([]);
  let recordTotal = $state(0);
  let loading = $state(true);
  let error = $state("");
  let requestID = 0;

  function applyRangePreset(preset: string): void {
    rangePreset = preset;
    const match = rangePresets.find((entry) => entry.id === preset);
    if (!match) return;
    const now = new Date();
    if (match.days === -1) {
      filters.start = new Date(now.getFullYear(), now.getMonth(), 1).toISOString();
    } else if (match.days === 0) {
      filters.start = startOfDay(now).toISOString();
    } else {
      filters.start = startOfDay(addDays(now, -(match.days - 1))).toISOString();
    }
    filters.end = undefined;
  }

  async function load(): Promise<void> {
    const id = ++requestID;
    loading = true;
    error = "";
    try {
      const [analyticsResult, optionsResult] = await Promise.all([
        fetchUsageAnalytics(filters),
        fetchUsageFilterOptions(filters),
      ]);
      if (id !== requestID) return;
      analytics = analyticsResult;
      options = optionsResult;
      const page = await fetchUsageRecords(filters, 200, 0);
      if (id !== requestID) return;
      records = page.data;
      recordTotal = page.total;
    } catch (cause) {
      if (id === requestID) error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (id === requestID) loading = false;
    }
  }

  function resetFilters(): void {
    filters.keys = [];
    filters.models = [];
    filters.groups = [];
    filters.endpoints = [];
    filters.start = startOfDay(addDays(new Date(), -29)).toISOString();
    filters.end = undefined;
    rangePreset = "30d";
    void load();
  }

  function selectValue(current: string[]): string {
    return current.length > 0 ? current[0] : "all";
  }

  function onSelectChange(field: "keys" | "models" | "groups" | "endpoints", value: string | undefined): void {
    if (!value) return;
    filters[field] = value === "all" ? [] : [value];
    void load();
  }

  function onGranularityChange(value: string | undefined): void {
    if (!value) return;
    if (value === "minute" || value === "hour" || value === "day") filters.granularity = value;
    void load();
  }

  function downloadCSV(): void {
    window.location.href = usageExportUrl(filters);
  }

  function formatDuration(ms: number): string {
    if (!Number.isFinite(ms) || ms <= 0) return "—";
    if (ms >= 1000) return `${(ms / 1000).toFixed(2)}s`;
    return `${Math.round(ms)}ms`;
  }

  function formatTimestamp(value: string): string {
    const parsed = new Date(value);
    return Number.isNaN(parsed.getTime()) ? value : parsed.toLocaleString();
  }

  function keyLabel(keyId: string, keyName: string): string {
    // Rows recorded without any API key show an explicit "no key" label —
    // the filter's "all" tag used to leak in here, which read like every
    // keyless row somehow belonged to every key.
    return keyName || keyId || $translate("usageRecords.noKey");
  }

  let totals = $derived(analytics?.totals);
  // The range card shows the active preset's label, so the card and the select
  // can never disagree about which window is selected.
  let activeRangeLabel = $derived(rangePresets.find((entry) => entry.id === rangePreset)?.labelKey ?? "usageRecords.range30d");

  onMount(() => {
    applyRangePreset("30d");
    void load();
  });
</script>

<div class="space-y-4">
  <header class="flex flex-wrap items-start justify-between gap-3">
    <div>
      <h2 class="text-lg font-semibold">{$translate("usageRecords.title")}</h2>
      <p class="text-muted-foreground text-sm">{$translate("usageRecords.description")}</p>
    </div>
    <div class="flex items-center gap-2">
      <Button variant="outline" size="sm" onclick={() => void load()} disabled={loading}>
        {#if loading}<LoaderCircle class="size-3.5 animate-spin" aria-hidden="true" />{:else}<RefreshCw class="size-3.5" aria-hidden="true" />{/if}
        {$translate("controlPlane.refresh")}
      </Button>
      <Button variant="outline" size="sm" onclick={downloadCSV} disabled={loading}>
        <Download class="size-3.5" aria-hidden="true" />
        {$translate("usageRecords.exportCSV")}
      </Button>
    </div>
  </header>

  {#if error}
    <div class="rounded-lg border border-destructive/40 bg-destructive/10 p-3 text-sm" role="alert">
      {$translate("controlPlane.error", { message: error })}
    </div>
  {/if}

  <!-- Summary cards -->
  <div class="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
    <Card.Root class="p-4">
      <div class="text-muted-foreground flex items-center gap-2 text-xs font-medium">
        <span class="bg-primary/10 text-primary rounded-md px-2 py-1">Σ</span>
        {$translate("usageRecords.totalRequests")}
      </div>
      <div class="mt-2 text-2xl font-semibold tabular-nums">{(totals?.requests ?? 0).toLocaleString()}</div>
      <div class="text-muted-foreground text-xs">{$translate(activeRangeLabel)}</div>
    </Card.Root>
    <Card.Root class="p-4">
      <div class="text-muted-foreground flex items-center gap-2 text-xs font-medium">
        <span class="bg-primary/10 text-primary rounded-md px-2 py-1">◈</span>
        {$translate("usageRecords.totalTokens")}
      </div>
      <div class="mt-2 text-2xl font-semibold tabular-nums">{formatTokenCount(totals?.totalTokens ?? 0)}</div>
      <div class="text-muted-foreground text-xs">
        {$translate("usageRecords.tokenBreakdown", {
          input: formatTokenCount(totals?.inputTokens ?? 0),
          output: formatTokenCount(totals?.outputTokens ?? 0),
          cache: formatTokenCount(totals?.cachedTokens ?? 0),
        })}
      </div>
    </Card.Root>
    <Card.Root class="p-4">
      <div class="text-muted-foreground flex items-center gap-2 text-xs font-medium">
        <span class="bg-primary/10 text-primary rounded-md px-2 py-1">$</span>
        {$translate("usageRecords.totalCost")}
      </div>
      <div class="mt-2 text-2xl font-semibold tabular-nums">${(totals?.estimatedCost ?? 0).toFixed(4)}</div>
      <div class="text-muted-foreground text-xs">
        {totals?.costEstimated ? $translate("usageRecords.standardCost", { standard: (totals?.estimatedCost ?? 0).toFixed(2) }) : ""}
      </div>
    </Card.Root>
    <Card.Root class="p-4">
      <div class="text-muted-foreground flex items-center gap-2 text-xs font-medium">
        <span class="bg-primary/10 text-primary rounded-md px-2 py-1">⏱</span>
        {$translate("usageRecords.averageDuration")}
      </div>
      <div class="mt-2 text-2xl font-semibold tabular-nums">{formatDuration(totals?.averageDurationMs ?? 0)}</div>
      <div class="text-muted-foreground text-xs">
        {(totals?.cacheHitRatio ?? 0) > 0
          ? `${(((totals?.cacheHitRatio ?? 0) as number) * 100).toFixed(1)}% ${$translate("usageRecords.trendCacheRead")}`
          : ""}
      </div>
    </Card.Root>
  </div>

  <!-- Distributions -->
  <div class="grid gap-4 xl:grid-cols-2">
    <UsageDonut
      title={$translate("usageRecords.modelDistribution")}
      rows={(analytics?.byModel ?? []).map((row) => ({
        name: row.name,
        value: row.requests,
        requests: row.requests,
        cost: row.estimatedCost,
        inputTokens: row.inputTokens,
        outputTokens: row.outputTokens,
      }))}
      valueLabel={$translate("usageRecords.requests")}
      emptyLabel={$translate("usageRecords.noData")}
    />
    <UsageDonut
      title={$translate("usageRecords.groupDistribution")}
      rows={(analytics?.byGroup ?? []).map((row) => ({
        name: row.name === "ungrouped" ? $translate("controlPlane.keyUngrouped") : row.name,
        value: row.requests,
        requests: row.requests,
        cost: row.estimatedCost,
        inputTokens: row.inputTokens,
        outputTokens: row.outputTokens,
      }))}
      valueLabel={$translate("usageRecords.requests")}
      emptyLabel={$translate("usageRecords.noData")}
    />
    <UsageDonut
      title={$translate("usageRecords.endpointDistribution")}
      rows={(analytics?.byEndpoint ?? []).map((row) => ({
        name: row.name,
        value: row.requests,
        requests: row.requests,
        cost: row.estimatedCost,
        inputTokens: row.inputTokens,
        outputTokens: row.outputTokens,
      }))}
      valueLabel={$translate("usageRecords.requests")}
      emptyLabel={$translate("usageRecords.noData")}
    />
    <UsageTrend buckets={analytics?.trend ?? []} granularity={filters.granularity} />
  </div>

  <!-- Filters -->
  <div class="flex flex-wrap items-end gap-3 rounded-xl border bg-card p-3">
    <div class="grid gap-1 text-xs">
      <span class="text-muted-foreground font-medium">{$translate("usageRecords.range")}</span>
      <Select.Root type="single" value={rangePreset} onValueChange={(value) => { if (value) { applyRangePreset(value); void load(); } }}>
        <Select.Trigger class="h-8 w-32" aria-label={$translate("usageRecords.range")}>
          {rangePreset === "today" ? $translate("usageRecords.rangeToday") : rangePreset === "7d" ? $translate("usageRecords.range7d") : rangePreset === "month" ? $translate("usageRecords.rangeMonth") : $translate("usageRecords.range30d")}
        </Select.Trigger>
        <Select.Content>
          {#each rangePresets as preset (preset.id)}
            <Select.Item value={preset.id}>{$translate(preset.labelKey)}</Select.Item>
          {/each}
        </Select.Content>
      </Select.Root>
    </div>
    <div class="grid gap-1 text-xs">
      <span class="text-muted-foreground font-medium">{$translate("usageRecords.granularity")}</span>
      <Select.Root type="single" value={filters.granularity} onValueChange={onGranularityChange}>
        <Select.Trigger class="h-8 w-28" aria-label={$translate("usageRecords.granularity")}>
          {$translate(`usageRecords.granularity${filters.granularity === "minute" ? "Minute" : filters.granularity === "hour" ? "Hour" : "Day"}`)}
        </Select.Trigger>
        <Select.Content>
          {#each granularityOptions as option (option.value)}
            <Select.Item value={option.value}>{$translate(option.labelKey)}</Select.Item>
          {/each}
        </Select.Content>
      </Select.Root>
    </div>
    <div class="grid gap-1 text-xs">
      <span class="text-muted-foreground font-medium">{$translate("usageRecords.filterKey")}</span>
      <Select.Root type="single" value={selectValue(filters.keys)} onValueChange={(value) => onSelectChange("keys", value)}>
        <Select.Trigger class="h-8 w-40" aria-label={$translate("usageRecords.filterKey")}>
          {selectValue(filters.keys) === "all"
            ? $translate("usageRecords.filterAll")
            : keyLabel(selectValue(filters.keys), options.keys.find((key) => key.id === selectValue(filters.keys))?.name ?? "")}
        </Select.Trigger>
        <Select.Content>
          <Select.Item value="all">{$translate("usageRecords.filterAll")}</Select.Item>
          {#each options.keys as key (key.id)}
            <Select.Item value={key.id}>{key.name || key.id}</Select.Item>
          {/each}
        </Select.Content>
      </Select.Root>
    </div>
    <div class="grid gap-1 text-xs">
      <span class="text-muted-foreground font-medium">{$translate("usageRecords.filterModel")}</span>
      <Select.Root type="single" value={selectValue(filters.models)} onValueChange={(value) => onSelectChange("models", value)}>
        <Select.Trigger class="h-8 w-40" aria-label={$translate("usageRecords.filterModel")}>
          {selectValue(filters.models) === "all" ? $translate("usageRecords.filterAll") : selectValue(filters.models)}
        </Select.Trigger>
        <Select.Content>
          <Select.Item value="all">{$translate("usageRecords.filterAll")}</Select.Item>
          {#each options.models as model (model)}
            <Select.Item value={model}>{model}</Select.Item>
          {/each}
        </Select.Content>
      </Select.Root>
    </div>
    <div class="grid gap-1 text-xs">
      <span class="text-muted-foreground font-medium">{$translate("usageRecords.filterGroup")}</span>
      <Select.Root type="single" value={selectValue(filters.groups)} onValueChange={(value) => onSelectChange("groups", value)}>
        <Select.Trigger class="h-8 w-36" aria-label={$translate("usageRecords.filterGroup")}>
          {selectValue(filters.groups) === "all"
            ? $translate("usageRecords.filterAll")
            : selectValue(filters.groups) === "ungrouped"
              ? $translate("controlPlane.keyUngrouped")
              : selectValue(filters.groups)}
        </Select.Trigger>
        <Select.Content>
          <Select.Item value="all">{$translate("usageRecords.filterAll")}</Select.Item>
          {#each options.groups as group (group)}
            <Select.Item value={group}>{group === "ungrouped" ? $translate("controlPlane.keyUngrouped") : group}</Select.Item>
          {/each}
        </Select.Content>
      </Select.Root>
    </div>
    <div class="grid gap-1 text-xs">
      <span class="text-muted-foreground font-medium">{$translate("usageRecords.filterEndpoint")}</span>
      <Select.Root type="single" value={selectValue(filters.endpoints)} onValueChange={(value) => onSelectChange("endpoints", value)}>
        <Select.Trigger class="h-8 w-44" aria-label={$translate("usageRecords.filterEndpoint")}>
          {selectValue(filters.endpoints) === "all" ? $translate("usageRecords.filterAll") : selectValue(filters.endpoints)}
        </Select.Trigger>
        <Select.Content>
          <Select.Item value="all">{$translate("usageRecords.filterAll")}</Select.Item>
          {#each options.endpoints as endpoint (endpoint)}
            <Select.Item value={endpoint}>{endpoint}</Select.Item>
          {/each}
        </Select.Content>
      </Select.Root>
    </div>
    <div class="ml-auto flex items-center gap-2">
      <Button variant="outline" size="sm" onclick={resetFilters} disabled={loading}>
        <RotateCcw class="size-3.5" aria-hidden="true" />
        {$translate("usageRecords.reset")}
      </Button>
    </div>
  </div>

  <!-- Detail table -->
  <Card.Root class="overflow-hidden">
    <div class="overflow-x-auto">
      <table class="w-full min-w-[1100px] text-sm">
        <thead class="bg-muted/40 text-left text-xs">
          <tr>
            <th class="px-3 py-2 font-medium">{$translate("usageRecords.colKey")}</th>
            <th class="px-3 py-2 font-medium">{$translate("usageRecords.colModel")}</th>
            <th class="px-3 py-2 font-medium">{$translate("usageRecords.colEndpoint")}</th>
            <th class="px-3 py-2 font-medium">{$translate("usageRecords.colIP")}</th>
            <th class="px-3 py-2 font-medium">{$translate("usageRecords.colGroup")}</th>
            <th class="px-3 py-2 text-right font-medium">{$translate("usageRecords.colTokens")}</th>
            <th class="px-3 py-2 text-right font-medium">{$translate("usageRecords.colCost")}</th>
            <th class="px-3 py-2 text-right font-medium">{$translate("usageRecords.colLatency")}</th>
            <th class="px-3 py-2 font-medium">{$translate("usageRecords.colTime")}</th>
          </tr>
        </thead>
        <tbody>
          {#if loading && records.length === 0}
            <tr>
              <td colspan={9} class="text-muted-foreground py-8 text-center text-sm">
                <LoaderCircle class="mx-auto size-4 animate-spin" aria-hidden="true" />
              </td>
            </tr>
          {:else if records.length === 0}
            <tr>
              <td colspan={9} class="text-muted-foreground py-8 text-center text-sm">{$translate("usageRecords.noData")}</td>
            </tr>
          {:else}
            {#each records as record (record.id)}
              <tr class="border-t">
                <td class="px-3 py-2 text-xs">{keyLabel(record.keyId, record.keyName)}</td>
                <td class="px-3 py-2 text-xs">{record.model}</td>
                <td class="max-w-[220px] truncate px-3 py-2 text-xs" title={record.endpoint}>{record.endpoint}</td>
                <td class="px-3 py-2 font-mono text-xs">{record.clientIp || "—"}</td>
                <td class="px-3 py-2 text-xs">{record.keyGroup || $translate("controlPlane.keyUngrouped")}</td>
                <td class="px-3 py-2 text-right text-xs tabular-nums">
                  <span title={$translate("usageRecords.tokenBreakdown", { input: String(record.inputTokens), output: String(record.outputTokens), cache: String(record.cachedTokens) })}>
                    ↓ {record.inputTokens.toLocaleString()} ↑ {record.outputTokens.toLocaleString()}
                  </span>
                  {#if record.cachedTokens > 0}
                    <span class="text-muted-foreground ml-1">◈ {record.cachedTokens.toLocaleString()}</span>
                  {/if}
                </td>
                <td class="px-3 py-2 text-right text-xs tabular-nums">${record.estimatedCost.toFixed(6)}</td>
                <td class="px-3 py-2 text-right text-xs tabular-nums">{formatDuration(record.durationMs)}</td>
                <td class="px-3 py-2 text-xs whitespace-nowrap">{formatTimestamp(record.timestamp)}</td>
              </tr>
            {/each}
          {/if}
        </tbody>
      </table>
    </div>
    {#if recordTotal > records.length}
      <div class="text-muted-foreground border-t px-3 py-2 text-xs">
        {$translate("usageRecords.showingRows", { count: records.length, total: recordTotal })}
      </div>
    {/if}
  </Card.Root>
</div>
