<script lang="ts">
  import { AlertTriangle, Check, LoaderCircle, Plus, Save, Trash2, X } from "@lucide/svelte";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Input } from "$lib/components/ui/input/index.js";
  import { translate } from "../../lib/i18n";
  import { errorMessageFromPayload } from "../../lib/apiError";
  import { defaultForSchema, resolveSchema, schemaFieldLabel, topLevelSchema, type SchemaNode } from "../../lib/configSchema";
  import { pointerToken, record, summarizeChanges, type ConfigChange } from "../../lib/modelConfig";
  import SchemaField from "./SchemaField.svelte";

  interface ConfigSource { path: string; writable: boolean; managed?: boolean; }
  interface ConfigSnapshot {
    config?: Record<string, unknown>;
    yaml: string;
    etag: string;
    writable: boolean;
    sources?: ConfigSource[];
    ownership?: Record<string, string>;
  }
  interface DomainDefinition { key: string; title: string; description: string; collection?: boolean; }
  interface Validation {
    valid: boolean;
    issues?: { path: string; message: string }[];
    diff?: unknown[];
    restartRequired?: boolean;
  }
  interface Props {
    snapshot: ConfigSnapshot;
    domains: DomainDefinition[];
    onSnapshot: (snapshot: ConfigSnapshot) => void;
    title: string;
  }

  let { snapshot, domains, onSnapshot, title }: Props = $props();
  let selectedKey = $state("");
  let selectedEntity = $state("");
  let newEntityName = $state("");
  let creating = $state(false);
  let showNewEntity = $state(false);
  let value = $state<unknown>({});
  let loadedTarget = $state("");
  let loadedETag = $state("");
  let saving = $state(false);
  let error = $state("");
  let validation = $state<Validation | null>(null);
  let confirmOpen = $state(false);
  let pendingPatch = $state<unknown[]>([]);
  let pendingETag = $state("");
  let pendingPath = $state("");
  let pendingChanges = $state<ConfigChange[]>([]);

  let selected = $derived(domains.find((domain) => domain.key === selectedKey) ?? domains[0]);
  let domainSchema = $derived(topLevelSchema(selectedKey));
  let collectionSchema = $derived(resolveSchema(domainSchema).additionalProperties as SchemaNode | undefined);
  let editorSchema = $derived(selected?.collection ? collectionSchema ?? true : domainSchema);
  let entries = $derived(record(snapshot.config?.[selectedKey]));
  let modelOptions = $derived(Object.keys(record(snapshot.config?.models)).sort((left, right) => left.localeCompare(right, undefined, { numeric: true })));
  let entityKeys = $derived(Object.keys(entries).sort((left, right) => left.localeCompare(right, undefined, { numeric: true })));
  let targetPath = $derived(selected?.collection
    ? selectedEntity ? `/${selectedKey}/${pointerToken(selectedEntity)}` : `/${selectedKey}`
    : `/${selectedKey}`);

  function copyConfigValue<T>(input: T): T {
    return JSON.parse(JSON.stringify(input)) as T;
  }

  $effect(() => {
    if (!domains.some((domain) => domain.key === selectedKey)) {
      selectedKey = domains[0]?.key ?? "";
      return;
    }
    if (selected?.collection && !creating && !entityKeys.includes(selectedEntity)) {
      selectedEntity = entityKeys[0] ?? "";
      return;
    }
    const target = selected?.collection ? `${selectedKey}/${selectedEntity}/${creating}` : selectedKey;
    if (selectedKey && target && (loadedTarget !== target || loadedETag !== snapshot.etag)) {
      loadedTarget = target;
      loadedETag = snapshot.etag;
      const current = selected?.collection ? entries[selectedEntity] : snapshot.config?.[selectedKey];
      value = current === undefined ? defaultForSchema(editorSchema) : copyConfigValue(current);
      error = "";
      validation = null;
    }
  });

  function selectEntity(id: string): void {
    creating = false;
    showNewEntity = false;
    selectedEntity = id;
    newEntityName = "";
  }

  function startCreate(): void {
    const id = newEntityName.trim();
    if (!id) return;
    if (Object.prototype.hasOwnProperty.call(entries, id)) {
      error = $translate("controlPlane.domainDuplicateEntity");
      return;
    }
    creating = true;
    showNewEntity = false;
    selectedEntity = id;
    loadedTarget = "";
    newEntityName = "";
  }

  function resetPending(): void {
    pendingPatch = [];
    pendingETag = "";
    pendingPath = "";
    pendingChanges = [];
  }

  function prepareSave(): void {
    if (!selectedKey || !snapshot.writable || saving || (selected?.collection && !selectedEntity)) return;
    error = "";
    validation = null;
    const current = selected?.collection ? entries[selectedEntity] : snapshot.config?.[selectedKey];
    if (!creating && JSON.stringify(current ?? {}) === JSON.stringify(value)) {
      error = $translate("controlPlane.domainNoChanges");
      return;
    }
    const rootExists = Object.prototype.hasOwnProperty.call(snapshot.config ?? {}, selectedKey);
    let patch: unknown[];
    if (selected?.collection && creating && !rootExists) {
      patch = [{ op: "add", path: `/${selectedKey}`, value: { [selectedEntity]: copyConfigValue(value) } }];
    } else {
      const exists = selected?.collection ? Object.prototype.hasOwnProperty.call(entries, selectedEntity) : rootExists;
      patch = [{ op: exists ? "replace" : "add", path: targetPath, value: copyConfigValue(value) }];
    }
    preparePatch(patch, targetPath, summarizeChanges(current, value, targetPath));
  }

  function prepareDelete(): void {
    if (!selected?.collection || creating || !selectedEntity || !Object.prototype.hasOwnProperty.call(entries, selectedEntity)) return;
    preparePatch([{ op: "remove", path: targetPath }], targetPath, summarizeChanges(entries[selectedEntity], undefined, targetPath));
  }

  function preparePatch(patch: unknown[], path: string, changes: ConfigChange[]): void {
    pendingPatch = patch;
    pendingETag = snapshot.etag;
    pendingPath = path;
    pendingChanges = changes;
    void validatePatch(patch);
  }

  function displayValue(item: unknown): string {
    if (item === undefined) return $translate("controlPlane.configValueEmpty");
    if (typeof item === "boolean") return item ? $translate("common.yes") : $translate("common.no");
    if (typeof item === "object" && item !== null) return JSON.stringify(item);
    return String(item);
  }

  function changeLabel(path: string): string {
    const key = path.split(/[./]/).filter(Boolean).pop() ?? path;
    return schemaFieldLabel(key, $translate);
  }

  async function validatePatch(patch: unknown[]): Promise<void> {
    saving = true;
    try {
      const response = await fetch("/api/config/validate", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(patch),
      });
      const payload = await response.json().catch(() => ({})) as Validation;
      if (!response.ok) throw new Error(errorMessageFromPayload(payload, `HTTP ${response.status}`));
      validation = payload;
      if (!payload.valid) { resetPending(); return; }
      confirmOpen = true;
    } catch (cause) {
      error = cause instanceof Error ? cause.message : String(cause);
      resetPending();
    } finally {
      saving = false;
    }
  }

  function cancelSave(): void {
    if (saving) return;
    confirmOpen = false;
    validation = null;
    resetPending();
  }

  async function confirmSave(): Promise<void> {
    if (pendingPatch.length === 0 || !pendingETag || saving) return;
    saving = true;
    error = "";
    try {
      const response = await fetch("/api/config", {
        method: "PATCH",
        headers: { "Content-Type": "application/json", "If-Match": pendingETag },
        body: JSON.stringify(pendingPatch),
      });
      const payload: unknown = await response.json().catch(() => ({}));
      if (response.status === 412) throw new Error($translate("controlPlane.conflict"));
      if (!response.ok) throw new Error(errorMessageFromPayload(payload, `HTTP ${response.status}`));
      onSnapshot(payload as ConfigSnapshot);
      creating = false;
      confirmOpen = false;
      validation = null;
      resetPending();
    } catch (cause) {
      error = cause instanceof Error ? cause.message : String(cause);
      confirmOpen = false;
      resetPending();
    } finally {
      saving = false;
    }
  }
