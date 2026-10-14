<script lang="ts">
  import type { ActivityStatsData } from "../lib/types";
  import { models } from "../stores/api";
  import { persistentStore } from "../stores/persistent";
  import TokenHistogram from "./TokenHistogram.svelte";
  import {
    Activity,
    Boxes,
    ChevronDown,
    ChevronUp,
    Coins,
    Cpu,
    TrendingUp,
    Zap,
  } from "@lucide/svelte";
  import * as Card from "$lib/components/ui/card/index.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import { formatCompactNumber } from "../lib/format";
  import { locale, localeToIntl, translate } from "../lib/i18n";
  import { link } from "svelte-spa-router";

  interface Props {
    stats: ActivityStatsData | null;
    inflightCount?: number;
    onSelectTab?: (tab: string) => void;
  }

  let { stats, inflightCount = 0, onSelectTab }: Props = $props();

  let nf = $derived(new Intl.NumberFormat(localeToIntl($locale)));
  const histogramCollapsed = persistentStore<boolean>("activity-histogram-collapsed", false);
  let hasCacheCreation = $derived((stats?.total_cache_creation_tokens ?? 0) > 0);
  let totalTokens = $derived((stats?.total_input_tokens ?? 0) + (stats?.total_output_tokens ?? 0));
  let cacheHitRatio = $derived(stats?.cache_hit_ratio ?? 0);
  let readyModels = $derived($models.filter((m) => m.state === "ready").length);
  let sleepingModels = $derived($models.filter((m) => m.state === "sleeping").length);
  let totalModels = $derived($models.length);

  function formatCostVal(value?: number): string {
    if (value === undefined || !Number.isFinite(value) || value <= 0) return "—";
    if (value < 0.001) return `$${value.toFixed(6)}`;
    if (value < 1) return `$${value.toFixed(4)}`;
    return `$${value.toFixed(2)}`;
  }
</script>

