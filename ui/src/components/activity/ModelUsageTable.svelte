<script lang="ts">
  import type { ActivityModelUsage } from "../../lib/types";
  import { formatCompactNumber } from "../../lib/format";
  import { locale, localeToIntl, translate } from "../../lib/i18n";
  import HeaderLabel from "../activity-table/HeaderLabel.svelte";
  import * as Card from "$lib/components/ui/card/index.js";
  import * as Table from "$lib/components/ui/table/index.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Input } from "$lib/components/ui/input/index.js";
  import { link } from "svelte-spa-router";
  import { ArrowRight, ArrowUpDown, Boxes, Search } from "@lucide/svelte";

  interface Props {
    rows?: ActivityModelUsage[];
    limit?: number;
    onViewAll?: () => void;
  }

  let { rows = [], limit, onViewAll }: Props = $props();

  let nf = $derived(new Intl.NumberFormat(localeToIntl($locale)));
  let searchQuery = $state("");
  let sortBy = $state<"requests" | "tokens" | "cached" | "cacheHit" | "cost">("requests");
  let sortAsc = $state(false);

  let hasCacheCreation = $derived(rows.some((row) => row.cacheCreationTokens > 0));

  function compact(value: number): string {
    return formatCompactNumber(value, $locale);
  }

  function full(value: number): string {
    return nf.format(value);
  }

  function ratio(value?: number): string {
    return `${nf.format((value ?? 0) * 100)}%`;
  }

  function formatCost(value: number, estimated: boolean): string {
    if (!estimated || value <= 0) return "—";
    if (value < 0.001) return `$${value.toFixed(6)}`;
    if (value < 1) return `$${value.toFixed(4)}`;
    return `$${value.toFixed(2)}`;
  }

  let filteredRows = $derived.by(() => {
    let result = [...rows];
    if (searchQuery.trim()) {
      const q = searchQuery.trim().toLowerCase();
      result = result.filter((r) => r.model.toLowerCase().includes(q));
    }

    result.sort((a, b) => {
      let diff = 0;
      switch (sortBy) {
        case "requests":
          diff = a.requests - b.requests;
          break;
        case "tokens":
          diff = (a.inputTokens + a.outputTokens) - (b.inputTokens + b.outputTokens);
          break;
        case "cached":
          diff = a.cachedTokens - b.cachedTokens;
          break;
        case "cacheHit":
          diff = (a.cacheHitRatio ?? 0) - (b.cacheHitRatio ?? 0);
          break;
        case "cost":
          diff = a.estimatedCost - b.estimatedCost;
          break;
      }
      return sortAsc ? diff : -diff;
    });

    return limit ? result.slice(0, limit) : result;
  });

  // Calculate totals
  let totals = $derived.by(() => {
    let totalRequests = 0;
    let totalInput = 0;
    let totalOutput = 0;
    let totalCached = 0;
    let totalCacheCreation = 0;
    let totalCost = 0;
    let anyCostEstimated = false;

    for (const r of rows) {
      totalRequests += r.requests;
      totalInput += r.inputTokens;
      totalOutput += r.outputTokens;
      totalCached += r.cachedTokens;
      totalCacheCreation += r.cacheCreationTokens;
      totalCost += r.estimatedCost;
      if (r.costEstimated) anyCostEstimated = true;
    }

    const totalInputWithCache = totalInput + totalCached;
    const overallCacheHit = totalInputWithCache > 0 ? totalCached / totalInputWithCache : 0;
    const overallCacheCreation = totalInputWithCache > 0 ? totalCacheCreation / totalInputWithCache : 0;

    return {
      totalRequests,
      totalInput,
      totalOutput,
      totalCached,
      totalCacheCreation,
      totalCost,
      anyCostEstimated,
      overallCacheHit,
      overallCacheCreation,
    };
  });

  function toggleSort(field: "requests" | "tokens" | "cached" | "cacheHit" | "cost"): void {
    if (sortBy === field) {
      sortAsc = !sortAsc;
    } else {
      sortBy = field;
      sortAsc = false;
    }
  }
</script>

