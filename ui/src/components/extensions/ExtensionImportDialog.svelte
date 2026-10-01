<script lang="ts">
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Upload, FileArchive } from "@lucide/svelte";
  import { translate } from "../../lib/i18n";
  import { toast } from "../../lib/toast";
  import { previewExtensionImport, importExtension, getExtension, type ExtensionImportPreview } from "../../lib/extensionsApi";

  let { open = $bindable(false), onImported }: { open: boolean; onImported: (id: string) => void } = $props();

  let file = $state<File | null>(null);
  let preview = $state<ExtensionImportPreview | null>(null);
  let loading = $state(false);
  let error = $state("");
  let saving = $state(false);
  // On an id conflict the operator picks replace (needs the live etag) or
  // save-as under a new id.
  let resolution = $state<"replace" | "saveAs">("replace");
  let newId = $state("");
  let existingEtag = $state("");

  const canImport = $derived(preview !== null && !saving && (preview.conflict ? resolution === "replace" || newId.trim().length > 0 : true));

  function reset() {
    file = null; preview = null; loading = false; error = ""; saving = false;
    resolution = "replace"; newId = ""; existingEtag = "";
  }

  async function chooseFile(event: Event) {
    const input = event.target as HTMLInputElement;
    const chosen = input.files?.[0] ?? null;
    input.value = "";
    if (!chosen) return;
    file = chosen; preview = null; error = "";
    loading = true;
    try {
      preview = await previewExtensionImport(chosen);
      newId = "";
      resolution = "replace";
      if (preview.conflict) {
        try { existingEtag = (await getExtension(preview.manifest.id)).etag; } catch { existingEtag = ""; }
      }
    } catch (cause) { error = String(cause); }
    finally { loading = false; }
  }

  async function confirm() {
    if (!file || !preview || saving) return;
    saving = true; error = "";
    try {
      const targetId = preview.conflict && resolution === "saveAs" ? newId.trim() : "";
      const saved = await importExtension(file, {
        mode: preview.conflict && resolution === "replace" ? "replace" : "create",
        id: targetId || undefined,
        etag: preview.conflict && resolution === "replace" ? existingEtag : undefined,
      });
      toast.success($translate("extensions.import.imported", { id: saved.manifest.id }));
      open = false;
      onImported(saved.manifest.id);
    } catch (cause) { error = String(cause); }
    finally { saving = false; }
  }

  $effect(() => {
    if (!open) reset();
  });
</script>

<Dialog.Root bind:open>
  <Dialog.Content class="sm:max-w-xl">
    <Dialog.Header>
      <Dialog.Title>{$translate("extensions.import.title")}</Dialog.Title>
      <Dialog.Description>{$translate("extensions.import.hint")}</Dialog.Description>
    </Dialog.Header>

    {#if !preview}
      <label class="flex cursor-pointer flex-col items-center gap-2 rounded-lg border border-dashed border-border p-10 text-center hover:border-primary/50">
        <Upload class="size-6 text-muted-foreground" />
        <span class="text-sm">{$translate("extensions.import.choose")}</span>
        <span class="text-xs text-muted-foreground">{$translate("extensions.import.format")}</span>
        <input class="hidden" type="file" accept=".zip,application/zip" onchange={chooseFile} aria-label={$translate("extensions.import.choose")} />
      </label>
    {:else}
      <div class="space-y-4">
        <div class="flex items-start gap-3 rounded-lg border border-border p-3">
          <FileArchive class="mt-0.5 size-5 shrink-0 text-primary" />
          <div class="min-w-0 flex-1">
            <p class="font-medium">{preview.manifest.name || preview.manifest.id}</p>
            <p class="font-mono text-xs text-muted-foreground">{preview.manifest.id}</p>
            {#if preview.manifest.description}<p class="mt-1 text-xs text-muted-foreground">{preview.manifest.description}</p>{/if}
          </div>
          <span class="rounded-full bg-muted px-2 py-0.5 text-xs text-muted-foreground">{preview.files.length} {$translate("extensions.import.files")}</span>
        </div>

        <div class="space-y-1">
          <p class="text-xs font-medium text-muted-foreground">{$translate("extensions.import.fileList")}</p>
          <ul class="max-h-32 overflow-y-auto rounded-md border border-border bg-muted/20 p-2 font-mono text-xs">
            {#each preview.files as name (name)}<li class="truncate py-0.5">{name}</li>{/each}
          </ul>
        </div>

        {#if preview.diagnostics.length}
          <ul class="space-y-1 rounded-md bg-warning/10 p-3 text-xs text-warning">
            {#each preview.diagnostics as note (note)}<li>{note}</li>{/each}
          </ul>
        {/if}

        {#if preview.conflict}
          <div class="space-y-3 rounded-md border border-warning/40 bg-warning/10 p-3">
            <p class="text-sm text-warning">{$translate("extensions.import.conflict", { id: preview.manifest.id })}</p>
            <label class="flex items-center gap-2 text-sm"><input type="radio" name="import-resolution" value="replace" bind:group={resolution} />{$translate("extensions.import.replace")}</label>
            <label class="flex items-center gap-2 text-sm"><input type="radio" name="import-resolution" value="saveAs" bind:group={resolution} />{$translate("extensions.import.saveAs")}</label>
            {#if resolution === "saveAs"}<input class="w-full rounded-md border border-border bg-background px-3 py-2 text-sm" bind:value={newId} placeholder={preview.manifest.id + "-copy"} aria-label={$translate("extensions.import.newId")} />{/if}
          </div>
        {/if}
        {#if error}<p role="alert" class="break-words rounded-md bg-destructive/10 p-3 text-sm text-destructive">{error}</p>{/if}
      </div>
    {/if}

    {#if loading}<p role="status" class="p-4 text-center text-sm text-muted-foreground">{$translate("common.loading")}</p>{/if}

    <Dialog.Footer>
      <Button variant="outline" disabled={saving} onclick={() => open = false}>{$translate("common.cancel")}</Button>
      {#if preview}<Button disabled={!canImport} onclick={() => void confirm()}>{$translate("extensions.import.confirm")}</Button>{/if}
    </Dialog.Footer>
  </Dialog.Content>
</Dialog.Root>
