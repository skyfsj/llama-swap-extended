<script lang="ts">
  // Confirmation dialog for deleting a model configuration, opened from the
  // model list rows. It fetches the config snapshot (ETag + writability) and
  // the model file catalog on demand so the row does not have to carry them.
  import { AlertTriangle, LoaderCircle } from "@lucide/svelte";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import { translate } from "../../lib/i18n";
  import { getModelFiles } from "../../lib/modelFiles";
  import { deleteModelConfig, ModelConfigRequestError } from "../../lib/modelConfigApi";
  import { routingBlockNamesFor } from "../../lib/routerReferences";
  import type { ModelFile } from "../../lib/types";

  interface Props {
    modelId: string;
    open?: boolean;
    onDeleted?: (modelId: string) => void;
  }

  let { modelId, open = $bindable(false), onDeleted = () => {} }: Props = $props();

  let displayName = $state("");
  let etag = $state("");
  let writable = $state(true);
  let loadError = $state("");
  let files = $state<ModelFile[]>([]);
  let filesLoading = $state(false);
  let filesError = $state("");
  let deleteModelFile = $state(false);
  let deletingModel = $state(false);
  let deleteError = $state("");
  // The fetched configuration, kept so the routing-reference notice below can
  // scan it once both requests land.
  let configValue = $state<unknown>(null);

  let associatedModelFiles = $derived(
    files.filter((file) => (file.registered_models ?? []).includes(modelId)),
  );
  let modelFileDeleteBlocked = $derived.by(() => {
    if (filesLoading) return $translate("controlPlane.modelDeleteFilesLoading");
    if (filesError) return $translate("controlPlane.modelDeleteFilesUnavailable");
    if (associatedModelFiles.length === 0) return $translate("controlPlane.modelDeleteNoFiles");
    if (associatedModelFiles.some((file) => (file.in_use_models ?? []).length > 0)) {
      return $translate("controlPlane.modelDeleteFilesInUse");
    }
    if (associatedModelFiles.some((file) => (file.registered_models ?? []).some((id) => id !== modelId))) {
      return $translate("controlPlane.modelDeleteFilesShared");
    }
    return "";
  });
  let canDeleteModelFiles = $derived(associatedModelFiles.length > 0 && modelFileDeleteBlocked === "");

  // The routing blocks that name this model today. Deleting it would leave
  // those references dangling, which the config validator rejects, so the
  // server prunes them as part of the delete. Telling the operator up front
  // keeps that from being a surprise.
  let routingNotice = $derived.by(() => {
    if (!modelId) return "";
    const blocks = routingBlockNamesFor(configValue, modelId);
    if (blocks.length === 0) return "";
    const labels: Record<string, string> = {
      matrix: $translate("controlPlane.matrix"),
      groups: $translate("controlPlane.schemaGroups"),
      gpus: $translate("controlPlane.schemaGpus"),
      priority: $translate("controlPlane.modelPriority"),
      selectors: $translate("controlPlane.modelSelectors"),
      profiles: $translate("controlPlane.schemaProfiles"),
    };
    return blocks.map((block) => labels[block] ?? block).join("、");
  });

  $effect(() => {
    if (!open || !modelId) return;
    let active = true;
    displayName = modelId;
    etag = "";
    writable = true;
    loadError = "";
    deleteError = "";
    configValue = null;
    deleteModelFile = false;
    files = [];
    filesLoading = true;
    filesError = "";
    (async () => {
      try {
        const response = await fetch("/api/config");
        const payload = await response.json().catch(() => ({}));
        if (!response.ok) throw new Error(`HTTP ${response.status}`);
        if (!active) return;
        etag = typeof payload?.etag === "string" ? payload.etag : "";
        writable = payload?.writable !== false;
        configValue = payload?.config ?? null;
        const models = ((payload?.config ?? {}) as Record<string, unknown>)?.models as Record<string, unknown> | undefined;
        const model = (models?.[modelId] ?? {}) as Record<string, unknown>;
        displayName = typeof model.name === "string" && model.name.trim() !== "" ? model.name : modelId;
      } catch (cause) {
        if (active) loadError = cause instanceof Error ? cause.message : String(cause);
      }
    })();
    (async () => {
      try {
        const catalog = await getModelFiles();
        if (active) files = catalog.data;
      } catch {
        if (active) filesError = "unavailable";
      } finally {
        if (active) filesLoading = false;
      }
    })();
    return () => { active = false; };
  });

  function closeDeleteModel(): void {
    if (deletingModel) return;
    open = false;
  }

  async function confirmDeleteModel(): Promise<void> {
    if (!modelId || !etag || !writable || deletingModel) return;
    deletingModel = true;
    deleteError = "";
    try {
      await deleteModelConfig(modelId, etag, deleteModelFile);
      open = false;
      deleteModelFile = false;
      onDeleted(modelId);
    } catch (cause) {
      if (cause instanceof ModelConfigRequestError && cause.status === 409) {
        const models = [...(cause.payload?.in_use ?? []), ...(cause.payload?.registered ?? [])];
        deleteError = models.length > 0
          ? $translate("controlPlane.modelDeleteConflict", { models: [...new Set(models)].join(", ") })
          : $translate("controlPlane.modelDeleteFailed", { message: cause.message });
      } else if (cause instanceof ModelConfigRequestError && cause.status === 412) {
        deleteError = $translate("controlPlane.conflict");
      } else {
        deleteError = $translate("controlPlane.modelDeleteFailed", {
          message: cause instanceof Error ? cause.message : String(cause),
        });
      }
    } finally {
      deletingModel = false;
    }
  }
