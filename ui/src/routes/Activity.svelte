<script lang="ts">
  import { untrack } from "svelte";
  import type { ActivityStatsData } from "../lib/types";
  import {
    activityRevision,
    getActivityStats,
    inflightRequestEntries,
    models,
  } from "../stores/api";
  import { connectionState } from "../stores/theme";
  import { persistentStore } from "../stores/persistent";
  import {
    activeFilterCount,
    emptyActivityFilters,
    normalizeActivityFilters,
    type ActivityFilters,
  } from "../lib/activityFilters";
  import { translate } from "../lib/i18n";
  import ActivityStats from "../components/ActivityStats.svelte";
  import ModelUsageTable from "../components/activity/ModelUsageTable.svelte";
  import RequestRecords from "../components/activity/RequestRecords.svelte";
  import InferenceSpeedPanel from "../components/activity/InferenceSpeedPanel.svelte";
  import EnergyPanel from "../components/activity/EnergyPanel.svelte";
  import * as Tabs from "$lib/components/ui/tabs/index.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import {
    Boxes,
    Columns3,
    Gauge,
    Layers,
    ListFilter,
    RefreshCw,
    X,
    Zap,
  } from "@lucide/svelte";

  const storedFilters = persistentStore<ActivityFilters>(
    "activity-filters",
    emptyActivityFilters(),
  );

  const initialFilters = normalizeActivityFilters($storedFilters);
  const activeTab = persistentStore<string>("activity-active-tab", "overview");

  let stats = $state<ActivityStatsData | null>(null);
  // svelte-ignore state_referenced_locally
  let filters = $state<ActivityFilters>(initialFilters);
  let statsRequestID = 0;
  let refreshTimer: ReturnType<typeof setTimeout> | null = null;
  let lastRefresh = 0;
  let refreshing = $state(false);

  let filterCount = $derived(activeFilterCount(filters));

  async function refreshStats(): Promise<void> {
    if (refreshTimer !== null) {
      clearTimeout(refreshTimer);
      refreshTimer = null;
    }
    lastRefresh = Date.now();
    const id = ++statsRequestID;
    try {
      const result = await getActivityStats({ filters, configuredOnly: true });
      if (id === statsRequestID) stats = result;
    } catch (error) {
      console.error("Failed to refresh activity stats:", error);
    } finally {
      if (id === statsRequestID) {
        refreshing = false;
      }
    }
  }

  function handleManualRefresh(): void {
    refreshing = true;
    void refreshStats();
  }

  function setFilters(nextFilters: ActivityFilters): void {
    filters = { ...nextFilters };
    storedFilters.set(filters);
  }

  function clearAllFilters(): void {
    setFilters(emptyActivityFilters());
  }

  // Keep the summary cards current without making the table and the summary
  // maintain separate refresh loops for the same request-record dataset.
  function scheduleRefresh(): void {
    if (refreshTimer !== null) return;
    const wait = Math.max(0, 1000 - (Date.now() - lastRefresh));
    refreshTimer = setTimeout(() => {
      refreshTimer = null;
      void refreshStats();
    }, wait);
  }

  $effect(() => {
    if ($connectionState !== "connected") return;
    $models;
    filters;
    untrack(() => {
      void refreshStats();
    });
  });

  let seenRevision = $activityRevision;
  $effect(() => {
    if ($connectionState !== "connected") return;
    const revision = $activityRevision;
    untrack(() => {
      if (revision === seenRevision) return;
      seenRevision = revision;
      scheduleRefresh();
    });
  });

  $effect(() => {
    return () => {
      if (refreshTimer !== null) clearTimeout(refreshTimer);
    };
  });
</script>

