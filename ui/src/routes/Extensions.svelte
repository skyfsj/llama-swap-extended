<script lang="ts">
  import { onMount } from "svelte";
  import { link, push } from "svelte-spa-router";
  import {
    Plus,
    Search,
    RefreshCw,
    Puzzle,
    MoreHorizontal,
    Trash2,
    Copy,
    FileCode2,
    Settings2,
    PackagePlus,
    Download,
    ScrollText,
    Check,
    LayoutGrid,
    List,
    SlidersHorizontal,
    CheckCircle2,
    PowerOff,
    AlertTriangle,
    BookOpen,
    Play,
    Network,
    HardDrive,
    X,
    ExternalLink,
    ArrowUpDown,
  } from "@lucide/svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Badge } from "$lib/components/ui/badge/index.js";
  import * as Switch from "$lib/components/ui/switch/index.js";
  import * as DropdownMenu from "$lib/components/ui/dropdown-menu/index.js";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import * as Table from "$lib/components/ui/table/index.js";
  import ConfirmDialog from "../components/ConfirmDialog.svelte";
  import ExtensionPresetStore from "../components/extensions/ExtensionPresetStore.svelte";
  import ExtensionConfiguration from "../components/extensions/ExtensionConfiguration.svelte";
  import ExtensionImportDialog from "../components/extensions/ExtensionImportDialog.svelte";
  import ExtensionConsolePanel from "../components/extensions/ExtensionConsolePanel.svelte";
  import { translate } from "../lib/i18n";
  import { toast } from "../lib/toast";
  import {
    listExtensions,
    getExtension,
    saveExtension,
    deleteExtension,
    duplicateExtension,
    reloadExtensions,
    listPresets,
    exportExtension,
    settingDiagnosticsFrom,
    type ExtensionDefinition,
    type ExtensionPreset,
  } from "../lib/extensionsApi";

  let items = $state<ExtensionDefinition[]>([]);
  let loading = $state(true);
  let busy = $state(false);
  let error = $state("");
  let search = $state("");
  let filter = $state<"all" | "enabled" | "disabled" | "issues">("all");
  let sortBy = $state<"name" | "priority" | "status">("name");
  let viewMode = $state<"grid" | "table">("grid");
  let copiedId = $state<string | null>(null);

  // Targets for dialogs
  let deleteTarget = $state<ExtensionDefinition | null>(null);
  let deleteOpen = $state(false);
  let duplicateTarget = $state<ExtensionDefinition | null>(null);
  let duplicateOpen = $state(false);
  let duplicateId = $state("");
  let storeOpen = $state(false);
  let importOpen = $state(false);
  let logViewTarget = $state<ExtensionDefinition | null>(null);
  let logOpen = $state(false);
  let presets = $state<ExtensionPreset[]>([]);

  // Batch actions
  let batchOpen = $state(false);
  let batchActionType = $state<"enable" | "disable">("enable");

  const enabledCount = $derived(items.filter((item) => item.manifest.enabled).length);
  const disabledCount = $derived(items.length - enabledCount);
  const issueCount = $derived(
    items.filter((item) => ["error", "invalid", "stale"].includes(item.status)).length,
  );

  const filteredAndSorted = $derived.by(() => {
    let result = items.filter((item) => {
      const m = item.manifest;
      const matchesFilter =
        filter === "all" ||
        (filter === "enabled" ? m.enabled : filter === "disabled" ? !m.enabled : ["error", "invalid", "stale"].includes(item.status));

      const query = search.trim().toLowerCase();
      const matchesSearch =
        !query ||
        [m.id, m.name, m.description, ...(m.match?.models ?? []), ...(m.match?.endpoints ?? [])]
          .join(" ")
          .toLowerCase()
          .includes(query);

      return matchesFilter && matchesSearch;
    });

    return result.sort((a, b) => {
      if (sortBy === "name") {
        const nameA = a.manifest.name || a.manifest.id;
        const nameB = b.manifest.name || b.manifest.id;
        return nameA.localeCompare(nameB);
      }
      if (sortBy === "priority") {
        return (a.manifest.priority ?? 100) - (b.manifest.priority ?? 100);
      }
      if (sortBy === "status") {
        const statusOrder: Record<string, number> = { error: 0, invalid: 1, stale: 2, ready: 3, disabled: 4 };
        return (statusOrder[a.status] ?? 5) - (statusOrder[b.status] ?? 5);
      }
      return 0;
    });
  });

  const editorUrl = (id: string) => `#/extensions/${encodeURIComponent(id)}/editor`;
  const settingsUrl = (id: string) => `#/extensions/${encodeURIComponent(id)}/settings`;

  async function load(reload = false) {
    loading = true;
    error = "";
    try {
      items = reload ? await reloadExtensions() : await listExtensions();
      if (reload) toast.success($translate("extensions.reloaded"));
      return true;
    } catch (cause) {
      error = String(cause);
      return false;
    } finally {
      loading = false;
    }
  }

  async function refresh() {
    try {
      items = await listExtensions();
      return true;
    } catch {
      return false;
    }
  }

  async function toggle(item: ExtensionDefinition) {
    busy = true;
    error = "";
    try {
      const current = await getExtension(item.manifest.id);
      if (current.etag !== item.etag) {
        await load();
        throw new Error($translate("extensions.workspace.changed"));
      }
      current.manifest.enabled = !item.manifest.enabled;
      const saved = await saveExtension(current);
      items = items.map((entry) => (entry.manifest.id === saved.manifest.id ? saved : entry));
      toast.success($translate("extensions.saved"));
    } catch (cause) {
      error = String(cause);
    } finally {
      busy = false;
    }
  }

  async function executeBatchAction() {
    batchOpen = false;
    busy = true;
    error = "";
    const targetState = batchActionType === "enable";
    try {
      for (const item of items) {
        if (item.manifest.enabled !== targetState && item.etag) {
          try {
            const current = await getExtension(item.manifest.id);
            current.manifest.enabled = targetState;
            await saveExtension(current);
          } catch {
            // continue with others
          }
        }
      }
      await refresh();
      toast.success(
        targetState
          ? $translate("extensions.workspace.batchEnable")
          : $translate("extensions.workspace.batchDisable"),
      );
    } catch (cause) {
      error = String(cause);
    } finally {
      busy = false;
    }
  }

  async function remove() {
    if (!deleteTarget || busy) return;
    const target = deleteTarget;
    deleteOpen = false;
    busy = true;
    error = "";
    try {
      await deleteExtension(target.manifest.id, target.etag);
      items = items.filter((entry) => entry.manifest.id !== target.manifest.id);
      toast.success($translate("extensions.deleted"));
    } catch (cause) {
      error = String(cause);
    } finally {
      busy = false;
    }
  }

  async function duplicate() {
    if (!duplicateTarget || !duplicateId.trim() || busy) return;
    busy = true;
    error = "";
    try {
      const result = await duplicateExtension(duplicateTarget.manifest.id, duplicateId.trim());
      duplicateOpen = false;
      await push(`/extensions/${encodeURIComponent(result.manifest.id)}/editor`);
    } catch (cause) {
      error = String(cause);
    } finally {
      busy = false;
    }
  }

  async function exportItem(item: ExtensionDefinition) {
    try {
      const blob = await exportExtension(item.manifest.id);
      const url = URL.createObjectURL(blob);
      const anchor = document.createElement("a");
      anchor.href = url;
      anchor.download = `${item.manifest.id}.zip`;
      anchor.click();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
      toast.success($translate("extensions.import.exported", { id: item.manifest.id }));
    } catch (cause) {
      error = String(cause);
    }
  }

  async function openStore() {
    busy = true;
    error = "";
    try {
      presets = await listPresets();
      storeOpen = true;
    } catch (cause) {
      error = String(cause);
    } finally {
      busy = false;
    }
  }

  async function copyId(id: string) {
    try {
      await navigator.clipboard.writeText(id);
      copiedId = id;
      setTimeout(() => (copiedId = null), 2000);
    } catch {
      // ignore
    }
  }

  // ===== Quick Settings Modal =====
  let settingsOpen = $state(false);
  let settingsLoading = $state(false);
  let settingsSaving = $state(false);
  let settingsDirty = $state(false);
  let settingsError = $state("");
  let settingsErrors = $state<Record<string, string>>({});
  let settingsDraft = $state<ExtensionDefinition | null>(null);
  let settingsSnapshot = $state("");

  async function openQuickSettings(item: ExtensionDefinition) {
    settingsOpen = true;
    settingsLoading = true;
    settingsDirty = false;
    settingsError = "";
    settingsErrors = {};
    settingsDraft = null;
    try {
      const fetched = await getExtension(item.manifest.id);
      settingsDraft = fetched;
      settingsSnapshot = JSON.stringify(fetched.manifest);
    } catch (cause) {
      settingsError = String(cause);
    } finally {
      settingsLoading = false;
    }
  }

  async function saveQuickSettings() {
    if (!settingsDraft || settingsSaving) return;
    settingsSaving = true;
    settingsError = "";
    settingsErrors = {};
    try {
      const saved = await saveExtension($state.snapshot(settingsDraft) as ExtensionDefinition);
      items = items.map((entry) => (entry.manifest.id === saved.manifest.id ? saved : entry));
      settingsOpen = false;
      toast.success($translate("extensions.saved"));
    } catch (cause) {
      settingsError = String(cause);
      settingsErrors = settingDiagnosticsFrom(cause);
    } finally {
      settingsSaving = false;
    }
  }

  onMount(() => {
    void load();
  });
