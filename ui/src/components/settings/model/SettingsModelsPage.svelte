<script lang="ts">
  // Models management list for the settings center: search + status filter,
  // one row per configured model, and per-row actions. "编辑配置" opens the
  // single-model config modal; start/stop/logs are runtime operations on the
  // live model, independent of config edits.
  import { t } from "../../../lib/i18n";
  import { push } from "svelte-spa-router";
  import {
    Boxes,
    Copy,
    FileText,
    Pencil,
    Play,
    Plus,
    Square,
    Trash2,
  } from "@lucide/svelte";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Input } from "$lib/components/ui/input/index.js";
  import * as Select from "$lib/components/ui/select/index.js";
  import * as Table from "$lib/components/ui/table/index.js";
  import { Badge } from "$lib/components/ui/badge/index.js";
  import EmptyState from "../../EmptyState.svelte";
  import { loadModel, models as liveModels, unloadSingleModel } from "../../../stores/api";
  import type { Model } from "../../../lib/types";
  import { record, property } from "../../../lib/modelConfig";
  import { readTtl } from "../../../lib/modelLifecycle";
  import ModelSettingsDialog from "../../model/ModelSettingsDialog.svelte";
  import ModelCreateDialog from "../../model/ModelCreateDialog.svelte";

  interface Props {
    models: Record<string, unknown>;
    writable: boolean;
    onChange: (next: Record<string, unknown>) => void;
    /** Invoked after the edit/create dialogs close so the settings page can
        re-sync its draft and config ETag with the server. */
    onExternalChange?: () => void | Promise<void>;
  }

  let { models, writable, onChange, onExternalChange = () => {} }: Props = $props();

  let search = $state("");
  let statusFilter = $state("all");
  let modalOpen = $state(false);
  let createOpen = $state(false);
  let editId = $state<string | null>(null);
  let deleteId = $state<string | null>(null);
  let busyId = $state("");
  let actionError = $state("");

  const ids = $derived(Object.keys(models).sort((a, b) => a.localeCompare(b)));
  const liveById = $derived.by(() => {
    const map = new Map<string, Model>();
    for (const model of $liveModels) map.set(model.id, model);
    return map;
  });

  interface Row {
    id: string;
    value: unknown;
    backend: string;
    state: string;
    context: string;
    ttl: string;
  }

  function backendLabel(value: unknown): string {
    const entry = record(value);
    const cmd = property(entry, "cmd");
    if (typeof cmd === "string" && cmd.trim()) return t("settingsCenter.model.backendCommand");
    const backend = record(property(entry, "backend"));
    const type = property(backend, "type");
    if (typeof type === "string" && type.trim()) return type;
    const runtime = property(backend, "runtime");
    if (typeof runtime === "string" && runtime.trim()) return runtime;
    return "—";
  }

  function stateOf(id: string): string {
    return liveById.get(id)?.state ?? "stopped";
  }

  function statusKey(state: string): string {
    if (state === "ready") return "settingsCenter.model.statusRunning";
    if (state === "sleeping") return "settingsCenter.model.statusSleeping";
    if (state === "starting" || state === "stopping") return "settingsCenter.model.statusLoading";
    return "settingsCenter.model.statusStopped";
  }

  function statusClass(state: string): string {
    if (state === "ready") return "is-running";
    if (state === "sleeping") return "is-sleeping";
    if (state === "starting" || state === "stopping") return "is-loading";
    return "is-stopped";
  }

  function formatContext(tokens: number): string {
    if (!Number.isFinite(tokens) || tokens <= 0) return "—";
    if (tokens >= 1000) return `${Math.round(tokens / 100) / 10}K`;
    return String(tokens);
  }

  function contextLabel(value: unknown): string {
    const capabilities = record(property(record(value), "capabilities"));
    const raw = property(capabilities, "context");
    const tokens = typeof raw === "number" ? raw : Number(raw);
    return formatContext(tokens);
  }

  function ttlLabel(value: unknown): string {
    const state = readTtl(property(record(value), "ttl"));
    if (state.mode === "inherit") return t("settingsCenter.model.ttlShortInherit");
    if (state.mode === "never") return t("settingsCenter.model.ttlShortNever");
    return `${state.seconds}s`;
  }

  const rows = $derived<Row[]>(
    ids
      .map((id) => {
        const value = models[id];
        const state = stateOf(id);
        return {
          id,
          value,
          backend: backendLabel(value),
          state,
          context: contextLabel(value),
          ttl: ttlLabel(value),
        };
      })
      .filter((row) => {
        const query = search.trim().toLowerCase();
        if (query && !`${row.id}`.toLowerCase().includes(query)) return false;
        if (statusFilter === "running" && row.state !== "ready" && row.state !== "sleeping") return false;
        if (statusFilter === "stopped" && (row.state === "ready" || row.state === "sleeping" || row.state === "starting")) return false;
        return true;
      }),
  );

  function openCreate(): void {
    createOpen = true;
  }

  function openEdit(id: string): void {
    editId = id;
    modalOpen = true;
  }

  // Clones the model entry into the page draft; it persists when the settings
  // page is saved (the model does not exist on the server until then, so the
  // edit dialog is not opened for it).
  function duplicate(id: string): void {
    const base = record(models[id]);
    let candidate = `${id}-copy`;
    let suffix = 2;
    while (candidate in models) candidate = `${id}-copy-${suffix++}`;
    const copy = structuredClone(base);
    const name = property(copy, "name");
    if (typeof name === "string" && name.trim()) copy.name = `${name} (copy)`;
    onChange({ ...models, [candidate]: copy });
  }

  async function toggleRuntime(id: string, running: boolean): Promise<void> {
    busyId = id;
    actionError = "";
    try {
      if (running) await unloadSingleModel(id);
      else await loadModel(id);
    } catch (cause) {
      actionError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      busyId = "";
    }
  }

  function openLogs(id: string): void {
    push(`/models/${encodeURIComponent(id)}`);
  }

  function confirmDelete(): void {
    if (!deleteId) return;
    const next = { ...models };
    delete next[deleteId];
    onChange(next);
    deleteId = null;
  }
