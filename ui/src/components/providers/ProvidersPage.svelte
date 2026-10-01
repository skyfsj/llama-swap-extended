<script lang="ts">
  import { onMount } from "svelte";
  import { LoaderCircle, Plus, Trash2, Download, FlaskConical, KeyRound } from "@lucide/svelte";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Input } from "$lib/components/ui/input/index.js";
  import { Switch } from "$lib/components/ui/switch/index.js";
  import ConfirmDialog from "../ConfirmDialog.svelte";
  import { translate } from "../../lib/i18n";
  import { errorMessageFromPayload } from "../../lib/apiError";
  import { toast } from "../../lib/toast";
  import {
    fetchConfigSnapshot,
    fetchProvidersStatus,
    probeProvider,
    saveProvidersSection,
    normalizeProvidersSection,
    type ConfigSnapshot,
    type ProviderConfigUI,
    type ProviderModelEntry,
    type ProvidersStatusResponse,
  } from "../../lib/providersApi";

  interface Props {
    onExternalChange?: () => void | Promise<void>;
  }
  let { onExternalChange = () => {} }: Props = $props();

  let providers = $state<Record<string, ProviderConfigUI>>({});
  let snapshot = $state<ConfigSnapshot | null>(null);
  let status = $state<ProvidersStatusResponse | null>(null);
  let loading = $state(true);
  let saving = $state(false);
  let error = $state("");
  let loadRequestID = 0;
  let statusTimer: ReturnType<typeof setInterval> | null = null;

  // Editor dialog state. editingID === null means "creating a new provider".
  let editorOpen = $state(false);
  let editingID = $state<string | null>(null);
  let draftID = $state("");
  let draft = $state<ProviderConfigUI>(emptyProvider());
  let probeBusy = $state(false);
  let probeError = $state("");

  let pendingDelete = $state<string | null>(null);
  let pendingDeleteKey = $state<number | null>(null);

  function emptyProvider(): ProviderConfigUI {
    return {
      type: "openai",
      endpoint: "",
      sticky: true,
      bareNamePolicy: "local-first",
      maxRetries: 2,
      disabled: false,
      apiKeys: [{ value: "" }],
      models: [],
    };
  }

  async function load(): Promise<void> {
    const id = ++loadRequestID;
    loading = true;
    error = "";
    try {
      const snap = await fetchConfigSnapshot();
      if (id !== loadRequestID) return;
      snapshot = snap;
      providers = normalizeProvidersSection(snap.config);
    } catch (cause) {
      if (id === loadRequestID) {
        error = cause instanceof Error ? cause.message : String(cause);
      }
    } finally {
      if (id === loadRequestID) loading = false;
    }
  }

  async function refreshStatus(): Promise<void> {
    try {
      status = await fetchProvidersStatus();
    } catch {
      status = null;
    }
  }

  onMount(() => {
    void load();
    void refreshStatus();
    statusTimer = setInterval(() => void refreshStatus(), 5000);
    return () => {
      loadRequestID++;
      if (statusTimer !== null) clearInterval(statusTimer);
      statusTimer = null;
    };
  });

  function openCreate(): void {
    editingID = null;
    draftID = "";
    draft = emptyProvider();
    probeError = "";
    editorOpen = true;
  }

  function openEdit(providerID: string): void {
    const existing = providers[providerID];
    if (!existing) return;
    editingID = providerID;
    draftID = providerID;
    // Deep-copy so edits stay local until saved; secret values arrive as
    // [REDACTED] sentinels and are restored server-side when untouched.
    draft = JSON.parse(JSON.stringify(existing)) as ProviderConfigUI;
    probeError = "";
    editorOpen = true;
  }

  async function save(): Promise<void> {
    if (!snapshot || saving) return;
    const trimmedID = draftID.trim();
    if (trimmedID === "") {
      probeError = $translate("providers.errors.nameRequired");
      return;
    }
    if (draft.endpoint.trim() === "") {
      probeError = $translate("providers.errors.endpointRequired");
      return;
    }
    if (draft.apiKeys.length === 0) {
      probeError = $translate("providers.errors.keysRequired");
      return;
    }
    if (draft.models.length === 0) {
      probeError = $translate("providers.errors.modelsRequired");
      return;
    }

    const next = { ...providers };
    if (editingID !== null && editingID !== trimmedID) delete next[editingID];
    next[trimmedID] = $state.snapshot(draft);

    saving = true;
    try {
      const saved = await saveProvidersSection(snapshot.etag, next);
      snapshot = saved;
      providers = normalizeProvidersSection(saved.config);
      editorOpen = false;
      toast.success($translate("providers.toast.saved", { id: trimmedID }));
      void onExternalChange();
      void refreshStatus();
    } catch (cause) {
      probeError = errorMessageFromPayload(cause, cause instanceof Error ? cause.message : String(cause));
    } finally {
      saving = false;
    }
  }

  async function confirmDelete(): Promise<void> {
    if (!snapshot || pendingDelete === null) return;
    const target = pendingDelete;
    pendingDelete = null;
    const next = { ...providers };
    delete next[target];
    saving = true;
    try {
      const saved = await saveProvidersSection(snapshot.etag, next);
      snapshot = saved;
      providers = normalizeProvidersSection(saved.config);
      toast.success($translate("providers.toast.deleted", { id: target }));
      void onExternalChange();
      void refreshStatus();
    } catch (cause) {
      error = errorMessageFromPayload(cause, cause instanceof Error ? cause.message : String(cause));
    } finally {
      saving = false;
    }
  }

  function addKey(): void {
    draft.apiKeys = [...draft.apiKeys, { value: "" }];
  }

  function removeKey(index: number): void {
    pendingDeleteKey = index;
  }

  function confirmDeleteKey(): void {
    if (pendingDeleteKey === null) return;
    draft.apiKeys = draft.apiKeys.filter((_, i) => i !== pendingDeleteKey);
    pendingDeleteKey = null;
  }

  function addModel(): void {
    draft.models = [...draft.models, { local: "", remote: "" }] as ProviderModelEntry[];
  }

  function removeModel(index: number): void {
    draft.models = draft.models.filter((_, i) => i !== index);
  }

  function onModelLocal(index: number, value: string): void {
    const next = [...draft.models];
    const entry = { ...next[index] };
    entry.local = value;
    if (entry.remote === "" || entry.remote === undefined) entry.remote = value;
    next[index] = entry;
    draft.models = next as ProviderModelEntry[];
  }

  function onModelRemote(index: number, value: string): void {
    const next = [...draft.models];
    next[index] = { ...next[index], remote: value };
    draft.models = next as ProviderModelEntry[];
  }

  /** Pulls the upstream model list using the draft values (no save needed). */
  async function pullModels(): Promise<void> {
    if (draft.endpoint.trim() === "") {
      probeError = $translate("providers.errors.endpointRequired");
      return;
    }
    probeBusy = true;
    probeError = "";
    try {
      const key = draft.apiKeys.find((k) => k.value && k.value !== "[REDACTED]")?.value
        ?? draft.apiKeys.find((k) => k.valueEnv)?.valueEnv;
      const result = await probeProvider({
        type: draft.type,
        endpoint: draft.endpoint,
        proxy: draft.proxy,
        apiKey: key,
      });
      if (result.models.length === 0) {
        toast.warning($translate("providers.toast.noModels"));
        return;
      }
      // Merge pulled models with existing entries: keep mappings, add new.
      const existing = new Map(draft.models.map((m) => [m.remote, m]));
      for (const remote of result.models) {
        if (!existing.has(remote)) {
          existing.set(remote, { local: remote, remote });
        }
      }
      draft.models = [...existing.values()].sort((a, b) => a.local.localeCompare(b.local));
      toast.success($translate("providers.toast.pulled", { count: result.models.length }));
    } catch (cause) {
      probeError = errorMessageFromPayload(cause, cause instanceof Error ? cause.message : String(cause));
    } finally {
      probeBusy = false;
    }
  }

  /** Connectivity test with the draft's first usable credential. */
  async function testDraft(): Promise<void> {
    probeBusy = true;
    probeError = "";
    try {
      const result = await probeProvider({
        type: draft.type,
        endpoint: draft.endpoint,
        proxy: draft.proxy,
        apiKey: draft.apiKeys.find((k) => k.value && k.value !== "[REDACTED]")?.value ?? "",
      });
      toast.success($translate("providers.toast.testOk", { count: result.models.length }));
    } catch (cause) {
      toast.error(errorMessageFromPayload(cause, cause instanceof Error ? cause.message : String(cause)));
    } finally {
      probeBusy = false;
    }
  }

  /** Runtime summary for one provider from the live status feed. */
  function keySummary(providerID: string): { ok: number; total: number; cooling: number } {
    const route = status?.routes.find((r) => r.providerID === providerID);
    if (!route) return { ok: 0, total: 0, cooling: 0 };
    const now = Date.now();
    let ok = 0;
    let cooling = 0;
    for (const key of route.keys) {
      const coolingNow =
        (key.authCooldownUntil && new Date(key.authCooldownUntil).getTime() > now) ||
        (key.rateLimitCooldownUntil && new Date(key.rateLimitCooldownUntil).getTime() > now) ||
        (key.transientCooldownUntil && new Date(key.transientCooldownUntil).getTime() > now);
      if (key.disabled || key.unresolvedEnv) continue;
      if (coolingNow) {
        cooling++;
      } else {
        ok++;
      }
    }
    return { ok, total: route.keys.length, cooling };
  }

  const providerIDs = $derived(Object.keys(providers).sort((a, b) => a.localeCompare(b)));