</script>

<Dialog.Root bind:open>
  <Dialog.Content class="max-h-[calc(100vh-2rem)] max-w-lg overflow-y-auto sm:max-w-lg" showCloseButton={!deletingModel}>
    <Dialog.Header>
      <Dialog.Title class="flex items-center gap-2">
        <AlertTriangle class="text-destructive size-4" aria-hidden="true" />
        {$translate("controlPlane.modelDeleteTitle")}
      </Dialog.Title>
      <Dialog.Description>
        {$translate("controlPlane.modelDeleteConfirm", { name: displayName })}
      </Dialog.Description>
    </Dialog.Header>

    <div class="border-border/70 bg-muted/20 grid gap-1 rounded-md border p-3 text-sm">
      <span class="font-medium">{displayName}</span>
      <code class="text-muted-foreground text-xs">{modelId}</code>
      <span class="text-muted-foreground text-xs">{$translate("controlPlane.modelDeleteConfigOnlyHint")}</span>
    </div>

    {#if routingNotice}
      <div class="border-border/70 grid gap-1 rounded-md border p-3 text-xs" role="status">
        <span class="font-medium">{$translate("controlPlane.modelDeleteRoutingTitle")}</span>
        <span class="text-muted-foreground">{$translate("controlPlane.modelDeleteRoutingHint", { locations: routingNotice })}</span>
      </div>
    {/if}

    {#if loadError}
      <div class="border-destructive/40 bg-destructive/10 text-destructive rounded-md border p-3 text-sm" role="alert">
        {$translate("controlPlane.modelDeleteFailed", { message: loadError })}
      </div>
    {:else if !writable}
      <div class="border-warning/30 bg-warning/10 text-warning rounded-md border px-3 py-2 text-sm" role="status">
        <AlertTriangle class="mr-1 inline size-4" aria-hidden="true" />{$translate("models.configNotWritable")}
      </div>
    {/if}

    {#if associatedModelFiles.length > 0}
      <div class="border-border/70 grid gap-2 rounded-md border p-3">
        <p class="text-sm font-medium">{$translate("controlPlane.modelDeleteAssociatedFiles")}</p>
        <ul class="text-muted-foreground max-h-32 space-y-1 overflow-y-auto text-xs" aria-label={$translate("controlPlane.modelDeleteAssociatedFiles")}>
          {#each associatedModelFiles as file (file.id)}
            <li class="break-all"><code>{file.path}</code></li>
          {/each}
        </ul>
      </div>
    {/if}

    <label class={`border-border/70 flex items-start gap-3 rounded-md border p-3 text-sm ${canDeleteModelFiles ? "" : "opacity-60"}`}>
      <input type="checkbox" class="mt-0.5 size-4 shrink-0" bind:checked={deleteModelFile} disabled={!canDeleteModelFiles || deletingModel} />
      <span class="grid gap-1">
        <span class="font-medium">{$translate("controlPlane.modelDeleteFiles")}</span>
        <span class="text-muted-foreground text-xs">{$translate("controlPlane.modelDeleteFilesHint")}</span>
      </span>
    </label>
    {#if modelFileDeleteBlocked}
      <p class="text-muted-foreground text-xs" role="status">{modelFileDeleteBlocked}</p>
    {/if}
    {#if deleteError}
      <div class="border-destructive/40 bg-destructive/10 text-destructive rounded-md border p-3 text-sm" role="alert">
        {deleteError}
      </div>
    {/if}

    <Dialog.Footer>
      <Button variant="outline" onclick={closeDeleteModel} disabled={deletingModel}>{$translate("common.cancel")}</Button>
      <Button variant="destructive" onclick={() => void confirmDeleteModel()} disabled={deletingModel || !modelId || !writable || loadError !== ""}>
        {#if deletingModel}<LoaderCircle class="size-4 animate-spin" aria-hidden="true" />{/if}
        {deletingModel ? $translate("common.loading") : $translate("controlPlane.modelDelete")}
      </Button>
    </Dialog.Footer>
  </Dialog.Content>
</Dialog.Root>
