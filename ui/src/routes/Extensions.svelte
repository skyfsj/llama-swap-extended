<script lang="ts">
  import { onMount } from "svelte";
  import { link, push } from "svelte-spa-router";
  import { Plus, Search, RefreshCw, Puzzle, MoreHorizontal, Trash2, Copy, FileCode2, Settings2, PackagePlus, Download, ScrollText } from "@lucide/svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import * as Switch from "$lib/components/ui/switch/index.js";
  import * as DropdownMenu from "$lib/components/ui/dropdown-menu/index.js";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import ConfirmDialog from "../components/ConfirmDialog.svelte";
  import ExtensionPresetStore from "../components/extensions/ExtensionPresetStore.svelte";
  import ExtensionSettingsForm from "../components/extensions/ExtensionSettingsForm.svelte";
  import ExtensionValueEditor from "../components/extensions/ExtensionValueEditor.svelte";
  import ExtensionImportDialog from "../components/extensions/ExtensionImportDialog.svelte";
  import ExtensionConsolePanel from "../components/extensions/ExtensionConsolePanel.svelte";
  import { translate } from "../lib/i18n";
  import { toast } from "../lib/toast";
  import { listExtensions, getExtension, saveExtension, deleteExtension, duplicateExtension, reloadExtensions, listPresets, exportExtension, settingDiagnosticsFrom, type ExtensionDefinition, type ExtensionPreset } from "../lib/extensionsApi";

  let items = $state<ExtensionDefinition[]>([]);
  let loading = $state(true);
  let busy = $state(false);
  let error = $state("");
  let search = $state("");
  let filter = $state("all");
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
  const enabledCount = $derived(items.filter((item) => item.manifest.enabled).length);
  const issueCount = $derived(items.filter((item) => ["error", "invalid", "stale"].includes(item.status)).length);
  const filtered = $derived(items.filter((item) => {
    const m = item.manifest;
    return (filter === "all" || (filter === "enabled" ? m.enabled : filter === "disabled" ? !m.enabled : ["error", "invalid", "stale"].includes(item.status))) &&
      [m.id, m.name, m.description, ...(m.match?.models ?? [])].join(" ").toLowerCase().includes(search.trim().toLowerCase());
  }));
  const editorUrl = (id: string) => `/extensions/${encodeURIComponent(id)}/editor`;

  async function load(reload = false) {
    loading = true; error = "";
    try { items = reload ? await reloadExtensions() : await listExtensions(); return true; }
    catch (cause) { error = String(cause); return false; }
    finally { loading = false; }
  }
  // refresh re-reads the list without the loading placeholder, so in-place
  // actions like toggling a card do not flash the whole grid.
  async function refresh() {
    try { items = await listExtensions(); return true; } catch (cause) { return false; }
  }
  async function toggle(item: ExtensionDefinition) {
    busy = true; error = "";
    try {
      const current = await getExtension(item.manifest.id);
      if (current.etag !== item.etag) { await load(); throw new Error($translate("extensions.workspace.changed")); }
      current.manifest.enabled = !item.manifest.enabled;
      const saved = await saveExtension(current);
      items = items.map((entry) => entry.manifest.id === saved.manifest.id ? saved : entry);
      if (await refresh()) toast.success($translate("extensions.saved"));
    } catch (cause) { error = String(cause); }
    finally { busy = false; }
  }
  async function remove() {
    if (!deleteTarget || busy) return;
    const target = deleteTarget;
    deleteOpen = false; busy = true; error = "";
    try { await deleteExtension(target.manifest.id, target.etag); items = items.filter((entry) => entry.manifest.id !== target.manifest.id); if (await refresh()) toast.success($translate("extensions.deleted")); }
    catch (cause) { error = String(cause); }
    finally { busy = false; }
  }
  async function duplicate() {
    if (!duplicateTarget || !duplicateId.trim() || busy) return;
    busy = true; error = "";
    try {
      const result = await duplicateExtension(duplicateTarget.manifest.id, duplicateId.trim());
      duplicateOpen = false;
      await push(editorUrl(result.manifest.id));
    } catch (cause) { error = String(cause); }
    finally { busy = false; }
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
    } catch (cause) { error = String(cause); }
  }
  async function openStore() {
    busy = true; error = "";
    try { presets = await listPresets(); storeOpen = true; }
    catch (cause) { error = String(cause); }
    finally { busy = false; }
  }

  // ===== 快捷设置模态 =====
  let settingsOpen = $state(false);
  let settingsLoading = $state(false);
  let settingsSaving = $state(false);
  let settingsDirty = $state(false);
  let settingsError = $state("");
  let settingsErrors = $state<Record<string, string>>({});
  let settingsDraft = $state<ExtensionDefinition | null>(null);
  const settingsID = $derived(settingsDraft?.manifest.id ?? "");

  async function openSettings(item: ExtensionDefinition) {
    settingsOpen = true; settingsLoading = true; settingsDirty = false;
    settingsError = ""; settingsErrors = {}; settingsDraft = null;
    try {
      settingsDraft = await getExtension(item.manifest.id);
    } catch (cause) { settingsError = String(cause); }
    finally { settingsLoading = false; }
  }
  function updateSetting(key: string, value: unknown) {
    if (!settingsDraft) return;
    settingsDraft.manifest.config = { ...settingsDraft.manifest.config, [key]: value };
    settingsDirty = true;
  }
  function updateRawConfig(value: unknown) {
    if (!settingsDraft) return;
    settingsDraft.manifest.config = (value ?? {}) as Record<string, unknown>;
    settingsDirty = true;
  }
  async function saveSettings() {
    if (!settingsDraft || settingsSaving || !settingsDirty) return;
    settingsSaving = true; settingsError = ""; settingsErrors = {};
    try {
      const saved = await saveExtension($state.snapshot(settingsDraft) as ExtensionDefinition);
      items = items.map((entry) => entry.manifest.id === saved.manifest.id ? saved : entry);
      settingsOpen = false;
      toast.success($translate("extensions.saved"));
    } catch (cause) {
      settingsError = String(cause);
      settingsErrors = settingDiagnosticsFrom(cause);
    } finally { settingsSaving = false; }
  }
  onMount(() => {
    void load();
  });