</script>

<div class="models-page">
  <div class="models-page__toolbar">
    <div class="models-page__search">
      <Input
        type="search"
        placeholder={t("settingsCenter.model.searchPlaceholder")}
        aria-label={t("settingsCenter.model.searchLabel")}
        value={search}
        oninput={(event) => (search = event.currentTarget.value)}
      />
    </div>
    <Select.Root type="single" value={statusFilter} onValueChange={(value) => value && (statusFilter = value)}>
      <Select.Trigger class="models-page__filter" aria-label={t("settingsCenter.model.filterStatus")}>
        {t(statusFilter === "all" ? "settingsCenter.model.statusAll" : statusFilter === "running" ? "settingsCenter.model.statusRunning" : "settingsCenter.model.statusStopped")}
      </Select.Trigger>
      <Select.Content>
        <Select.Item value="all">{t("settingsCenter.model.statusAll")}</Select.Item>
        <Select.Item value="running">{t("settingsCenter.model.statusRunning")}</Select.Item>
        <Select.Item value="stopped">{t("settingsCenter.model.statusStopped")}</Select.Item>
      </Select.Content>
    </Select.Root>
    <Button onclick={openCreate} disabled={!writable}>
      <Plus aria-hidden="true" />{t("settingsCenter.model.add")}
    </Button>
  </div>

  {#if actionError}
    <div class="models-page__error" role="alert">{actionError}</div>
  {/if}

  {#if ids.length === 0}
    <EmptyState message={t("settingsCenter.model.empty")}>
      {#snippet children()}
        <Boxes aria-hidden="true" />
      {/snippet}
    </EmptyState>
  {:else if rows.length === 0}
    <EmptyState message={t("settingsCenter.model.emptySearch")} />
  {:else}
    <div class="models-page__table">
      <Table.Root>
        <Table.Header>
          <Table.Row>
            <Table.Head>{t("settingsCenter.model.columnId")}</Table.Head>
            <Table.Head>{t("settingsCenter.model.columnBackend")}</Table.Head>
            <Table.Head>{t("settingsCenter.model.columnStatus")}</Table.Head>
            <Table.Head>{t("settingsCenter.model.columnContext")}</Table.Head>
            <Table.Head>{t("settingsCenter.model.columnTtl")}</Table.Head>
            <Table.Head class="models-page__actions-head">{t("settingsCenter.model.columnActions")}</Table.Head>
          </Table.Row>
        </Table.Header>
        <Table.Body>
          {#each rows as row (row.id)}
            {@const running = row.state === "ready" || row.state === "sleeping" || row.state === "starting"}
            <Table.Row>
              <Table.Cell>
                <button type="button" class="models-page__id" onclick={() => openEdit(row.id)} title={t("settingsCenter.model.editAction", { id: row.id })}>
                  {row.id}
                </button>
              </Table.Cell>
              <Table.Cell><Badge variant="secondary">{row.backend}</Badge></Table.Cell>
              <Table.Cell>
                <span class="models-page__status {statusClass(row.state)}">
                  <span class="models-page__dot" aria-hidden="true"></span>
                  {t(statusKey(row.state))}
                </span>
              </Table.Cell>
              <Table.Cell>{row.context}</Table.Cell>
              <Table.Cell>{row.ttl}</Table.Cell>
              <Table.Cell class="models-page__actions">
                <Button variant="ghost" size="icon-sm" disabled={!writable} title={t("settingsCenter.model.editAction", { id: row.id })} aria-label={t("settingsCenter.model.editAction", { id: row.id })} onclick={() => openEdit(row.id)}>
                  <Pencil aria-hidden="true" />
                </Button>
                <Button variant="ghost" size="icon-sm" disabled={!writable} title={t("settingsCenter.model.actionDuplicate")} aria-label={t("settingsCenter.model.actionDuplicate")} onclick={() => duplicate(row.id)}>
                  <Copy aria-hidden="true" />
                </Button>
                <Button variant="ghost" size="icon-sm" disabled={busyId === row.id} title={running ? t("settingsCenter.model.actionStop") : t("settingsCenter.model.actionStart")} aria-label={running ? t("settingsCenter.model.actionStop") : t("settingsCenter.model.actionStart")} onclick={() => void toggleRuntime(row.id, running)}>
                  {#if running}<Square aria-hidden="true" />{:else}<Play aria-hidden="true" />{/if}
                </Button>
                <Button variant="ghost" size="icon-sm" title={t("settingsCenter.model.actionLogs")} aria-label={t("settingsCenter.model.actionLogs")} onclick={() => openLogs(row.id)}>
                  <FileText aria-hidden="true" />
                </Button>
                <Button variant="ghost" size="icon-sm" disabled={!writable} class="models-page__danger" title={t("settingsCenter.model.actionDelete")} aria-label={t("settingsCenter.model.actionDelete")} onclick={() => (deleteId = row.id)}>
                  <Trash2 aria-hidden="true" />
                </Button>
              </Table.Cell>
            </Table.Row>
          {/each}
        </Table.Body>
      </Table.Root>
    </div>
    <p class="models-page__count">{t("settingsCenter.model.count", { count: String(rows.length), total: String(ids.length) })}</p>
  {/if}
</div>

<ModelSettingsDialog
  modelId={editId ?? ""}
  bind:open={modalOpen}
  onSaved={() => void onExternalChange()}
/>
<ModelCreateDialog bind:open={createOpen} onCreated={() => void onExternalChange()} />

<Dialog.Root open={deleteId !== null} onOpenChange={(value) => { if (!value) deleteId = null; }}>
  <Dialog.Content class="models-page__dialog">
    <Dialog.Header>
      <Dialog.Title>{t("settingsCenter.model.deleteTitle")}</Dialog.Title>
      <Dialog.Description>{t("settingsCenter.model.deleteDescription", { id: deleteId ?? "" })}</Dialog.Description>
    </Dialog.Header>
    <Dialog.Footer>
      <Button variant="outline" onclick={() => (deleteId = null)}>{t("common.cancel")}</Button>
      <Button variant="destructive" onclick={confirmDelete}>{t("settingsCenter.model.actionDelete")}</Button>
    </Dialog.Footer>
  </Dialog.Content>
</Dialog.Root>

<style>
  .models-page {
    display: grid;
    gap: 0.8rem;
  }

  .models-page__toolbar {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 0.55rem;
  }

  .models-page__search {
    flex: 1 1 14rem;
    min-inline-size: 10rem;
    max-inline-size: 22rem;
  }

  .models-page__filter {
    inline-size: 9.5rem;
  }

  .models-page__error {
    border: 1px solid color-mix(in oklab, var(--destructive) 45%, transparent);
    border-radius: 0.5rem;
    background: color-mix(in oklab, var(--destructive) 10%, transparent);
    color: var(--destructive);
    padding: 0.55rem 0.75rem;
    font-size: 0.8rem;
  }

  .models-page__table {
    border: 1px solid color-mix(in oklab, var(--border) 82%, transparent);
    border-radius: 0.6rem;
    overflow: auto;
    background: color-mix(in oklab, var(--card) 92%, var(--background));
  }

  .models-page__id {
    border: 0;
    background: transparent;
    padding: 0;
    color: var(--foreground);
    font: inherit;
    font-size: 0.82rem;
    font-weight: 600;
    cursor: pointer;
    text-align: start;
    overflow-wrap: anywhere;
  }

  .models-page__id:hover {
    color: var(--primary);
    text-decoration: underline;
  }

  .models-page__id:focus-visible {
    outline: 2px solid var(--ring);
    outline-offset: 2px;
  }

  .models-page__status {
    display: inline-flex;
    align-items: center;
    gap: 0.4rem;
    font-size: 0.78rem;
    white-space: nowrap;
  }

  .models-page__dot {
    inline-size: 0.5rem;
    block-size: 0.5rem;
    border-radius: 999px;
    background: var(--muted-foreground);
  }

  .models-page__status.is-running .models-page__dot {
    background: var(--success);
  }

  .models-page__status.is-loading .models-page__dot {
    background: var(--warning);
  }

  .models-page__status.is-sleeping .models-page__dot {
    background: var(--info);
  }

  .models-page__actions {
    white-space: nowrap;
  }

  .models-page__actions :global([data-slot="button"]) {
    color: var(--muted-foreground);
  }

  .models-page__actions :global([data-slot="button"]:hover:not(:disabled)) {
    color: var(--foreground);
  }

  .models-page__danger:hover:not(:disabled) {
    color: var(--destructive) !important;
  }

  .models-page__actions-head {
    text-align: end;
  }

  .models-page__count {
    margin: 0;
    color: var(--muted-foreground);
    font-size: 0.75rem;
  }

  :global(.models-page__dialog) {
    max-inline-size: min(26rem, calc(100vw - 2rem));
  }

  @media (max-width: 760px) {
    .models-page__search {
      max-inline-size: none;
    }
  }
</style>
