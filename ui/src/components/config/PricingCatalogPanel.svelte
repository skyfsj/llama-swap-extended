<script lang="ts">
  import { onMount } from "svelte";
  import { Check, ChevronLeft, ChevronRight, LoaderCircle, Pencil, Plus, RefreshCw, Search, Trash2 } from "@lucide/svelte";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import * as Switch from "$lib/components/ui/switch/index.js";
  import * as Table from "$lib/components/ui/table/index.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Input } from "$lib/components/ui/input/index.js";
  import { translate } from "../../lib/i18n";
  import { errorMessageFromPayload } from "../../lib/apiError";
  import {
    deletePricingPrice,
    getPricingCatalog,
    getPricingPrices,
    savePricingPrice,
    syncPricing,
    type PricingPrice,
    type PricingPriceInput,
  } from "../../lib/pricing";
  import { property, record } from "../../lib/modelConfig";

  interface ConfigSnapshot {
    config?: Record<string, unknown>;
    yaml: string;
    etag: string;
    writable: boolean;
  }

  interface Props {
    snapshot: ConfigSnapshot;
    targetModel: string;
    selectedProvider: string;
    selectedPricingModel: string;
    onMappingChange: (provider: string, model: string) => void;
    onSnapshot: (snapshot: ConfigSnapshot) => void;
  }

  type RateKey = "input" | "output" | "cacheRead" | "cacheWrite" | "reasoning";

  let {
    snapshot,
    targetModel,
    selectedProvider,
    selectedPricingModel,
    onMappingChange,
    onSnapshot,
  }: Props = $props();

  let rows = $state<PricingPrice[]>([]);
  let providers = $state<string[]>([]);
  let page = $state(1);
  let total = $state(0);
  let totalPages = $state(0);
  let query = $state("");
  let providerFilter = $state("");
  let loading = $state(false);
  let syncing = $state(false);
  let error = $state("");
  let syncedAt = $state("");
  let source = $state("");
  let requestID = 0;

  let editorOpen = $state(false);
  let editorMode = $state<"create" | "edit">("create");
  let editorSaving = $state(false);
  let editorError = $state("");
  let draft = $state<PricingPriceInput>(emptyPrice());

  let deleteOpen = $state(false);
  let deleteTarget = $state<PricingPrice | null>(null);
  let deleting = $state(false);

  let pickerOpen = $state(false);
  let pickerRows = $state<PricingPrice[]>([]);
  let pickerQuery = $state("");
  let pickerProvider = $state("");
  let pickerSelection = $state<PricingPrice | null>(null);
  let pickerLoading = $state(false);
  let pickerError = $state("");

  let pricingSettings = $derived(record(property(record(property(record(snapshot.config), "pricing")), "modelsDev")));
  let syncEnabled = $derived(pricingSettings.enabled !== false);
  let pricingURL = $derived(typeof pricingSettings.url === "string" && pricingSettings.url.trim()
    ? pricingSettings.url.trim()
    : "https://models.dev/api.json");
  let syncSaving = $state(false);

  const inputClass = "border-input bg-background h-9 w-full rounded-md border px-3 text-sm outline-none transition-colors focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/30 disabled:cursor-not-allowed disabled:opacity-60";

  function emptyPrice(): PricingPriceInput {
    return { provider: "", model: "", input: 0, output: 0, cacheRead: 0, cacheWrite: 0, reasoning: 0 };
  }

  function isSelected(row: PricingPrice): boolean {
    return row.provider === selectedProvider.trim() && row.model === selectedPricingModel.trim();
  }

  function rate(value: number): string {
    if (!Number.isFinite(value)) return "—";
    return `$${value.toLocaleString(undefined, { maximumFractionDigits: 6 })}`;
  }

  function absoluteTime(value: string): string {
    if (!value) return $translate("pricing.notSynced");
    const date = new Date(value);
    return Number.isNaN(date.valueOf()) ? value : date.toLocaleString();
  }

  function pageLabel(): string {
    if (total === 0) return $translate("pricing.noRows");
    return $translate("pricing.pageSummary", { page, totalPages, total });
  }

  async function loadRows(nextPage = page): Promise<void> {
    const current = ++requestID;
    loading = true;
    error = "";
    try {
      const response = await getPricingPrices({ query, provider: providerFilter, page: nextPage, limit: 50 });
      if (current !== requestID) return;
      rows = response.data;
      page = response.page;
      total = response.total;
      totalPages = response.total_pages;
      syncedAt = response.syncedAt ?? "";
      source = response.source ?? "";
    } catch (cause) {
      if (current === requestID) error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (current === requestID) loading = false;
    }
  }

  async function loadProviders(): Promise<void> {
    try {
      providers = (await getPricingCatalog({ limit: 200 })).providers;
    } catch {
      providers = [];
    }
  }

  function searchRows(): void {
    page = 1;
    void loadRows(1);
  }

  function clearFilters(): void {
    query = "";
    providerFilter = "";
    searchRows();
  }

  function openCreate(): void {
    editorMode = "create";
    draft = emptyPrice();
    editorError = "";
    editorOpen = true;
  }

  function openEdit(row: PricingPrice): void {
    editorMode = "edit";
    draft = {
      provider: row.provider,
      model: row.model,
      input: row.input,
      output: row.output,
      cacheRead: row.cacheRead,
      cacheWrite: row.cacheWrite,
      reasoning: row.reasoning,
    };
    editorError = "";
    editorOpen = true;
  }

  function setRate(key: RateKey, raw: string): void {
    const value = raw.trim() === "" ? 0 : Number(raw);
    if (Number.isFinite(value) && value >= 0) draft = { ...draft, [key]: value };
  }

  async function saveRow(): Promise<void> {
    const provider = draft.provider.trim();
    const model = draft.model.trim();
    if (!provider || !model) {
      editorError = $translate("pricing.identityRequired");
      return;
    }
    if (Object.values(draft).some((value) => typeof value !== "string" && (!Number.isFinite(value) || value < 0))) {
      editorError = $translate("pricing.rateInvalid");
      return;
    }
    editorSaving = true;
    editorError = "";
    try {
      await savePricingPrice({ ...draft, provider, model });
      editorOpen = false;
      await loadRows(editorMode === "create" ? 1 : page);
      await loadProviders();
    } catch (cause) {
      editorError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      editorSaving = false;
    }
  }

  function askDelete(row: PricingPrice): void {
    deleteTarget = row;
    deleteOpen = true;
  }

  async function confirmDelete(): Promise<void> {
    if (!deleteTarget || deleting) return;
    deleting = true;
    error = "";
    try {
      await deletePricingPrice(deleteTarget.provider, deleteTarget.model);
      deleteOpen = false;
      deleteTarget = null;
      await loadRows(page);
      await loadProviders();
    } catch (cause) {
      error = cause instanceof Error ? cause.message : String(cause);
      deleteOpen = false;
    } finally {
      deleting = false;
    }
  }

  function applyMapping(row: PricingPrice): void {
    onMappingChange(row.provider, row.model);
    pickerOpen = false;
  }

  function clearMapping(): void {
    onMappingChange("", "");
  }

  async function openPicker(): Promise<void> {
    pickerOpen = true;
    pickerQuery = "";
    pickerProvider = "";
    pickerSelection = rows.find(isSelected) ?? null;
    await loadPickerRows();
  }

  async function loadPickerRows(): Promise<void> {
    pickerLoading = true;
    pickerError = "";
    try {
      const response = await getPricingPrices({ query: pickerQuery, provider: pickerProvider, page: 1, limit: 100 });
      pickerRows = response.data;
      if (!pickerSelection) pickerSelection = pickerRows.find(isSelected) ?? null;
    } catch (cause) {
      pickerError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      pickerLoading = false;
    }
  }

  async function syncNow(): Promise<void> {
    if (syncing) return;
    syncing = true;
    error = "";
    try {
      await syncPricing();
      page = 1;
      await loadRows(1);
      await loadProviders();
    } catch (cause) {
      error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      syncing = false;
    }
  }

  async function setSyncEnabled(enabled: boolean): Promise<void> {
    if (syncSaving || !snapshot.writable || !snapshot.etag) return;
    syncSaving = true;
    error = "";
    try {
      const config = record(snapshot.config);
      const pricing = property(config, "pricing");
      const pricingRecord = record(pricing);
      let patch: unknown[];
      if (!Object.prototype.hasOwnProperty.call(config, "pricing")) {
        patch = [{ op: "add", path: "/pricing", value: { modelsDev: { enabled } } }];
      } else if (!Object.prototype.hasOwnProperty.call(pricingRecord, "modelsDev")) {
        patch = [{ op: "add", path: "/pricing/modelsDev", value: { enabled } }];
      } else {
        patch = [{ op: "add", path: "/pricing/modelsDev/enabled", value: enabled }];
      }
      const validationResponse = await fetch("/api/config/validate", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(patch),
      });
      const validation = await validationResponse.json().catch(() => ({})) as { valid?: boolean; issues?: { message?: string }[]; error?: string };
      if (!validationResponse.ok || validation.valid === false) {
        throw new Error(validation.error ?? validation.issues?.[0]?.message ?? $translate("pricing.settingsSaveFailed"));
      }
      const response = await fetch("/api/config", {
        method: "PATCH",
        headers: { "Content-Type": "application/json", "If-Match": snapshot.etag },
        body: JSON.stringify(patch),
      });
      const payload: unknown = await response.json().catch(() => ({}));
      if (response.status === 412) throw new Error($translate("controlPlane.conflict"));
      if (!response.ok) throw new Error(errorMessageFromPayload(payload, `HTTP ${response.status}`));
      onSnapshot(payload as ConfigSnapshot);
    } catch (cause) {
      error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      syncSaving = false;
    }
  }

  onMount(() => {
    void loadRows(1);
    void loadProviders();
  });
