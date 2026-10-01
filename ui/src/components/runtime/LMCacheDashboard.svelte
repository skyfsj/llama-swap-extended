<script lang="ts">
  import { Activity, CircleAlert, Database, Gauge, HeartPulse, LoaderCircle } from "@lucide/svelte";
  import { Badge } from "$lib/components/ui/badge/index.js";
  import { formatAbsoluteTime, formatCapacity } from "$lib/format";
  import { locale, translate } from "$lib/i18n";
  import { summarizeLMCacheMetricsText } from "$lib/lmcacheMetrics";
  import type { LMCacheDashboardResponse } from "$lib/types";

  interface Props {
    dashboard: LMCacheDashboardResponse | null;
    loading?: boolean;
    error?: string;
  }

  let { dashboard, loading = false, error = "" }: Props = $props();
  let metricSummary = $derived(summarizeLMCacheMetricsText(dashboard?.metrics?.body ?? ""));

  function noData(): string {
    return $translate("controlPlane.lmcacheDashboard.noDataValue");
  }

  function formatNumber(value: number | undefined): string {
    return value === undefined || !Number.isFinite(value) ? noData() : new Intl.NumberFormat($locale === "en" ? "en-US" : $locale).format(value);
  }

  function formatBytes(value: number | undefined): string {
    return value === undefined || !Number.isFinite(value) ? noData() : formatCapacity(value, noData());
  }

  function formatPercent(value: number | undefined): string {
    return value === undefined || !Number.isFinite(value) ? noData() : `${(value * 100).toFixed(1)}%`;
  }

  function checkedAt(value?: string): string {
    return value ? formatAbsoluteTime(value, $locale) : $translate("controlPlane.lmcacheDashboard.noData");
  }

  function adapterNames(value: unknown): string[] {
    if (Array.isArray(value)) {
      return value.map((item) => {
        if (typeof item === "string") return item;
        if (item && typeof item === "object") {
          const record = item as Record<string, unknown>;
          return String(record.type_name ?? record.name ?? record.type ?? record.adapter ?? "");
        }
        return "";
      }).filter(Boolean);
    }
    if (value && typeof value === "object") {
      const record = value as Record<string, unknown>;
      if ("adapters" in record) return adapterNames(record.adapters);
      return Object.keys(record);
    }
    return [];
  }

  function errorEntries(): Array<[string, string]> {
    return Object.entries(dashboard?.errors ?? {}).filter(([, value]) => value).slice(0, 3);
  }
</script>