<div class="space-y-3.5">
  <!-- Top 5 Key Metric Cards -->
  <div class="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-5">
    <!-- Card 1: Requests -->
    <Card.Root class="relative overflow-hidden p-4 transition-all duration-200 hover:shadow-sm">
      <div class="flex items-center justify-between gap-2">
        <span class="text-muted-foreground text-xs font-medium uppercase tracking-wider">
          {$translate("activity.stats.requests")}
        </span>
        <div class="flex size-7 shrink-0 items-center justify-center rounded-lg bg-blue-500/10 text-blue-600 dark:text-blue-400">
          <Activity class="size-4" />
        </div>
      </div>
      <div class="mt-2 flex items-baseline gap-2">
        <span
          class="text-2xl font-bold tracking-tight text-foreground"
          title={nf.format(stats?.total_requests ?? 0)}
        >
          {formatCompactNumber(stats?.total_requests ?? 0, $locale)}
        </span>
        <span class="text-muted-foreground text-xs">
          {$translate("activity.stats.completed")}
        </span>
      </div>
      <div class="mt-2.5 flex items-center gap-1.5 min-h-[1.25rem]">
        {#if inflightCount > 0}
          <span class="inline-flex items-center gap-1.5 rounded-full bg-emerald-500/10 px-2 py-0.5 text-xs font-medium text-emerald-600 dark:text-emerald-400">
            <span class="relative flex size-2">
              <span class="absolute inline-flex size-full animate-ping rounded-full bg-emerald-400 opacity-75"></span>
              <span class="relative inline-flex size-2 rounded-full bg-emerald-500"></span>
            </span>
            {$translate("activity.inFlightCount", { count: inflightCount })}
          </span>
        {:else}
          <span class="text-muted-foreground/80 text-xs">
            {nf.format(stats?.total_requests ?? 0)} {$translate("activity.stats.completed")}
          </span>
        {/if}
      </div>
    </Card.Root>

    <!-- Card 2: Total Tokens -->
    <Card.Root class="relative overflow-hidden p-4 transition-all duration-200 hover:shadow-sm">
      <div class="flex items-center justify-between gap-2">
        <span class="text-muted-foreground text-xs font-medium uppercase tracking-wider">
          {$translate("activity.totalTokens")}
        </span>
        <div class="flex size-7 shrink-0 items-center justify-center rounded-lg bg-purple-500/10 text-purple-600 dark:text-purple-400">
          <Cpu class="size-4" />
        </div>
      </div>
      <div class="mt-2 flex items-baseline gap-2">
        <span
          class="text-2xl font-bold tracking-tight text-foreground"
          title={nf.format(totalTokens)}
        >
          {formatCompactNumber(totalTokens, $locale)}
        </span>
        <span class="text-muted-foreground text-xs">Token</span>
      </div>
      <div class="mt-2.5 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-muted-foreground min-h-[1.25rem]">
        <span title={`${nf.format(stats?.total_input_tokens ?? 0)} Token`}>
          {$translate("activity.inputTokens")}: <strong class="font-medium text-foreground">{formatCompactNumber(stats?.total_input_tokens ?? 0, $locale)}</strong>
        </span>
        <span class="text-muted-foreground/40">·</span>
        <span title={`${nf.format(stats?.total_output_tokens ?? 0)} Token`}>
          {$translate("activity.outputTokens")}: <strong class="font-medium text-foreground">{formatCompactNumber(stats?.total_output_tokens ?? 0, $locale)}</strong>
        </span>
        {#if (stats?.total_reasoning_tokens ?? 0) > 0}
          <span class="text-muted-foreground/40">·</span>
          <span class="text-purple-600 dark:text-purple-400 font-medium" title={`${nf.format(stats?.total_reasoning_tokens ?? 0)} Token`}>
            {$translate("activity.reasoningTokens")}: {formatCompactNumber(stats?.total_reasoning_tokens ?? 0, $locale)}
          </span>
        {/if}
      </div>
    </Card.Root>

    <!-- Card 3: Cache Performance -->
    <Card.Root class="relative overflow-hidden p-4 transition-all duration-200 hover:shadow-sm">
      <div class="flex items-center justify-between gap-2">
        <span class="text-muted-foreground text-xs font-medium uppercase tracking-wider">
          {$translate("activity.stats.cacheHit")}
        </span>
        <div class="flex size-7 shrink-0 items-center justify-center rounded-lg bg-emerald-500/10 text-emerald-600 dark:text-emerald-400">
          <Zap class="size-4" />
        </div>
      </div>
      <div class="mt-2 flex items-baseline gap-2">
        <span class="text-2xl font-bold tracking-tight text-emerald-600 dark:text-emerald-400">
          {nf.format(cacheHitRatio * 100)}%
        </span>
      </div>
      <div class="mt-2 h-1.5 w-full overflow-hidden rounded-full bg-muted/80">
        <div
          class="h-full rounded-full bg-emerald-500 transition-all duration-500"
          style="width: {Math.min(100, Math.max(0, cacheHitRatio * 100))}%"
        ></div>
      </div>
      <div class="mt-1.5 text-xs text-muted-foreground truncate" title={`${nf.format(stats?.total_cache_tokens ?? 0)} Token`}>
        {$translate("activity.cacheSaved", { tokens: formatCompactNumber(stats?.total_cache_tokens ?? 0, $locale) })}
        {#if hasCacheCreation}
          · {$translate("activity.cacheWritten", {
            tokens: formatCompactNumber(stats?.total_cache_creation_tokens ?? 0, $locale),
            ratio: nf.format((stats?.cache_creation_ratio ?? 0) * 100)
          })}
        {/if}
      </div>
    </Card.Root>

    <!-- Card 4: Estimated Cost -->
    <Card.Root class="relative overflow-hidden p-4 transition-all duration-200 hover:shadow-sm">
      <div class="flex items-center justify-between gap-2">
        <span class="text-muted-foreground text-xs font-medium uppercase tracking-wider">
          {$translate("activity.costEstimated")}
        </span>
        <div class="flex size-7 shrink-0 items-center justify-center rounded-lg bg-amber-500/10 text-amber-600 dark:text-amber-400">
          <Coins class="size-4" />
        </div>
      </div>
      <div class="mt-2 flex items-baseline gap-2">
        <span class="text-2xl font-bold tracking-tight text-foreground">
          {formatCostVal(stats?.estimated_cost)}
        </span>
      </div>
      <div class="mt-2.5 text-xs text-muted-foreground truncate min-h-[1.25rem]">
        {#if stats?.cost_estimated}
          <span>{$translate("activity.stats.byModel.columns.cost")}</span>
        {:else}
          <span class="text-muted-foreground/70">{$translate("activity.costNotConfigured")}</span>
        {/if}
      </div>
    </Card.Root>

    <!-- Card 5: Active Models -->
    <Card.Root class="relative overflow-hidden p-4 transition-all duration-200 hover:shadow-sm">
      <div class="flex items-center justify-between gap-2">
        <span class="text-muted-foreground text-xs font-medium uppercase tracking-wider">
          {$translate("activity.activeModels")}
        </span>
        <a
          href="/models"
          use:link
          class="flex size-7 shrink-0 items-center justify-center rounded-lg bg-cyan-500/10 text-cyan-600 transition-colors hover:bg-cyan-500/20 dark:text-cyan-400"
          title={$translate("navigation.models")}
        >
          <Boxes class="size-4" />
        </a>
      </div>
      <div class="mt-2 flex items-baseline gap-2">
        <span class="text-2xl font-bold tracking-tight text-foreground">
          {readyModels}
        </span>
        <span class="text-muted-foreground text-xs">
          / {totalModels} {$translate("activity.stats.byModel.columns.model")}
        </span>
      </div>
      <div class="mt-2.5 flex items-center justify-between text-xs text-muted-foreground min-h-[1.25rem]">
        <span>
          <span class="font-medium text-emerald-600 dark:text-emerald-400">{readyModels}</span> {$translate("status.model.ready")}
          · <span class="font-medium text-muted-foreground">{sleepingModels}</span> {$translate("status.model.sleeping")}
        </span>
        {#if onSelectTab}
          <button
            type="button"
            class="text-primary hover:underline flex items-center gap-0.5 text-xs font-medium"
            onclick={() => onSelectTab("models")}
          >
            {$translate("activity.tabs.models")}
          </button>
        {/if}
      </div>
    </Card.Root>
  </div>

  <!-- TPS Histograms Collapsible Card -->
  <Card.Root class="overflow-hidden">
    <div class="flex items-center justify-between border-b px-4 py-2.5 bg-muted/20">
      <div class="flex items-center gap-2">
        <TrendingUp class="size-4 text-primary" />
        <div>
          <span class="text-sm font-semibold text-foreground">
            {$translate("activity.speedDistribution")}
          </span>
          <span class="text-muted-foreground ml-2 hidden text-xs sm:inline">
            {$translate("activity.speedDistributionSubtitle")}
          </span>
        </div>
      </div>
      <Button
        variant="ghost"
        size="sm"
        class="h-7 gap-1 px-2 text-xs text-muted-foreground hover:text-foreground"
        onclick={() => ($histogramCollapsed = !$histogramCollapsed)}
      >
        <span>{$translate($histogramCollapsed ? "activity.stats.showHistograms" : "activity.stats.hideHistograms")}</span>
        {#if $histogramCollapsed}
          <ChevronDown class="size-3.5" />
        {:else}
          <ChevronUp class="size-3.5" />
        {/if}
      </Button>
    </div>

    {#if !$histogramCollapsed}
      <div class="p-4">
        <div class="grid grid-cols-1 gap-6 md:grid-cols-2">
          <!-- Prefill Histogram -->
          <div class="rounded-lg border bg-card/60 p-3 shadow-xs">
            <div class="mb-2 flex items-center justify-between">
              <span class="text-xs font-semibold uppercase tracking-wider text-muted-foreground">
                {$translate("activity.stats.promptProcessing")}
              </span>
              <span class="rounded-md bg-amber-500/10 px-2 py-0.5 text-xs font-medium text-amber-600 dark:text-amber-400">
                {$translate("activity.stats.promptTokensPerSecond")}
              </span>
            </div>
            {#if stats?.prompt_histogram}
              <TokenHistogram
                data={stats.prompt_histogram}
                unit={$translate("activity.stats.promptTokensPerSecond")}
                colorClass="text-amber-500 dark:text-amber-400"
              />
            {:else}
              <div class="text-muted-foreground py-10 text-center text-sm">
                {$translate("activity.stats.noPromptSpeed")}
              </div>
            {/if}
          </div>

          <!-- Token Generation Histogram -->
          <div class="rounded-lg border bg-card/60 p-3 shadow-xs">
            <div class="mb-2 flex items-center justify-between">
              <span class="text-xs font-semibold uppercase tracking-wider text-muted-foreground">
                {$translate("activity.stats.tokenGeneration")}
              </span>
              <span class="rounded-md bg-blue-500/10 px-2 py-0.5 text-xs font-medium text-blue-600 dark:text-blue-400">
                {$translate("activity.stats.tokensPerSecond")}
              </span>
            </div>
            {#if stats?.gen_histogram}
              <TokenHistogram
                data={stats.gen_histogram}
                unit={$translate("activity.stats.tokensPerSecond")}
                colorClass="text-blue-500 dark:text-blue-400"
              />
            {:else}
              <div class="text-muted-foreground py-10 text-center text-sm">
                {$translate("activity.stats.noGenerationSpeed")}
              </div>
            {/if}
          </div>
        </div>
      </div>
    {/if}
  </Card.Root>
</div>