</script>

<section class="grid gap-4" aria-label={$translate("pricing.title")}>
  <div class="border-border/70 overflow-hidden rounded-md border">
    <div class="flex flex-wrap items-start gap-3 border-b px-4 py-3">
      <div class="min-w-0">
        <h3 class="text-sm font-semibold">{$translate("pricing.tableTitle")}</h3>
        <p class="text-muted-foreground mt-1 text-xs">{$translate("pricing.tableDescription")}</p>
      </div>
      <Button class="ml-auto" size="sm" onclick={openCreate} disabled={!snapshot.writable}>
        <Plus class="size-3.5" aria-hidden="true" />
        {$translate("pricing.add")}
      </Button>
    </div>

    <div class="bg-muted/10 grid gap-2 border-b p-3 md:grid-cols-[minmax(0,1fr)_minmax(11rem,16rem)_auto_auto]">
      <label class="relative min-w-0" aria-label={$translate("pricing.searchPlaceholder")}>
        <Search class="text-muted-foreground pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2" aria-hidden="true" />
        <Input class="h-9 pl-8" placeholder={$translate("pricing.searchPlaceholder")} bind:value={query} onkeydown={(event) => { if (event.key === "Enter") searchRows(); }} />
      </label>
      <select class="border-input bg-background h-9 min-w-0 rounded-lg border px-3 text-sm outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/30" aria-label={$translate("pricing.providerFilter")} bind:value={providerFilter} onchange={searchRows}>
        <option value="">{$translate("pricing.allProviders")}</option>
        {#each providers as provider (provider)}<option value={provider}>{provider}</option>{/each}
      </select>
      <Button variant="outline" size="sm" onclick={searchRows} disabled={loading}>
        {#if loading}<LoaderCircle class="size-3.5 animate-spin" aria-hidden="true" />{:else}<Search class="size-3.5" aria-hidden="true" />{/if}
        {$translate("pricing.search")}
      </Button>
      <Button variant="ghost" size="sm" onclick={clearFilters} disabled={!query && !providerFilter}>{$translate("pricing.clearFilters")}</Button>
    </div>

    {#if error}<div class="border-destructive/40 bg-destructive/10 text-destructive m-3 rounded-md border p-3 text-sm" role="alert">{error}</div>{/if}

    <div class="overflow-x-auto">
      <Table.Root class="min-w-[820px]">
        <Table.Header>
          <Table.Row>
            <Table.Head class="min-w-[18rem]">{$translate("pricing.columns.model")}</Table.Head>
            <Table.Head class="text-right">{$translate("pricing.columns.input")}</Table.Head>
            <Table.Head class="text-right">{$translate("pricing.columns.output")}</Table.Head>
            <Table.Head class="text-right">{$translate("pricing.columns.cacheRead")}</Table.Head>
            <Table.Head class="text-right">{$translate("pricing.columns.cacheWrite")}</Table.Head>
            <Table.Head class="text-right">{$translate("pricing.columns.reasoning")}</Table.Head>
            <Table.Head class="text-right">{$translate("pricing.columns.actions")}</Table.Head>
          </Table.Row>
        </Table.Header>
        <Table.Body>
          {#if loading && rows.length === 0}
            <Table.Row><Table.Cell colspan={7} class="text-muted-foreground py-10 text-center">{$translate("pricing.loading")}</Table.Cell></Table.Row>
          {:else if rows.length === 0}
            <Table.Row><Table.Cell colspan={7} class="text-muted-foreground py-10 text-center">{$translate("pricing.noRows")}</Table.Cell></Table.Row>
          {:else}
            {#each rows as row (`${row.provider}:${row.model}`)}
              <Table.Row class={isSelected(row) ? "bg-primary/5" : undefined}>
                <Table.Cell class="max-w-[28rem]">
                  <div class="flex min-w-0 items-center gap-2">
                    {#if isSelected(row)}<Check class="text-primary size-3.5 shrink-0" aria-label={$translate("pricing.selected")} />{/if}
                    <div class="min-w-0">
                      <div class="truncate font-medium" title={row.model}>{row.model}</div>
                      <div class="text-muted-foreground truncate text-xs" title={row.provider}>{row.provider}</div>
                    </div>
                  </div>
                </Table.Cell>
                <Table.Cell class="text-right tabular-nums">{rate(row.input)}</Table.Cell>
                <Table.Cell class="text-right tabular-nums">{rate(row.output)}</Table.Cell>
                <Table.Cell class="text-right tabular-nums">{rate(row.cacheRead)}</Table.Cell>
                <Table.Cell class="text-right tabular-nums">{rate(row.cacheWrite)}</Table.Cell>
                <Table.Cell class="text-right tabular-nums">{rate(row.reasoning)}</Table.Cell>
                <Table.Cell class="text-right">
                  <div class="flex justify-end gap-1">
                    <Button variant="outline" size="xs" onclick={() => applyMapping(row)}>{$translate("pricing.use")}</Button>
                    <Button variant="ghost" size="icon-sm" title={$translate("pricing.edit")} aria-label={$translate("pricing.edit")} onclick={() => openEdit(row)} disabled={!snapshot.writable}><Pencil class="size-3.5" aria-hidden="true" /></Button>
                    <Button variant="ghost" size="icon-sm" class="text-destructive hover:text-destructive" title={$translate("pricing.delete")} aria-label={$translate("pricing.delete")} onclick={() => askDelete(row)} disabled={!snapshot.writable}><Trash2 class="size-3.5" aria-hidden="true" /></Button>
                  </div>
                </Table.Cell>
              </Table.Row>
            {/each}
          {/if}
        </Table.Body>
      </Table.Root>
    </div>

    <div class="text-muted-foreground flex flex-wrap items-center justify-between gap-2 border-t px-3 py-2 text-xs">
      <span>{pageLabel()}</span>
      {#if totalPages > 1}
        <div class="flex items-center gap-1">
          <Button variant="ghost" size="icon-xs" title={$translate("pricing.previousPage")} aria-label={$translate("pricing.previousPage")} onclick={() => void loadRows(page - 1)} disabled={page <= 1 || loading}><ChevronLeft class="size-3.5" aria-hidden="true" /></Button>
          <span class="min-w-16 text-center tabular-nums">{page} / {totalPages}</span>
          <Button variant="ghost" size="icon-xs" title={$translate("pricing.nextPage")} aria-label={$translate("pricing.nextPage")} onclick={() => void loadRows(page + 1)} disabled={page >= totalPages || loading}><ChevronRight class="size-3.5" aria-hidden="true" /></Button>
        </div>
      {/if}
    </div>
  </div>

  <div class="border-border/70 rounded-md border p-4">
    <div class="flex flex-wrap items-start gap-3">
      <div class="min-w-0">
        <h3 class="text-sm font-semibold">{$translate("pricing.syncTitle")}</h3>
        <p class="text-muted-foreground mt-1 text-xs">{$translate("pricing.syncDescription")}</p>
      </div>
      <div class="ml-auto flex items-center gap-2 text-xs">
        <span class="text-muted-foreground">{syncEnabled ? $translate("pricing.enabled") : $translate("pricing.disabled")}</span>
        <Switch.Root checked={syncEnabled} onCheckedChange={(checked) => void setSyncEnabled(checked)} disabled={syncSaving || !snapshot.writable} aria-label={$translate("pricing.syncTitle")} />
      </div>
    </div>
    <div class="text-muted-foreground mt-4 grid gap-3 text-xs sm:grid-cols-2">
      <div class="min-w-0">
        <div class="mb-1">{$translate("pricing.source")}</div>
        <code class="text-foreground block truncate" title={source || pricingURL}>{source || pricingURL}</code>
      </div>
      <div>
        <div class="mb-1">{$translate("pricing.lastSynced")}</div>
        <span class="text-foreground">{absoluteTime(syncedAt)}</span>
      </div>
    </div>
    <div class="mt-4 flex flex-wrap justify-end gap-2">
      <Button variant="outline" size="sm" onclick={() => void openPicker()} disabled={loading || total === 0}>
        {$translate("pricing.chooseModel")}
      </Button>
      <Button variant="outline" size="sm" onclick={() => void loadRows(page)} disabled={loading}>
        <RefreshCw class={`size-3.5 ${loading ? "animate-spin" : ""}`} aria-hidden="true" />
        {$translate("pricing.reload")}
      </Button>
      <Button size="sm" onclick={() => void syncNow()} disabled={syncing || !syncEnabled || !snapshot.writable}>
        {#if syncing}<LoaderCircle class="size-3.5 animate-spin" aria-hidden="true" />{:else}<RefreshCw class="size-3.5" aria-hidden="true" />{/if}
        {$translate("pricing.syncNow")}
      </Button>
    </div>
  </div>

  <div class="border-border/70 bg-muted/10 rounded-md border px-3 py-2 text-xs">
    <div class="text-muted-foreground">{$translate("pricing.currentMapping")}</div>
    <div class="mt-1 flex flex-wrap items-center gap-2">
      <code class="text-foreground max-w-full truncate">{targetModel || $translate("pricing.unsavedModel")}</code>
      <span class="text-muted-foreground">→</span>
      {#if selectedProvider && selectedPricingModel}
        <code class="text-foreground max-w-full truncate">{selectedProvider} / {selectedPricingModel}</code>
        <Button variant="ghost" size="xs" onclick={clearMapping}>{$translate("pricing.clearMapping")}</Button>
      {:else}
        <span class="text-muted-foreground">{$translate("pricing.noMapping")}</span>
      {/if}
      <Button class="ml-auto" variant="outline" size="xs" onclick={() => void openPicker()} disabled={total === 0}>{$translate("pricing.chooseModel")}</Button>
    </div>
    <p class="text-muted-foreground mt-2">{$translate("pricing.mappingHint")}</p>
  </div>
</section>

<Dialog.Root bind:open={editorOpen}>
  <Dialog.Content class="max-h-[calc(100vh-2rem)] max-w-2xl overflow-y-auto" showCloseButton={!editorSaving}>
    <Dialog.Header>
      <Dialog.Title>{editorMode === "create" ? $translate("pricing.addTitle") : $translate("pricing.editTitle")}</Dialog.Title>
      <Dialog.Description>{$translate("pricing.editorDescription")}</Dialog.Description>
    </Dialog.Header>
    {#if editorError}<div class="border-destructive/40 bg-destructive/10 text-destructive rounded-md border p-3 text-sm" role="alert">{editorError}</div>{/if}
    <div class="grid gap-4 sm:grid-cols-2">
      <label class="grid gap-1.5 text-sm"><span class="font-medium">{$translate("pricing.columns.provider")}</span><Input class={inputClass} bind:value={draft.provider} disabled={editorMode === "edit" || editorSaving} list="pricing-provider-options" autocomplete="off" /></label>
      <label class="grid gap-1.5 text-sm"><span class="font-medium">{$translate("pricing.columns.model")}</span><Input class={`${inputClass} font-mono text-xs`} bind:value={draft.model} disabled={editorMode === "edit" || editorSaving} autocomplete="off" /></label>
    </div>
    <datalist id="pricing-provider-options">{#each providers as provider (provider)}<option value={provider}></option>{/each}</datalist>
    <div class="grid gap-4 sm:grid-cols-2">
      <label class="grid gap-1.5 text-sm"><span class="font-medium">{$translate("pricing.columns.input")}</span><Input class={inputClass} type="number" min="0" step="any" value={draft.input} oninput={(event) => setRate("input", event.currentTarget.value)} /></label>
      <label class="grid gap-1.5 text-sm"><span class="font-medium">{$translate("pricing.columns.output")}</span><Input class={inputClass} type="number" min="0" step="any" value={draft.output} oninput={(event) => setRate("output", event.currentTarget.value)} /></label>
      <label class="grid gap-1.5 text-sm"><span class="font-medium">{$translate("pricing.columns.cacheRead")}</span><Input class={inputClass} type="number" min="0" step="any" value={draft.cacheRead} oninput={(event) => setRate("cacheRead", event.currentTarget.value)} /></label>
      <label class="grid gap-1.5 text-sm"><span class="font-medium">{$translate("pricing.columns.cacheWrite")}</span><Input class={inputClass} type="number" min="0" step="any" value={draft.cacheWrite} oninput={(event) => setRate("cacheWrite", event.currentTarget.value)} /></label>
      <label class="grid gap-1.5 text-sm sm:col-span-2"><span class="font-medium">{$translate("pricing.columns.reasoning")}</span><Input class={inputClass} type="number" min="0" step="any" value={draft.reasoning} oninput={(event) => setRate("reasoning", event.currentTarget.value)} /></label>
    </div>
    <Dialog.Footer>
      <Button variant="outline" onclick={() => (editorOpen = false)} disabled={editorSaving}>{$translate("common.cancel")}</Button>
      <Button onclick={() => void saveRow()} disabled={editorSaving || !snapshot.writable}>
        {#if editorSaving}<LoaderCircle class="size-4 animate-spin" aria-hidden="true" />{:else}<Check class="size-4" aria-hidden="true" />{/if}
        {$translate("common.save")}
      </Button>
    </Dialog.Footer>
  </Dialog.Content>
</Dialog.Root>

<Dialog.Root bind:open={deleteOpen}>
  <Dialog.Content class="max-w-lg" showCloseButton={!deleting}>
    <Dialog.Header>
      <Dialog.Title>{$translate("pricing.deleteTitle")}</Dialog.Title>
      <Dialog.Description>{$translate("pricing.deleteDescription", { model: deleteTarget?.model ?? "" })}</Dialog.Description>
    </Dialog.Header>
    <Dialog.Footer>
      <Button variant="outline" onclick={() => (deleteOpen = false)} disabled={deleting}>{$translate("common.cancel")}</Button>
      <Button variant="destructive" onclick={() => void confirmDelete()} disabled={deleting}>
        {#if deleting}<LoaderCircle class="size-4 animate-spin" aria-hidden="true" />{:else}<Trash2 class="size-4" aria-hidden="true" />{/if}
        {$translate("pricing.delete")}
      </Button>
    </Dialog.Footer>
  </Dialog.Content>
</Dialog.Root>

<Dialog.Root bind:open={pickerOpen}>
  <Dialog.Content class="max-h-[calc(100vh-2rem)] max-w-3xl overflow-y-auto" showCloseButton={!pickerLoading}>
    <Dialog.Header>
      <Dialog.Title>{$translate("pricing.pickerTitle")}</Dialog.Title>
      <Dialog.Description>{$translate("pricing.pickerDescription")}</Dialog.Description>
    </Dialog.Header>
    <div class="grid gap-2 sm:grid-cols-[minmax(0,1fr)_minmax(10rem,14rem)_auto]">
      <label class="relative min-w-0" aria-label={$translate("pricing.searchPlaceholder")}>
        <Search class="text-muted-foreground pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2" aria-hidden="true" />
        <Input class="h-9 pl-8" placeholder={$translate("pricing.searchPlaceholder")} bind:value={pickerQuery} onkeydown={(event) => { if (event.key === "Enter") void loadPickerRows(); }} />
      </label>
      <select class="border-input bg-background h-9 min-w-0 rounded-lg border px-3 text-sm" aria-label={$translate("pricing.providerFilter")} bind:value={pickerProvider} onchange={() => void loadPickerRows()}>
        <option value="">{$translate("pricing.allProviders")}</option>
        {#each providers as provider (provider)}<option value={provider}>{provider}</option>{/each}
      </select>
      <Button variant="outline" size="sm" onclick={() => void loadPickerRows()} disabled={pickerLoading}><Search class="size-3.5" aria-hidden="true" />{$translate("pricing.search")}</Button>
    </div>
    {#if pickerError}<div class="border-destructive/40 bg-destructive/10 text-destructive mt-3 rounded-md border p-3 text-sm" role="alert">{pickerError}</div>{/if}
    <div class="border-border/70 mt-3 overflow-hidden rounded-md border">
      <div class="max-h-[22rem] overflow-y-auto">
        <Table.Root class="min-w-[640px]">
          <Table.Header><Table.Row><Table.Head class="w-10"></Table.Head><Table.Head>{$translate("pricing.columns.model")}</Table.Head><Table.Head class="text-right">{$translate("pricing.columns.input")}</Table.Head><Table.Head class="text-right">{$translate("pricing.columns.output")}</Table.Head></Table.Row></Table.Header>
          <Table.Body>
            {#if pickerLoading && pickerRows.length === 0}
              <Table.Row><Table.Cell colspan={4} class="text-muted-foreground py-8 text-center">{$translate("pricing.loading")}</Table.Cell></Table.Row>
            {:else if pickerRows.length === 0}
              <Table.Row><Table.Cell colspan={4} class="text-muted-foreground py-8 text-center">{$translate("pricing.noRows")}</Table.Cell></Table.Row>
            {:else}
              {#each pickerRows as row (`picker-${row.provider}:${row.model}`)}
                <Table.Row class="cursor-pointer" onclick={() => (pickerSelection = row)}>
                  <Table.Cell><input type="radio" name="pricing-picker" checked={pickerSelection?.provider === row.provider && pickerSelection?.model === row.model} onchange={() => (pickerSelection = row)} aria-label={`${row.provider} / ${row.model}`} /></Table.Cell>
                  <Table.Cell><div class="font-medium">{row.model}</div><div class="text-muted-foreground text-xs">{row.provider}</div></Table.Cell>
                  <Table.Cell class="text-right tabular-nums">{rate(row.input)}</Table.Cell>
                  <Table.Cell class="text-right tabular-nums">{rate(row.output)}</Table.Cell>
                </Table.Row>
              {/each}
            {/if}
          </Table.Body>
        </Table.Root>
      </div>
    </div>
    <Dialog.Footer>
      <Button variant="outline" onclick={() => (pickerOpen = false)}>{$translate("common.cancel")}</Button>
      <Button onclick={() => pickerSelection && applyMapping(pickerSelection)} disabled={!pickerSelection}>{$translate("pricing.applySelection")}</Button>
    </Dialog.Footer>
  </Dialog.Content>
</Dialog.Root>
