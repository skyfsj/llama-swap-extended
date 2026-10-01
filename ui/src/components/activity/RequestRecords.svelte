<script lang="ts">
  import { untrack } from "svelte";
  import type {
    ActivityLogEntry,
    AuditConversation,
    InflightRequestEntry,
  } from "../../lib/types";
  import { activityRevision, getActivity, models } from "../../stores/api";
  import { persistentStore } from "../../stores/persistent";
  import {
    emptyActivityFilters,
    normalizeActivityFilters,
    type ActivityFilters,
  } from "../../lib/activityFilters";
  import { translate } from "../../lib/i18n";
  import { errorMessageFromPayload } from "$lib/apiError";
  import ActivityTable from "../ActivityTable.svelte";
  import AuditConversationDialog from "../audit/AuditConversationDialog.svelte";

  interface Props {
    modelId?: string;
    storagePrefix: string;
    showModelColumn?: boolean;
    inflightRequests?: InflightRequestEntry[];
    showInflight?: boolean;
    configuredOnly?: boolean;
    onFiltersChanged?: (filters: ActivityFilters) => void;
  }

  let {
    modelId = "",
    storagePrefix,
    showModelColumn = true,
    inflightRequests = [],
    showInflight = true,
    configuredOnly = false,
    onFiltersChanged,
  }: Props = $props();

  // This component contract uses a stable storage prefix for its lifetime.
  // svelte-ignore state_referenced_locally
  const storedPageSize = persistentStore<number>(`${storagePrefix}-page-size`, 25);
  // svelte-ignore state_referenced_locally
  const storedFilters = persistentStore<ActivityFilters>(
    `${storagePrefix}-filters`,
    emptyActivityFilters(),
  );

  const initialFilters = normalizeActivityFilters($storedFilters);
  // svelte-ignore state_referenced_locally
  if (modelId) {
    initialFilters.model = "";
    initialFilters.minID = "";
    initialFilters.maxID = "";
  }

  let activityRows = $state<ActivityLogEntry[]>([]);
  let loading = $state(true);
  let error = $state("");
  let page = $state(1);
  let limit = $state($storedPageSize);
  let sort = $state("id");
  let order = $state<"asc" | "desc">("desc");
  let total = $state(0);
  let totalPages = $state(0);
  // svelte-ignore state_referenced_locally
  let filters = $state<ActivityFilters>(initialFilters);
  let requestID = 0;
  let conversationRequestID = 0;
  let refreshTimer: ReturnType<typeof setTimeout> | null = null;
  let lastRefresh = 0;
  let selectedConversation = $state<AuditConversation | null>(null);
  let conversationDialogOpen = $state(false);
  let detailLoading = $state(false);
  let detailError = $state("");

  let rows = $derived(activityRows);
  let emptyMessage = $derived(
    $translate(modelId ? "modelDetail.noActivity" : "activity.empty"),
  );

  async function load(): Promise<void> {
    const id = ++requestID;
    loading = true;
    error = "";

    try {
      const activity = await getActivity({
        model: modelId.trim() || undefined,
        page,
        limit,
        sort,
        order,
        filters,
        configuredOnly,
      });
      if (id !== requestID) return;

      activityRows = activity.data;
      total = activity.total;
      totalPages = activity.total_pages;
    } catch (cause) {
      if (id !== requestID) return;
      activityRows = [];
      total = 0;
      totalPages = 0;
      error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (id === requestID) {
        loading = false;
        lastRefresh = Date.now();
      }
    }
  }

  function setPage(nextPage: number): void {
    page = nextPage;
  }

  function setPageSize(nextLimit: number): void {
    limit = nextLimit;
    page = 1;
    storedPageSize.set(nextLimit);
  }

  function setSort(nextSort: string, nextOrder: "asc" | "desc"): void {
    sort = nextSort;
    order = nextOrder;
    page = 1;
  }

  function setFilters(nextFilters: ActivityFilters): void {
    filters = {
      ...nextFilters,
      model: modelId ? "" : nextFilters.model,
      minID: modelId ? "" : nextFilters.minID,
      maxID: modelId ? "" : nextFilters.maxID,
    };
    page = 1;
    storedFilters.set(filters);
    onFiltersChanged?.(filters);
  }

  function scheduleRefresh(): void {
    if (refreshTimer !== null) return;
    const wait = Math.max(0, 1000 - (Date.now() - lastRefresh));
    refreshTimer = setTimeout(() => {
      refreshTimer = null;
      void load();
    }, wait);
  }

  async function openConversation(row: ActivityLogEntry): Promise<void> {
    if (!Number.isInteger(row.id) || row.id < 1 || !row.has_audit) return;

    // Serial guard: a slow detail response must never overwrite the record a
    // later click selected.
    const id = ++conversationRequestID;
    selectedConversation = null;
    detailError = "";
    detailLoading = true;
    conversationDialogOpen = true;
    try {
      const response = await fetch(`/api/audit/conversations/${row.id}`);
      const payload: unknown = await response.json().catch(() => ({}));
      if (id !== conversationRequestID) return;
      if (!response.ok) throw new Error(errorMessageFromPayload(payload, `HTTP ${response.status}`));
      selectedConversation = payload as AuditConversation;
    } catch (cause) {
      if (id !== conversationRequestID) return;
      detailError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (id === conversationRequestID) {
        detailLoading = false;
      }
    }
  }

  function canViewRow(row: ActivityLogEntry): boolean {
    return row.has_audit === true;
  }

  function filterBySession(row: ActivityLogEntry): void {
    const sessionID = row.session_id?.trim();
    if (!sessionID) return;
    setFilters({ ...filters, sessionID });
  }

  // Load when the scope or table controls change. The request itself is
  // untracked so loading/error state changes do not cause a fetch loop.
  $effect(() => {
    modelId;
    $models;
    page;
    limit;
    sort;
    order;
    filters;
    untrack(() => {
      void load();
    });
  });

  // Activity and audit records are written alongside one another, so the same
  // event revision keeps the unified table current without polling.
  let seenRevision = $activityRevision;
  $effect(() => {
    const revision = $activityRevision;
    untrack(() => {
      if (revision === seenRevision) return;
      seenRevision = revision;
      if (page === 1) scheduleRefresh();
    });
  });

  $effect(() => {
    return () => {
      if (refreshTimer !== null) clearTimeout(refreshTimer);
    };
  });
