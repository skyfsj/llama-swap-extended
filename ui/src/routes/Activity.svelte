<script lang="ts">
  import { untrack } from "svelte";
  import type { ActivityStatsData } from "../lib/types";
  import { activityRevision, getActivityStats, inflightRequestEntries, models } from "../stores/api";
  import { connectionState } from "../stores/theme";
  import { persistentStore } from "../stores/persistent";
  import {
    emptyActivityFilters,
    normalizeActivityFilters,
    type ActivityFilters,
  } from "../lib/activityFilters";
  import ActivityStats from "../components/ActivityStats.svelte";
  import ModelUsageTable from "../components/activity/ModelUsageTable.svelte";
  import RequestRecords from "../components/activity/RequestRecords.svelte";
  import InferenceSpeedPanel from "../components/activity/InferenceSpeedPanel.svelte";

  const storedFilters = persistentStore<ActivityFilters>(
    "activity-filters",
    emptyActivityFilters(),
  );

  const initialFilters = normalizeActivityFilters($storedFilters);

  let stats = $state<ActivityStatsData | null>(null);
  // svelte-ignore state_referenced_locally
  let filters = $state<ActivityFilters>(initialFilters);
  let statsRequestID = 0;
  let refreshTimer: ReturnType<typeof setTimeout> | null = null;
  let lastRefresh = 0;

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
    }
  }

  function setFilters(nextFilters: ActivityFilters): void {
    filters = { ...nextFilters };
    storedFilters.set(filters);
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

<div class="p-2">
  <div class="mt-4 mb-4">
    <ActivityStats {stats} />
  </div>

  <div class="mb-4">
    <InferenceSpeedPanel filters={filters} inflightRequests={$inflightRequestEntries} />
  </div>

  <div class="mb-4">
    <ModelUsageTable rows={stats?.by_model} />
  </div>

  <RequestRecords
    storagePrefix="activity"
    showModelColumn={true}
    inflightRequests={$inflightRequestEntries}
    configuredOnly={true}
    onFiltersChanged={setFilters}
  />
</div>
