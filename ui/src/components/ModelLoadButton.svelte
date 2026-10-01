<script lang="ts">
  import type { Model, ModelLoadConflict } from "../lib/types";
  import { getModelLoadConflicts, sleepModel, unloadSingleModel, wakeModel } from "../stores/api";
  import { handleLoadModel, isPending, pendingLoads, onToggleLoad } from "../stores/modelLoad";
  import { translate } from "../lib/i18n";
  import { Moon, Play, PowerOff, Loader2, Square, Sun } from "@lucide/svelte";
  import ConfirmDialog from "./ConfirmDialog.svelte";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import { Button } from "$lib/components/ui/button/index.js";

  interface Props {
    model: Model;
    /** "md" for list rows (size-7), "sm" for the detail header (size-5). */
    size?: "md" | "sm";
  }

  let { model, size = "md" }: Props = $props();

  /** How long an accepted stop may stay ineffective before it is reported. */
  const STOP_INEFFECTIVE_MS = 10_000;

  let btnSize = $derived(size === "sm" ? "size-5 rounded-sm" : "size-7 rounded-md");
  let iconSize = $derived(size === "sm" ? "size-3.5" : "size-4");
  let pending = $derived(!!$pendingLoads[model.id]);
  let lifecycleBusy = $derived(model.state === "starting" || model.state === "stopping");
  let conflictLoading = $state(false);
  let conflictDialogOpen = $state(false);
  let conflictModels = $state<ModelLoadConflict[]>([]);
  let conflictError = $state("");
  let sleepBusy = $state(false);
  let sleepError = $state("");
  let stopBusy = $state(false);
  let stopError = $state("");
  let stopDialogOpen = $state(false);
  let stopNotice = $state("");
  // Timestamp of the accepted stop request, used to notice a stop that the
  // backend accepted but that never took effect: a start wedged in a compile
  // step keeps reporting "starting" indefinitely, which used to look like the
  // button simply did nothing.
  let stopRequestedAt = $state(0);
  // Only "stopping" blocks the button: while a model is "starting" the button
  // becomes the stop affordance because the backend can abort a mid-start
  // process, and the load request's own cancellation cannot.
  let buttonBusy = $derived(model.state === "stopping" || stopBusy || (conflictLoading && !pending));
  let sleepCapable = $derived(
    model.backendType?.trim().toLowerCase() === "vllm" &&
      (model.state === "ready" || model.state === "sleeping"),
  );

  async function handleClick(): Promise<void> {
    if (model.state === "stopping" || stopBusy || conflictLoading) return;
    if (model.state === "starting") {
      // Confirm first: aborting a start discards minutes of weight loading and
      // kernel compilation, so it must not be a silent one-click action.
      stopError = "";
      stopNotice = "";
      stopDialogOpen = true;
      return;
    }
    if (model.state !== "stopped") {
      onToggleLoad(model);
      return;
    }
    if (isPending(model.id)) {
      onToggleLoad(model);
      return;
    }

    conflictError = "";
    conflictLoading = true;
    try {
      const conflicts = await getModelLoadConflicts(model.id);
      if (conflicts.length > 0) {
        conflictModels = conflicts;
        conflictDialogOpen = true;
      } else {
        void handleLoadModel(model.id);
      }
    } catch (error) {
      conflictError = error instanceof Error ? error.message : String(error);
    } finally {
      conflictLoading = false;
    }
  }

  async function confirmStop(): Promise<void> {
    stopDialogOpen = false;
    stopError = "";
    stopNotice = "";
    stopBusy = true;
    try {
      // The endpoint blocks until the stop is applied, so a clean return means
      // the process is gone or on its way out; the notice below states that,
      // because the button returning to "start" is otherwise easy to miss.
      await unloadSingleModel(model.id);
      stopRequestedAt = Date.now();
      stopNotice = $translate("modelLoad.stopRequested", { model: model.name || model.id });
    } catch (error) {
      stopError = error instanceof Error ? error.message : String(error);
    } finally {
      stopBusy = false;
    }
  }

  // Keep the notice honest: clear it once the model leaves "starting", and
  // warn when the process is still starting well after the stop was accepted.
  // That case is a wedged process, not a slow one, and calling it out is the
  // difference between "the button is broken" and "the backend did not stop".
  $effect(() => {
    if (model.state !== "starting") {
      stopNotice = "";
      stopRequestedAt = 0;
      return;
    }
    if (stopRequestedAt === 0) return;
    const timer = setTimeout(() => {
      stopNotice = $translate("modelLoad.stopIneffective");
    }, STOP_INEFFECTIVE_MS);
    return () => clearTimeout(timer);
  });

  function confirmLoad(): void {
    if (conflictModels.length === 0) return;
    conflictDialogOpen = false;
    conflictModels = [];
    void handleLoadModel(model.id);
  }

  async function handleSleepClick(): Promise<void> {
    if (!sleepCapable || sleepBusy || lifecycleBusy || conflictLoading) return;
    sleepBusy = true;
    sleepError = "";
    try {
      if (model.state === "sleeping") await wakeModel(model.id);
      else await sleepModel(model.id);
    } catch (error) {
      sleepError = error instanceof Error ? error.message : String(error);
    } finally {
      sleepBusy = false;
    }
  }

  function conflictStateLabel(state: string): string {
    const key = `status.model.${state}`;
    const label = $translate(key);
    return label === key ? state : label;
  }
