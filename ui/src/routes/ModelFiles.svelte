<script lang="ts">
  import { onMount } from "svelte";
  import { AlertTriangle, FileBox, LoaderCircle, RefreshCw, Search } from "@lucide/svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import * as Select from "$lib/components/ui/select/index.js";
  import { deleteModelFile, getModelFiles, ModelFileRequestError } from "$lib/modelFiles";
  import { translate } from "$lib/i18n";
  import { toast } from "$lib/toast";
  import type { ModelDownload, ModelFile, ModelFilesResponse } from "$lib/types";
  import ModelDownloadPanel from "../components/model-files/ModelDownloadPanel.svelte";
  import ModelFileTable from "../components/model-files/ModelFileTable.svelte";

  let catalog = $state<ModelFilesResponse | null>(null);
  let selectedSource = $state("all");
  let query = $state("");
  let loading = $state(true);
  let refreshing = $state(false);
  let error = $state("");
  let copiedId = $state("");
  let deletingId = $state("");
  let deleteDialogOpen = $state(false);
  let deleteTarget = $state<ModelFile | null>(null);
  let deleteError = $state("");
  let downloadTasks = $state<ModelDownload[]>([]);
  let requestSerial = 0;

  let availableSources = $derived((catalog?.sources ?? []).filter((source) => source.available));
  let sourceNames = $derived.by(() => Object.fromEntries((catalog?.sources ?? []).map((source) => [source.id, source.configured
    ? source.name
    : source.type === "hf_cache"
      ? $translate("modelFiles.hfCache")
      : source.type === "modelscope_cache"
        ? $translate("modelFiles.modelScopeCache")
        : source.name])));

  let visibleFiles = $derived.by(() => {
    const term = query.trim().toLowerCase();
    const availableIDs = new Set(availableSources.map((source) => source.id));
    return (catalog?.data ?? []).filter((file) => {
      if (!availableIDs.has(file.source_id)) return false;
      if (selectedSource !== "all" && file.source_id !== selectedSource) return false;
      if (!term) return true;
      return [file.name, file.path, file.relative_path, file.repository, file.revision]
        .filter(Boolean)
        .some((value) => value!.toLowerCase().includes(term));
    });
  });

  let visibleDownloads = $derived.by(() => {
    const term = query.trim().toLowerCase();
    return downloadTasks.filter((task) => {
      if (task.status !== "queued" && task.status !== "downloading") return false;
      if (selectedSource !== "all" && task.source_id !== selectedSource) return false;
      return !term || task.repo_id.toLowerCase().includes(term) || (task.current_file ?? "").toLowerCase().includes(term);
    });
  });

  $effect(() => {
    if (selectedSource !== "all" && !availableSources.some((source) => source.id === selectedSource)) {
      selectedSource = "all";
    }
  });

  async function load(): Promise<void> {
    const serial = ++requestSerial;
    loading = catalog === null;
    refreshing = true;
    error = "";
    try {
      const next = await getModelFiles();
      if (serial === requestSerial) catalog = next;
    } catch (cause) {
      if (serial === requestSerial) {
        error = $translate("modelFiles.scanError", { message: cause instanceof Error ? cause.message : String(cause) });
      }
    } finally {
      if (serial === requestSerial) {
        loading = false;
        refreshing = false;
      }
    }
  }

  async function copyPath(file: ModelFile): Promise<void> {
    try {
      if (!navigator.clipboard) throw new Error("Clipboard API unavailable");
      await navigator.clipboard.writeText(file.path);
      copiedId = file.id;
      window.setTimeout(() => {
        if (copiedId === file.id) copiedId = "";
      }, 1600);
    } catch (cause) {
      error = $translate("modelFiles.copyFailed", { message: cause instanceof Error ? cause.message : String(cause) });
    }
  }

  function requestDelete(file: ModelFile): void {
    if ((file.registered_models?.length ?? 0) > 0 || (file.in_use_models?.length ?? 0) > 0) return;
    deleteTarget = file;
    deleteError = "";
    deleteDialogOpen = true;
  }

  function closeDeleteDialog(): void {
    if (deletingId !== "") return;
    deleteDialogOpen = false;
    deleteTarget = null;
    deleteError = "";
  }

  async function confirmDelete(): Promise<void> {
    const file = deleteTarget;
    if (!file || deletingId !== "") return;
    deletingId = file.id;
    error = "";
    deleteError = "";
    try {
      await deleteModelFile(file);
      deleteDialogOpen = false;
      deleteTarget = null;
      toast.success($translate("modelFiles.deleted"));
      await load();
    } catch (cause) {
      if (cause instanceof ModelFileRequestError && cause.status === 409) {
        const names = [...(cause.conflict?.in_use ?? []), ...(cause.conflict?.registered ?? [])];
        deleteError = $translate("modelFiles.deleteConflict", { models: names.join(", ") || cause.message });
      } else {
        deleteError = $translate("modelFiles.deleteFailed", { message: cause instanceof Error ? cause.message : String(cause) });
      }
    } finally {
      deletingId = "";
    }
  }

  onMount(() => void load());
</script>

