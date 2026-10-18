<script lang="ts">
  import { onMount } from "svelte";
  import { ChevronDown, ChevronRight, KeyRound, LoaderCircle, Pencil, Plus, RotateCcw, Trash2 } from "@lucide/svelte";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import * as Select from "$lib/components/ui/select/index.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import ConfirmDialog from "../components/ConfirmDialog.svelte";
  import { fetchPlaygroundModels, playgroundModels } from "../stores/api";
  import { translate } from "../lib/i18n";
  import ModelMultiSelect from "../components/ModelMultiSelect.svelte";
  import { ipMatchesEntry } from "../lib/ipMatch";
  import {
    createAPIKey,
    fetchAPIKeys,
    revokeAPIKey,
    rotateAPIKey,
    updateAPIKey,
    type APIKey,
    type ApiKeyKind,
  } from "../lib/apiKeys";

  type ExpiryMode = "unlimited" | "custom";
  type PendingAction = { kind: "delete" | "rotate"; id: string; name: string };

  interface Draft {
    id: string;
    name: string;
    models: string[];
    allowManagementLogin: boolean;
    // Access-key only fields.
    parentId: string;
    allowedIps: string;
    maxConcurrency: string;
    group: string;
    expiryMode: ExpiryMode;
    expiresAt: string;
  }

  const emptyDraft = (): Draft => ({
    id: "",
    name: "",
    models: [],
    allowManagementLogin: false,
    parentId: "",
    allowedIps: "",
    maxConcurrency: "0",
    group: "",
    expiryMode: "unlimited",
    expiresAt: "",
  });

  let keys = $state<APIKey[]>([]);
  let expanded = $state<Record<string, boolean>>({});
  let dialogKind = $state<ApiKeyKind>("management");
  let editing = $state<Draft | null>(null);
  let createDialogOpen = $state(false);
  let editDialogOpen = $state(false);
  let pendingAction = $state<PendingAction | null>(null);
  let newlyCreated = $state("");
  let secretDialogOpen = $state(false);
  let loading = $state(true);
  let saving = $state(false);
  let error = $state("");
  let loadRequestID = 0;

  let managementKeys = $derived(
    keys.filter((key) => key.kind === "management" && !key.revokedAt).sort((a, b) => a.id.localeCompare(b.id)),
  );
  let accessKeysByParent = $derived.by(() => {
    const grouped = new Map<string, APIKey[]>();
    for (const key of keys) {
      if (key.kind !== "access" || !key.parentId) continue;
      const list = grouped.get(key.parentId) ?? [];
      list.push(key);
      grouped.set(key.parentId, list);
    }
    return grouped;
  });

  // Model groups for the allowlist picker: each model followed by the alias
  // names that resolve to it, so an old name reads as one of the model's
  // routing names instead of a model of its own.
  let modelGroups = $derived.by(() => {
    const seen = new Set<string>();
    const groups: { model: string; aliases: string[] }[] = [];
    const ordered = [...$playgroundModels].sort((a, b) =>
      (a.id ?? "").localeCompare(b.id ?? "", undefined, { numeric: true })
    );
    for (const model of ordered) {
      const id = (model.id ?? "").trim();
      if (!id || seen.has(id)) continue;
      seen.add(id);
      const aliases: string[] = [];
      for (const alias of model.aliases ?? []) {
        const name = (alias ?? "").trim();
        if (!name || seen.has(name)) continue;
        seen.add(name);
        aliases.push(name);
      }
      groups.push({ model: id, aliases });
    }
    return groups;
  });

  // The access-key dialog shows the parent management key read-only: the
  // nesting is fixed at creation time.
  let parentKeyLabel = $derived.by(() => {
    if (!editing) return "";
    const parentKey = managementKeys.find((key) => key.id === editing?.parentId);
    return parentKey?.name || parentKey?.id || editing.parentId;
  });

  // A key the configuration file declares is re-asserted on every config
  // change, so editing it here would be undone: the panel shows it read-only
  // and points at where it is edited.
  function configManaged(key: APIKey): boolean {
    return key.source === "config";
  }

  // matchedAllowedIP finds the allowlist entry a recorded address falls under,
  // so the table can show whether the key's last caller would still be allowed
  // (an address outside every entry is the interesting case: it means the key
  // was used before the allowlist tightened, or by a denied caller).
  function matchedAllowedIP(key: APIKey): string {
    const ip = key.lastIp ?? "";
    if (ip === "" || key.allowedIps.length === 0) return "";
    return key.allowedIps.find((entry) => ipMatchesEntry(entry, ip)) ?? "";
  }

  function accessKeysFor(parentId: string): APIKey[] {
    return (accessKeysByParent.get(parentId) ?? []).filter((key) => !key.revokedAt);
  }

  function formatDate(value?: string): string {
    if (!value) return "—";
    const parsed = new Date(value);
    return Number.isNaN(parsed.getTime()) ? value : parsed.toLocaleString();
  }

  function localDate(value: string): string {
    const parsed = new Date(value);
    if (Number.isNaN(parsed.getTime())) return "";
    const offset = parsed.getTimezoneOffset() * 60_000;
    return new Date(parsed.getTime() - offset).toISOString().slice(0, 10);
  }

  function expiryValue(value: string, mode: ExpiryMode): string | null {
    if (mode === "unlimited") return null;
    const trimmed = value.trim();
    if (trimmed === "") throw new Error($translate("controlPlane.keyExpiryRequired"));
    const parsed = /^\d{4}-\d{2}-\d{2}$/.test(trimmed)
      ? new Date(`${trimmed}T23:59:59`)
      : new Date(trimmed);
    if (Number.isNaN(parsed.getTime())) throw new Error("invalid expiration time");
    return parsed.toISOString();
  }

  function splitIPs(value: string): string[] {
    return value
      .split(/[\n,]/)
      .map((entry) => entry.trim())
      .filter((entry) => entry.length > 0);
  }

  function concurrencyLabel(key: APIKey): string {
    if (key.maxConcurrency <= 0) return $translate("controlPlane.keyConcurrencyUnlimited");
    return `${key.maxConcurrency}`;
  }

  async function load(): Promise<void> {
    const requestID = ++loadRequestID;
    loading = true;
    error = "";
    try {
      const records = await fetchAPIKeys();
      if (requestID !== loadRequestID) return;
      keys = records;
      // The first management key starts expanded so the nested access keys are
      // visible without an extra click, matching how the page is usually used.
      const first = managementKeys[0];
      if (first && !(first.id in expanded)) expanded[first.id] = true;
    } catch (cause) {
      if (requestID === loadRequestID) error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (requestID === loadRequestID) loading = false;
    }
  }

  function openCreate(kind: ApiKeyKind, parentId = ""): void {
    if (saving) return;
    dialogKind = kind;
    editing = { ...emptyDraft(), parentId };
    // Management keys are for signing in and administering the control plane,
    // so the panel always creates them with panel access (an API-only
    // management key can still be made through the API).
    editing.allowManagementLogin = kind === "management";
    // Opening the nested list up front means the new access key is visible as
    // soon as the dialog closes.
    if (parentId) expanded[parentId] = true;
    error = "";
    createDialogOpen = true;
  }

  function openEdit(key: APIKey): void {
    if (saving) return;
    dialogKind = key.kind;
    editing = {
      id: key.id,
      name: key.name,
      models: [...key.models],
      allowManagementLogin: key.allowManagementLogin,
      parentId: key.parentId ?? "",
      allowedIps: key.allowedIps.join("\n"),
      maxConcurrency: String(key.maxConcurrency ?? 0),
      group: key.group ?? "",
      expiryMode: key.expiresAt ? "custom" : "unlimited",
      expiresAt: key.expiresAt ? localDate(key.expiresAt) : "",
    };
    error = "";
    editDialogOpen = true;
  }

  async function submitDraft(): Promise<void> {
    const draft = editing;
    if (!draft || saving) return;
    saving = true;
    error = "";
    try {
      const expiry = expiryValue(draft.expiresAt, draft.expiryMode);
      const common = {
        name: draft.name.trim(),
        models: draft.models,
        expiresAt: expiry,
      };
      let secret = "";
      if (createDialogOpen) {
        const created =
          dialogKind === "management"
            ? await createAPIKey({ ...common, kind: "management", allowManagementLogin: draft.allowManagementLogin })
            : await createAPIKey({
                ...common,
                kind: "access",
                parentId: draft.parentId,
                allowedIps: splitIPs(draft.allowedIps),
                maxConcurrency: Number(draft.maxConcurrency) || 0,
                group: draft.group.trim(),
              });
        secret = created.key;
      } else {
        const accessFields =
          dialogKind === "access"
            ? {
                allowedIps: splitIPs(draft.allowedIps),
                maxConcurrency: Number(draft.maxConcurrency) || 0,
                group: draft.group.trim(),
              }
            : { allowManagementLogin: draft.allowManagementLogin };
        await updateAPIKey(draft.id, { ...common, ...accessFields });
      }
      createDialogOpen = false;
      editDialogOpen = false;
      editing = null;
      await load();
      if (secret) {
        newlyCreated = secret;
        secretDialogOpen = true;
      }
    } catch (cause) {
      error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      saving = false;
    }
  }

  function requestAction(kind: PendingAction["kind"], key: APIKey): void {
    if (saving) return;
    error = "";
    pendingAction = { kind, id: key.id, name: key.name || key.id };
  }

  async function confirmAction(): Promise<void> {
    const action = pendingAction;
    if (!action || saving) return;
    saving = true;
    error = "";
    try {
      const secret = action.kind === "delete" ? "" : (await rotateAPIKey(action.id)).key;
      if (action.kind === "delete") await revokeAPIKey(action.id);
      pendingAction = null;
      await load();
      if (secret) {
        newlyCreated = secret;
        secretDialogOpen = true;
      }
    } catch (cause) {
      error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      saving = false;
    }
  }

  async function copySecret(): Promise<void> {
    if (!newlyCreated) return;
    try {
      await navigator.clipboard?.writeText(newlyCreated);
    } catch (cause) {
      error = cause instanceof Error ? cause.message : String(cause);
    }
  }

  function closeSecret(): void {
    secretDialogOpen = false;
    newlyCreated = "";
  }

  $effect(() => {
    if (!secretDialogOpen && newlyCreated) newlyCreated = "";
  });

  $effect(() => {
    // Release the draft once no dialog is using it. The create dialog shares
    // `editing` with the edit dialog, so testing only editDialogOpen threw the
    // draft away the moment the create dialog opened and left it blank.
    if (!createDialogOpen && !editDialogOpen && editing && !saving) editing = null;
  });

  onMount(() => {
    void load();
    void fetchPlaygroundModels();
  });
