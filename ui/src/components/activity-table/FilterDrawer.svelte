<script lang="ts">
  import {
    activeFilterCount,
    emptyActivityFilters,
    type ActivityTimeRange,
    type ActivityFilters,
  } from "../../lib/activityFilters";
  import { Input } from "$lib/components/ui/input/index.js";
  import { Label } from "$lib/components/ui/label/index.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import * as Select from "$lib/components/ui/select/index.js";
  import { translate } from "../../lib/i18n";

  const PAGE_SIZES = [10, 25, 50, 100, 250, 500];

  interface Props {
    filters: ActivityFilters;
    onchange: (filters: ActivityFilters) => void;
    showModelFilter?: boolean;
    showIDFilters?: boolean;
    idPrefix?: string;
    showRows?: boolean;
    limit?: number;
    onLimitChange?: (limit: number) => void;
  }

  let {
    filters,
    onchange,
    showModelFilter = true,
    showIDFilters = true,
    idPrefix = "filter",
    showRows = false,
    limit = 25,
    onLimitChange,
  }: Props = $props();

  // Committed on change rather than input so a partially typed number does not
  // trigger a fetch per keystroke.
  function update(patch: Partial<ActivityFilters>) {
    onchange({ ...filters, ...patch });
  }

  function updateRange(value: string) {
    const range = value as ActivityTimeRange;
    if (!["all", "hour", "day", "week", "month", "custom"].includes(range)) return;
    if (range === "all") {
      update({ range, start: "", end: "" });
    } else if (range === "custom") {
      update({ range });
    } else {
      // Presets are relative to the current time when the request is built.
      // Clear custom bounds so an old date does not silently override the
      // selected preset.
      update({ range, start: "", end: "" });
    }
  }

  let canClear = $derived(activeFilterCount(filters) > 0);
</script>