<section class="mx-auto max-w-[1600px] space-y-4" aria-labelledby="model-files-title">
  <header class="flex flex-wrap items-center justify-between gap-3">
    <h1 id="model-files-title" class="text-lg font-semibold">{$translate("modelFiles.title")}</h1>
    <div class="flex flex-wrap items-center gap-2">
      <Button variant="outline" onclick={() => void load()} disabled={refreshing} aria-label={$translate("modelFiles.refresh")}>
        <RefreshCw class={refreshing ? "animate-spin" : ""} />
        <span>{$translate("modelFiles.refresh")}</span>
      </Button>
      <ModelDownloadPanel sources={catalog?.sources ?? []} onTasksChange={(tasks) => { downloadTasks = tasks; }} onDownloadCompleted={() => void load()} />
    </div>
  </header>

  {#if error}
    <div class="border-destructive/40 bg-destructive/10 text-destructive flex items-start gap-2 rounded-lg border p-3 text-sm" role="alert">
      <AlertTriangle class="mt-0.5 size-4 shrink-0" />
      <span>{error}</span>
    </div>
  {/if}

  <section class="space-y-4 border-t pt-6" aria-label={$translate("modelFiles.catalogTitle")}>
    {#if loading}
      <div class="overflow-hidden rounded-lg border" aria-busy="true" aria-label={$translate("modelFiles.loading")}>
        <div class="bg-muted/50 h-10 animate-pulse" aria-hidden="true"></div>
        {#each [1, 2, 3] as item}
          <div class="bg-muted/25 h-14 animate-pulse border-t" data-skeleton={item} aria-hidden="true"></div>
        {/each}
      </div>
    {:else if catalog}
      <div class="flex flex-col gap-3 rounded-lg border bg-card p-3 sm:flex-row sm:items-center">
        <div class="relative min-w-0 flex-1">
          <Search class="text-muted-foreground absolute top-1/2 left-3 size-4 -translate-y-1/2" />
          <label class="sr-only" for="model-file-search">{$translate("modelFiles.search")}</label>
          <input id="model-file-search" class="border-input bg-background placeholder:text-muted-foreground focus-visible:ring-ring/50 h-9 w-full rounded-lg border py-1 pl-9 pr-3 text-sm outline-none focus-visible:ring-3" type="search" bind:value={query} placeholder={$translate("modelFiles.search")} />
        </div>
        <Select.Root type="single" value={selectedSource} onValueChange={(value) => { if (value) selectedSource = value; }}>
          <Select.Trigger class="w-full sm:w-64" aria-label={$translate("modelFiles.source")}>
            {selectedSource === "all" ? $translate("modelFiles.allSources") : sourceNames[selectedSource] ?? selectedSource}
          </Select.Trigger>
          <Select.Content>
            <Select.Item value="all">{$translate("modelFiles.allSources")}</Select.Item>
            {#each availableSources as source (source.id)}<Select.Item value={source.id}>{sourceNames[source.id] ?? source.name}</Select.Item>{/each}
          </Select.Content>
        </Select.Root>
      </div>

      {#if catalog.truncated}
        <p class="text-warning text-sm" role="status">{$translate("modelFiles.truncated")}</p>
      {/if}
      {#if visibleFiles.length === 0 && visibleDownloads.length === 0}
        <div class="rounded-lg border border-dashed p-8 text-center" role="status">
          <FileBox class="text-muted-foreground mx-auto size-8" />
          <p class="mt-3 text-sm font-medium">{$translate("modelFiles.empty")}</p>
          <p class="text-muted-foreground mt-1 text-sm">{$translate("modelFiles.emptyDescription")}</p>
        </div>
      {:else}
        <div class="overflow-x-auto rounded-lg border bg-card">
          <ModelFileTable files={visibleFiles} downloads={visibleDownloads} {sourceNames} filterActive={query.trim() !== ""} {copiedId} {deletingId} onCopy={copyPath} onDelete={requestDelete} />
        </div>
      {/if}
    {/if}
  </section>
</section>

<Dialog.Root bind:open={deleteDialogOpen}>
  <Dialog.Content class="max-w-lg sm:max-w-lg" showCloseButton={deletingId === ""}>
    <Dialog.Header>
      <Dialog.Title class="flex items-center gap-2">
        <AlertTriangle class="text-destructive size-4" aria-hidden="true" />
        {$translate("modelFiles.deleteTitle")}
      </Dialog.Title>
      {#if deleteTarget}
        <Dialog.Description>{$translate("modelFiles.deleteConfirm", { name: deleteTarget.name })}</Dialog.Description>
      {/if}
    </Dialog.Header>

    {#if deleteTarget}
      <div class="grid gap-1 rounded-lg border bg-muted/30 p-3 text-sm">
        <span class="font-medium">{deleteTarget.name}</span>
        <code class="text-muted-foreground break-all text-xs">{deleteTarget.path}</code>
      </div>
    {/if}
    {#if deleteError}
      <div class="border-destructive/40 bg-destructive/10 text-destructive flex items-start gap-2 rounded-lg border p-3 text-sm" role="alert">
        <AlertTriangle class="mt-0.5 size-4 shrink-0" aria-hidden="true" />
        <span>{deleteError}</span>
      </div>
    {/if}

    <Dialog.Footer>
      <Button variant="outline" onclick={closeDeleteDialog} disabled={deletingId !== ""}>{$translate("common.cancel")}</Button>
      <Button variant="destructive" onclick={() => void confirmDelete()} disabled={!deleteTarget || deletingId !== ""}>
        {#if deletingId}<LoaderCircle class="size-4 animate-spin" aria-hidden="true" />{/if}
        {deletingId ? $translate("common.loading") : $translate("modelFiles.delete")}
      </Button>
    </Dialog.Footer>
  </Dialog.Content>
</Dialog.Root>
