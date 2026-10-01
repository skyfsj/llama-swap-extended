<script lang="ts">
  import { onMount } from "svelte";
  import { LoaderCircle } from "@lucide/svelte";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import { translate } from "../../lib/i18n";
  import { errorMessageFromPayload } from "../../lib/apiError";
  import ModelConfigEditor from "../config/ModelConfigEditor.svelte";

  interface ConfigSnapshot {
    config?: Record<string, unknown>;
    yaml: string;
    etag: string;
    writable: boolean;
    restartRequired?: boolean;
    restartPaths?: string[];
  }

  interface Props {
    modelId: string;
    open?: boolean;
    onSaved?: (modelId: string) => void;
  }

  let { modelId, open = $bindable(false), onSaved = () => {} }: Props = $props();
  let snapshot = $state<ConfigSnapshot | null>(null);
  let loading = $state(false);
  let error = $state("");
  let requestID = 0;

  async function load(): Promise<void> {
    const id = ++requestID;
    loading = true;
    error = "";
    try {
      const response = await fetch("/api/config");
      const payload = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(errorMessageFromPayload(payload, `HTTP ${response.status}`));
      if (id === requestID) snapshot = payload as ConfigSnapshot;
    } catch (cause) {
      if (id === requestID) {
        snapshot = null;
        error = cause instanceof Error ? cause.message : String(cause);
      }
    } finally {
      if (id === requestID) loading = false;
    }
  }

  $effect(() => {
    modelId;
    if (open) void load();
  });

  onMount(() => {
    return () => {
      requestID++;
    };
  });
</script>

<Dialog.Root bind:open>
  <Dialog.Content class="max-h-[calc(100dvh-1.5rem)] max-w-3xl overflow-y-auto sm:max-w-3xl">
    <Dialog.Header>
      <Dialog.Title>{$translate("settingsCenter.model.editTitle")}</Dialog.Title>
    </Dialog.Header>

    {#if loading}
      <div class="flex items-center gap-2 py-8 text-sm text-muted-foreground" role="status" aria-live="polite">
        <LoaderCircle class="size-4 animate-spin" aria-hidden="true" />
        {$translate("controlPlane.loading")}
      </div>
    {:else if error}
      <div class="rounded-md border border-destructive/40 bg-destructive/10 p-3 text-sm" role="alert">
        {$translate("controlPlane.error", { message: error })}
      </div>
      <div class="flex justify-end">
        <Button variant="outline" onclick={() => void load()}>{$translate("controlPlane.refresh")}</Button>
      </div>
    {:else if snapshot}
      <ModelConfigEditor
        snapshot={snapshot}
        modelId={modelId}
        showTitle={false}
        embedded={true}
        showModelName={false}
        onCancel={() => (open = false)}
        onSnapshot={(next) => { snapshot = { ...snapshot, ...next }; }}
        onSaved={(savedModelId) => { open = false; onSaved(savedModelId); }}
      />
    {/if}
  </Dialog.Content>
</Dialog.Root>