{#if loading && !dashboard}
  <div class="flex items-center gap-2 rounded-lg border border-dashed p-4 text-xs text-muted-foreground" role="status" aria-live="polite">
    <LoaderCircle class="size-4 animate-spin" aria-hidden="true" />
    {$translate("controlPlane.lmcacheDashboard.loading")}
  </div>
{:else if error}
  <div class="flex items-start gap-2 rounded-lg border border-destructive/35 bg-destructive/10 p-3 text-xs text-destructive" role="alert">
    <CircleAlert class="mt-0.5 size-4 shrink-0" aria-hidden="true" />
    <span>{error}</span>
  </div>
{:else if !dashboard || !dashboard.available}
  <div class="rounded-lg border border-dashed p-4 text-xs text-muted-foreground" role="status" aria-live="polite">
    {$translate(dashboard?.reason === "unhealthy" ? "controlPlane.lmcacheDashboard.unhealthy" : "controlPlane.lmcacheDashboard.noData")}
  </div>
{:else}
  <section class="grid gap-3" aria-labelledby="lmcache-dashboard-title">
    <div class="flex flex-wrap items-center justify-between gap-2">
      <h3 id="lmcache-dashboard-title" class="flex items-center gap-2 text-xs font-semibold uppercase tracking-[0.12em] text-muted-foreground">
        <Activity class="size-3.5" aria-hidden="true" />
        {$translate("controlPlane.lmcacheDashboard.title")}
      </h3>
      <span class="text-[11px] text-muted-foreground">{$translate("controlPlane.lmcacheDashboard.checkedAt", { time: checkedAt(dashboard.checkedAt) })}</span>
    </div>

    <div class="flex flex-wrap items-center gap-2 text-xs">
      <Badge variant="default"><HeartPulse data-icon="inline-start" />{$translate("controlPlane.lmcacheDashboard.healthy")}</Badge>
      {#if dashboard.periodicHealth.healthy === false}<Badge variant="destructive"><CircleAlert data-icon="inline-start" />{$translate("controlPlane.lmcacheDashboard.periodicUnhealthy")}</Badge>{/if}
      {#if dashboard.status?.engineType}<Badge variant="outline">{dashboard.status.engineType}</Badge>{/if}
      {#each adapterNames(dashboard.adapters) as adapter (adapter)}<Badge variant="outline">{adapter}</Badge>{/each}
    </div>

    <div class="grid gap-2 sm:grid-cols-2 lg:grid-cols-4">
      <div class="rounded-lg border p-3">
        <div class="flex items-center gap-2 text-xs text-muted-foreground"><Gauge class="size-3.5" aria-hidden="true" />{$translate("controlPlane.lmcacheDashboard.lookupHitRatio")}</div>
        <div class="mt-2 text-lg font-semibold tabular-nums">{formatPercent(metricSummary.lookupHitRatio)}</div>
        <div class="mt-1 text-[11px] text-muted-foreground">{formatNumber(metricSummary.lookupHit)} / {formatNumber(metricSummary.lookupRequested)}</div>
      </div>
      <div class="rounded-lg border p-3">
        <div class="flex items-center gap-2 text-xs text-muted-foreground"><Database class="size-3.5" aria-hidden="true" />{$translate("controlPlane.lmcacheDashboard.cacheUsage")}</div>
        <div class="mt-2 text-lg font-semibold tabular-nums">{formatPercent(metricSummary.l1UsageRatio)}</div>
        <div class="mt-1 text-[11px] text-muted-foreground">{formatBytes(metricSummary.l1MemoryUsageBytes ?? dashboard.status?.l1MemoryUsedBytes)}</div>
      </div>
      <div class="rounded-lg border p-3">
        <div class="text-xs text-muted-foreground">{$translate("controlPlane.lmcacheDashboard.activeSessions")}</div>
        <div class="mt-2 text-lg font-semibold tabular-nums">{formatNumber(dashboard.status?.activeSessions)}</div>
        <div class="mt-1 text-[11px] text-muted-foreground">{$translate("controlPlane.lmcacheDashboard.prefetchJobs")}: {formatNumber(metricSummary.activePrefetchJobs ?? dashboard.status?.activePrefetchJobs)}</div>
      </div>
      <div class="rounded-lg border p-3">
        <div class="text-xs text-muted-foreground">{$translate("controlPlane.lmcacheDashboard.failures")}</div>
        <div class="mt-2 text-lg font-semibold tabular-nums">{formatNumber(metricSummary.failures)}</div>
        <div class="mt-1 text-[11px] text-muted-foreground">{$translate("controlPlane.lmcacheDashboard.metricsAt", { time: checkedAt(dashboard.metrics.fetchedAt) })}</div>
      </div>
    </div>

    {#if metricSummary.l2UsageBytes.length > 0}
      <div class="rounded-lg border p-3">
        <div class="mb-2 text-xs font-medium">{$translate("controlPlane.lmcacheDashboard.backendUsage")}</div>
        <div class="grid gap-2 sm:grid-cols-2">
          {#each metricSummary.l2UsageBytes as backend (backend.name)}
            <div class="flex items-center justify-between gap-3 text-xs"><span class="truncate text-muted-foreground">{backend.name}</span><span class="shrink-0 font-medium tabular-nums">{formatBytes(backend.value)}</span></div>
          {/each}
        </div>
      </div>
    {:else if !dashboard.metrics.body?.trim()}
      <p class="text-xs text-muted-foreground" role="status">{$translate("controlPlane.lmcacheDashboard.metricsUnavailable")}</p>
    {/if}

    {#if errorEntries().length > 0}
      <div class="grid gap-1 rounded-lg border border-amber-500/30 bg-amber-500/5 p-3 text-xs" role="status">
        {#each errorEntries() as [name, detail] (name)}<div><span class="font-medium">{name}</span>: <span class="text-muted-foreground">{detail}</span></div>{/each}
      </div>
    {/if}
  </section>
{/if}