<div class="space-y-4 pb-8">
  <!-- Top Status & Actions Bar -->
  <div class="flex items-center justify-between gap-2">
    <div class="flex items-center gap-2">
      {#if $connectionState === "connected"}
        <span class="inline-flex items-center gap-1.5 rounded-full border border-emerald-500/20 bg-emerald-500/10 px-2.5 py-0.5 text-xs font-medium text-emerald-600 dark:text-emerald-400">
          <span class="relative flex size-2">
            <span class="absolute inline-flex size-full animate-ping rounded-full bg-emerald-400 opacity-75"></span>
            <span class="relative inline-flex size-2 rounded-full bg-emerald-500"></span>
          </span>
          {$translate("activity.liveSync")}
        </span>
      {/if}

      <!-- Active Filters Pill -->
      {#if filterCount > 0}
        <div class="inline-flex items-center gap-1.5 rounded-lg border bg-muted/40 px-2.5 py-0.5 text-xs">
          <span class="font-medium text-foreground">
            {filterCount} {$translate("activity.table.columnsMenu")}
          </span>
          <Button
            variant="ghost"
            size="icon-xs"
            class="size-4 rounded-full p-0 text-muted-foreground hover:text-foreground"
            onclick={clearAllFilters}
            title={$translate("activity.filters.clear")}
          >
            <X class="size-3" />
          </Button>
        </div>
      {/if}
    </div>

    <!-- Manual Refresh Button -->
    <Button
      variant="outline"
      size="sm"
      class="h-7 gap-1.5 px-2.5 text-xs font-medium"
      onclick={handleManualRefresh}
      disabled={refreshing}
    >
      <RefreshCw class="size-3.5 {refreshing ? 'animate-spin' : ''}" />
      <span>{$translate("activity.speed.refresh")}</span>
    </Button>
  </div>

  <!-- Executive Stats Deck (Cards + Histograms) -->
  <ActivityStats
    {stats}
    inflightCount={$inflightRequestEntries.length}
  />

  <!-- Main Activity Workspace Tabs -->
  <Tabs.Root
    value={$activeTab}
    onValueChange={(val) => {
      if (val) $activeTab = val;
    }}
    class="w-full space-y-4"
  >
    <!-- Tab triggers navigation bar -->
    <div class="border-b pb-2">
      <Tabs.List class="h-9 w-full justify-start overflow-x-auto p-1 sm:w-auto">
        <Tabs.Trigger value="overview" class="gap-1.5 px-3 text-xs sm:text-sm">
          <Layers class="size-3.5 sm:size-4" />
          <span>{$translate("activity.tabs.overview")}</span>
        </Tabs.Trigger>

        <Tabs.Trigger value="requests" class="gap-1.5 px-3 text-xs sm:text-sm">
          <ListFilter class="size-3.5 sm:size-4" />
          <span>{$translate("activity.tabs.requests")}</span>
        </Tabs.Trigger>

        <Tabs.Trigger value="speed" class="gap-1.5 px-3 text-xs sm:text-sm">
          <Gauge class="size-3.5 sm:size-4" />
          <span>{$translate("activity.tabs.speed")}</span>
        </Tabs.Trigger>

        <Tabs.Trigger value="models" class="gap-1.5 px-3 text-xs sm:text-sm">
          <Boxes class="size-3.5 sm:size-4" />
          <span>{$translate("activity.tabs.models")}</span>
        </Tabs.Trigger>

        <Tabs.Trigger value="energy" class="gap-1.5 px-3 text-xs sm:text-sm">
          <Zap class="size-3.5 sm:size-4" />
          <span>{$translate("activity.tabs.energy")}</span>
        </Tabs.Trigger>

        <Tabs.Trigger value="all" class="gap-1.5 px-3 text-xs sm:text-sm">
          <Columns3 class="size-3.5 sm:size-4" />
          <span>{$translate("activity.tabs.all")}</span>
        </Tabs.Trigger>
      </Tabs.List>
    </div>

    <!-- Tab 1: Overview -->
    <Tabs.Content value="overview" class="space-y-5 outline-none">
      <!-- Top Models Snapshot -->
      <ModelUsageTable
        rows={stats?.by_model}
        limit={5}
        onViewAll={() => {
          $activeTab = "models";
        }}
      />

      <!-- Real-time Request Stream -->
      <RequestRecords
        storagePrefix="activity"
        showModelColumn={true}
        inflightRequests={$inflightRequestEntries}
        configuredOnly={true}
        onFiltersChanged={setFilters}
      />
    </Tabs.Content>

    <!-- Tab 2: Requests Only -->
    <Tabs.Content value="requests" class="outline-none">
      <RequestRecords
        storagePrefix="activity"
        showModelColumn={true}
        inflightRequests={$inflightRequestEntries}
        configuredOnly={true}
        onFiltersChanged={setFilters}
      />
    </Tabs.Content>

    <!-- Tab 3: Inference Speed -->
    <Tabs.Content value="speed" class="outline-none">
      <InferenceSpeedPanel {filters} inflightRequests={$inflightRequestEntries} />
    </Tabs.Content>

    <!-- Tab 4: Model Usage -->
    <Tabs.Content value="models" class="outline-none">
      <ModelUsageTable rows={stats?.by_model} />
    </Tabs.Content>

    <!-- Tab 5: Energy & Power -->
    <Tabs.Content value="energy" class="outline-none">
      <EnergyPanel />
    </Tabs.Content>

    <!-- Tab 6: All Panels Flat View -->
    <Tabs.Content value="all" class="space-y-6 outline-none">
      <InferenceSpeedPanel {filters} inflightRequests={$inflightRequestEntries} />
      <EnergyPanel />
      <ModelUsageTable rows={stats?.by_model} />
      <RequestRecords
        storagePrefix="activity"
        showModelColumn={true}
        inflightRequests={$inflightRequestEntries}
        configuredOnly={true}
        onFiltersChanged={setFilters}
      />
    </Tabs.Content>
  </Tabs.Root>
</div>