<Card.Root class="overflow-hidden p-0 shadow-xs">
  <Card.Header class="flex flex-col gap-2 border-b bg-muted/15 px-4 py-3 sm:flex-row sm:items-center sm:justify-between">
    <div class="flex items-center gap-2">
      <Boxes class="size-4 text-primary" />
      <Card.Title class="text-sm font-semibold">
        {$translate("activity.stats.byModel.title")}
        <span class="text-muted-foreground ml-1.5 text-xs font-normal">
          ({rows.length})
        </span>
      </Card.Title>
    </div>

    <div class="flex items-center gap-2">
      {#if !limit && rows.length > 5}
        <div class="relative w-48 sm:w-56">
          <Search class="text-muted-foreground absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 pointer-events-none" />
          <Input
            type="search"
            placeholder={$translate("activity.filters.modelPlaceholder")}
            bind:value={searchQuery}
            class="h-8 pl-8 text-xs"
          />
        </div>
      {/if}

      {#if limit && rows.length > limit && onViewAll}
        <Button
          variant="outline"
          size="sm"
          class="h-8 gap-1.5 text-xs"
          onclick={onViewAll}
        >
          <span>{$translate("activity.viewAllModels")}</span>
          <ArrowRight class="size-3.5" />
        </Button>
      {/if}
    </div>
  </Card.Header>

  <div class="overflow-x-auto">
    <Table.Root class={hasCacheCreation ? "min-w-[64rem]" : "min-w-[54rem]"}>
      <Table.Header>
        <Table.Row class="bg-muted/30">
          <Table.Head>{$translate("activity.stats.byModel.columns.model")}</Table.Head>
          <Table.Head class="cursor-pointer select-none" onclick={() => toggleSort("requests")}>
            <div class="flex items-center gap-1">
              <span>{$translate("activity.stats.byModel.columns.requests")}</span>
              <ArrowUpDown class="size-3 opacity-50" />
            </div>
          </Table.Head>
          <Table.Head class="cursor-pointer select-none" onclick={() => toggleSort("tokens")}>
            <div class="flex items-center gap-1">
              <span>{$translate("activity.stats.byModel.columns.input")}</span>
              <ArrowUpDown class="size-3 opacity-50" />
            </div>
          </Table.Head>
          <Table.Head>{$translate("activity.stats.byModel.columns.output")}</Table.Head>
          <Table.Head class="cursor-pointer select-none" onclick={() => toggleSort("cached")}>
            <div class="flex items-center gap-1">
              <span>{$translate("activity.stats.byModel.columns.cached")}</span>
              <ArrowUpDown class="size-3 opacity-50" />
            </div>
          </Table.Head>
          {#if hasCacheCreation}
            <Table.Head>{$translate("activity.stats.byModel.columns.cacheCreation")}</Table.Head>
          {/if}
          <Table.Head class="cursor-pointer select-none" onclick={() => toggleSort("cacheHit")}>
            <div class="flex items-center gap-1">
              <HeaderLabel
                label={$translate("activity.stats.byModel.columns.cacheHit")}
                tooltip={$translate("activity.table.tooltips.cacheHitRatio")}
              />
              <ArrowUpDown class="size-3 opacity-50" />
            </div>
          </Table.Head>
          {#if hasCacheCreation}
            <Table.Head>
              <HeaderLabel
                label={$translate("activity.stats.byModel.columns.cacheCreationRatio")}
                tooltip={$translate("activity.table.tooltips.cacheCreationRatio")}
              />
            </Table.Head>
          {/if}
          <Table.Head class="cursor-pointer select-none" onclick={() => toggleSort("cost")}>
            <div class="flex items-center gap-1">
              <span>{$translate("activity.stats.byModel.columns.cost")}</span>
              <ArrowUpDown class="size-3 opacity-50" />
            </div>
          </Table.Head>
        </Table.Row>
      </Table.Header>

      <Table.Body>
        {#if filteredRows.length === 0}
          <Table.Row>
            <Table.Cell colspan={hasCacheCreation ? 9 : 7} class="py-12 text-center">
              <div class="flex flex-col items-center justify-center gap-2 text-muted-foreground">
                <Boxes class="size-8 opacity-40" />
                <span class="text-sm">{$translate("activity.stats.byModel.empty")}</span>
              </div>
            </Table.Cell>
          </Table.Row>
        {:else}
          {#each filteredRows as row (row.model)}
            <Table.Row class="transition-colors hover:bg-muted/40">
              <!-- Model Name with link -->
              <Table.Cell class="max-w-[18rem] truncate font-medium">
                <a
                  href={`/models/${encodeURIComponent(row.model)}`}
                  use:link
                  class="text-primary hover:underline"
                  title={row.model}
                >
                  {row.model}
                </a>
              </Table.Cell>

              <!-- Requests -->
              <Table.Cell title={full(row.requests)}>
                <span class="font-medium text-foreground">{compact(row.requests)}</span>
              </Table.Cell>

              <!-- Input Tokens -->
              <Table.Cell title={`${full(row.inputTokens)} Token`}>
                <span class="text-muted-foreground">{compact(row.inputTokens)}</span>
              </Table.Cell>

              <!-- Output Tokens -->
              <Table.Cell title={`${full(row.outputTokens)} Token`}>
                <span class="text-muted-foreground">{compact(row.outputTokens)}</span>
              </Table.Cell>

              <!-- Cached Tokens -->
              <Table.Cell title={`${full(row.cachedTokens)} Token`}>
                <span class="text-muted-foreground">{compact(row.cachedTokens)}</span>
              </Table.Cell>

              <!-- Cache Creation Tokens -->
              {#if hasCacheCreation}
                <Table.Cell title={`${full(row.cacheCreationTokens)} Token`}>
                  <span class="text-muted-foreground">{compact(row.cacheCreationTokens)}</span>
                </Table.Cell>
              {/if}

              <!-- Cache Hit Ratio -->
              <Table.Cell>
                {#if (row.cacheHitRatio ?? 0) >= 0.5}
                  <span class="inline-flex rounded-md bg-emerald-500/10 px-2 py-0.5 text-xs font-medium text-emerald-600 dark:text-emerald-400">
                    {ratio(row.cacheHitRatio)}
                  </span>
                {:else if (row.cacheHitRatio ?? 0) >= 0.2}
                  <span class="inline-flex rounded-md bg-blue-500/10 px-2 py-0.5 text-xs font-medium text-blue-600 dark:text-blue-400">
                    {ratio(row.cacheHitRatio)}
                  </span>
                {:else}
                  <span class="text-muted-foreground">{ratio(row.cacheHitRatio)}</span>
                {/if}
              </Table.Cell>

              <!-- Cache Creation Ratio -->
              {#if hasCacheCreation}
                <Table.Cell>
                  <span class="text-muted-foreground">{ratio(row.cacheCreationRatio)}</span>
                </Table.Cell>
              {/if}

              <!-- Cost -->
              <Table.Cell>
                <span class="font-mono text-xs text-muted-foreground">
                  {formatCost(row.estimatedCost, row.costEstimated)}
                </span>
              </Table.Cell>
            </Table.Row>
          {/each}
        {/if}
      </Table.Body>

      <!-- Table Footer with Totals -->
      {#if rows.length > 0}
        <Table.Footer class="bg-muted/50 font-medium">
          <Table.Row>
            <Table.Cell class="font-bold">{$translate("activity.totalSummary")}</Table.Cell>
            <Table.Cell class="font-bold text-foreground" title={full(totals.totalRequests)}>
              {compact(totals.totalRequests)}
            </Table.Cell>
            <Table.Cell class="font-bold" title={`${full(totals.totalInput)} Token`}>
              {compact(totals.totalInput)}
            </Table.Cell>
            <Table.Cell class="font-bold" title={`${full(totals.totalOutput)} Token`}>
              {compact(totals.totalOutput)}
            </Table.Cell>
            <Table.Cell class="font-bold" title={`${full(totals.totalCached)} Token`}>
              {compact(totals.totalCached)}
            </Table.Cell>
            {#if hasCacheCreation}
              <Table.Cell class="font-bold" title={`${full(totals.totalCacheCreation)} Token`}>
                {compact(totals.totalCacheCreation)}
              </Table.Cell>
            {/if}
            <Table.Cell class="font-bold text-emerald-600 dark:text-emerald-400">
              {ratio(totals.overallCacheHit)}
            </Table.Cell>
            {#if hasCacheCreation}
              <Table.Cell class="font-bold">
                {ratio(totals.overallCacheCreation)}
              </Table.Cell>
            {/if}
            <Table.Cell class="font-mono text-xs font-bold">
              {formatCost(totals.totalCost, totals.anyCostEstimated)}
            </Table.Cell>
          </Table.Row>
        </Table.Footer>
      {/if}
    </Table.Root>
  </div>
</Card.Root>
