<script lang="ts">
  import { params } from "svelte-spa-router";
  import { fetchPlaygroundModels, models } from "../stores/api";
  import { statusDotColor } from "../stores/modelLoad";
  import type { Model } from "../lib/types";
  import ModelLoadButton from "../components/ModelLoadButton.svelte";
  import ModelSettingsDialog from "../components/model/ModelSettingsDialog.svelte";
  import * as Card from "$lib/components/ui/card/index.js";
  import { Tabs, TabsList, TabsTrigger, TabsContent } from "$lib/components/ui/tabs/index.js";
  import { Settings2 } from "@lucide/svelte";
  import ModelActivityTab from "../components/model/ModelActivityTab.svelte";
  import ModelLogsTab from "../components/model/ModelLogsTab.svelte";
  import ModelDetailsTab from "../components/model/ModelDetailsTab.svelte";
  import { translate } from "../lib/i18n";
  import { forceRestartModel, restartModel } from "../stores/api";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import { Button } from "$lib/components/ui/button/index.js";

  const safeExitStatusPrefix = "model_config_update_failed_exit_";

  let modelId = $derived($params?.id ?? "");

  // Resolve the route param to a model record by ID, falling back to an
  // alias match so links to alias targets (e.g. selector targets) resolve.
  let model = $derived<Model | undefined>(
    $models.find((m) => m.id === modelId) ??
      $models.find((m) => m.aliases?.includes(modelId)),
  );
  let resolvedId = $derived(model?.id ?? modelId);
  let configStatus = $derived(model?.configStatus ?? "applied");
  let detailsAvailable = $derived.by(() => {
    const current = model;
    return Boolean(
      current &&
        (Object.values(current.capabilities ?? {}).some(Boolean) || (current.context_length ?? 0) > 0),
    );
  });
  let restartable = $derived(configStatus === "modified" || configStatus === "apply_failed");
  let lifecycleBusy = $derived(
    configStatus === "draining" || configStatus === "restarting" || configStatus === "rolling_back" ||
      configStatus === "removing" || configStatus === "unloading",
  );
  let restartDialogOpen = $state(false);
  let forceRestartDialogOpen = $state(false);
  let settingsDialogOpen = $state(false);
  let restartBusy = $state(false);
  let forceRestartBusy = $state(false);
  let restartError = $state("");
  let forceRestartError = $state("");

  async function confirmRestart(): Promise<void> {
    if (!model || !restartable || restartBusy || lifecycleBusy) return;
    restartBusy = true;
    restartError = "";
    try {
      await restartModel(resolvedId);
      restartDialogOpen = false;
    } catch (error) {
      restartError = error instanceof Error ? error.message : String(error);
    } finally {
      restartBusy = false;
    }
  }

  async function confirmForceRestart(): Promise<void> {
    if (
      !model ||
      (configStatus !== "draining" && configStatus !== "restarting") ||
      forceRestartBusy
    ) return;
    forceRestartBusy = true;
    forceRestartError = "";
    try {
      await forceRestartModel(resolvedId);
      forceRestartDialogOpen = false;
    } catch (error) {
      forceRestartError = error instanceof Error ? error.message : String(error);
    } finally {
      forceRestartBusy = false;
    }
  }
</script>