</script>

<svelte:head>
  <title>{$translate("extensions.workspace.manager")} · llama-swap</title>
</svelte:head>

<section class="space-y-6">
  <!-- Page Header -->
  <header class="flex flex-wrap items-center justify-between gap-4">
    <div class="flex items-center gap-3">
      <div class="flex size-10 shrink-0 items-center justify-center rounded-xl bg-primary/10 text-primary">
        <Puzzle class="size-6" />
      </div>
      <div>
        <h1 class="!pb-0 text-xl font-bold tracking-tight">{$translate("extensions.workspace.manager")}</h1>
        <p class="mt-0.5 text-xs text-muted-foreground">{$translate("extensions.workspace.subtitle")}</p>
      </div>
    </div>

    <div class="flex flex-wrap items-center gap-2">
      <Button
        variant="outline"
        size="icon"
        aria-label={$translate("extensions.reload")}
        disabled={loading || busy}
        onclick={() => load(true)}
      >
        <RefreshCw class={`size-4 ${loading ? "animate-spin" : ""}`} />
      </Button>

      <Button variant="outline" disabled={busy} onclick={openStore}>
        <SlidersHorizontal class="size-4 mr-1.5 text-primary" />
        {$translate("extensions.workspace.presetsMarketplace")}
        {#if presets.length > 0}
          <Badge variant="secondary" class="ml-1 px-1.5 py-0 text-[10px]">{presets.length}</Badge>
        {/if}
      </Button>

      <Button variant="outline" disabled={busy} onclick={() => (importOpen = true)}>
        <PackagePlus class="size-4 mr-1.5" />
        {$translate("extensions.import.title")}
      </Button>

      <Button variant="ghost" href="#/extensions/docs" class="gap-1.5 text-xs">
        <BookOpen class="size-4" />
        {$translate("extensions.workspace.viewDocs")}
      </Button>

      <Button href="#/extensions/new">
        <Plus class="size-4 mr-1" />
        {$translate("extensions.workspace.create")}
      </Button>
    </div>
  </header>

  <!-- Error Banner -->
  {#if error}
    <div role="alert" class="flex items-center gap-2 rounded-lg border border-destructive/40 bg-destructive/10 p-3 text-xs text-destructive">
      <AlertTriangle class="size-4 shrink-0" />
      <span class="flex-1 break-words">{error}</span>
      <Button variant="ghost" size="icon-xs" onclick={() => (error = "")}>
        <X class="size-3.5" />
      </Button>
    </div>
  {/if}

  <!-- KPI Overview Stat Cards -->
  <div class="grid grid-cols-2 gap-3 sm:grid-cols-4">
    <!-- Card 1: Total -->
    <button
      type="button"
      class={`flex items-center justify-between rounded-xl border p-4 text-left transition-all ${
        filter === "all"
          ? "border-primary bg-primary/5 shadow-xs"
          : "border-border bg-card/60 hover:bg-accent/40"
      }`}
      onclick={() => (filter = "all")}
    >
      <div class="space-y-1">
        <span class="text-xs text-muted-foreground">{$translate("extensions.workspace.totalKpi")}</span>
        <div class="text-2xl font-bold tracking-tight">{items.length}</div>
      </div>
      <div class="flex size-9 items-center justify-center rounded-lg bg-primary/10 text-primary">
        <Puzzle class="size-5" />
      </div>
    </button>

    <!-- Card 2: Enabled -->
    <button
      type="button"
      class={`flex items-center justify-between rounded-xl border p-4 text-left transition-all ${
        filter === "enabled"
          ? "border-success bg-success/5 shadow-xs"
          : "border-border bg-card/60 hover:bg-accent/40"
      }`}
      onclick={() => (filter = "enabled")}
    >
      <div class="space-y-1">
        <span class="text-xs text-muted-foreground">{$translate("extensions.workspace.enabledKpi")}</span>
        <div class="text-2xl font-bold tracking-tight text-success">{enabledCount}</div>
      </div>
      <div class="flex size-9 items-center justify-center rounded-lg bg-success/10 text-success">
        <CheckCircle2 class="size-5" />
      </div>
    </button>

    <!-- Card 3: Disabled -->
    <button
      type="button"
      class={`flex items-center justify-between rounded-xl border p-4 text-left transition-all ${
        filter === "disabled"
          ? "border-muted-foreground bg-muted/30 shadow-xs"
          : "border-border bg-card/60 hover:bg-accent/40"
      }`}
      onclick={() => (filter = "disabled")}
    >
      <div class="space-y-1">
        <span class="text-xs text-muted-foreground">{$translate("extensions.workspace.disabledKpi")}</span>
        <div class="text-2xl font-bold tracking-tight text-muted-foreground">{disabledCount}</div>
      </div>
      <div class="flex size-9 items-center justify-center rounded-lg bg-muted text-muted-foreground">
        <PowerOff class="size-5" />
      </div>
    </button>

    <!-- Card 4: Issues -->
    <button
      type="button"
      class={`flex items-center justify-between rounded-xl border p-4 text-left transition-all ${
        filter === "issues"
          ? "border-destructive bg-destructive/5 shadow-xs"
          : "border-border bg-card/60 hover:bg-accent/40"
      }`}
      onclick={() => (filter = "issues")}
    >
      <div class="space-y-1">
        <span class="text-xs text-muted-foreground">{$translate("extensions.workspace.issuesKpi")}</span>
        <div class={`text-2xl font-bold tracking-tight ${issueCount > 0 ? "text-destructive" : "text-muted-foreground"}`}>
          {issueCount}
        </div>
      </div>
      <div
        class={`flex size-9 items-center justify-center rounded-lg ${
          issueCount > 0 ? "bg-destructive/10 text-destructive" : "bg-muted text-muted-foreground"
        }`}
      >
        <AlertTriangle class="size-5" />
      </div>
    </button>
  </div>

  <!-- Search & Toolbar -->
  <div class="flex flex-wrap items-center justify-between gap-3">
    <!-- Search Bar -->
    <div class="flex min-w-0 flex-1 items-center gap-2 rounded-lg border border-border bg-background px-3 py-1.5 sm:max-w-md shadow-2xs">
      <Search class="size-4 shrink-0 text-muted-foreground" />
      <input
        class="min-w-0 flex-1 bg-transparent text-sm outline-none placeholder:text-muted-foreground"
        bind:value={search}
        aria-label={$translate("extensions.search")}
        placeholder={$translate("extensions.workspace.search")}
      />
      {#if search}
        <button
          type="button"
          class="text-muted-foreground hover:text-foreground"
          onclick={() => (search = "")}
        >
          <X class="size-3.5" />
        </button>
      {/if}
    </div>

    <!-- Right Controls: Sort & View Mode & Batch -->
    <div class="flex flex-wrap items-center gap-2">
      <!-- Sort Dropdown -->
      <DropdownMenu.Root>
        <DropdownMenu.Trigger class="flex items-center gap-1.5 rounded-lg border border-border bg-background px-3 py-1.5 text-xs font-medium hover:bg-muted">
          <ArrowUpDown class="size-3.5 text-muted-foreground" />
          <span>{$translate("extensions.workspace.sortBy")}: {$translate(`extensions.workspace.sortBy${sortBy.charAt(0).toUpperCase() + sortBy.slice(1)}`)}</span>
        </DropdownMenu.Trigger>
        <DropdownMenu.Content align="end">
          <DropdownMenu.Item onclick={() => (sortBy = "name")}>
            {$translate("extensions.workspace.sortByName")}
          </DropdownMenu.Item>
          <DropdownMenu.Item onclick={() => (sortBy = "priority")}>
            {$translate("extensions.workspace.sortByPriority")}
          </DropdownMenu.Item>
          <DropdownMenu.Item onclick={() => (sortBy = "status")}>
            {$translate("extensions.workspace.sortByStatus")}
          </DropdownMenu.Item>
        </DropdownMenu.Content>
      </DropdownMenu.Root>

      <!-- View Switcher -->
      <div class="flex items-center rounded-lg border border-border bg-muted/40 p-0.5">
        <button
          type="button"
          class={`flex size-7 items-center justify-center rounded-md text-xs transition-colors ${
            viewMode === "grid" ? "bg-background text-foreground shadow-xs" : "text-muted-foreground hover:text-foreground"
          }`}
          onclick={() => (viewMode = "grid")}
          title={$translate("extensions.workspace.grid")}
        >
          <LayoutGrid class="size-3.5" />
        </button>
        <button
          type="button"
          class={`flex size-7 items-center justify-center rounded-md text-xs transition-colors ${
            viewMode === "table" ? "bg-background text-foreground shadow-xs" : "text-muted-foreground hover:text-foreground"
          }`}
          onclick={() => (viewMode = "table")}
          title={$translate("extensions.workspace.table")}
        >
          <List class="size-3.5" />
        </button>
      </div>

      <!-- Batch Dropdown -->
      <DropdownMenu.Root>
        <DropdownMenu.Trigger class="flex items-center gap-1 rounded-lg border border-border bg-background px-2.5 py-1.5 text-xs text-muted-foreground hover:bg-muted hover:text-foreground">
          <span>{$translate("extensions.workspace.batchEnable")} / {$translate("extensions.workspace.batchDisable")}</span>
        </DropdownMenu.Trigger>
        <DropdownMenu.Content align="end">
          <DropdownMenu.Item
            disabled={busy || items.length === 0}
            onclick={() => {
              batchActionType = "enable";
              batchOpen = true;
            }}
          >
            <CheckCircle2 class="size-3.5 mr-2 text-success" />
            {$translate("extensions.workspace.batchEnable")}
          </DropdownMenu.Item>
          <DropdownMenu.Item
            disabled={busy || items.length === 0}
            onclick={() => {
              batchActionType = "disable";
              batchOpen = true;
            }}
          >
            <PowerOff class="size-3.5 mr-2 text-muted-foreground" />
            {$translate("extensions.workspace.batchDisable")}
          </DropdownMenu.Item>
        </DropdownMenu.Content>
      </DropdownMenu.Root>
    </div>
  </div>

  <!-- Content List / Grid -->
  {#if loading}
    <div class="flex h-64 flex-col items-center justify-center gap-3 text-muted-foreground">
      <RefreshCw class="size-6 animate-spin text-primary" />
      <span class="text-sm">{$translate("common.loading")}</span>
    </div>
  {:else if filteredAndSorted.length === 0}
    <!-- Empty State -->
    <div class="flex flex-col items-center justify-center gap-4 rounded-xl border border-dashed border-border bg-card/40 px-6 py-16 text-center">
      <div class="flex size-14 items-center justify-center rounded-2xl bg-primary/10 text-primary">
        <Puzzle class="size-7" />
      </div>
      <div class="max-w-md space-y-1">
        <h2 class="font-semibold text-base">
          {$translate(items.length ? "extensions.workspace.noMatches" : "extensions.empty")}
        </h2>
        <p class="text-xs text-muted-foreground">
          {$translate(items.length ? "extensions.workspace.filterHint" : "extensions.workspace.emptyHint")}
        </p>
      </div>

      <div class="flex flex-wrap items-center justify-center gap-2 pt-2">
        {#if items.length}
          <Button
            variant="outline"
            size="sm"
            onclick={() => {
              search = "";
              filter = "all";
            }}
          >
            {$translate("extensions.workspace.clear")}
          </Button>
        {:else}
          <Button variant="outline" size="sm" onclick={openStore}>
            {$translate("extensions.workspace.fromPreset")}
          </Button>
          <Button size="sm" href="#/extensions/new">
            <Plus class="size-4 mr-1" />
            {$translate("extensions.workspace.create")}
          </Button>
        {/if}
      </div>
    </div>
  {:else if viewMode === "grid"}
    <!-- Modern Grid View -->
    <ul class="grid gap-4 sm:grid-cols-2 xl:grid-cols-3">
      {#each filteredAndSorted as item (item.manifest.id)}
        {@const m = item.manifest}
        <li class="group flex flex-col justify-between rounded-xl border border-border bg-card p-4 transition-all hover:border-primary/50 hover:shadow-sm">
          <!-- Card Top: Avatar, Name/ID, Status & Priority -->
          <div class="space-y-3">
            <div class="flex items-start gap-3">
              <span class="flex size-10 shrink-0 items-center justify-center rounded-xl bg-primary/10 text-primary">
                <FileCode2 class="size-5" />
              </span>

              <div class="min-w-0 flex-1">
                <div class="flex items-center gap-1.5">
                  <a
                    href={editorUrl(m.id)}
                    class="truncate font-semibold text-sm hover:text-primary transition-colors"
                  >
                    {m.name || m.id}
                  </a>
                </div>

                <div class="mt-0.5 flex items-center gap-1.5 font-mono text-[11px] text-muted-foreground">
                  <span class="truncate">{m.id}</span>
                  <button
                    type="button"
                    class="hover:text-foreground text-muted-foreground transition-colors"
                    onclick={() => copyId(m.id)}
                    title={$translate("extensions.workspace.copyId")}
                  >
                    {#if copiedId === m.id}
                      <Check class="size-3 text-success" />
                    {:else}
                      <Copy class="size-3" />
                    {/if}
                  </button>
                </div>
              </div>

              <!-- Status Badge -->
              <span
                class={`inline-flex shrink-0 items-center gap-1 rounded-full px-2 py-0.5 text-[11px] font-medium ${
                  item.status === "ready"
                    ? "bg-success/15 text-success"
                    : ["error", "invalid"].includes(item.status)
                      ? "bg-destructive/15 text-destructive"
                      : item.status === "stale"
                        ? "bg-warning/15 text-warning"
                        : "bg-muted text-muted-foreground"
                }`}
              >
                <span
                  class={`size-1.5 rounded-full ${
                    item.status === "ready"
                      ? "bg-success animate-pulse"
                      : ["error", "invalid"].includes(item.status)
                        ? "bg-destructive"
                        : item.status === "stale"
                          ? "bg-warning"
                          : "bg-muted-foreground"
                  }`}
                ></span>
                {$translate(item.status === "ready" ? "extensions.workspace.enabled" : `extensions.status.${item.status}`)}
              </span>
            </div>

            <!-- Description -->
            {#if m.description}
              <p class="line-clamp-2 text-xs leading-relaxed text-muted-foreground min-h-8">
                {m.description}
              </p>
            {:else}
              <div class="min-h-8 text-xs text-muted-foreground/60 italic">无描述信息</div>
            {/if}

            <!-- Tags & Meta -->
            <div class="flex flex-wrap items-center gap-1.5 pt-1">
              <Badge variant="outline" class="font-mono text-[10px] px-1.5 py-0 border-border/80">
                JS
              </Badge>

              <Badge variant="outline" class="font-mono text-[10px] px-1.5 py-0 border-border/80">
                P{m.priority ?? 100}
              </Badge>

              <!-- Models Scope -->
              {#if m.match?.models?.length}
                <Badge variant="secondary" class="text-[10px] px-2 py-0 truncate max-w-28">
                  {m.match.models[0]}
                </Badge>
                {#if m.match.models.length > 1}
                  <Badge variant="outline" class="text-[10px] px-1.5 py-0" title={m.match.models.join(", ")}>
                    +{m.match.models.length - 1}
                  </Badge>
                {/if}
              {:else}
                <Badge variant="outline" class="border-success/30 bg-success/10 text-success text-[10px] px-1.5 py-0">
                  {$translate("extensions.workspace.allModels")}
                </Badge>
              {/if}

              <!-- Endpoints -->
              {#if m.match?.endpoints?.length}
                <Badge variant="outline" class="font-mono text-[10px] px-1.5 py-0 text-muted-foreground">
                  {m.match.endpoints[0].replace("/v1/", "")}
                  {#if m.match.endpoints.length > 1}
                    +{m.match.endpoints.length - 1}
                  {/if}
                </Badge>
              {/if}

              <!-- Permissions indicator icons -->
              {#if (m.permissions?.networkHosts ?? []).length > 0}
                <span title="网络访问已配置" class="flex size-5 items-center justify-center rounded bg-primary/10 text-primary">
                  <Network class="size-3" />
                </span>
              {/if}
              {#if m.permissions?.storage && m.permissions.storage !== "none"}
                <span title={`存储: ${m.permissions.storage}`} class="flex size-5 items-center justify-center rounded bg-muted text-muted-foreground">
                  <HardDrive class="size-3" />
                </span>
              {/if}
            </div>

            <!-- Error alert if any -->
            {#if item.lastError}
              <p class="break-words rounded-md bg-destructive/10 p-2 text-xs text-destructive">
                {item.lastError}
              </p>
            {/if}
          </div>

          <!-- Card Bottom Action Bar -->
          <div class="mt-4 flex items-center justify-between border-t border-border pt-3">
            <div class="flex items-center gap-2">
              <Switch.Root
                checked={m.enabled}
                disabled={busy || !item.etag}
                aria-label={`${$translate("extensions.enabled")} ${m.name || m.id}`}
                onCheckedChange={() => void toggle(item)}
              />
              <span class="text-xs text-muted-foreground">
                {$translate(m.enabled ? "extensions.workspace.enabled" : "extensions.workspace.disabled")}
              </span>
            </div>

            <div class="flex items-center gap-1.5">
              <!-- Settings Button -->
              <Button
                variant="outline"
                size="sm"
                class="h-8 gap-1 px-2.5 text-xs"
                disabled={busy || !item.etag}
                href={settingsUrl(m.id)}
              >
                <Settings2 class="size-3.5" />
                <span>{$translate("extensions.workspace.quickSettings")}</span>
              </Button>

              <!-- Edit Code Button -->
              <Button
                variant="default"
                size="sm"
                class="h-8 gap-1 px-2.5 text-xs"
                href={editorUrl(m.id)}
              >
                <FileCode2 class="size-3.5" />
                <span>{$translate("extensions.workspace.edit")}</span>
              </Button>

              <!-- More Dropdown -->
              <DropdownMenu.Root>
                <DropdownMenu.Trigger
                  class="flex size-8 items-center justify-center rounded-md border border-border bg-background hover:bg-muted"
                  aria-label={`${$translate("extensions.workspace.actions")} ${m.id}`}
                >
                  <MoreHorizontal class="size-4" />
                </DropdownMenu.Trigger>
                <DropdownMenu.Content align="end">
                  <DropdownMenu.Item onclick={() => void push(editorUrl(m.id))}>
                    <Play class="size-4 mr-2" />
                    {$translate("extensions.test")}
                  </DropdownMenu.Item>

                  <DropdownMenu.Item onclick={() => openQuickSettings(item)}>
                    <Settings2 class="size-4 mr-2" />
                    {$translate("extensions.workspace.quickSettings")} (模态)
                  </DropdownMenu.Item>

                  <DropdownMenu.Item
                    onclick={() => {
                      logViewTarget = item;
                      logOpen = true;
                    }}
                  >
                    <ScrollText class="size-4 mr-2" />
                    {$translate("extensions.logs.title")}
                  </DropdownMenu.Item>

                  <DropdownMenu.Item disabled={busy || !item.etag} onclick={() => void exportItem(item)}>
                    <Download class="size-4 mr-2" />
                    {$translate("extensions.import.export")}
                  </DropdownMenu.Item>

                  <DropdownMenu.Separator />

                  <DropdownMenu.Item
                    disabled={busy || !item.etag}
                    onclick={() => {
                      duplicateTarget = item;
                      duplicateId = `${m.id}-copy`;
                      duplicateOpen = true;
                    }}
                  >
                    <Copy class="size-4 mr-2" />
                    {$translate("extensions.duplicate")}
                  </DropdownMenu.Item>

                  <DropdownMenu.Separator />

                  <DropdownMenu.Item
                    class="text-destructive"
                    disabled={busy || !item.etag}
                    onclick={() => {
                      deleteTarget = item;
                      deleteOpen = true;
                    }}
                  >
                    <Trash2 class="size-4 mr-2" />
                    {$translate("extensions.delete")}
                  </DropdownMenu.Item>
                </DropdownMenu.Content>
              </DropdownMenu.Root>
            </div>
          </div>
        </li>
      {/each}
    </ul>
  {:else}
    <!-- Modern Table View -->
    <div class="rounded-xl border border-border bg-card overflow-hidden">
      <Table.Root>
        <Table.Header>
          <Table.Row class="bg-muted/30">
            <Table.Head class="w-16">{$translate("extensions.enabled")}</Table.Head>
            <Table.Head class="min-w-44">{$translate("extensions.name")} & {$translate("extensions.id")}</Table.Head>
            <Table.Head class="w-28">{$translate("extensions.workspace.status")}</Table.Head>
            <Table.Head class="min-w-40">{$translate("extensions.workspace.models")}</Table.Head>
            <Table.Head class="w-20">{$translate("extensions.priority")}</Table.Head>
            <Table.Head class="text-right">{$translate("extensions.workspace.actions")}</Table.Head>
          </Table.Row>
        </Table.Header>

        <Table.Body>
          {#each filteredAndSorted as item (item.manifest.id)}
            {@const m = item.manifest}
            <Table.Row class="hover:bg-muted/20">
              <!-- Switch -->
              <Table.Cell>
                <Switch.Root
                  checked={m.enabled}
                  disabled={busy || !item.etag}
                  aria-label={`${$translate("extensions.enabled")} ${m.id}`}
                  onCheckedChange={() => void toggle(item)}
                />
              </Table.Cell>

              <!-- Name & ID -->
              <Table.Cell>
                <div class="space-y-0.5">
                  <a
                    href={editorUrl(m.id)}
                    class="font-semibold text-sm hover:text-primary transition-colors block"
                  >
                    {m.name || m.id}
                  </a>
                  <div class="flex items-center gap-1 font-mono text-[11px] text-muted-foreground">
                    <span>{m.id}</span>
                    <button
                      type="button"
                      class="hover:text-foreground text-muted-foreground"
                      onclick={() => copyId(m.id)}
                    >
                      {#if copiedId === m.id}
                        <Check class="size-3 text-success" />
                      {:else}
                        <Copy class="size-3" />
                      {/if}
                    </button>
                  </div>
                </div>
              </Table.Cell>

              <!-- Status -->
              <Table.Cell>
                <span
                  class={`inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-xs font-medium ${
                    item.status === "ready"
                      ? "bg-success/15 text-success"
                      : ["error", "invalid"].includes(item.status)
                        ? "bg-destructive/15 text-destructive"
                        : "bg-muted text-muted-foreground"
                  }`}
                >
                  <span
                    class={`size-1.5 rounded-full ${
                      item.status === "ready" ? "bg-success" : "bg-muted-foreground"
                    }`}
                  ></span>
                  {$translate(item.status === "ready" ? "extensions.workspace.enabled" : `extensions.status.${item.status}`)}
                </span>
              </Table.Cell>

              <!-- Models & Endpoints -->
              <Table.Cell>
                <div class="flex flex-wrap gap-1">
                  {#if m.match?.models?.length}
                    {#each m.match.models.slice(0, 2) as model}
                      <Badge variant="secondary" class="text-[10px]">{model}</Badge>
                    {/each}
                    {#if m.match.models.length > 2}
                      <Badge variant="outline" class="text-[10px]">+{m.match.models.length - 2}</Badge>
                    {/if}
                  {:else}
                    <Badge variant="outline" class="border-success/30 bg-success/10 text-success text-[10px]">
                      {$translate("extensions.workspace.allModels")}
                    </Badge>
                  {/if}
                </div>
              </Table.Cell>

              <!-- Priority -->
              <Table.Cell class="font-mono text-xs">
                P{m.priority ?? 100}
              </Table.Cell>

              <!-- Actions -->
              <Table.Cell class="text-right">
                <div class="flex items-center justify-end gap-1.5">
                  <Button variant="outline" size="sm" class="h-7 text-xs" href={settingsUrl(m.id)}>
                    <Settings2 class="size-3 mr-1" />
                    {$translate("extensions.workspace.quickSettings")}
                  </Button>

                  <Button size="sm" class="h-7 text-xs" href={editorUrl(m.id)}>
                    <FileCode2 class="size-3 mr-1" />
                    {$translate("extensions.workspace.edit")}
                  </Button>

                  <DropdownMenu.Root>
                    <DropdownMenu.Trigger class="flex size-7 items-center justify-center rounded-md border border-border bg-background hover:bg-muted">
                      <MoreHorizontal class="size-3.5" />
                    </DropdownMenu.Trigger>
                    <DropdownMenu.Content align="end">
                      <DropdownMenu.Item onclick={() => openQuickSettings(item)}>
                        <Settings2 class="size-4 mr-2" />
                        {$translate("extensions.workspace.quickSettings")} (模态)
                      </DropdownMenu.Item>
                      <DropdownMenu.Item
                        onclick={() => {
                          logViewTarget = item;
                          logOpen = true;
                        }}
                      >
                        <ScrollText class="size-4 mr-2" />
                        {$translate("extensions.logs.title")}
                      </DropdownMenu.Item>
                      <DropdownMenu.Item disabled={busy || !item.etag} onclick={() => void exportItem(item)}>
                        <Download class="size-4 mr-2" />
                        {$translate("extensions.import.export")}
                      </DropdownMenu.Item>
                      <DropdownMenu.Separator />
                      <DropdownMenu.Item
                        class="text-destructive"
                        disabled={busy || !item.etag}
                        onclick={() => {
                          deleteTarget = item;
                          deleteOpen = true;
                        }}
                      >
                        <Trash2 class="size-4 mr-2" />
                        {$translate("extensions.delete")}
                      </DropdownMenu.Item>
                    </DropdownMenu.Content>
                  </DropdownMenu.Root>
                </div>
              </Table.Cell>
            </Table.Row>
          {/each}
        </Table.Body>
      </Table.Root>
    </div>
  {/if}

  <!-- Footer count -->
  <div class="flex items-center justify-between text-xs text-muted-foreground pt-2">
    <span>{$translate("extensions.workspace.count", { count: filteredAndSorted.length })}</span>
  </div>
</section>

<!-- Dialogs -->
<ConfirmDialog
  bind:open={deleteOpen}
  title={$translate("extensions.delete")}
  message={$translate("extensions.deleteConfirm", { id: deleteTarget?.manifest.id ?? "" })}
  confirmLabel={$translate("extensions.delete")}
  onConfirm={() => void remove()}
/>

<ConfirmDialog
  bind:open={batchOpen}
  title={batchActionType === "enable" ? $translate("extensions.workspace.batchEnable") : $translate("extensions.workspace.batchDisable")}
  message={batchActionType === "enable"
    ? `确认启用全部 ${items.length} 个拓展？`
    : `确认停用全部 ${items.length} 个拓展？`}
  confirmLabel={batchActionType === "enable" ? $translate("extensions.workspace.batchEnable") : $translate("extensions.workspace.batchDisable")}
  onConfirm={() => void executeBatchAction()}
/>

<Dialog.Root bind:open={duplicateOpen}>
  <Dialog.Content>
    <Dialog.Header>
      <Dialog.Title>{$translate("extensions.duplicate")}</Dialog.Title>
      <Dialog.Description>{$translate("extensions.duplicateId")}</Dialog.Description>
    </Dialog.Header>
    <input
      class="rounded-md border border-border bg-background p-2 text-sm"
      bind:value={duplicateId}
      aria-label={$translate("extensions.duplicateId")}
    />
    {#if error}
      <p role="alert" class="text-sm text-destructive">{error}</p>
    {/if}
    <Dialog.Footer>
      <Button variant="outline" onclick={() => (duplicateOpen = false)}>
        {$translate("common.cancel")}
      </Button>
      <Button disabled={busy || !duplicateId.trim()} onclick={duplicate}>
        {$translate("extensions.duplicate")}
      </Button>
    </Dialog.Footer>
  </Dialog.Content>
</Dialog.Root>

<ExtensionImportDialog bind:open={importOpen} onImported={() => void refresh()} />

<Dialog.Root bind:open={logOpen}>
  <Dialog.Content class="flex h-[80vh] max-h-[80vh] flex-col sm:max-w-3xl">
    <Dialog.Header>
      <Dialog.Title>{$translate("extensions.logs.title")} · {logViewTarget?.manifest.name || logViewTarget?.manifest.id}</Dialog.Title>
      <Dialog.Description>{$translate("extensions.logs.subtitle")}</Dialog.Description>
    </Dialog.Header>
    <div class="min-h-0 flex-1 overflow-hidden rounded-md border border-border">
      {#if logViewTarget}
        <ExtensionConsolePanel id={logViewTarget.manifest.id} />
      {/if}
    </div>
  </Dialog.Content>
</Dialog.Root>

<Dialog.Root bind:open={storeOpen}>
  <Dialog.Content class="max-h-[85vh] overflow-y-auto sm:max-w-4xl">
    <Dialog.Header class="sr-only">
      <Dialog.Title>{$translate("extensions.presets.title")}</Dialog.Title>
      <Dialog.Description>{$translate("extensions.presets.subtitle")}</Dialog.Description>
    </Dialog.Header>
    <ExtensionPresetStore
      {presets}
      onInstalled={(id) => {
        storeOpen = false;
        void push(editorUrl(id));
      }}
    />
  </Dialog.Content>
</Dialog.Root>

<!-- Quick Settings Modal Dialog -->
<Dialog.Root bind:open={settingsOpen}>
  <Dialog.Content class="max-h-[85vh] overflow-y-auto sm:max-w-2xl p-0">
    <div class="p-6 border-b border-border">
      <Dialog.Header>
        <Dialog.Title>{$translate("extensions.workspace.quickSettings")} · {settingsDraft?.manifest.name || settingsDraft?.manifest.id || "…"}</Dialog.Title>
        <Dialog.Description>{$translate("extensions.settings.modalHint")}</Dialog.Description>
      </Dialog.Header>
    </div>

    <div class="p-6 pt-2">
      {#if settingsLoading}
        <p role="status" class="p-8 text-center text-sm text-muted-foreground">{$translate("common.loading")}</p>
      {:else if settingsDraft}
        <ExtensionConfiguration
          bind:draft={settingsDraft}
          errors={settingsErrors}
          layout="tabs"
        />
      {:else if settingsError}
        <p role="alert" class="rounded-md bg-destructive/10 p-3 text-sm text-destructive">{settingsError}</p>
      {/if}
    </div>

    <div class="p-4 border-t border-border flex items-center justify-between bg-muted/20">
      {#if settingsDraft}
        <Button variant="ghost" size="sm" href={settingsUrl(settingsDraft.manifest.id)}>
          <ExternalLink class="size-3.5 mr-1" />
          全屏设置页
        </Button>
      {:else}
        <div></div>
      {/if}

      <div class="flex items-center gap-2">
        <Button variant="outline" disabled={settingsSaving} onclick={() => (settingsOpen = false)}>
          {$translate("common.cancel")}
        </Button>
        <Button disabled={settingsSaving || !settingsDraft} onclick={() => void saveQuickSettings()}>
          {$translate("extensions.save")}
        </Button>
      </div>
    </div>
  </Dialog.Content>
</Dialog.Root>