</script>

<svelte:head><title>{$translate("extensions.workspace.manager")} · llama-swap</title></svelte:head>

<section class="space-y-6 rounded-lg border border-border bg-card p-4 md:p-6">
  <header class="flex flex-wrap items-start justify-between gap-4">
    <div><h1 class="!pb-0 text-xl font-semibold">{$translate("extensions.workspace.manager")}</h1><p class="mt-1 text-sm text-muted-foreground">{$translate("extensions.workspace.subtitle")}</p></div>
    <div class="flex flex-wrap gap-2">
      <Button variant="outline" size="icon" aria-label={$translate("extensions.reload")} disabled={loading || busy} onclick={() => load(true)}><RefreshCw class="size-4" /></Button>
      <Button variant="outline" disabled={busy} onclick={openStore}>{$translate("extensions.workspace.fromPreset")}</Button>
      <Button variant="outline" disabled={busy} onclick={() => importOpen = true}><PackagePlus class="size-4" />{$translate("extensions.import.title")}</Button>
      <Button href="#/extensions/new"><Plus class="size-4" />{$translate("extensions.workspace.create")}</Button>
    </div>
  </header>
  {#if error}<div role="alert" class="rounded-lg border border-destructive/30 bg-destructive/10 p-3 text-sm text-destructive">{error}</div>{/if}
  <div class="flex flex-wrap items-center justify-between gap-3">
    <div class="flex min-w-0 flex-1 items-center gap-2 rounded-md border border-border px-3 py-2 sm:max-w-md"><Search class="size-4 shrink-0 text-muted-foreground" /><input class="min-w-0 flex-1 bg-transparent text-sm outline-none focus-visible:ring-2 focus-visible:ring-ring" bind:value={search} aria-label={$translate("extensions.search")} placeholder={$translate("extensions.workspace.search")} /></div>
    <div class="flex flex-wrap gap-2" aria-label={$translate("extensions.workspace.status")}>
      {#each [["all", items.length], ["enabled", enabledCount], ["disabled", items.length - enabledCount], ["issues", issueCount]] as [key, count]}
        {#if key !== "issues" || count || filter === "issues"}<Button variant="outline" size="sm" class={filter === key ? "border-primary/30 bg-primary/10 text-primary" : ""} aria-pressed={filter === key} onclick={() => filter = String(key)}>{$translate(`extensions.workspace.${key}`)}<span class="rounded-full bg-muted px-1.5 text-xs tabular-nums">{count}</span></Button>{/if}
      {/each}
    </div>
  </div>
  {#if loading}<p role="status" class="p-12 text-center text-sm text-muted-foreground">{$translate("common.loading")}</p>
  {:else if filtered.length === 0}
    <div class="flex flex-col items-center gap-3 border-y border-border px-6 py-16 text-center"><Puzzle class="size-8 text-muted-foreground" /><h2 class="font-medium">{$translate(items.length ? "extensions.workspace.noMatches" : "extensions.empty")}</h2><p class="text-sm text-muted-foreground">{$translate(items.length ? "extensions.workspace.filterHint" : "extensions.workspace.emptyHint")}</p>{#if items.length}<Button variant="outline" onclick={() => { search = ""; filter = "all"; }}>{$translate("extensions.workspace.clear")}</Button>{:else if !error}<Button href="#/extensions/new"><Plus class="size-4" />{$translate("extensions.workspace.create")}</Button>{/if}</div>
  {:else}
    <ul class="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
      {#each filtered as item (item.manifest.id)}
        <li class="flex flex-col gap-3 rounded-lg border border-border bg-background/40 p-4 transition-colors hover:border-primary/40">
          <div class="flex items-start gap-3">
            <span class="flex size-9 shrink-0 items-center justify-center rounded-md bg-primary/10 text-primary"><FileCode2 class="size-5" /></span>
            <div class="min-w-0 flex-1"><a href={editorUrl(item.manifest.id)} use:link class="block truncate font-medium hover:text-primary">{item.manifest.name || item.manifest.id}</a><p class="truncate font-mono text-[11px] text-muted-foreground">{item.manifest.id}</p></div>
            <span class={`shrink-0 rounded-full px-2 py-0.5 text-xs ${item.status === "ready" ? "bg-success/10" : "bg-muted"}`} class:text-success={item.status === "ready"} class:text-destructive={["error", "invalid"].includes(item.status)} class:text-warning={item.status === "stale"}>{$translate(item.status === "ready" ? "extensions.workspace.enabled" : `extensions.status.${item.status}`)}</span>
          </div>
          {#if item.manifest.description}<p class="line-clamp-2 text-xs leading-relaxed text-muted-foreground">{item.manifest.description}</p>{/if}
          <div class="flex flex-wrap gap-1.5">
            <span class="rounded-full border border-border px-2 py-0.5 font-mono text-[11px] text-muted-foreground">JavaScript</span>
            {#if item.manifest.match?.models?.length}
              {#each item.manifest.match.models.slice(0, 2) as model (model)}<span class="rounded-full border border-border px-2 py-0.5 text-[11px] text-muted-foreground">{model}</span>{/each}
              {#if item.manifest.match.models.length > 2}<span class="rounded-full border border-border px-2 py-0.5 text-[11px] text-muted-foreground" title={item.manifest.match.models.join(", ")}>+{item.manifest.match.models.length - 2}</span>{/if}
            {:else}<span class="rounded-full border border-success/40 bg-success/10 px-2 py-0.5 text-[11px] text-success">{$translate("extensions.workspace.allModels")}</span>{/if}
            {#if item.manifest.match?.endpoints?.length}<span class="rounded-full border border-border px-2 py-0.5 font-mono text-[11px] text-muted-foreground" title={$translate("extensions.endpoints")}>{item.manifest.match.endpoints[0]}{item.manifest.match.endpoints.length > 1 ? ` +${item.manifest.match.endpoints.length - 1}` : ""}</span>{/if}
            {#if item.manifest.match?.excludeModels?.length}<span class="rounded-full border border-border px-2 py-0.5 text-[11px] text-muted-foreground" title={`${$translate("extensions.excludeModels")}: ${item.manifest.match.excludeModels.join(", ")}`}>{$translate("extensions.excludeModels")} ×{item.manifest.match.excludeModels.length}</span>{/if}
          </div>
          {#if item.lastError}<p class="break-words text-xs text-destructive">{item.lastError}</p>{/if}
          <div class="mt-auto flex items-center gap-2.5 border-t border-border pt-3">
            <Switch.Root checked={item.manifest.enabled} disabled={busy || !item.etag} aria-label={`${$translate("extensions.enabled")} ${item.manifest.name || item.manifest.id}`} onCheckedChange={() => void toggle(item)} />
            <span class="text-xs text-muted-foreground">{$translate(item.manifest.enabled ? "extensions.workspace.enabled" : "extensions.workspace.disabled")}</span>
            <div class="ml-auto flex items-center gap-2">
              {#if item.settings?.length}<Button variant="outline" size="sm" disabled={busy || !item.etag} onclick={() => void openSettings(item)}><Settings2 class="size-3.5" />{$translate("extensions.workspace.quickSettings")}</Button>{/if}
              <DropdownMenu.Root><DropdownMenu.Trigger class="flex size-7 items-center justify-center rounded-md border border-border hover:bg-muted" aria-label={`${$translate("extensions.workspace.actions")} ${item.manifest.id}`}><MoreHorizontal class="size-4" /></DropdownMenu.Trigger><DropdownMenu.Content align="end"><DropdownMenu.Item onclick={() => void push(editorUrl(item.manifest.id))}><FileCode2 class="size-4" />{$translate("extensions.workspace.edit")}</DropdownMenu.Item><DropdownMenu.Item onclick={() => { logViewTarget = item; logOpen = true; }}><ScrollText class="size-4" />{$translate("extensions.logs.title")}</DropdownMenu.Item><DropdownMenu.Item disabled={busy || !item.etag} onclick={() => void exportItem(item)}><Download class="size-4" />{$translate("extensions.import.export")}</DropdownMenu.Item><DropdownMenu.Separator /><DropdownMenu.Item disabled={busy || !item.etag} onclick={() => { duplicateTarget = item; duplicateId = `${item.manifest.id}-copy`; duplicateOpen = true; }}><Copy class="size-4" />{$translate("extensions.duplicate")}</DropdownMenu.Item><DropdownMenu.Separator /><DropdownMenu.Item class="text-destructive" disabled={busy || !item.etag} onclick={() => { deleteTarget = item; deleteOpen = true; }}><Trash2 class="size-4" />{$translate("extensions.delete")}</DropdownMenu.Item></DropdownMenu.Content></DropdownMenu.Root>
            </div>
          </div>
        </li>
      {/each}
    </ul>
  {/if}
  <p class="text-xs text-muted-foreground">{$translate("extensions.workspace.count", { count: filtered.length })}</p>
</section>
<ConfirmDialog bind:open={deleteOpen} title={$translate("extensions.delete")} message={$translate("extensions.deleteConfirm", { id: deleteTarget?.manifest.id ?? "" })} confirmLabel={$translate("extensions.delete")} onConfirm={() => void remove()} />
<Dialog.Root bind:open={duplicateOpen}><Dialog.Content><Dialog.Header><Dialog.Title>{$translate("extensions.duplicate")}</Dialog.Title><Dialog.Description>{$translate("extensions.duplicateId")}</Dialog.Description></Dialog.Header><input class="rounded-md border border-border bg-background p-2" bind:value={duplicateId} aria-label={$translate("extensions.duplicateId")} />{#if error}<p role="alert" class="text-sm text-destructive">{error}</p>{/if}<Dialog.Footer><Button variant="outline" onclick={() => duplicateOpen = false}>{$translate("common.cancel")}</Button><Button disabled={busy || !duplicateId.trim()} onclick={duplicate}>{$translate("extensions.duplicate")}</Button></Dialog.Footer></Dialog.Content></Dialog.Root>
<ExtensionImportDialog bind:open={importOpen} onImported={() => void refresh()} />
<Dialog.Root bind:open={logOpen}><Dialog.Content class="flex h-[80vh] max-h-[80vh] flex-col sm:max-w-3xl"><Dialog.Header><Dialog.Title>{$translate("extensions.logs.title")} · {logViewTarget?.manifest.name || logViewTarget?.manifest.id}</Dialog.Title><Dialog.Description>{$translate("extensions.logs.subtitle")}</Dialog.Description></Dialog.Header><div class="min-h-0 flex-1 overflow-hidden rounded-md border border-border">{#if logViewTarget}<ExtensionConsolePanel id={logViewTarget.manifest.id} />{/if}</div></Dialog.Content></Dialog.Root>
<Dialog.Root bind:open={storeOpen}><Dialog.Content class="max-h-[85vh] overflow-y-auto sm:max-w-4xl"><Dialog.Header class="sr-only"><Dialog.Title>{$translate("extensions.presets.title")}</Dialog.Title><Dialog.Description>{$translate("extensions.presets.subtitle")}</Dialog.Description></Dialog.Header><ExtensionPresetStore {presets} onInstalled={(id) => { storeOpen = false; void push(editorUrl(id)); }} /></Dialog.Content></Dialog.Root>
<Dialog.Root bind:open={settingsOpen}><Dialog.Content class="max-h-[85vh] overflow-y-auto sm:max-w-lg">
  <Dialog.Header>
    <Dialog.Title>{$translate("extensions.workspace.quickSettings")} · {settingsDraft?.manifest.name || settingsID || "…"}</Dialog.Title>
    <Dialog.Description>{$translate("extensions.settings.modalHint")}</Dialog.Description>
  </Dialog.Header>
  {#if settingsLoading}<p role="status" class="p-8 text-center text-sm text-muted-foreground">{$translate("common.loading")}</p>
  {:else if !settingsDraft}
    {#if settingsError}<p role="alert" class="break-words rounded-md bg-destructive/10 p-3 text-sm text-destructive">{settingsError}</p>{/if}
  {:else if settingsDraft.settings?.length}
    <div class="settings-form space-y-5"><ExtensionSettingsForm settings={settingsDraft.settings} config={settingsDraft.manifest.config ?? {}} errors={settingsErrors} onChange={updateSetting} /></div>
  {:else}
    <p class="rounded-md border border-dashed border-border p-3 text-sm text-muted-foreground">{$translate("extensions.settings.none")}</p>
    <div class="mt-3"><ExtensionValueEditor root value={settingsDraft.manifest.config ?? {}} onChange={updateRawConfig} /></div>
  {/if}
  {#if settingsError && settingsDraft}<p role="alert" class="mt-3 break-words rounded-md bg-destructive/10 p-3 text-sm text-destructive">{settingsError}</p>{/if}
  <Dialog.Footer>
    <Button variant="outline" disabled={settingsSaving} onclick={() => settingsOpen = false}>{$translate("common.cancel")}</Button>
    <Button disabled={!settingsDirty || settingsSaving} onclick={() => void saveSettings()}>{$translate("extensions.save")}</Button>
  </Dialog.Footer>
</Dialog.Content></Dialog.Root>