<div class="flex h-full flex-col gap-4 overflow-y-auto p-2">
  {#if !model}
    <Card.Root class="shrink-0 p-6">
      <p class="text-muted-foreground">{$translate("modelDetail.notFound", { id: modelId })}</p>
      <a href="/" class="text-primary hover:underline">{$translate("modelDetail.backToPlayground")}</a>
    </Card.Root>
  {:else}
    <Card.Root class="shrink-0 gap-0 overflow-hidden py-0">
      <Card.Header class="shrink-0 gap-2 border-b px-4 py-3">
        <div class="flex min-w-0 flex-wrap items-center gap-2">
          <span class={`size-2.5 shrink-0 rounded-full ${statusDotColor(model)}`}></span>
          <Card.Title class="min-w-0 truncate text-lg">{model.name || model.id}</Card.Title>
          <span class="text-muted-foreground text-sm">({model.id})</span>
          <span class="text-muted-foreground text-xs uppercase tracking-wide">{$translate(`status.model.${model.state}`)}</span>
          <div class="ml-auto flex shrink-0 items-center gap-2">
            {#if !model.peerID}
              <ModelLoadButton {model} size="sm" />
              <Button
                variant="outline"
                size="sm"
                title={$translate("controlPlane.modelSettings")}
                aria-label={$translate("controlPlane.modelSettings")}
                onclick={() => (settingsDialogOpen = true)}
              >
                <Settings2 class="size-3.5" aria-hidden="true" />
                <span>{$translate("controlPlane.modelSettings")}</span>
              </Button>
            {/if}
          </div>
        </div>
        {#if model.description}
          <p class="text-muted-foreground text-sm"><em>{model.description}</em></p>
        {/if}
        {#if model.aliases && model.aliases.length > 0}
          <p class="text-muted-foreground text-xs">{$translate("modelDetail.aliases")}: {model.aliases.join(", ")}</p>
        {/if}
      </Card.Header>
    </Card.Root>

    <ModelSettingsDialog
      modelId={resolvedId}
      bind:open={settingsDialogOpen}
      onSaved={() => void fetchPlaygroundModels()}
    />

    {#if configStatus !== "applied"}
      <Card.Root
        class="shrink-0 border-warning/40 bg-warning/10 p-4"
        role="status"
        aria-live="polite"
        aria-label={$translate(`modelDetail.configStatus.${configStatus}`)}
      >
        <div class="flex flex-wrap items-start gap-3">
          <div class="min-w-0 flex-1">
            <p class="font-medium text-warning">{$translate("modelDetail.configNotice")}</p>
            <p class="mt-1 text-sm text-muted-foreground">
              {$translate(`modelDetail.configStatus.${configStatus}`)}
              {#if (model.oldRequests ?? 0) > 0}
                · {$translate("modelDetail.oldRequests", { count: model.oldRequests ?? 0 })}
              {/if}
              {#if (model.waitingRequests ?? 0) > 0}
                · {$translate("modelDetail.waitingRequests", { count: model.waitingRequests ?? 0 })}
              {/if}
            </p>
            {#if model.error}
              <p class="mt-1 text-sm text-destructive">
                {#if model.error === "model_config_update_failed"}
                  {$translate("modelDetail.configUpdateFailed")}
                {:else if model.error.startsWith(safeExitStatusPrefix)}
                  {$translate("modelDetail.configUpdateExitStatus", { status: model.error.slice(safeExitStatusPrefix.length) })}
                {:else}
                  {model.error}
                {/if}
              </p>
            {/if}
          </div>
          {#if restartable}
            <Button
              variant="outline"
              class="border-warning/50 hover:bg-warning/15"
              disabled={restartBusy || lifecycleBusy}
              onclick={() => {
                restartError = "";
                restartDialogOpen = true;
              }}
            >
              {$translate(configStatus === "apply_failed" ? "modelDetail.retryRestart" : "modelDetail.restartModel")}
            </Button>
          {/if}
          {#if configStatus === "draining" || configStatus === "restarting"}
            <Button
              variant="destructive"
              disabled={forceRestartBusy}
              onclick={() => {
                forceRestartError = "";
                forceRestartDialogOpen = true;
              }}
            >
              {$translate("modelDetail.forceRestartModel")}
            </Button>
          {/if}
        </div>
        {#if restartError}
          <p class="mt-2 text-sm text-destructive" role="alert">
            {$translate("modelDetail.restartFailed", { message: restartError })}
          </p>
        {/if}
        {#if forceRestartError}
          <p class="mt-2 text-sm text-destructive" role="alert">
            {$translate("modelDetail.forceRestartFailed", { message: forceRestartError })}
          </p>
        {/if}
      </Card.Root>
    {/if}

    <Dialog.Root bind:open={restartDialogOpen}>
      <Dialog.Content class="max-w-lg">
        <Dialog.Header>
          <Dialog.Title>{$translate("modelDetail.restartModel")}</Dialog.Title>
          <Dialog.Description>
            {$translate("modelDetail.restartConfirm", { name: model.name || model.id })}
          </Dialog.Description>
        </Dialog.Header>
        <Dialog.Footer>
          <Button variant="outline" onclick={() => (restartDialogOpen = false)} disabled={restartBusy}>
            {$translate("common.cancel")}
          </Button>
          <Button onclick={() => void confirmRestart()} disabled={restartBusy}>
            {restartBusy ? $translate("common.loading") : $translate("modelDetail.restartModel")}
          </Button>
        </Dialog.Footer>
      </Dialog.Content>
    </Dialog.Root>

    <Dialog.Root bind:open={forceRestartDialogOpen}>
      <Dialog.Content class="max-w-lg">
        <Dialog.Header>
          <Dialog.Title>{$translate("modelDetail.forceRestartModel")}</Dialog.Title>
          <Dialog.Description>
            {$translate("modelDetail.forceRestartConfirm", { name: model.name || model.id })}
          </Dialog.Description>
        </Dialog.Header>
        <Dialog.Footer>
          <Button variant="outline" onclick={() => (forceRestartDialogOpen = false)} disabled={forceRestartBusy}>
            {$translate("common.cancel")}
          </Button>
          <Button variant="destructive" onclick={() => void confirmForceRestart()} disabled={forceRestartBusy}>
            {forceRestartBusy ? $translate("common.loading") : $translate("modelDetail.forceRestartModel")}
          </Button>
        </Dialog.Footer>
      </Dialog.Content>
    </Dialog.Root>

    <Tabs value="activity" class="min-h-0 flex-1">
      <TabsList variant="line">
        <TabsTrigger value="activity">{$translate("modelDetail.activity")}</TabsTrigger>
        <TabsTrigger value="logs">{$translate("modelDetail.logs")}</TabsTrigger>
        {#if detailsAvailable}
          <TabsTrigger value="details">{$translate("modelDetail.details")}</TabsTrigger>
        {/if}
      </TabsList>

      <!-- Activity -->
      <TabsContent value="activity">
        <ModelActivityTab modelId={resolvedId} />
      </TabsContent>

      <!-- Logs -->
      <TabsContent value="logs" class="min-h-0 flex-1">
        <ModelLogsTab modelId={resolvedId} />
      </TabsContent>

      <!-- Details -->
      {#if detailsAvailable}
        <TabsContent value="details">
          <ModelDetailsTab model={model} />
        </TabsContent>
      {/if}
    </Tabs>
  {/if}
</div>