</script>

<section class="space-y-6" aria-labelledby="keys-title">
  <header class="flex flex-wrap items-center justify-between gap-3">
    <div>
      <h3 id="keys-title" class="text-lg font-semibold">{$translate("controlPlane.keyTitle")}</h3>
      <p class="text-muted-foreground mt-1 text-sm">{$translate("controlPlane.keyDescription")}</p>
    </div>
    <div class="flex shrink-0 items-center gap-2">
      <Button variant="outline" size="sm" onclick={() => openCreate("management")} disabled={saving}>
        <Plus class="size-3.5" aria-hidden="true" />
        {$translate("controlPlane.keyCreateManagement")}
      </Button>
      <Button variant="outline" size="sm" onclick={() => void load()} disabled={loading || saving}>
        {$translate("controlPlane.refresh")}
      </Button>
    </div>
  </header>

  {#if error}
    <div class="rounded-lg border border-destructive/40 bg-destructive/10 p-3 text-sm" role="alert">
      {$translate("controlPlane.error", { message: error })}
    </div>
  {/if}

  {#if loading}
    <p class="text-muted-foreground text-sm" role="status" aria-live="polite">{$translate("controlPlane.loading")}</p>
  {:else if managementKeys.length === 0}
    <div class="rounded-xl border border-dashed bg-card/50 p-10 text-center" role="status">
      <KeyRound class="text-muted-foreground mx-auto size-8" aria-hidden="true" />
      <p class="mt-3 font-medium">{$translate("controlPlane.keyManagementEmpty")}</p>
      <p class="text-muted-foreground mt-1 text-sm">{$translate("controlPlane.keyManagementEmptyHint")}</p>
      <Button class="mt-4" onclick={() => openCreate("management")} disabled={saving}>
        <Plus class="size-4" aria-hidden="true" />
        {$translate("controlPlane.keyCreateManagement")}
      </Button>
    </div>
  {:else}
    <!-- One table for every management key. Rendering a table per key repeated
         the header and the empty-access note once per parent and pushed the
         row actions onto a second line. -->
    <div class="overflow-hidden rounded-xl border bg-card">
      <table class="keys-table w-full table-fixed text-sm">
        <colgroup>
          <col />
          <col class="w-[13rem]" />
          <col class="w-[5.5rem]" />
          <col class="w-[9rem]" />
        </colgroup>
        <thead class="bg-muted/40 text-left">
          <tr>
            <th class="px-3 py-2.5">{$translate("controlPlane.keyName")}</th>
            <th class="px-3 py-2.5 whitespace-nowrap">{$translate("controlPlane.keyLastUsed")}</th>
            <th class="px-3 py-2.5 whitespace-nowrap">{$translate("controlPlane.keyAccessTitle")}</th>
            <th class="px-3 py-2.5 text-right">{$translate("controlPlane.runtimeActions")}</th>
          </tr>
        </thead>
        <tbody>
          {#each managementKeys as parent (parent.id)}
            <tr class="border-t align-middle">
              <td class="px-3 py-2.5">
                <div class="flex items-start gap-1.5">
                  <button
                    type="button"
                    class="hover:bg-muted/60 mt-0.5 rounded p-0.5"
                    aria-expanded={expanded[parent.id] === true}
                    aria-label={$translate("controlPlane.keyAccessTitle")}
                    onclick={() => (expanded[parent.id] = !expanded[parent.id])}
                  >
                    {#if expanded[parent.id]}
                      <ChevronDown class="size-4" aria-hidden="true" />
                    {:else}
                      <ChevronRight class="size-4" aria-hidden="true" />
                    {/if}
                  </button>
                  <div class="min-w-0">
                    <div class="flex items-center gap-1.5">
                      <span class="truncate font-medium">{parent.name || $translate("controlPlane.keyUnnamed")}</span>
                      {#if configManaged(parent)}
                        <span
                          class="rounded-sm border border-border px-1 py-px text-[10px] text-muted-foreground"
                          title={$translate("controlPlane.keyManagedByConfigHint")}
                        >{$translate("controlPlane.keyManagedByConfig")}</span>
                      {/if}
                    </div>
                    <div class="text-muted-foreground truncate font-mono text-xs">{parent.id}</div>
                    <div class="text-muted-foreground mt-0.5 text-xs">
                      {parent.expiresAt
                        ? `${$translate("controlPlane.keyExpires")}: ${formatDate(parent.expiresAt)}`
                        : $translate("controlPlane.keyExpiryUnlimited")}
                    </div>
                  </div>
                </div>
              </td>
              <td class="px-3 py-2.5 text-xs">
                <div class="whitespace-nowrap">{formatDate(parent.lastUsedAt ?? parent.createdAt)}</div>
                {#if parent.lastIp}
                  <div class="text-muted-foreground mt-0.5 font-mono text-[11px]" title={$translate("controlPlane.keyLastIPHint")}>
                    {$translate("controlPlane.keyFromIP", { ip: parent.lastIp })}
                  </div>
                {/if}
              </td>
              <td class="w-px px-3 py-2.5 text-right text-xs tabular-nums">{accessKeysFor(parent.id).length}</td>
              <td class="px-3 py-2.5">
                <div class="keys-table__actions">
                  <Button
                    variant="ghost"
                    size="icon-sm"
                    title={$translate("controlPlane.keyAddAccess")}
                    aria-label={$translate("controlPlane.keyAddAccess")}
                    onclick={() => openCreate("access", parent.id)}
                    disabled={saving}
                  >
                    <Plus aria-hidden="true" />
                  </Button>
                  <Button
                    variant="ghost"
                    size="icon-sm"
                    aria-label={$translate("controlPlane.editKey")}
                    onclick={() => openEdit(parent)}
                    disabled={saving || configManaged(parent)}
                    title={configManaged(parent) ? $translate("controlPlane.keyManagedByConfigHint") : $translate("controlPlane.editKey")}
                  >
                    <Pencil aria-hidden="true" />
                  </Button>
                  <Button
                    variant="ghost"
                    size="icon-sm"
                    aria-label={$translate("controlPlane.rotate")}
                    onclick={() => requestAction("rotate", parent)}
                    disabled={saving || configManaged(parent)}
                    title={configManaged(parent) ? $translate("controlPlane.keyManagedByConfigHint") : $translate("controlPlane.rotate")}
                  >
                    <RotateCcw aria-hidden="true" />
                  </Button>
                  <Button
                    variant="ghost"
                    size="icon-sm"
                    class="keys-table__danger"
                    aria-label={$translate("controlPlane.deleteKey")}
                    onclick={() => requestAction("delete", parent)}
                    disabled={saving || configManaged(parent)}
                    title={configManaged(parent) ? $translate("controlPlane.keyManagedByConfigHint") : $translate("controlPlane.deleteKey")}
                  >
                    <Trash2 aria-hidden="true" />
                  </Button>
                </div>
              </td>
            </tr>

            {#if expanded[parent.id]}
              {@const children = accessKeysFor(parent.id)}
              <tr class="border-t bg-muted/20">
                <td class="px-3 py-3" colspan="5">
                  {#if children.length === 0}
                    <div class="flex items-center gap-2">
                      <span class="text-muted-foreground text-xs">{$translate("controlPlane.keyAccessEmpty")}</span>
                      <Button
                        variant="ghost"
                        size="sm"
                        class="text-muted-foreground h-6 px-2 text-xs"
                        onclick={() => openCreate("access", parent.id)}
                        disabled={saving}
                      >
                        <Plus class="size-3" aria-hidden="true" />
                        {$translate("controlPlane.keyAddAccess")}
                      </Button>
                    </div>
                  {:else}
                    <table class="keys-table w-full table-fixed text-sm">
                      <caption class="sr-only">{$translate("controlPlane.keyAccessTitle")}: {parent.name || parent.id}</caption>
                      <colgroup>
                        <col />
                        <col class="w-[11rem]" />
                        <col class="w-[10rem]" />
                        <col class="w-[7rem]" />
                        <col class="w-[13rem]" />
                        <col class="w-[8rem]" />
                      </colgroup>
                      <thead class="text-left">
                        <tr>
                          <th class="px-3 py-2">{$translate("controlPlane.keyName")}</th>
                          <th class="px-3 py-2 whitespace-nowrap">{$translate("controlPlane.keyModels")}</th>
                          <th class="px-3 py-2 whitespace-nowrap">{$translate("controlPlane.keyAllowedIPs")}</th>
                          <th class="px-3 py-2 whitespace-nowrap">{$translate("controlPlane.keyMaxConcurrency")}</th>
                          <th class="px-3 py-2 whitespace-nowrap">{$translate("controlPlane.keyLastUsed")}</th>
                          <th class="px-3 py-2 text-right">{$translate("controlPlane.runtimeActions")}</th>
                        </tr>
                      </thead>
                      <tbody>
                        {#each children as child (child.id)}
                          <tr class="border-t align-middle">
                            <td class="px-3 py-2">
                              <div class="truncate font-medium">{child.name || $translate("controlPlane.keyUnnamed")}</div>
                              <div class="text-muted-foreground truncate font-mono text-xs">{child.id}</div>
                              <div class="text-muted-foreground mt-0.5 text-xs">
                                {child.group || $translate("controlPlane.keyUngrouped")}
                                {#if child.expiresAt}
                                  · {$translate("controlPlane.keyExpires")}: {formatDate(child.expiresAt)}
                                {/if}
                              </div>
                            </td>
                            <td class="px-3 py-2 text-xs break-words" title={child.models.length > 0 ? child.models.join(", ") : ""}>
                              {#if child.models.length > 0}
                                {child.models.join(", ")}
                              {:else}
                                <span class="text-muted-foreground">{$translate("controlPlane.keyModelsAllModels")}</span>
                              {/if}
                            </td>
                            <td class="px-3 py-2 text-xs">
                              {#if child.allowedIps.length > 0}
                                {@const matched = matchedAllowedIP(child)}
                                {#each child.allowedIps as entry (entry)}
                                  <div
                                    class={matched && entry === matched ? "break-words font-medium text-success" : "break-words text-muted-foreground"}
                                    title={matched && entry === matched ? $translate("controlPlane.keyAllowedIPsMatchedHint") : ""}
                                  >{entry}</div>
                                {/each}
                              {:else}
                                <span class="text-muted-foreground">{$translate("controlPlane.keyAllowedIPsAll")}</span>
                              {/if}
                            </td>
                            <td class="px-3 py-2 text-xs">
                              {concurrencyLabel(child)}
                              {#if child.maxConcurrency > 0 && (child.activeRequests ?? 0) > 0}
                                <div class="text-muted-foreground">
                                  {$translate("controlPlane.keyInFlight", { count: child.activeRequests ?? 0 })}
                                </div>
                              {/if}
                            </td>
                            <td class="px-3 py-2 text-xs">
                              <div class="whitespace-nowrap">{formatDate(child.lastUsedAt ?? child.createdAt)}</div>
                              {#if child.lastIp}
                                <div
                                  class={matchedAllowedIP(child) ? "text-muted-foreground mt-0.5 font-mono text-[11px]" : "text-destructive mt-0.5 font-mono text-[11px]"}
                                  title={matchedAllowedIP(child) ? $translate("controlPlane.keyLastIPHint") : $translate("controlPlane.keyLastIPBlockedHint")}
                                >
                                  {$translate("controlPlane.keyFromIP", { ip: child.lastIp })}
                                </div>
                              {/if}
                            </td>
                            <td class="px-3 py-2">
                              <div class="keys-table__actions">
                                <Button
                                  variant="ghost"
                                  size="icon-sm"
                                  title={$translate("controlPlane.editKey")}
                                  aria-label={$translate("controlPlane.editKey")}
                                  onclick={() => openEdit(child)}
                                  disabled={saving}
                                >
                                  <Pencil aria-hidden="true" />
                                </Button>
                                <Button
                                  variant="ghost"
                                  size="icon-sm"
                                  title={$translate("controlPlane.rotate")}
                                  aria-label={$translate("controlPlane.rotate")}
                                  onclick={() => requestAction("rotate", child)}
                                  disabled={saving}
                                >
                                  <RotateCcw aria-hidden="true" />
                                </Button>
                                <Button
                                  variant="ghost"
                                  size="icon-sm"
                                  class="keys-table__danger"
                                  title={$translate("controlPlane.deleteKey")}
                                  aria-label={$translate("controlPlane.deleteKey")}
                                  onclick={() => requestAction("delete", child)}
                                  disabled={saving}
                                >
                                  <Trash2 aria-hidden="true" />
                                </Button>
                              </div>
                            </td>
                          </tr>
                        {/each}
                      </tbody>
                    </table>
                  {/if}
                </td>
              </tr>
            {/if}
          {/each}
        </tbody>
      </table>
    </div>
  {/if}
</section>

<Dialog.Root bind:open={createDialogOpen}>
  <Dialog.Content class="max-h-[calc(100vh-2rem)] max-w-2xl overflow-y-auto sm:max-w-2xl" showCloseButton={!saving}>
    {#if editing}
      <Dialog.Header>
        <Dialog.Title>
          {dialogKind === "management"
            ? $translate("controlPlane.keyCreateManagement")
            : $translate("controlPlane.keyCreateAccess")}
        </Dialog.Title>
        <Dialog.Description>
          {dialogKind === "management"
            ? $translate("controlPlane.keyManagementDescription")
            : $translate("controlPlane.keyAccessDescription")}
        </Dialog.Description>
      </Dialog.Header>
      <form
        class="grid gap-4"
        onsubmit={(event) => {
          event.preventDefault();
          void submitDraft();
        }}
      >
        <fieldset class="grid gap-4" disabled={saving}>
          {#if dialogKind === "access"}
            <div class="grid gap-1.5 text-sm">
              <span class="font-medium">{$translate("controlPlane.keyParent")}</span>
              <p class="text-muted-foreground text-xs">{parentKeyLabel}</p>
            </div>
          {/if}
          <label class="grid gap-1.5 text-sm" for="create-key-name">
            <span class="font-medium">{$translate("controlPlane.keyName")}</span>
            <input
              id="create-key-name"
              class="border-input bg-background focus-visible:border-ring focus-visible:ring-ring/50 h-9 rounded-lg border px-3 outline-none focus-visible:ring-3"
              bind:value={editing.name}
              maxlength="120"
              placeholder={dialogKind === "management" ? "web-ui" : "chat-client"}
              autocomplete="off"
            />
          </label>
          <label class="grid gap-1.5 text-sm">
            <span class="font-medium">{$translate("controlPlane.keyModels")}</span>
            <ModelMultiSelect
              bind:value={editing.models}
              groups={modelGroups}
              ariaLabel={$translate("controlPlane.keyModels")}
              allLabel={$translate("controlPlane.keyModelAll")}
              selectedLabel={$translate("controlPlane.keyModelSelected", { count: editing.models.length })}
              emptyLabel={$translate("controlPlane.keyModelEmpty")}
              disabled={saving}
            />
          </label>
          {#if dialogKind === "access"}
            <label class="grid gap-1.5 text-sm" for="create-key-ips">
              <span class="font-medium">{$translate("controlPlane.keyAllowedIPs")}</span>
              <textarea
                id="create-key-ips"
                class="border-input bg-background focus-visible:border-ring focus-visible:ring-ring/50 min-h-20 rounded-lg border px-3 py-2 font-mono text-xs outline-none focus-visible:ring-3"
                bind:value={editing.allowedIps}
                placeholder={$translate("controlPlane.keyAllowedIPsPlaceholder")}
                spellcheck="false"
              ></textarea>
              <span class="text-muted-foreground text-xs">{$translate("controlPlane.keyAllowedIPsHint")}</span>
            </label>
            <div class="grid gap-1.5 text-sm">
              <label class="font-medium" for="create-key-concurrency">{$translate("controlPlane.keyMaxConcurrency")}</label>
              <input
                id="create-key-concurrency"
                type="number"
                min="0"
                max="1024"
                class="border-input bg-background focus-visible:border-ring focus-visible:ring-ring/50 h-9 rounded-lg border px-3 outline-none focus-visible:ring-3"
                bind:value={editing.maxConcurrency}
              />
              <span class="text-muted-foreground text-xs">{$translate("controlPlane.keyMaxConcurrencyHint")}</span>
            </div>
            <label class="grid gap-1.5 text-sm" for="create-key-group">
              <span class="font-medium">{$translate("controlPlane.keyGroup")}</span>
              <input
                id="create-key-group"
                class="border-input bg-background focus-visible:border-ring focus-visible:ring-ring/50 h-9 rounded-lg border px-3 outline-none focus-visible:ring-3"
                bind:value={editing.group}
                maxlength="64"
                placeholder={$translate("controlPlane.keyGroupPlaceholder")}
                autocomplete="off"
              />
              <span class="text-muted-foreground text-xs">{$translate("controlPlane.keyGroupHint")}</span>
            </label>
          {/if}
          <div class="grid gap-1.5 text-sm">
            <label class="font-medium" for="create-key-expiry-mode">{$translate("controlPlane.keyExpires")}</label>
            <Select.Root
              type="single"
              value={editing.expiryMode}
              onValueChange={(value) => {
                if (editing && (value === "unlimited" || value === "custom")) editing.expiryMode = value;
              }}
            >
              <Select.Trigger id="create-key-expiry-mode" class="h-9 w-full" aria-label={$translate("controlPlane.keyExpires")}>
                {editing.expiryMode === "unlimited"
                  ? $translate("controlPlane.keyExpiryUnlimited")
                  : $translate("controlPlane.keyExpiryCustom")}
              </Select.Trigger>
              <Select.Content>
                <Select.Item value="unlimited">{$translate("controlPlane.keyExpiryUnlimited")}</Select.Item>
                <Select.Item value="custom">{$translate("controlPlane.keyExpiryCustom")}</Select.Item>
              </Select.Content>
            </Select.Root>
            {#if editing.expiryMode === "custom"}
              <input
                class="border-input bg-background focus-visible:border-ring focus-visible:ring-ring/50 h-9 rounded-lg border px-3 outline-none focus-visible:ring-3"
                type="date"
                bind:value={editing.expiresAt}
                aria-label={$translate("controlPlane.keyExpires")}
              />
            {/if}
          </div>
          {#if dialogKind === "management"}
            <p class="text-muted-foreground text-xs">{$translate("controlPlane.keyLoginOnlyHint")}</p>
          {/if}
        </fieldset>
        {#if error}
          <div class="rounded-md border border-destructive/40 bg-destructive/10 p-3 text-sm" role="alert">
            {$translate("controlPlane.error", { message: error })}
          </div>
        {/if}
        <Dialog.Footer>
          <Button
            variant="outline"
            type="button"
            onclick={() => (createDialogOpen = false)}
            disabled={saving}
          >
            {$translate("common.cancel")}
          </Button>
          <Button type="submit" disabled={saving}>
            {#if saving}<LoaderCircle class="size-4 animate-spin" aria-hidden="true" />{/if}
            {dialogKind === "management"
              ? $translate("controlPlane.keyCreateManagement")
              : $translate("controlPlane.keyCreateAccess")}
          </Button>
        </Dialog.Footer>
      </form>
    {/if}
  </Dialog.Content>
</Dialog.Root>

<Dialog.Root bind:open={editDialogOpen}>
  <Dialog.Content class="max-h-[calc(100vh-2rem)] max-w-2xl overflow-y-auto sm:max-w-2xl" showCloseButton={!saving}>
    {#if editing}
      <Dialog.Header>
        <Dialog.Title>
          {dialogKind === "management"
            ? $translate("controlPlane.keyEditManagement")
            : $translate("controlPlane.keyEditAccess")}
        </Dialog.Title>
        <Dialog.Description>{editing.id}</Dialog.Description>
      </Dialog.Header>
      <form
        class="grid gap-4"
        onsubmit={(event) => {
          event.preventDefault();
          void submitDraft();
        }}
      >
        <fieldset class="grid gap-4" disabled={saving}>
          <label class="grid gap-1.5 text-sm" for="edit-key-name">
            <span class="font-medium">{$translate("controlPlane.keyName")}</span>
            <input
              id="edit-key-name"
              class="border-input bg-background focus-visible:border-ring focus-visible:ring-ring/50 h-9 rounded-lg border px-3 outline-none focus-visible:ring-3"
              bind:value={editing.name}
              maxlength="120"
              autocomplete="off"
            />
          </label>
          <label class="grid gap-1.5 text-sm">
            <span class="font-medium">{$translate("controlPlane.keyModels")}</span>
            <ModelMultiSelect
              bind:value={editing.models}
              groups={modelGroups}
              ariaLabel={$translate("controlPlane.keyModels")}
              allLabel={$translate("controlPlane.keyModelAll")}
              selectedLabel={$translate("controlPlane.keyModelSelected", { count: editing.models.length })}
              emptyLabel={$translate("controlPlane.keyModelEmpty")}
              disabled={saving}
            />
          </label>
          {#if dialogKind === "access"}
            <label class="grid gap-1.5 text-sm" for="edit-key-ips">
              <span class="font-medium">{$translate("controlPlane.keyAllowedIPs")}</span>
              <textarea
                id="edit-key-ips"
                class="border-input bg-background focus-visible:border-ring focus-visible:ring-ring/50 min-h-20 rounded-lg border px-3 py-2 font-mono text-xs outline-none focus-visible:ring-3"
                bind:value={editing.allowedIps}
                placeholder={$translate("controlPlane.keyAllowedIPsPlaceholder")}
                spellcheck="false"
              ></textarea>
              <span class="text-muted-foreground text-xs">{$translate("controlPlane.keyAllowedIPsHint")}</span>
            </label>
            <div class="grid gap-1.5 text-sm">
              <label class="font-medium" for="edit-key-concurrency">{$translate("controlPlane.keyMaxConcurrency")}</label>
              <input
                id="edit-key-concurrency"
                type="number"
                min="0"
                max="1024"
                class="border-input bg-background focus-visible:border-ring focus-visible:ring-ring/50 h-9 rounded-lg border px-3 outline-none focus-visible:ring-3"
                bind:value={editing.maxConcurrency}
              />
            </div>
            <label class="grid gap-1.5 text-sm" for="edit-key-group">
              <span class="font-medium">{$translate("controlPlane.keyGroup")}</span>
              <input
                id="edit-key-group"
                class="border-input bg-background focus-visible:border-ring focus-visible:ring-ring/50 h-9 rounded-lg border px-3 outline-none focus-visible:ring-3"
                bind:value={editing.group}
                maxlength="64"
                placeholder={$translate("controlPlane.keyGroupPlaceholder")}
                autocomplete="off"
              />
            </label>
          {/if}
          <div class="grid gap-1.5 text-sm">
            <label class="font-medium" for="edit-key-expiry-mode">{$translate("controlPlane.keyExpires")}</label>
            <Select.Root
              type="single"
              value={editing.expiryMode}
              onValueChange={(value) => {
                if (editing && (value === "unlimited" || value === "custom")) editing.expiryMode = value;
              }}
            >
              <Select.Trigger id="edit-key-expiry-mode" class="h-9 w-full" aria-label={$translate("controlPlane.keyExpires")}>
                {editing.expiryMode === "unlimited"
                  ? $translate("controlPlane.keyExpiryUnlimited")
                  : $translate("controlPlane.keyExpiryCustom")}
              </Select.Trigger>
              <Select.Content>
                <Select.Item value="unlimited">{$translate("controlPlane.keyExpiryUnlimited")}</Select.Item>
                <Select.Item value="custom">{$translate("controlPlane.keyExpiryCustom")}</Select.Item>
              </Select.Content>
            </Select.Root>
            {#if editing.expiryMode === "custom"}
              <input
                class="border-input bg-background focus-visible:border-ring focus-visible:ring-ring/50 h-9 rounded-lg border px-3 outline-none focus-visible:ring-3"
                type="date"
                bind:value={editing.expiresAt}
                aria-label={$translate("controlPlane.keyExpires")}
              />
            {/if}
          </div>
          {#if dialogKind === "management" && !editing.allowManagementLogin}
            <label class="flex items-center gap-2 rounded-lg border border-transparent px-1 py-1.5 text-sm hover:bg-muted/50">
              <input class="accent-primary mt-0.5 size-4 shrink-0" type="checkbox" bind:checked={editing.allowManagementLogin} />
              <span class="font-medium">{$translate("controlPlane.keyAllowManagementLogin")}</span>
            </label>
            <p class="text-muted-foreground text-xs">{$translate("controlPlane.keyAPIOnlyHint")}</p>
          {/if}
        </fieldset>
        {#if error}
          <div class="rounded-md border border-destructive/40 bg-destructive/10 p-3 text-sm" role="alert">
            {$translate("controlPlane.error", { message: error })}
          </div>
        {/if}
        <Dialog.Footer>
          <Button variant="outline" type="button" onclick={() => (editDialogOpen = false)} disabled={saving}>
            {$translate("common.cancel")}
          </Button>
          <Button type="submit" disabled={saving}>
            {#if saving}<LoaderCircle class="size-4 animate-spin" aria-hidden="true" />{/if}
            {$translate("controlPlane.saveKey")}
          </Button>
        </Dialog.Footer>
      </form>
    {/if}
  </Dialog.Content>
</Dialog.Root>

<ConfirmDialog
  open={Boolean(pendingAction)}
  onOpenChange={(open) => {
    if (!open) pendingAction = null;
  }}
  title={pendingAction?.kind === "delete"
    ? $translate("controlPlane.deleteKey")
    : $translate("controlPlane.rotate")}
  message={pendingAction?.kind === "delete"
    ? $translate("controlPlane.deleteKeyConfirm", { name: pendingAction?.name ?? "" })
    : $translate("controlPlane.rotateConfirm", { name: pendingAction?.name ?? "" })}
  confirmLabel={pendingAction?.kind === "delete"
    ? $translate("controlPlane.deleteKey")
    : $translate("controlPlane.rotate")}
  onConfirm={() => void confirmAction()}
/>

<Dialog.Root bind:open={secretDialogOpen}>
  <Dialog.Content class="max-w-lg">
    <Dialog.Header>
      <Dialog.Title>{$translate("controlPlane.secretTitle")}</Dialog.Title>
      <Dialog.Description>{$translate("controlPlane.secretOnce")}</Dialog.Description>
    </Dialog.Header>
    <div class="rounded-lg border border-warning/50 bg-warning/10 p-3" role="status" aria-live="polite">
      <code class="block max-h-28 overflow-auto break-all rounded bg-background px-2 py-2 text-sm">{newlyCreated}</code>
    </div>
    <div class="flex justify-end">
      <Button variant="outline" onclick={() => void copySecret()}>{$translate("common.copy")}</Button>
    </div>
    <Dialog.Footer>
      <Button onclick={closeSecret}>{$translate("common.close")}</Button>
    </Dialog.Footer>
  </Dialog.Content>
</Dialog.Root>

<style>
  /* The action cells hold four icon buttons; without this the row wraps them
     onto a second line and every management key grows two rows tall. */
  .keys-table__actions {
    display: flex;
    flex-wrap: nowrap;
    align-items: center;
    justify-content: flex-end;
    gap: 0.125rem;
    white-space: nowrap;
  }

  /* The class is passed to a Button component, so the compiler cannot see it
     from the markup; the selector stays global for that, like the models page. */
  :global(.keys-table__danger) {
    color: var(--destructive);
  }

  :global(.keys-table__danger):hover:not(:disabled) {
    color: var(--destructive);
    background: color-mix(in oklab, var(--destructive) 12%, transparent);
  }
</style>