</script>

<section class="space-y-2" aria-busy={loading}>
  {#if error}
    <div class="rounded-lg border border-destructive/40 bg-destructive/10 p-3 text-sm" role="alert">
      {$translate("controlPlane.error", { message: error })}
    </div>
  {/if}

  <ActivityTable
    metrics={rows}
    storagePrefix={storagePrefix + "-table"}
    {inflightRequests}
    {showInflight}
    {showModelColumn}
    showModelFilter={!modelId}
    showIDFilters={!modelId}
    filterIdPrefix={storagePrefix + "-filter"}
    showRowActions={true}
    showPagination={true}
    {page}
    {limit}
    {total}
    {totalPages}
    onPageChange={setPage}
    onPageSizeChange={setPageSize}
    {sort}
    {order}
    onSortChange={setSort}
    {filters}
    onFiltersChange={setFilters}
    compact={true}
    title={$translate("controlPlane.auditConversations")}
    cardClass="min-h-[18rem] overflow-auto"
    {emptyMessage}
    onViewRow={openConversation}
    {canViewRow}
    onFilterSession={filterBySession}
  />
</section>

<AuditConversationDialog
  conversation={selectedConversation}
  open={conversationDialogOpen}
  loading={detailLoading}
  error={detailError}
  onclose={() => {
    conversationDialogOpen = false;
    selectedConversation = null;
    detailLoading = false;
    detailError = "";
  }}
/>