</script>

<button
  type="button"
  class="text-muted-foreground hover:bg-accent hover:text-accent-foreground flex {btnSize} shrink-0 items-center justify-center disabled:opacity-50"
  title={model.disabled ? $translate("modelLoad.disabledHint") : model.state === "ready" || model.state === "sleeping" ? $translate("common.unload") : model.state === "starting" ? $translate("common.stopStart") : pending ? $translate("common.cancel") : $translate("common.load")}
  aria-label={model.disabled ? $translate("modelLoad.disabledHint") : model.state === "ready" || model.state === "sleeping" ? $translate("common.unloadModel") : model.state === "starting" ? $translate("common.stopStart") : $translate("common.loadModel")}
  aria-busy={conflictLoading || stopBusy}
  disabled={buttonBusy || model.disabled}
  onclick={() => void handleClick()}
>
  {#if conflictLoading || stopBusy}
    <Loader2 class="{iconSize} animate-spin" />
  {:else if model.state === "ready" || model.state === "sleeping"}
    <PowerOff class={iconSize} />
  {:else if model.state === "starting"}
    <!-- A stop affordance, not a spinner: the start is cancellable, and a
         spinner reads as "busy, nothing to do here", which is exactly why the
         cancel action was invisible. -->
    <Square class={iconSize} />
  {:else if model.state === "stopping"}
    <Loader2 class="{iconSize} animate-spin" />
  {:else if pending && model.state === "stopped"}
    <Loader2 class="{iconSize} animate-spin" />
  {:else}
    <Play class={iconSize} />
  {/if}
</button>

{#if sleepCapable}
  <button
    type="button"
    class="text-info hover:bg-accent hover:text-accent-foreground flex {btnSize} shrink-0 items-center justify-center disabled:opacity-50"
    title={model.state === "sleeping" ? $translate("common.wakeModel") : $translate("common.sleepModel")}
    aria-label={model.state === "sleeping" ? $translate("common.wakeModel") : $translate("common.sleepModel")}
    aria-busy={sleepBusy}
    disabled={buttonBusy || sleepBusy}
    onclick={() => void handleSleepClick()}
  >
    {#if sleepBusy}
      <Loader2 class="{iconSize} animate-spin" />
    {:else if model.state === "sleeping"}
      <Sun class={iconSize} />
    {:else}
      <Moon class={iconSize} />
    {/if}
  </button>
{/if}

{#if conflictError}
  <p class="mt-1 max-w-[22rem] text-xs text-destructive" role="alert">
    {$translate("modelLoad.lookupFailed", { message: conflictError })}
  </p>
{/if}

{#if sleepError}
  <p class="mt-1 max-w-[22rem] text-xs text-destructive" role="alert">
    {sleepError}
  </p>
{/if}

{#if stopError}
  <p class="mt-1 max-w-[22rem] text-xs text-destructive" role="alert">
    {stopError}
  </p>
{:else if stopNotice}
  <p class="mt-1 max-w-[22rem] text-xs text-muted-foreground" role="status">
    {stopNotice}
  </p>
{/if}

<ConfirmDialog
  bind:open={stopDialogOpen}
  title={$translate("modelLoad.stopTitle")}
  message={$translate("modelLoad.stopDescription", { model: model.name || model.id })}
  confirmLabel={$translate("modelLoad.confirmStop")}
  onConfirm={() => void confirmStop()}
/>

<Dialog.Root bind:open={conflictDialogOpen}>
  <Dialog.Content class="max-w-lg">
    <Dialog.Header>
      <Dialog.Title>{$translate("modelLoad.conflictTitle")}</Dialog.Title>
      <Dialog.Description>
        {$translate("modelLoad.conflictDescription", { model: model.name || model.id })}
      </Dialog.Description>
    </Dialog.Header>
    <ul class="border-border bg-muted/30 divide-y rounded-lg border text-sm">
      {#each conflictModels as conflict (conflict.id)}
        <li class="flex items-center justify-between gap-3 px-3 py-2">
          <span class="min-w-0 truncate font-medium" title={conflict.id}>{conflict.name || conflict.id}</span>
          <span class="text-muted-foreground shrink-0">{conflictStateLabel(conflict.state)}</span>
        </li>
      {/each}
    </ul>
    <Dialog.Footer>
      <Button variant="outline" onclick={() => (conflictDialogOpen = false)}>
        {$translate("common.cancel")}
      </Button>
      <Button variant="destructive" onclick={confirmLoad}>
        {$translate("modelLoad.confirmConflict")}
      </Button>
    </Dialog.Footer>
  </Dialog.Content>
</Dialog.Root>
