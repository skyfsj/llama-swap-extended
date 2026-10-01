<script lang="ts">
  import type { ActivityModelUsage } from "../../lib/types";
  import { formatCompactNumber } from "../../lib/format";
  import { locale, localeToIntl, translate } from "../../lib/i18n";
  import HeaderLabel from "../activity-table/HeaderLabel.svelte";
  import * as Card from "$lib/components/ui/card/index.js";
  import * as Table from "$lib/components/ui/table/index.js";

  interface Props {
    rows?: ActivityModelUsage[];
  }

  let { rows = [] }: Props = $props();
  let nf = $derived(new Intl.NumberFormat(localeToIntl($locale)));
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
</script>

<Card.Root class="overflow-hidden p-0">
  <Card.Header class="flex items-center justify-between border-b px-4 py-2">
    <Card.Title class="text-sm font-semibold">
      {$translate("activity.stats.byModel.title")}
      <span class="text-muted-foreground text-xs font-normal">({rows.length})</span>
    </Card.Title>
  </Card.Header>
  <div class="overflow-x-auto">
    <Table.Root class={hasCacheCreation ? "min-w-[60rem]" : "min-w-[52rem]"}>
      <Table.Header>
        <Table.Row>
          <Table.Head>{$translate("activity.stats.byModel.columns.model")}</Table.Head>
          <Table.Head>{$translate("activity.stats.byModel.columns.requests")}</Table.Head>
          <Table.Head>{$translate("activity.stats.byModel.columns.input")}</Table.Head>
          <Table.Head>{$translate("activity.stats.byModel.columns.output")}</Table.Head>
          <Table.Head>{$translate("activity.stats.byModel.columns.cached")}</Table.Head>
          {#if hasCacheCreation}
            <Table.Head>{$translate("activity.stats.byModel.columns.cacheCreation")}</Table.Head>
          {/if}
          <Table.Head>
            <HeaderLabel
              label={$translate("activity.stats.byModel.columns.cacheHit")}
              tooltip={$translate("activity.table.tooltips.cacheHitRatio")}
            />
          </Table.Head>
          {#if hasCacheCreation}
            <Table.Head>
              <HeaderLabel
                label={$translate("activity.stats.byModel.columns.cacheCreationRatio")}
                tooltip={$translate("activity.table.tooltips.cacheCreationRatio")}
              />
            </Table.Head>
          {/if}
          <Table.Head>{$translate("activity.stats.byModel.columns.cost")}</Table.Head>
        </Table.Row>
      </Table.Header>
      <Table.Body>
        {#if rows.length === 0}
          <Table.Row>
            <Table.Cell colspan={hasCacheCreation ? 9 : 7} class="text-muted-foreground py-6 text-center text-sm">
              {$translate("activity.stats.byModel.empty")}
            </Table.Cell>
          </Table.Row>
        {:else}
          {#each rows as row (row.model)}
            <Table.Row>
              <Table.Cell class="max-w-[20rem] truncate font-medium" title={row.model}>{row.model}</Table.Cell>
              <Table.Cell title={full(row.requests)}>{compact(row.requests)}</Table.Cell>
              <Table.Cell title={`${full(row.inputTokens)} Token`}>{compact(row.inputTokens)} Token</Table.Cell>
              <Table.Cell title={`${full(row.outputTokens)} Token`}>{compact(row.outputTokens)} Token</Table.Cell>
              <Table.Cell title={`${full(row.cachedTokens)} Token`}>{compact(row.cachedTokens)} Token</Table.Cell>
              {#if hasCacheCreation}
                <Table.Cell title={`${full(row.cacheCreationTokens)} Token`}>{compact(row.cacheCreationTokens)} Token</Table.Cell>
              {/if}
              <Table.Cell>{ratio(row.cacheHitRatio)}</Table.Cell>
              {#if hasCacheCreation}
                <Table.Cell>{ratio(row.cacheCreationRatio)}</Table.Cell>
              {/if}
              <Table.Cell>{row.costEstimated ? row.estimatedCost.toFixed(6) : "—"}</Table.Cell>
            </Table.Row>
          {/each}
        {/if}
      </Table.Body>
    </Table.Root>
  </div>
</Card.Root>
