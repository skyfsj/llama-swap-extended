<script lang="ts">
  import type { ActivityStatsData } from "../lib/types";
  import { persistentStore } from "../stores/persistent";
  import TokenHistogram from "./TokenHistogram.svelte";
  import { ChevronDown, X } from "@lucide/svelte";
  import * as Card from "$lib/components/ui/card/index.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import { formatCompactNumber } from "../lib/format";
  import { locale, localeToIntl, translate } from "../lib/i18n";

  interface Props {
    stats: ActivityStatsData | null;
  }

  let { stats }: Props = $props();

  let nf = $derived(new Intl.NumberFormat(localeToIntl($locale)));
  const histogramCollapsed = persistentStore<boolean>("activity-histogram-collapsed", false);
  let hasCacheCreation = $derived((stats?.total_cache_creation_tokens ?? 0) > 0);
</script>

<Card.Root class="relative p-3">
  <Button
    variant="ghost"
    size="icon-xs"
    class="text-muted-foreground absolute right-2 top-2 rounded-full"
    onclick={() => ($histogramCollapsed = !$histogramCollapsed)}
    title={$translate($histogramCollapsed ? "activity.stats.showHistograms" : "activity.stats.hideHistograms")}
  >
    {#if $histogramCollapsed}
      <ChevronDown />
    {:else}
      <X />
    {/if}
  </Button>
  {#if !$histogramCollapsed}
    <div class="mb-3 flex flex-col gap-6 sm:flex-row">
      <div class="w-full min-w-0 sm:w-1/2">
        <div class="text-muted-foreground mb-1 text-sm font-medium">{$translate("activity.stats.promptProcessing")}</div>
        {#if stats?.prompt_histogram}
          <TokenHistogram
            data={stats.prompt_histogram}
            unit={$translate("activity.stats.promptTokensPerSecond")}
            colorClass="text-amber-500 dark:text-amber-400"
          />
        {:else}
          <div class="text-muted-foreground py-6 text-center text-sm">{$translate("activity.stats.noPromptSpeed")}</div>
        {/if}
      </div>
      <div class="w-full min-w-0 sm:w-1/2">
        <div class="text-muted-foreground mb-1 text-sm font-medium">{$translate("activity.stats.tokenGeneration")}</div>
        {#if stats?.gen_histogram}
          <TokenHistogram data={stats.gen_histogram} unit={$translate("activity.stats.tokensPerSecond")} />
        {:else}
          <div class="text-muted-foreground py-6 text-center text-sm">{$translate("activity.stats.noGenerationSpeed")}</div>
        {/if}
      </div>
    </div>
  {/if}
  <div class={`grid grid-cols-2 gap-x-6 gap-y-1 text-sm ${hasCacheCreation ? "sm:grid-cols-7" : "sm:grid-cols-5"}`}>
    <div class="text-muted-foreground text-xs uppercase tracking-wider">{$translate("activity.stats.requests")}</div>
    <div class="text-muted-foreground text-xs uppercase tracking-wider">{$translate("activity.stats.cached")}</div>
    {#if hasCacheCreation}
      <div class="text-muted-foreground text-xs uppercase tracking-wider">{$translate("activity.stats.cacheCreation")}</div>
      <div class="text-muted-foreground text-xs uppercase tracking-wider">{$translate("activity.stats.cacheCreationRatio")}</div>
    {/if}
    <div class="text-muted-foreground text-xs uppercase tracking-wider">{$translate("activity.stats.processed")}</div>
    <div class="text-muted-foreground text-xs uppercase tracking-wider">{$translate("activity.stats.generated")}</div>
    <div class="text-muted-foreground text-xs uppercase tracking-wider">{$translate("activity.stats.cacheHit")}</div>
    <div class="text-sm">
      <span class="font-semibold" title={nf.format(stats?.total_requests ?? 0)} aria-label={nf.format(stats?.total_requests ?? 0)}>{formatCompactNumber(stats?.total_requests ?? 0, $locale)}</span> {$translate("activity.stats.completed")}
    </div>
    <div class="text-sm">
      <span class="font-semibold" title={nf.format(stats?.total_cache_tokens ?? 0)} aria-label={nf.format(stats?.total_cache_tokens ?? 0)}>{formatCompactNumber(stats?.total_cache_tokens ?? 0, $locale)}</span> {$translate("activity.stats.tokens")}
    </div>
    {#if hasCacheCreation}
      <div class="text-sm">
        <span class="font-semibold" title={nf.format(stats?.total_cache_creation_tokens ?? 0)} aria-label={nf.format(stats?.total_cache_creation_tokens ?? 0)}>{formatCompactNumber(stats?.total_cache_creation_tokens ?? 0, $locale)}</span> {$translate("activity.stats.tokens")}
      </div>
      <div class="text-sm">
        <span class="font-semibold">{nf.format((stats?.cache_creation_ratio ?? 0) * 100)}%</span>
      </div>
    {/if}
    <div class="text-sm">
      <span class="font-semibold" title={nf.format(stats?.total_input_tokens ?? 0)} aria-label={nf.format(stats?.total_input_tokens ?? 0)}>{formatCompactNumber(stats?.total_input_tokens ?? 0, $locale)}</span> {$translate("activity.stats.tokens")}
    </div>
    <div class="text-sm">
      <span class="font-semibold" title={nf.format(stats?.total_output_tokens ?? 0)} aria-label={nf.format(stats?.total_output_tokens ?? 0)}>{formatCompactNumber(stats?.total_output_tokens ?? 0, $locale)}</span> {$translate("activity.stats.tokens")}
    </div>
    <div class="text-sm">
      <span class="font-semibold">{nf.format((stats?.cache_hit_ratio ?? 0) * 100)}%</span>
    </div>
  </div>
</Card.Root>
