<script lang="ts">
  import type { ActivityLogEntry, InflightRequestEntry } from "../lib/types";
  import { cancelInflightRequest, uiConfig } from "../stores/api";
  import { persistentStore } from "../stores/persistent";
  import { flip } from "svelte/animate";
  import {
    type ColumnDef,
    type SortingState,
    type VisibilityState,
    getCoreRowModel,
    getSortedRowModel,
  } from "@tanstack/table-core";
  import {
    FlexRender,
    createSvelteTable,
    renderComponent,
  } from "$lib/components/ui/data-table/index.js";
  import * as Table from "$lib/components/ui/table/index.js";
  import * as Card from "$lib/components/ui/card/index.js";
  import * as DropdownMenu from "$lib/components/ui/dropdown-menu/index.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Badge } from "$lib/components/ui/badge/index.js";
  import {
    Columns3,
    ChevronDown,
    ChevronLeft,
    ChevronRight,
    ChevronsLeft,
    ChevronsRight,
    ArrowUp,
    ArrowDown,
    ArrowUpDown,
    CircleX,
    Download,
    GripVertical,
    ListFilter,
    X,
  } from "@lucide/svelte";
  import FilterDrawer from "./activity-table/FilterDrawer.svelte";
  import { activeFilterCount, type ActivityFilters } from "../lib/activityFilters";
  import HeaderLabel from "./activity-table/HeaderLabel.svelte";
  import ActivityRowActions from "./activity-table/ActivityRowActions.svelte";
  import MetaCell from "./activity-table/MetaCell.svelte";
  import ModelLink from "./activity-table/ModelLink.svelte";
  import MiddleEllipsis from "./activity-table/MiddleEllipsis.svelte";
  import ExportDialog from "./activity-table/ExportDialog.svelte";
  import TimeCell from "./activity-table/TimeCell.svelte";
  import {
    buildActivityMarkdown,
    formatCacheCreationTokens,
    formatCacheHitRatio,
    formatDrafted,
    formatPrefixStability,
  } from "../lib/activityExport";
  import { formatDuration, formatSpeed } from "../lib/format";
  import { formatBytes, liveElapsedMs, requestHeader, sessionID } from "../lib/inflight";
  import { locale, t, translate } from "../lib/i18n";

  interface Props {
    metrics: ActivityLogEntry[];
    inflightRequests?: InflightRequestEntry[];
    storagePrefix: string;
    showModelColumn?: boolean;
    showInflight?: boolean;
    showRowActions?: boolean;
    showPagination?: boolean;
    page?: number;
    limit?: number;
    total?: number;
    totalPages?: number;
    onPageChange?: (page: number) => void;
    onPageSizeChange?: (limit: number) => void;
    sort?: string;
    order?: "asc" | "desc";
    onSortChange?: (sort: string, order: "asc" | "desc") => void;
    title?: string;
    compact?: boolean;
    emptyMessage?: string;
    cardClass?: string;
    filters?: ActivityFilters;
    onFiltersChange?: (filters: ActivityFilters) => void;
    showModelFilter?: boolean;
    showIDFilters?: boolean;
    filterIdPrefix?: string;
    onViewRow?: (row: ActivityLogEntry) => void;
    canViewRow?: (row: ActivityLogEntry) => boolean;
    onFilterSession?: (row: ActivityLogEntry) => void;
    onDeleteRow?: (row: ActivityLogEntry) => void;
  }

  let {
    metrics,
    inflightRequests = [],
    storagePrefix,
    showModelColumn = true,
    showInflight = true,
    showRowActions = false,
    showPagination = false,
    page = 1,
    limit = 25,
    total = metrics.length,
    totalPages = metrics.length > 0 ? 1 : 0,
    onPageChange,
    onPageSizeChange,
    sort,
    order = "desc",
    onSortChange,
    title,
    compact = false,
    emptyMessage,
    cardClass = "",
    filters,
    onFiltersChange,
    showModelFilter = true,
    showIDFilters = true,
    filterIdPrefix = "filter",
    onViewRow,
    canViewRow,
    onFilterSession,
    onDeleteRow,
  }: Props = $props();

  // The filter drawer only renders when the parent owns filter state.
  let filtersEnabled = $derived(!!filters && !!onFiltersChange);
  let filterCount = $derived(filters ? activeFilterCount(filters) : 0);

  interface ColMeta {
    id: string;
    label: string;
    defaultVisible: boolean;
  }

  function buildColumnMeta(withModel: boolean, withActions: boolean): ColMeta[] {
    const cols: ColMeta[] = [
      { id: "id", label: t("activity.table.columns.id"), defaultVisible: true },
      { id: "time", label: t("activity.table.columns.time"), defaultVisible: true },
    ];
    if (withModel) cols.push({ id: "model", label: t("activity.table.columns.model"), defaultVisible: true });
    cols.push(
      { id: "req_path", label: t("activity.table.columns.path"), defaultVisible: false },
      { id: "resp_status_code", label: t("activity.table.columns.status"), defaultVisible: true },
      { id: "resp_content_type", label: t("activity.table.columns.contentType"), defaultVisible: false },
      { id: "cached", label: t("activity.table.columns.cached"), defaultVisible: true },
      { id: "cache_creation", label: t("activity.table.columns.cacheCreation"), defaultVisible: true },
      { id: "cache_hit_ratio", label: t("activity.table.columns.cacheHitRatio"), defaultVisible: true },
      { id: "repair_applied", label: t("activity.table.columns.repairApplied"), defaultVisible: true },
      { id: "prefix_stability", label: t("activity.table.columns.prefixStability"), defaultVisible: true },
      { id: "prompt", label: t("activity.table.columns.prompt"), defaultVisible: true },
      { id: "generated", label: t("activity.table.columns.generated"), defaultVisible: true },
      { id: "drafted", label: t("activity.table.columns.drafted"), defaultVisible: false },
      { id: "prompt_speed", label: t("activity.table.columns.prefill"), defaultVisible: true },
      { id: "gen_speed", label: t("activity.table.columns.decode"), defaultVisible: true },
      { id: "duration", label: t("activity.table.columns.duration"), defaultVisible: true },
      { id: "meta", label: t("activity.table.columns.meta"), defaultVisible: false }
    );
    if (withActions) {
      cols.push({ id: "actions", label: t("activity.table.columns.actions"), defaultVisible: true });
    }
    return cols;
  }

  let columnMeta = $derived.by(() => {
    $locale;
    return buildColumnMeta(
      showModelColumn,
      showRowActions && (!!onViewRow || !!onFilterSession || !!onDeleteRow),
    );
  });

  let columnLabelMap = $derived(
    Object.fromEntries(columnMeta.map((c) => [c.id, c.label])) as Record<string, string>
  );

  let defaultVisibility = $derived.by(() => {
    const v: VisibilityState = {};
    for (const c of columnMeta) v[c.id] = c.defaultVisible;
    return v;
  });

  // svelte-ignore state_referenced_locally
  const storedVisibility = persistentStore<VisibilityState>(
    `${storagePrefix}-columns`,
    {}
  );

  // svelte-ignore state_referenced_locally
  let columnVisibility = $state<VisibilityState>(
    Object.keys($storedVisibility).length > 0 ? $storedVisibility : defaultVisibility
  );

  // svelte-ignore state_referenced_locally
  const storedColumnOrder = persistentStore<string[]>(`${storagePrefix}-column-order`, []);

  let defaultColumnOrder = $derived(columnMeta.map((c) => c.id));

  // svelte-ignore state_referenced_locally
  let columnOrder = $state<string[]>(
    $storedColumnOrder.length > 0 ? $storedColumnOrder : defaultColumnOrder
  );

  // Reconcile stored order against the current column set: drop stale ids
  // and append any new ones so all columns are always represented.
  $effect(() => {
    const known = new Set(columnMeta.map((c) => c.id));
    const order = columnOrder;
    const hasStale = order.some((k) => !known.has(k));
    const missing = columnMeta.filter((c) => !order.includes(c.id)).map((c) => c.id);
    if (hasStale || missing.length > 0) {
      const cleaned = order.filter((k) => known.has(k));
      columnOrder = [...cleaned, ...missing];
      storedColumnOrder.set(columnOrder);
    }
  });

  // svelte-ignore state_referenced_locally
  const storedInflightOpen = persistentStore<boolean>(`${storagePrefix}-inflight-open`, true);

  // svelte-ignore state_referenced_locally
  const storedFilterOpen = persistentStore<boolean>(`${storagePrefix}-filter-open`, false);

  function buildInflightColumnMeta(withModel: boolean): ColMeta[] {
    const cols: ColMeta[] = [
      { id: "cancel", label: t("activity.table.inFlightColumns.cancel"), defaultVisible: true },
      { id: "elapsed", label: t("activity.table.inFlightColumns.elapsed"), defaultVisible: true },
      { id: "phase", label: t("activity.table.inFlightColumns.phase"), defaultVisible: true },
    ];
    if (withModel) cols.push({ id: "model", label: t("activity.table.inFlightColumns.model"), defaultVisible: true });
    cols.push(
      { id: "request", label: t("activity.table.inFlightColumns.request"), defaultVisible: true },
      { id: "identity", label: t("activity.table.inFlightColumns.address"), defaultVisible: true },
      { id: "user_agent", label: t("activity.table.inFlightColumns.userAgent"), defaultVisible: true },
      { id: "session_id", label: t("activity.table.inFlightColumns.sessionId"), defaultVisible: true },
      { id: "bytes_received", label: t("activity.table.inFlightColumns.bytesReceived"), defaultVisible: true },
      { id: "prefill_speed", label: t("activity.table.inFlightColumns.prefillSpeed"), defaultVisible: true },
      { id: "decode_speed", label: t("activity.table.inFlightColumns.decodeSpeed"), defaultVisible: true }
    );
    return cols;
  }

  let inflightColumnMeta = $derived.by(() => {
    $locale;
    return buildInflightColumnMeta(showModelColumn);
  });
  let inflightColumnLabelMap = $derived(
    Object.fromEntries(inflightColumnMeta.map((column) => [column.id, column.label])) as Record<string, string>
  );
  let inflightDefaultVisibility = $derived.by(() => {
    const visibility: VisibilityState = {};
    for (const column of inflightColumnMeta) visibility[column.id] = column.defaultVisible;
    return visibility;
  });

  // svelte-ignore state_referenced_locally
  const storedInflightVisibility = persistentStore<VisibilityState>(
    `${storagePrefix}-inflight-columns`,
    {}
  );
  // svelte-ignore state_referenced_locally
  let inflightColumnVisibility = $state<VisibilityState>(
    Object.keys($storedInflightVisibility).length > 0
      ? $storedInflightVisibility
      : inflightDefaultVisibility
  );
  // svelte-ignore state_referenced_locally
  const storedInflightColumnOrder = persistentStore<string[]>(
    `${storagePrefix}-inflight-column-order`,
    []
  );
  let inflightDefaultColumnOrder = $derived(inflightColumnMeta.map((column) => column.id));
  // svelte-ignore state_referenced_locally
  let inflightColumnOrder = $state<string[]>(
    $storedInflightColumnOrder.length > 0
      ? $storedInflightColumnOrder
      : inflightDefaultColumnOrder
  );
  let visibleInflightColumns = $derived(
    inflightColumnOrder.filter((id) => inflightColumnVisibility[id] !== false)
  );

  $effect(() => {
    const known = new Set(inflightColumnMeta.map((column) => column.id));
    const hasStale = inflightColumnOrder.some((id) => !known.has(id));
    const missing = inflightColumnMeta
      .filter((column) => !inflightColumnOrder.includes(column.id))
      .map((column) => column.id);
    if (hasStale || missing.length > 0) {
      inflightColumnOrder = [
        ...inflightColumnOrder.filter((id) => known.has(id)),
        ...missing,
      ];
      storedInflightColumnOrder.set(inflightColumnOrder);
    }
  });

  // When onSortChange is provided the table sorts on the server: the sort
  // state is driven by the sort/order props and toggles are forwarded to the
  // parent. Otherwise it falls back to client-side sorting of the current page.
  let localSorting = $state<SortingState>([]);
  let sorting = $derived<SortingState>(
    onSortChange ? (sort ? [{ id: sort, desc: order === "desc" }] : []) : localSorting
  );
  let inflightOpen = $state($storedInflightOpen);
  let filterOpen = $state($storedFilterOpen);

  let exportOpen = $state(false);
  let exportMarkdown = $state("");
  let cancelingInflightIds = $state<string[]>([]);
  let inflightNowMs = $state(performance.now());

  $effect(() => {
    if (inflightRequests.length === 0) return;

    // Refresh synchronously when the first request appears so the initial
    // render does not use the timestamp from when this component mounted.
    inflightNowMs = performance.now();

    let frame = 0;
    const tick = () => {
      inflightNowMs = performance.now();
      frame = requestAnimationFrame(tick);
    };
    frame = requestAnimationFrame(tick);

    return () => cancelAnimationFrame(frame);
  });

  // Built on demand rather than in a $derived: the source only matters while
  // the dialog is open, and activity refreshes land every second.
  function openExport() {
    exportMarkdown = buildActivityMarkdown(
      table.getRowModel().rows.map((row) => row.original),
      exportColumns,
      new Date(),
      $locale
    );
    exportOpen = true;
  }

  function closeExport() {
    exportOpen = false;
    exportMarkdown = "";
  }

  function setInflightOpen(open: boolean) {
    inflightOpen = open;
    storedInflightOpen.set(open);
  }

  function setFilterOpen(open: boolean) {
    filterOpen = open;
    storedFilterOpen.set(open);
  }

  async function cancelInflight(id: string) {
    if (cancelingInflightIds.includes(id)) return;
    cancelingInflightIds = [...cancelingInflightIds, id];
    try {
      await cancelInflightRequest(id);
    } finally {
      cancelingInflightIds = cancelingInflightIds.filter((current) => current !== id);
    }
  }

  function buildColumns(withModel: boolean, withActions: boolean): ColumnDef<ActivityLogEntry>[] {
    const cols: ColumnDef<ActivityLogEntry>[] = [
      {
        id: "id",
        accessorKey: "id",
        header: "ID",
        cell: ({ row }) => String(row.original.id),
      },
      {
        id: "time",
        accessorKey: "timestamp",
        header: t("activity.table.columns.time"),
        cell: ({ row }) => renderComponent(TimeCell, { timestamp: row.original.timestamp }),
      },
    ];

    if (withModel) {
      cols.push({
        id: "model",
        accessorKey: "model",
        header: t("activity.table.columns.model"),
        cell: ({ row }) =>
          renderComponent(ModelLink, { model: row.original.model }),
      });
    }

    cols.push(
      {
        id: "req_path",
        accessorKey: "req_path",
        header: t("activity.table.columns.path"),
        cell: ({ row }) => row.original.req_path || "-",
      },
      {
        id: "resp_status_code",
        accessorKey: "resp_status_code",
        header: t("activity.table.columns.status"),
        cell: ({ row }) => String(row.original.resp_status_code || "-"),
      },
      {
        id: "resp_content_type",
        accessorKey: "resp_content_type",
        header: t("activity.table.columns.contentType"),
        cell: ({ row }) => row.original.resp_content_type || "-",
      },
      {
        id: "cached",
        accessorFn: (row) => row.tokens.cache_tokens,
        header: () => renderComponent(HeaderLabel, { label: t("activity.table.columns.cached"), tooltip: t("activity.table.tooltips.cached") }),
        cell: ({ row }) =>
          row.original.tokens.cache_tokens > 0
            ? row.original.tokens.cache_tokens.toLocaleString()
            : "-",
      },
      {
        id: "cache_creation",
        accessorFn: (row) => row.cache_creation_tokens ?? 0,
        header: () => renderComponent(HeaderLabel, { label: t("activity.table.columns.cacheCreation"), tooltip: t("activity.table.tooltips.cacheCreation") }),
        enableSorting: false,
        cell: ({ row }) => formatCacheCreationTokens(row.original.cache_creation_tokens, $locale),
      },
      {
        id: "cache_hit_ratio",
        accessorFn: (row) => row.cache_hit_ratio ?? -1,
        header: () => renderComponent(HeaderLabel, { label: t("activity.table.columns.cacheHitRatio"), tooltip: t("activity.table.tooltips.cacheHitRatio") }),
        enableSorting: false,
        cell: ({ row }) => formatCacheHitRatio(row.original),
      },
      {
        id: "repair_applied",
        accessorFn: (row) => (row.repair_applied ? 1 : 0),
        header: () => renderComponent(HeaderLabel, { label: t("activity.table.columns.repairApplied"), tooltip: t("activity.table.tooltips.repairApplied") }),
        enableSorting: false,
        cell: ({ row }) => row.original.repair_applied ? t("activity.table.values.applied") : "-",
      },
      {
        id: "prefix_stability",
        accessorFn: (row) => row.prefix_hash ?? "",
        header: () => renderComponent(HeaderLabel, { label: t("activity.table.columns.prefixStability"), tooltip: t("activity.table.tooltips.prefixStability") }),
        enableSorting: false,
        cell: ({ row }) => {
          const value = formatPrefixStability(row.original);
          return value === "-"
            ? value
            : renderComponent(MiddleEllipsis, { value: row.original.prefix_hash ?? value, tailLength: 4, className: "max-w-[8rem] font-mono text-xs" });
        },
      },
      {
        id: "prompt",
        accessorFn: (row) => row.tokens.input_tokens,
        header: () => renderComponent(HeaderLabel, { label: t("activity.table.columns.prompt"), tooltip: t("activity.table.tooltips.prompt") }),
        cell: ({ row }) => row.original.tokens.input_tokens.toLocaleString(),
      },
      {
        id: "generated",
        accessorFn: (row) => row.tokens.output_tokens,
        header: t("activity.table.columns.generated"),
        cell: ({ row }) => row.original.tokens.output_tokens.toLocaleString(),
      },
      {
        id: "drafted",
        accessorFn: (row) => row.tokens.draft_tokens,
        header: () => renderComponent(HeaderLabel, { label: t("activity.table.columns.drafted"), tooltip: t("activity.table.tooltips.drafted") }),
        cell: ({ row }) =>
          formatDrafted(row.original.tokens.draft_tokens, row.original.tokens.draft_acc_tokens),
      },
      {
        id: "prompt_speed",
        accessorFn: (row) => row.tokens.prompt_per_second,
        header: () => renderComponent(HeaderLabel, { label: t("activity.table.columns.prefill"), tooltip: t("activity.table.tooltips.prefillSpeed") }),
        cell: ({ row }) => formatSpeed(row.original.tokens.prompt_per_second, t("common.unknown")),
      },
      {
        id: "gen_speed",
        accessorFn: (row) => row.tokens.tokens_per_second,
        header: () => renderComponent(HeaderLabel, { label: t("activity.table.columns.decode"), tooltip: t("activity.table.tooltips.decodeSpeed") }),
        cell: ({ row }) => formatSpeed(row.original.tokens.tokens_per_second, t("common.unknown")),
      },
      {
        id: "duration",
        accessorKey: "duration_ms",
        header: t("activity.table.columns.duration"),
        cell: ({ row }) => formatFirstTokenDuration(row.original),
      },
      {
        id: "meta",
        header: t("activity.table.columns.meta"),
        enableSorting: false,
        cell: ({ row }) =>
          renderComponent(MetaCell, { metadata: row.original.metadata }),
      }
    );
    if (withActions) {
      cols.push({
        id: "actions",
        header: t("activity.table.columns.actions"),
        enableSorting: false,
        cell: ({ row }) => renderComponent(ActivityRowActions, {
          onView: onViewRow && (!canViewRow || canViewRow(row.original))
            ? () => onViewRow(row.original)
            : undefined,
          onFilterSession: onFilterSession && row.original.session_id?.trim()
            ? () => onFilterSession(row.original)
            : undefined,
          onDelete: onDeleteRow ? () => onDeleteRow(row.original) : undefined,
        }),
      });
    }
    return cols;
  }

  let columns = $derived.by(() => {
    $locale;
    return buildColumns(
      showModelColumn,
      showRowActions && (!!onViewRow || !!onFilterSession || !!onDeleteRow),
    );
  });

  const table = createSvelteTable({
    get data() {
      return metrics;
    },
    get columns() {
      return columns;
    },
    state: {
      get columnVisibility() {
        return columnVisibility;
      },
      get sorting() {
        return sorting;
      },
      get columnOrder() {
        return columnOrder;
      },
    },
    // svelte-ignore state_referenced_locally
    manualSorting: !!onSortChange,
    onSortingChange: (updater) => {
      const next = typeof updater === "function" ? updater(sorting) : updater;
      if (onSortChange) {
        const first = next[0];
        onSortChange(first?.id ?? "", first?.desc === false ? "asc" : "desc");
      } else {
        localSorting = next;
      }
    },
    onColumnOrderChange: (updater) => {
      columnOrder =
        typeof updater === "function" ? updater(columnOrder) : updater;
      storedColumnOrder.set(columnOrder);
    },
    onColumnVisibilityChange: (updater) => {
      columnVisibility =
        typeof updater === "function" ? updater(columnVisibility) : updater;
      storedVisibility.set(columnVisibility);
    },
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: getSortedRowModel(),
  });

  // Precomputed (rather than filtered inline in the template) because the
  // animate:flip element must be the sole child of the each block.
  let menuColumnIds = $derived(
    columnOrder.filter((id) => table.getColumn(id)?.getCanHide() ?? false)
  );

  // Mirrors the visible table order while excluding button-only row actions.
  let exportColumns = $derived(
    table
      .getVisibleLeafColumns()
      .filter((column) => column.id !== "actions")
      .map((column) => ({ id: column.id, label: columnLabelMap[column.id] ?? column.id }))
  );

  // The header sticks to the top of the scrolling table container. It needs an
  // opaque background so rows pass underneath, and the bottom rule is drawn as
  // an inset shadow because a collapsed table border does not paint reliably on
  // a sticky cell.
  let thClass = $derived(
    (compact ? "px-2 py-2 h-9" : "px-3 py-3 h-12") +
      " bg-card sticky top-0 z-10 shadow-[inset_0_-1px_0_var(--border)]"
  );
  let tdClass = $derived(compact ? "px-2 py-2" : "px-3 py-4");
  let inflightThClass = $derived(compact ? "h-7 px-2 py-1" : "h-8 px-3 py-1.5");
  let inflightTdClass = $derived(compact ? "px-2 py-1" : "px-3 py-1.5");
  let visibleColumnCount = $derived(table.getVisibleLeafColumns().length);
  let pageCount = $derived(Math.max(totalPages, 1));
  let visiblePages = $derived.by(() => {
    const maxButtons = 7;
    const count = pageCount;
    const current = Math.min(Math.max(page, 1), count);
    const half = Math.floor(maxButtons / 2);
    let start = Math.max(1, current - half);
    let end = Math.min(count, start + maxButtons - 1);
    start = Math.max(1, end - maxButtons + 1);
    return Array.from({ length: end - start + 1 }, (_, i) => start + i);
  });

  function setServerPage(nextPage: number) {
    const clamped = Math.min(Math.max(nextPage, 1), pageCount);
    if (clamped !== page) onPageChange?.(clamped);
  }

  function setServerPageSize(nextLimit: number) {
    onPageSizeChange?.(nextLimit);
  }

  function sortIcon(state: false | "asc" | "desc") {
    if (state === "asc") return ArrowUp;
    if (state === "desc") return ArrowDown;
    return ArrowUpDown;
  }

  // A hovered row only yields once the pointer passes its midpoint in the
  // direction of travel. Without that hysteresis the swap fires as soon as
  // the pointer touches the neighbour and the rows oscillate.
  //
  // The midpoint comes from offsetTop/offsetHeight rather than
  // getBoundingClientRect() because the rect includes the in-flight
  // animate:flip transform: hit-testing a row that is still sliding compares
  // against a position that no longer matches where it will settle.
  function crossedMidpoint(e: DragEvent, movingDown: boolean): boolean {
    const el = e.currentTarget as HTMLElement | null;
    const parent = el?.offsetParent as HTMLElement | null;
    if (!el || !parent) return true;
    const top = parent.getBoundingClientRect().top - parent.scrollTop + el.offsetTop;
    const midpoint = top + el.offsetHeight / 2;
    return movingDown ? e.clientY > midpoint : e.clientY < midpoint;
  }

  let dragColId: string | null = $state(null);

  function handleColDragStart(e: DragEvent, colId: string) {
    dragColId = colId;
    if (e.dataTransfer) {
      e.dataTransfer.effectAllowed = "move";
      e.dataTransfer.setData("text/plain", colId);
    }
  }

  // Reorders columnOrder live as the drag crosses other rows, so the
  // animate:flip on each row animates the columns sliding out of the way
  // and the menu's order always matches what onColumnOrderChange applies
  // to the table.
  function handleColDragOver(e: DragEvent, colId: string) {
    e.preventDefault();
    if (e.dataTransfer) e.dataTransfer.dropEffect = "move";
    if (!dragColId || dragColId === colId) return;
    const order = [...columnOrder];
    const fromIndex = order.indexOf(dragColId);
    const toIndex = order.indexOf(colId);
    if (fromIndex === -1 || toIndex === -1 || fromIndex === toIndex) return;
    if (!crossedMidpoint(e, toIndex > fromIndex)) return;
    order.splice(fromIndex, 1);
    order.splice(toIndex, 0, dragColId);
    columnOrder = order;
  }

  function handleColDrop(e: DragEvent) {
    e.preventDefault();
    dragColId = null;
    storedColumnOrder.set(columnOrder);
  }

  function handleColDragEnd() {
    dragColId = null;
    storedColumnOrder.set(columnOrder);
  }

  function formatInflightElapsed(request: InflightRequestEntry, nowMs: number): string {
    return `${(liveElapsedMs(request.elapsed_ms, request.client_received_at_ms, nowMs) / 1000).toFixed(2)}s`;
  }

  function formatInflightTokenSpeed(
    speed: number | undefined,
    requestPhase: string | undefined,
    speedPhase: "prefill" | "decode",
  ): string {
    return speed !== undefined && Number.isFinite(speed) && speed > 0
      ? formatSpeed(speed, t("common.unknown"))
      : requestPhase === speedPhase
        ? t("activity.table.values.waitingForTelemetry")
        : "-";
  }

  function inflightColumnTooltip(columnId: string): string | undefined {
    switch (columnId) {
      case "prefill_speed": return t("activity.table.tooltips.inFlightPrefillSpeed");
      case "decode_speed": return t("activity.table.tooltips.inFlightDecodeSpeed");
      default: return undefined;
    }
  }

  function formatFirstTokenDuration(row: ActivityLogEntry): string {
    const total = row.duration_ms >= 0 ? formatDuration(row.duration_ms) : "-";
    const first = row.first_token_ms > 0 ? formatDuration(row.first_token_ms) : "-";
    return `${first} / ${total}`;
  }

  function inflightPhaseLabel(phase?: string): string {
    switch (phase) {
      case "waiting": return t("activity.table.phases.waiting");
      case "prefill": return t("activity.table.phases.prefill");
      case "decode": return t("activity.table.phases.decode");
      default: return t("activity.table.phases.unknown");
    }
  }

  function toggleInflightColumn(id: string, visible: boolean) {
    inflightColumnVisibility = { ...inflightColumnVisibility, [id]: visible };
    storedInflightVisibility.set(inflightColumnVisibility);
  }

  let inflightDragColId: string | null = $state(null);

  function handleInflightColDragStart(e: DragEvent, id: string) {
    inflightDragColId = id;
    if (e.dataTransfer) {
      e.dataTransfer.effectAllowed = "move";
      e.dataTransfer.setData("text/plain", id);
    }
  }

  function handleInflightColDragOver(e: DragEvent, id: string) {
    e.preventDefault();
    if (e.dataTransfer) e.dataTransfer.dropEffect = "move";
    if (!inflightDragColId || inflightDragColId === id) return;
    const next = [...inflightColumnOrder];
    const from = next.indexOf(inflightDragColId);
    const to = next.indexOf(id);
    if (from === -1 || to === -1 || from === to) return;
    if (!crossedMidpoint(e, to > from)) return;
    next.splice(from, 1);
    next.splice(to, 0, inflightDragColId);
    inflightColumnOrder = next;
  }

  function handleInflightColDrop(e: DragEvent) {
    e.preventDefault();
    inflightDragColId = null;
    storedInflightColumnOrder.set(inflightColumnOrder);
  }

  function clearInflightColumnDrag() {
    inflightDragColId = null;
    storedInflightColumnOrder.set(inflightColumnOrder);
  }