<div class="bg-muted/30 border-b px-4 py-3">
  <div class="flex flex-wrap items-end gap-3">
    {#if showModelFilter}
      <div class="flex flex-col gap-1">
        <Label for={`${idPrefix}-model`} class="text-muted-foreground text-xs">{$translate("activity.filters.model")}</Label>
        <Input
          id={`${idPrefix}-model`}
          type="text"
          placeholder={$translate("activity.filters.modelPlaceholder")}
          class="h-8 w-[9rem] text-xs"
          value={filters.model}
          onchange={(event) => update({ model: event.currentTarget.value })}
        />
      </div>
    {/if}

    <div class="flex flex-col gap-1">
      <Label for={`${idPrefix}-key-id`} class="text-muted-foreground text-xs">{$translate("activity.filters.key")}</Label>
      <Input
        id={`${idPrefix}-key-id`}
        type="text"
        placeholder={$translate("activity.filters.keyPlaceholder")}
        class="h-8 w-[9rem] text-xs"
        value={filters.keyID}
        onchange={(event) => update({ keyID: event.currentTarget.value })}
      />
    </div>

    <div class="flex flex-col gap-1">
      <Label for={`${idPrefix}-session-id`} class="text-muted-foreground text-xs">{$translate("activity.filters.session")}</Label>
      <Input
        id={`${idPrefix}-session-id`}
        type="text"
        placeholder={$translate("activity.filters.sessionPlaceholder")}
        class="h-8 w-[9rem] text-xs"
        value={filters.sessionID}
        onchange={(event) => update({ sessionID: event.currentTarget.value })}
      />
    </div>

    <div class="flex flex-col gap-1">
      <span class="text-muted-foreground text-xs">{$translate("activity.filters.range")}</span>
      <Select.Root
        type="single"
        value={filters.range}
        onValueChange={updateRange}
      >
        <Select.Trigger size="sm" class="h-8 w-[10rem] text-xs">
          {#if filters.range === "hour"}
            {$translate("activity.filters.rangeHour")}
          {:else if filters.range === "day"}
            {$translate("activity.filters.rangeDay")}
          {:else if filters.range === "week"}
            {$translate("activity.filters.rangeWeek")}
          {:else if filters.range === "month"}
            {$translate("activity.filters.rangeMonth")}
          {:else if filters.range === "custom"}
            {$translate("activity.filters.rangeCustom")}
          {:else}
            {$translate("activity.filters.rangeAll")}
          {/if}
        </Select.Trigger>
        <Select.Content>
          <Select.Item value="all">{$translate("activity.filters.rangeAll")}</Select.Item>
          <Select.Item value="hour">{$translate("activity.filters.rangeHour")}</Select.Item>
          <Select.Item value="day">{$translate("activity.filters.rangeDay")}</Select.Item>
          <Select.Item value="week">{$translate("activity.filters.rangeWeek")}</Select.Item>
          <Select.Item value="month">{$translate("activity.filters.rangeMonth")}</Select.Item>
          <Select.Item value="custom">{$translate("activity.filters.rangeCustom")}</Select.Item>
        </Select.Content>
      </Select.Root>
    </div>

    <div class="flex flex-col gap-1">
      <Label for={`${idPrefix}-start`} class="text-muted-foreground text-xs">{$translate("activity.filters.start")}</Label>
      <Input
        id={`${idPrefix}-start`}
        type="datetime-local"
        class="h-8 w-[11rem] text-xs"
        value={filters.start}
        onchange={(event) => update({ range: "custom", start: event.currentTarget.value })}
      />
    </div>

    <div class="flex flex-col gap-1">
      <Label for={`${idPrefix}-end`} class="text-muted-foreground text-xs">{$translate("activity.filters.end")}</Label>
      <Input
        id={`${idPrefix}-end`}
        type="datetime-local"
        class="h-8 w-[11rem] text-xs"
        value={filters.end}
        onchange={(event) => update({ range: "custom", end: event.currentTarget.value })}
      />
    </div>

    {#if showIDFilters}
      <div class="flex flex-col gap-1">
        <Label for={`${idPrefix}-min-id`} class="text-muted-foreground text-xs">{$translate("activity.filters.idFrom")}</Label>
        <Input
          id={`${idPrefix}-min-id`}
          type="number"
          min="1"
          step="1"
          placeholder={$translate("activity.filters.min")}
          class="h-8 w-[6.5rem] text-xs"
          value={filters.minID}
          onchange={(event) => update({ minID: event.currentTarget.value })}
        />
      </div>

      <div class="flex flex-col gap-1">
        <Label for={`${idPrefix}-max-id`} class="text-muted-foreground text-xs">{$translate("activity.filters.idTo")}</Label>
        <Input
          id={`${idPrefix}-max-id`}
          type="number"
          min="1"
          step="1"
          placeholder={$translate("activity.filters.max")}
          class="h-8 w-[6.5rem] text-xs"
          value={filters.maxID}
          onchange={(event) => update({ maxID: event.currentTarget.value })}
        />
      </div>
    {/if}

    <Button
      variant="ghost"
      size="sm"
      class="h-8 text-xs"
      disabled={!canClear}
      onclick={() => onchange(emptyActivityFilters())}
    >
      {$translate("activity.filters.clear")}
    </Button>

    {#if showRows && onLimitChange}
      <!-- Rows is a view setting rather than a filter: it sits apart on the
           right, is left alone by "Clear filters", and is not counted by the
           header's active-filter badge. -->
      <div class="ml-auto flex flex-col gap-1">
        <span class="text-muted-foreground text-xs">{$translate("activity.filters.rows")}</span>
        <Select.Root
          type="single"
          value={String(limit)}
          onValueChange={(value) => onLimitChange(Number(value))}
        >
          <Select.Trigger size="sm" class="h-8 w-[5.5rem] text-xs">
            {limit}
          </Select.Trigger>
          <Select.Content>
            {#each PAGE_SIZES as size (size)}
              <Select.Item value={String(size)}>{size}</Select.Item>
            {/each}
          </Select.Content>
        </Select.Root>
      </div>
    {/if}
  </div>
</div>