</script>

<section class="border-border/70 overflow-hidden rounded-md border" aria-label={title}>
  <div class="grid min-h-[30rem] md:grid-cols-[13rem_minmax(0,1fr)]">
    <nav class="border-border/70 bg-muted/10 border-b p-2 md:border-r md:border-b-0" aria-label={title}>
      {#each domains as domain (domain.key)}
        <button type="button" class={`mb-0.5 block h-9 w-full rounded-md px-3 text-left text-sm transition-colors ${selectedKey === domain.key ? "bg-muted font-medium text-foreground" : "text-muted-foreground hover:bg-muted/60 hover:text-foreground"}`} onclick={() => { selectedKey = domain.key; creating = false; showNewEntity = false; selectedEntity = ""; loadedTarget = ""; }}>
          {domain.title}
        </button>
      {/each}
    </nav>
    <form class="min-w-0 p-4" onsubmit={(event) => { event.preventDefault(); prepareSave(); }}>
      {#if selected}
        <div class="border-border/70 mb-4 flex min-h-9 flex-wrap items-center gap-2 border-b pb-3">
          <h2 class="text-sm font-semibold">{selected.title}</h2>
          {#if selected.collection}<Button class="ml-auto" type="button" variant="outline" size="icon-sm" title={$translate("controlPlane.domainCreateEntity")} onclick={() => (showNewEntity = true)}><Plus class="size-3.5" aria-hidden="true" /></Button>{/if}
        </div>

        {#if selected.collection}
          <div class="mb-4 flex flex-wrap items-center gap-2">
            <select class="border-input bg-background h-9 min-w-52 rounded-md border pr-9 pl-3 text-sm" aria-label={$translate("controlPlane.domainEntity")} value={creating ? "" : selectedEntity} onchange={(event) => selectEntity(event.currentTarget.value)} disabled={entityKeys.length === 0}>{#if entityKeys.length === 0}<option value="">{$translate("controlPlane.domainNoEntities")}</option>{/if}{#each entityKeys as id (id)}<option value={id}>{id}</option>{/each}</select>
            {#if showNewEntity}
              <Input class="h-9 min-w-52 flex-1" aria-label={$translate("controlPlane.domainNewEntity")} bind:value={newEntityName} autocomplete="off" autofocus />
              <Button type="button" onclick={startCreate} disabled={!newEntityName.trim()}>{$translate("controlPlane.domainCreateEntity")}</Button>
              <Button type="button" variant="ghost" size="icon-sm" title={$translate("common.cancel")} onclick={() => { showNewEntity = false; newEntityName = ""; }}><X class="size-3.5" aria-hidden="true" /></Button>
            {/if}
          </div>
        {/if}

        {#if error}<div class="border-destructive/40 bg-destructive/10 mb-3 rounded-md border p-3 text-sm" role="alert">{error}</div>{/if}
        {#if validation && !validation.valid}<div class="border-destructive/40 bg-destructive/10 mb-3 rounded-md border p-3 text-xs" role="alert">{#each validation.issues ?? [] as issue}<div>{issue.path}: {issue.message}</div>{/each}</div>{/if}

        {#if !selected.collection || selectedEntity}
          {#if selected.collection}<div class="border-border/70 bg-muted/20 mb-4 flex items-center gap-2 rounded-md border px-3 py-2"><code class="min-w-0 flex-1 truncate text-sm">{selectedEntity}</code>{#if creating}<span class="text-primary text-xs font-medium">{$translate("controlPlane.domainUnsavedEntity")}</span>{:else}<Button type="button" variant="ghost" size="icon-sm" title={$translate("controlPlane.domainDeleteEntity")} onclick={prepareDelete}><Trash2 class="size-3.5" aria-hidden="true" /></Button>{/if}</div>{/if}
          <fieldset disabled={saving || confirmOpen || !snapshot.writable}>
            {#key `${selectedKey}:${selectedEntity}:${creating}:${loadedETag}`}
              <SchemaField schema={editorSchema} {value} label="" fieldPath={selectedKey} {modelOptions} onChange={(next) => (value = next)} />
            {/key}
          </fieldset>
          <div class="border-border/70 mt-4 flex justify-end border-t pt-4"><Button type="submit" disabled={saving || !snapshot.writable}>{#if saving}<LoaderCircle class="size-4 animate-spin" aria-hidden="true" />{:else}<Save class="size-4" aria-hidden="true" />{/if}{$translate("controlPlane.domainSave")}</Button></div>
        {/if}
      {/if}
    </form>
  </div>
</section>

<Dialog.Root bind:open={confirmOpen}>
  <Dialog.Content class="max-h-[calc(100vh-2rem)] max-w-2xl overflow-y-auto sm:max-w-2xl" showCloseButton={!saving}>
    <Dialog.Header><Dialog.Title class="flex items-center gap-2"><Check class="text-success size-4" aria-hidden="true" />{$translate("controlPlane.configDiffTitle")}</Dialog.Title><Dialog.Description>{$translate("controlPlane.configDiffDescription")}</Dialog.Description></Dialog.Header>
    <div class="border-border/70 bg-muted/20 rounded-md border px-3 py-2"><code class="text-sm">{pendingPath}</code></div>
    <div class="border-border/70 divide-border/70 overflow-hidden rounded-md border">
      {#each pendingChanges as change (change.path)}
        <div class="grid gap-2 border-b p-3 last:border-b-0 sm:grid-cols-[minmax(0,0.8fr)_minmax(0,1.2fr)]">
          <span class="min-w-0 text-sm font-medium">{changeLabel(change.path)}</span>
          <div class="grid min-w-0 gap-1 text-xs sm:grid-cols-[minmax(0,1fr)_auto_minmax(0,1fr)]">
            <span class="text-muted-foreground truncate" title={displayValue(change.before)}>{change.kind === "added" ? "" : displayValue(change.before)}</span>
            {#if change.kind === "changed"}<span class="text-muted-foreground">→</span>{/if}
            <span class="truncate" title={displayValue(change.after)}>{change.kind === "removed" ? "" : displayValue(change.after)}</span>
          </div>
        </div>
      {/each}
    </div>
    {#if validation?.restartRequired}<p class="border-warning/30 bg-warning/10 text-warning rounded-md border px-3 py-2 text-sm"><AlertTriangle class="mr-1 inline size-4" aria-hidden="true" />{$translate("controlPlane.restartRequired")}</p>{/if}
    <Dialog.Footer><Button variant="outline" onclick={cancelSave} disabled={saving}>{$translate("common.cancel")}</Button><Button onclick={() => void confirmSave()} disabled={saving || pendingPatch.length === 0}>{#if saving}<LoaderCircle class="size-4 animate-spin" aria-hidden="true" />{/if}{$translate("controlPlane.configDiffSave")}</Button></Dialog.Footer>
  </Dialog.Content>
</Dialog.Root>