</script>

{#if showInflight}
<Card.Root class="relative p-3">
  <div class="pr-16">
    <Card.Title class="flex items-center gap-2 text-sm">
      {$translate("activity.table.inFlightTitle")}
      {#if inflightRequests.length > 0}
        <Badge variant="secondary">{inflightRequests.length}</Badge>
      {/if}
    </Card.Title>
  </div>

  <div class="absolute right-2 top-2 flex items-center gap-1">
    <DropdownMenu.Root>
      <DropdownMenu.Trigger
        class="text-muted-foreground hover:bg-muted inline-flex size-6 items-center justify-center rounded-full"
        title={$translate("activity.table.selectInFlightColumns")}
      >
        <Columns3 class="size-4" />
      </DropdownMenu.Trigger>
      <DropdownMenu.Content align="end" class="min-w-[18rem] max-h-[60vh] overflow-y-auto p-0">
        <DropdownMenu.Label class="text-muted-foreground border-b px-3 py-2 text-xs font-medium uppercase tracking-wider">
          {$translate("activity.table.columnsMenu")} <span class="text-[10px] normal-case tracking-normal">{$translate("activity.table.dragToReorderHint")}</span>
        </DropdownMenu.Label>
        {#each inflightColumnOrder as columnId (columnId)}
          <div
            animate:flip={{ duration: 200 }}
            draggable="true"
            role="button"
            tabindex="-1"
            aria-label={$translate("activity.table.dragToReorder", { label: inflightColumnLabelMap[columnId] ?? columnId })}
            ondragstart={(event) => handleInflightColDragStart(event, columnId)}
            ondragover={(event) => handleInflightColDragOver(event, columnId)}
            ondrop={handleInflightColDrop}
            ondragend={clearInflightColumnDrag}
            class={inflightDragColId === columnId ? "opacity-40" : ""}
          >
            <DropdownMenu.CheckboxItem
              checked={inflightColumnVisibility[columnId] !== false}
              onCheckedChange={(visible) => toggleInflightColumn(columnId, !!visible)}
              closeOnSelect={false}
            >
              <GripVertical class="text-muted-foreground/50 size-4 cursor-grab active:cursor-grabbing" />
              <span class="flex-1">{inflightColumnLabelMap[columnId] ?? columnId}</span>
            </DropdownMenu.CheckboxItem>
          </div>
        {/each}
      </DropdownMenu.Content>
    </DropdownMenu.Root>

    <Button
      variant="ghost"
      size="icon-xs"
      class="text-muted-foreground rounded-full"
      onclick={() => setInflightOpen(!inflightOpen)}
      title={$translate(inflightOpen ? "activity.table.hideInFlight" : "activity.table.showInFlight")}
    >
      {#if inflightOpen}
        <X />
      {:else}
        <ChevronDown />
      {/if}
    </Button>
  </div>

  {#if inflightOpen}
    <div class="mt-2 overflow-x-auto">
      <Table.Root class="min-w-full">
        <Table.Header>
          <Table.Row>
            {#each visibleInflightColumns as columnId (columnId)}
              <Table.Head class={inflightThClass} title={inflightColumnTooltip(columnId)}>
                {inflightColumnLabelMap[columnId] ?? columnId}
              </Table.Head>
            {/each}
          </Table.Row>
        </Table.Header>
        <Table.Body>
          {#each inflightRequests as request (request.id)}
            <Table.Row>
              {#each visibleInflightColumns as columnId (columnId)}
                <Table.Cell class={inflightTdClass}>
                  {#if columnId === "cancel"}
                    <Button
                      variant="ghost"
                      size="icon-sm"
                      class="text-muted-foreground hover:text-destructive size-6"
                      onclick={() => cancelInflight(request.id)}
                      disabled={cancelingInflightIds.includes(request.id)}
                      title={$translate("activity.table.cancelRequest")}
                      aria-label={$translate("activity.table.cancelInFlight")}
                    >
                      <CircleX class="size-4" />
                    </Button>
                  {:else if columnId === "elapsed"}
                    <span class="font-mono text-xs tabular-nums">
                      {formatInflightElapsed(request, inflightNowMs)}
                    </span>
                  {:else if columnId === "phase"}
                    <Badge
                      variant={request.phase === "decode" ? "default" : request.phase === "prefill" ? "secondary" : "outline"}
                      title={request.phase_message || inflightPhaseLabel(request.phase)}
                    >{inflightPhaseLabel(request.phase)}</Badge>
                  {:else if columnId === "model"}
                    <MiddleEllipsis value={request.model} tailLength={10} className="max-w-[14rem]" />
                  {:else if columnId === "request"}
                    {@const requestLabel = `${request.method || "-"} ${request.req_path || "-"}`}
                    <MiddleEllipsis value={requestLabel} tailLength={14} className="max-w-[20rem] font-mono text-xs" />
                  {:else if columnId === "identity"}
                    <MiddleEllipsis value={request.remote_ip} tailLength={8} className="max-w-[12rem] font-mono text-xs" />
                  {:else if columnId === "user_agent"}
                    {@const userAgent = requestHeader(request.req_headers, "User-Agent")}
                    <MiddleEllipsis value={userAgent} tailLength={18} className="max-w-[20rem] text-xs" />
                  {:else if columnId === "session_id"}
                    {@const session = sessionID(request.req_headers, $uiConfig.activity.session_id)}
                    <MiddleEllipsis value={session} tailLength={8} className="max-w-[14rem] font-mono text-xs" />
                  {:else if columnId === "bytes_received"}
                    <span class="font-mono text-xs tabular-nums">{formatBytes(request.resp_bytes)}</span>
                  {:else if columnId === "prefill_speed"}
                    <span class="font-mono text-xs tabular-nums">{formatInflightTokenSpeed(request.prompt_per_second, request.phase, "prefill")}</span>
                  {:else if columnId === "decode_speed"}
                    <span class="font-mono text-xs tabular-nums">{formatInflightTokenSpeed(request.tokens_per_second, request.phase, "decode")}</span>
                  {/if}
                </Table.Cell>
              {/each}
            </Table.Row>
          {:else}
            <Table.Row>
              <Table.Cell colspan={Math.max(visibleInflightColumns.length, 1)} class="text-muted-foreground py-4 text-center text-sm">
                {$translate("activity.table.noInFlight")}
              </Table.Cell>
            </Table.Row>
          {/each}
        </Table.Body>
      </Table.Root>
    </div>
    {#if inflightRequests.length > 0}
      <details
        open
        class="mt-2 overflow-hidden rounded-md border bg-background/40"
        aria-label={$translate("activity.table.inFlightOutput.title")}
      >
        <summary class="flex cursor-pointer list-none items-center justify-between px-3 py-1.5 [&::-webkit-details-marker]:hidden">
          <span class="flex items-center gap-1.5">
            <ChevronDown class="size-3 shrink-0 text-muted-foreground" />
            <span class="text-muted-foreground text-xs font-medium uppercase tracking-wider">
              {$translate("activity.table.inFlightOutput.title")}
            </span>
          </span>
          <span class="text-muted-foreground text-xs">
            {$translate("activity.table.inFlightOutput.requestCount", { count: inflightRequests.length })}
          </span>
        </summary>
        <div class="h-52 overflow-y-auto border-t">
          {#each inflightRequests as request (request.id)}
            {@const preview = request.output_preview || $translate("activity.table.values.noOutput")}
            <details class="group border-b last:border-b-0">
              <summary class="flex min-w-0 cursor-pointer list-none items-center gap-2 px-3 py-2 text-xs [&::-webkit-details-marker]:hidden">
                <ChevronDown class="size-3 shrink-0 text-muted-foreground transition-transform group-open:rotate-180" />
                <span class="shrink-0 font-mono tabular-nums">#{request.id}</span>
                <Badge
                  variant={request.phase === "decode" ? "default" : request.phase === "prefill" ? "secondary" : "outline"}
                  title={request.phase_message || inflightPhaseLabel(request.phase)}
                >{inflightPhaseLabel(request.phase)}</Badge>
                <span
                  class="min-w-0 flex-1 truncate text-muted-foreground"
                  title={preview}
                  aria-label={preview}
                >{preview}</span>
              </summary>
              <div class="border-t bg-muted/20 px-3 py-2">
                <pre class="max-h-24 overflow-y-auto whitespace-pre-wrap break-words font-mono text-xs leading-relaxed">{preview}</pre>
              </div>
            </details>
          {/each}
        </div>
      </details>
    {/if}
  {/if}
</Card.Root>
{/if}

<Card.Root class="mt-3 shrink-0 gap-0 overflow-hidden py-0 {cardClass}">
  <Card.Header class="flex items-center justify-between border-b px-4 py-2">
    <div class="flex items-center gap-2">
      {#if title}
        <Card.Title class="text-sm font-semibold">
          {title}
          <span class="text-muted-foreground text-xs font-normal">({total})</span>
        </Card.Title>
      {/if}
    </div>
    <div class="flex items-center gap-2">
      <button
        type="button"
        class="hover:bg-muted inline-flex size-7 items-center justify-center rounded-[min(var(--radius-md),12px)]"
        title={$translate("activity.table.exportMarkdown")}
        onclick={openExport}
      >
        <Download class="size-4" />
      </button>
      {#if filtersEnabled}
        <button
          type="button"
          class="hover:bg-muted relative inline-flex size-7 items-center justify-center rounded-[min(var(--radius-md),12px)] {filterOpen
            ? 'bg-muted'
            : ''}"
          title={$translate(filterOpen ? "activity.table.hideFilters" : "activity.table.showFilters")}
          aria-expanded={filterOpen}
          onclick={() => setFilterOpen(!filterOpen)}
        >
          <ListFilter class="size-4" />
          {#if filterCount > 0}
            <span
              class="bg-primary absolute -right-0.5 -top-0.5 size-2 rounded-full"
              aria-hidden="true"
            ></span>
          {/if}
        </button>
      {/if}
      <DropdownMenu.Root>
        <DropdownMenu.Trigger
          class="hover:bg-muted inline-flex size-7 items-center justify-center rounded-[min(var(--radius-md),12px)]"
          title={$translate("activity.table.selectColumns")}
        >
          <Columns3 class="size-4" />
        </DropdownMenu.Trigger>
        <DropdownMenu.Content align="end" class="min-w-[18rem] max-h-[60vh] overflow-y-auto p-0">
          <DropdownMenu.Label class="text-muted-foreground border-b px-3 py-2 text-xs font-medium uppercase tracking-wider">
            {$translate("activity.table.columnsMenu")} <span class="text-[10px] normal-case tracking-normal">{$translate("activity.table.dragToReorderHint")}</span>
          </DropdownMenu.Label>
          {#each menuColumnIds as columnId (columnId)}
            {@const column = table.getColumn(columnId)}
            <div
              animate:flip={{ duration: 200 }}
              draggable="true"
              role="button"
              tabindex="-1"
              aria-label={$translate("activity.table.dragToReorder", { label: columnLabelMap[columnId] ?? columnId })}
              ondragstart={(e) => handleColDragStart(e, columnId)}
              ondragover={(e) => handleColDragOver(e, columnId)}
              ondrop={handleColDrop}
              ondragend={handleColDragEnd}
              class={dragColId === columnId ? "opacity-40" : ""}
            >
              <DropdownMenu.CheckboxItem
                checked={column?.getIsVisible() ?? false}
                onCheckedChange={(v) => column?.toggleVisibility(!!v)}
                closeOnSelect={false}
              >
                <GripVertical class="text-muted-foreground/50 size-4 cursor-grab active:cursor-grabbing" />
                <span class="flex-1">{columnLabelMap[columnId] ?? columnId}</span>
              </DropdownMenu.CheckboxItem>
            </div>
          {/each}
        </DropdownMenu.Content>
      </DropdownMenu.Root>
    </div>
  </Card.Header>
  {#if filtersEnabled && filterOpen && filters && onFiltersChange}
    <FilterDrawer
      {filters}
      onchange={onFiltersChange}
      {showModelFilter}
      {showIDFilters}
      idPrefix={filterIdPrefix}
      showRows={showPagination}
      {limit}
      onLimitChange={setServerPageSize}
    />
  {/if}
  <!--
    Table.Root always wraps the table in an overflow-x-auto div, and CSS
    promotes that box's overflow-y to auto as well, making it the sticky
    header's scrollport. Bounding its height here is what turns it into a real
    scrolling box so `sticky top-0` on the header has something to stick to.
    Horizontal scrolling stays on that same wrapper, so Card.Content does not
    need its own overflow. Override the bound per call site by setting
    --activity-table-max-h (e.g. cardClass="[--activity-table-max-h:70vh]").
  -->
  <Card.Content
    class="p-0 [&>[data-slot=table-container]]:max-h-[var(--activity-table-max-h,60vh)] [&>[data-slot=table-container]]:overflow-y-auto"
  >
    <Table.Root class="min-w-full">
      <Table.Header>
        {#each table.getHeaderGroups() as headerGroup (headerGroup.id)}
          <Table.Row>
            {#each headerGroup.headers as header (header.id)}
              <Table.Head class={thClass} colspan={header.colSpan}>
                {#if !header.isPlaceholder}
                  {#if header.column.getCanSort()}
                    {@const sorted = header.column.getIsSorted()}
                    {@const Icon = sortIcon(sorted)}
                    <button
                      type="button"
                      class="text-muted-foreground hover:text-foreground -mx-1 inline-flex items-center gap-1 text-left"
                      onclick={() => header.column.toggleSorting(sorted === "asc")}
                    >
                      <FlexRender content={header.column.columnDef.header} context={header.getContext()} />
                      <Icon class={`size-3 ${sorted ? "text-foreground" : "opacity-50"}`} />
                    </button>
                  {:else}
                    <FlexRender content={header.column.columnDef.header} context={header.getContext()} />
                  {/if}
                {/if}
              </Table.Head>
            {/each}
          </Table.Row>
        {/each}
      </Table.Header>
      <Table.Body>
        {#each table.getRowModel().rows as row (row.id)}
          <Table.Row>
            {#each row.getVisibleCells() as cell (cell.id)}
              <Table.Cell class={tdClass}>
                <FlexRender content={cell.column.columnDef.cell} context={cell.getContext()} />
              </Table.Cell>
            {/each}
          </Table.Row>
        {/each}
        {#if table.getRowModel().rows.length === 0}
          <Table.Row>
            <Table.Cell colspan={visibleColumnCount} class="text-muted-foreground py-6 text-center text-sm">
              {emptyMessage ?? $translate("activity.empty")}
            </Table.Cell>
          </Table.Row>
        {/if}
      </Table.Body>
    </Table.Root>

    {#if showPagination && total > 0}
      <div class="flex items-center justify-between gap-2 border-t px-4 py-2 text-sm">
        <span class="text-muted-foreground text-xs">
          {$translate("common.page.info", { page, pages: pageCount, total })}
        </span>
        <div class="flex items-center gap-1">
          <Button
            variant="ghost"
            size="icon-sm"
            onclick={() => setServerPage(1)}
            disabled={page <= 1}
            title={$translate("common.page.first")}
          >
            <ChevronsLeft />
          </Button>
          <Button
            variant="ghost"
            size="icon-sm"
            onclick={() => setServerPage(page - 1)}
            disabled={page <= 1}
            title={$translate("common.page.previous")}
          >
            <ChevronLeft />
          </Button>
          {#each visiblePages as pageNumber (pageNumber)}
            <Button
              variant={pageNumber === page ? "secondary" : "ghost"}
              size="sm"
              class="h-7 min-w-7 px-2 text-xs"
              onclick={() => setServerPage(pageNumber)}
              disabled={pageNumber === page}
            >
              {pageNumber}
            </Button>
          {/each}
          <Button
            variant="ghost"
            size="icon-sm"
            onclick={() => setServerPage(page + 1)}
            disabled={page >= pageCount}
            title={$translate("common.page.next")}
          >
            <ChevronRight />
          </Button>
          <Button
            variant="ghost"
            size="icon-sm"
            onclick={() => setServerPage(pageCount)}
            disabled={page >= pageCount}
            title={$translate("common.page.last")}
          >
            <ChevronsRight />
          </Button>
        </div>
      </div>
    {/if}
  </Card.Content>
</Card.Root>

<ExportDialog markdown={exportMarkdown} open={exportOpen} onclose={closeExport} />