</script>

<div class="flex flex-col gap-4">
  <div class="flex items-center justify-between">
    <div>
      <h2 class="text-lg font-semibold">{$translate("providers.title")}</h2>
      <p class="text-sm text-muted-foreground">{$translate("providers.subtitle")}</p>
    </div>
    <Button onclick={openCreate}>
      <Plus aria-hidden="true" />
      {$translate("providers.actions.add")}
    </Button>
  </div>

  {#if error}
    <div class="rounded-md border border-destructive/40 bg-destructive/10 p-3 text-sm" role="alert">
      {error}
    </div>
  {/if}

  {#if loading}
    <div class="flex items-center gap-2 py-8 text-sm text-muted-foreground" role="status" aria-live="polite">
      <LoaderCircle class="size-4 animate-spin" aria-hidden="true" />
      {$translate("controlPlane.loading")}
    </div>
  {:else if providerIDs.length === 0}
    <div class="rounded-md border border-dashed p-8 text-center text-sm text-muted-foreground">
      {$translate("providers.empty")}
    </div>
  {:else}
    <div class="flex flex-col gap-2">
      {#each providerIDs as id (id)}
        {@const summary = keySummary(id)}
        {@const cfg = providers[id]}
        <div class="flex items-center gap-4 rounded-lg border p-4">
          <div class="min-w-0 flex-1">
            <div class="flex items-center gap-2">
              <button
                class="text-sm font-semibold hover:underline"
                onclick={() => openEdit(id)}
              >{id}</button>
              <span class="rounded bg-muted px-1.5 py-0.5 text-xs uppercase">{cfg.type}</span>
              {#if cfg.disabled}
                <span class="rounded bg-destructive/20 px-1.5 py-0.5 text-xs text-destructive">
                  {$translate("providers.status.disabled")}
                </span>
              {/if}
            </div>
            <p class="truncate text-xs text-muted-foreground">{cfg.endpoint}</p>
          </div>
          <div class="flex items-center gap-3 text-xs text-muted-foreground">
            <span class="inline-flex items-center gap-1" title={$translate("providers.fields.apiKeys")}>
              <KeyRound class="size-3.5" aria-hidden="true" />
              {#if summary.total > 0}
                {summary.ok}/{summary.total}{#if summary.cooling > 0}&nbsp;({summary.cooling} {$translate("providers.status.coolingShort")}){/if}
              {:else}
                {cfg.apiKeys.length}
              {/if}
            </span>
            <span>{$translate("providers.status.models")}: {cfg.models.length}</span>
          </div>
          <div class="flex items-center gap-1">
            <Button variant="ghost" size="sm" onclick={() => openEdit(id)}>
              {$translate("common.view")}
            </Button>
            <Button variant="ghost" size="sm" onclick={() => (pendingDelete = id)} aria-label={$translate("common.delete")}>
              <Trash2 class="size-4 text-destructive" aria-hidden="true" />
            </Button>
          </div>
        </div>
      {/each}
    </div>
  {/if}
</div>

<Dialog.Root bind:open={editorOpen}>
  <Dialog.Content class="max-h-[calc(100dvh-1.5rem)] max-w-3xl overflow-y-auto sm:max-w-3xl">
      <Dialog.Header>
        <Dialog.Title>
          {editingID === null ? $translate("providers.editor.createTitle") : $translate("providers.editor.editTitle", { id: editingID })}
        </Dialog.Title>
      </Dialog.Header>

      <div class="flex flex-col gap-4">
        <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
          <label class="flex flex-col gap-1.5">
            <span class="text-sm font-medium">{$translate("providers.fields.name")}</span>
            <Input bind:value={draftID} disabled={editingID !== null} placeholder="deepseek" />
          </label>
          <label class="flex flex-col gap-1.5">
            <span class="text-sm font-medium">{$translate("providers.fields.type")}</span>
            <select class="h-9 rounded-md border bg-transparent px-3 text-sm" bind:value={draft.type}>
              <option value="openai">OpenAI</option>
              <option value="anthropic">Anthropic</option>
            </select>
          </label>
        </div>

        <label class="flex flex-col gap-1.5">
          <span class="text-sm font-medium">{$translate("providers.fields.endpoint")}</span>
          <Input bind:value={draft.endpoint} placeholder="https://api.deepseek.com" />
        </label>

        <div class="grid grid-cols-1 gap-4 sm:grid-cols-3">
          <label class="flex flex-col gap-1.5">
            <span class="text-sm font-medium">{$translate("providers.fields.proxy")}</span>
            <Input bind:value={draft.proxy} placeholder="http://..." />
          </label>
          <label class="flex flex-col gap-1.5">
            <span class="text-sm font-medium">{$translate("providers.fields.priority")}</span>
            <Input type="number" min="0" bind:value={draft.priority} />
          </label>
          <label class="flex flex-col gap-1.5">
            <span class="text-sm font-medium">{$translate("providers.fields.weight")}</span>
            <Input type="number" min="0" bind:value={draft.weight} />
          </label>
        </div>

        <div class="grid grid-cols-1 gap-4 sm:grid-cols-3">
          <label class="flex flex-col gap-1.5">
            <span class="text-sm font-medium">{$translate("providers.fields.bareNamePolicy")}</span>
            <select class="h-9 rounded-md border bg-transparent px-3 text-sm" bind:value={draft.bareNamePolicy}>
              <option value="local-first">{$translate("providers.fields.localFirst")}</option>
              <option value="provider-first">{$translate("providers.fields.providerFirst")}</option>
            </select>
          </label>
          <label class="flex flex-col gap-1.5">
            <span class="text-sm font-medium">{$translate("providers.fields.maxRetries")}</span>
            <Input type="number" min="0" bind:value={draft.maxRetries} />
          </label>
          <div class="flex items-end gap-6 pb-1.5">
            <label class="flex items-center gap-2 text-sm">
              <Switch bind:checked={draft.sticky} />
              {$translate("providers.fields.sticky")}
            </label>
            <label class="flex items-center gap-2 text-sm">
              <Switch bind:checked={draft.disabled} />
              {$translate("providers.fields.disabled")}
            </label>
          </div>
        </div>

        <div class="rounded-lg border p-3">
          <div class="mb-2 flex items-center justify-between">
            <h3 class="text-sm font-semibold">{$translate("providers.fields.apiKeys")}</h3>
            <Button variant="outline" size="sm" onclick={addKey}>
              <Plus class="size-3.5" aria-hidden="true" />
              {$translate("providers.actions.addKey")}
            </Button>
          </div>
          <div class="flex flex-col gap-2">
            {#each draft.apiKeys as key, i}
              <div class="grid grid-cols-1 items-center gap-2 sm:grid-cols-[2fr_1fr_1fr_1fr_auto]">
                <Input
                  type="password"
                  placeholder={$translate("providers.fields.keyValueOrEnv")}
                  value={key.value ?? key.valueEnv !== undefined ? `env:${key.valueEnv}` : ""}
                  onchange={(e) => {
                    const raw = e.currentTarget.value;
                    const next = [...draft.apiKeys];
                    if (raw.startsWith("env:")) {
                      next[i] = { ...key, value: undefined, valueEnv: raw.slice(4) };
                    } else {
                      next[i] = { ...key, value: raw, valueEnv: undefined };
                    }
                    draft.apiKeys = next;
                  }}
                />
                <Input
                  type="number"
                  min="0"
                  placeholder={$translate("providers.fields.weight")}
                  value={key.weight ?? ""}
                  onchange={(e) => {
                    const next = [...draft.apiKeys];
                    next[i] = { ...key, weight: e.currentTarget.valueAsNumber || 0 };
                    draft.apiKeys = next;
                  }}
                />
                <Input
                  type="number"
                  min="0"
                  placeholder={$translate("providers.fields.priority")}
                  value={key.priority ?? ""}
                  onchange={(e) => {
                    const next = [...draft.apiKeys];
                    next[i] = { ...key, priority: e.currentTarget.valueAsNumber || 0 };
                    draft.apiKeys = next;
                  }}
                />
                <Input
                  type="number"
                  min="0"
                  placeholder={$translate("providers.fields.maxConcurrent")}
                  value={key.maxConcurrent ?? ""}
                  onchange={(e) => {
                    const next = [...draft.apiKeys];
                    next[i] = { ...key, maxConcurrent: e.currentTarget.valueAsNumber || 0 };
                    draft.apiKeys = next;
                  }}
                />
                <Button variant="ghost" size="sm" onclick={() => removeKey(i)} aria-label={$translate("common.delete")}>
                  <Trash2 class="size-4 text-destructive" aria-hidden="true" />
                </Button>
              </div>
            {/each}
          </div>
        </div>

        <div class="rounded-lg border p-3">
          <div class="mb-2 flex items-center justify-between">
            <h3 class="text-sm font-semibold">{$translate("providers.fields.models")}</h3>
            <div class="flex gap-2">
              <Button variant="outline" size="sm" onclick={() => void testDraft()} disabled={probeBusy}>
                <FlaskConical class="size-3.5" aria-hidden="true" />
                {$translate("providers.actions.test")}
              </Button>
              <Button variant="outline" size="sm" onclick={() => void pullModels()} disabled={probeBusy}>
                {#if probeBusy}
                  <LoaderCircle class="size-3.5 animate-spin" aria-hidden="true" />
                {:else}
                  <Download class="size-3.5" aria-hidden="true" />
                {/if}
                {$translate("providers.actions.pull")}
              </Button>
              <Button variant="outline" size="sm" onclick={addModel}>
                <Plus class="size-3.5" aria-hidden="true" />
                {$translate("providers.actions.addModel")}
              </Button>
            </div>
          </div>
          <div class="flex flex-col gap-2">
            {#each draft.models as model, i}
              <div class="grid grid-cols-2 items-center gap-2">
                <Input
                  placeholder={$translate("providers.fields.modelLocal")}
                  value={model.local}
                  onchange={(e) => onModelLocal(i, e.currentTarget.value)}
                />
                <div class="flex gap-2">
                  <Input
                    placeholder={$translate("providers.fields.modelRemote")}
                    value={model.remote}
                    onchange={(e) => onModelRemote(i, e.currentTarget.value)}
                  />
                  <Button variant="ghost" size="sm" onclick={() => removeModel(i)} aria-label={$translate("common.delete")}>
                    <Trash2 class="size-4 text-destructive" aria-hidden="true" />
                  </Button>
                </div>
              </div>
            {/each}
            {#if draft.models.length === 0}
              <p class="text-xs text-muted-foreground">{$translate("providers.editor.noModels")}</p>
            {/if}
          </div>
        </div>

        {#if probeError}
          <div class="rounded-md border border-destructive/40 bg-destructive/10 p-3 text-sm" role="alert">
            {probeError}
          </div>
        {/if}
      </div>

      <Dialog.Footer>
        <Button variant="outline" onclick={() => (editorOpen = false)}>
          {$translate("common.cancel")}
        </Button>
        <Button onclick={() => void save()} disabled={saving}>
          {#if saving}
            <LoaderCircle class="size-4 animate-spin" aria-hidden="true" />
          {/if}
          {$translate("common.save")}
        </Button>
      </Dialog.Footer>
  </Dialog.Content>
</Dialog.Root>

{#if pendingDelete !== null}
  <ConfirmDialog
    open={true}
    title={$translate("providers.delete.title")}
    message={$translate("providers.delete.message", { id: pendingDelete })}
    confirmLabel={$translate("common.delete")}
    onConfirm={() => void confirmDelete()}
  />
{/if}
{#if pendingDeleteKey !== null}
  <ConfirmDialog
    open={true}
    title={$translate("providers.deleteKey.title")}
    message={$translate("providers.deleteKey.message")}
    confirmLabel={$translate("common.delete")}
    onConfirm={confirmDeleteKey}
  />
{/if}
