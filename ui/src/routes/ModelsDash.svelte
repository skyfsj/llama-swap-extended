<script lang="ts">
  import { onMount } from "svelte";
  import { link } from "svelte-spa-router";
  import {
    activeProfile,
    fetchPlaygroundModels,
    models,
    profiles,
    selectorModels,
    unloadAllModels,
  } from "../stores/api";
  import { statusDotColor } from "../stores/modelLoad";
  import { showUnlistedModels as showUnlisted, showCapabilityTags } from "../stores/modelDisplay";
  import { listCapabilityBadges, capabilityBadgeClass, capabilityMessageKeys } from "../lib/capabilities";
  import type { Model } from "../lib/types";
  import ModelLoadButton from "../components/ModelLoadButton.svelte";
  import Tag from "../components/Tag.svelte";
  import * as Card from "$lib/components/ui/card/index.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import * as Switch from "$lib/components/ui/switch/index.js";
  import * as Label from "$lib/components/ui/label/index.js";
  import { Loader2, Pencil, Plus, PowerOff, Search, SquareStack, Trash2 } from "@lucide/svelte";
  import { Input } from "$lib/components/ui/input/index.js";
  import { translate } from "../lib/i18n";
  import ModelCreateDialog from "../components/model/ModelCreateDialog.svelte";
  import ModelDeleteDialog from "../components/model/ModelDeleteDialog.svelte";
  import ModelSettingsDialog from "../components/model/ModelSettingsDialog.svelte";

  let unloadingAll = $state(false);
  let createDialogOpen = $state(false);
  let settingsDialogOpen = $state(false);
  let settingsModelID = $state("");
  let deleteDialogOpen = $state(false);
  let deleteModelID = $state("");
  let modelQuery = $state("");

  onMount(() => {
    void fetchPlaygroundModels();
  });

  let visibleModels = $derived(
    $showUnlisted ? $models : $models.filter((m) => !m.unlisted)
  );
  let localModels = $derived(visibleModels.filter((model) => !model.peerID));
  let peerModels = $derived(visibleModels.filter((model) => model.peerID));
  let matchingLocalModels = $derived(localModels.filter((model) => matchesModel(model, modelQuery)));
  let matchingPeerModels = $derived(peerModels.filter((model) => matchesModel(model, modelQuery)));
  let selectedProfile = $derived(
    $profiles.find((profile) => profile.id === $activeProfile)
  );
  let profileMappings = $derived(
    Object.entries(selectedProfile?.pins ?? {}).sort(([a], [b]) =>
      a.localeCompare(b, undefined, { numeric: true })
    )
  );

  let readyCount = $derived($models.filter((m) => m.state === "ready").length);
  let anyLoaded = $derived($models.some((m) => m.state === "ready" || m.state === "sleeping"));

  async function handleUnloadAll(): Promise<void> {
    unloadingAll = true;
    try {
      await unloadAllModels();
    } catch (e) {
      console.error(e);
    } finally {
      unloadingAll = false;
    }
  }

  function openModelSettings(modelID: string): void {
    settingsModelID = modelID;
    settingsDialogOpen = true;
  }

  function openModelDelete(modelID: string): void {
    deleteModelID = modelID;
    deleteDialogOpen = true;
  }

  function matchesModel(model: Model, query: string): boolean {
    const needle = query.trim().toLocaleLowerCase();
    if (!needle) return true;
    return [model.id, model.name, model.description, ...(model.aliases ?? [])]
      .join(" ")
      .toLocaleLowerCase()
      .includes(needle);
  }
</script>

{#snippet modelSection(titleKey: string, sectionModels: Model[], local = false)}
  <Card.Root class="shrink-0 gap-0 overflow-hidden py-0">
    <Card.Header class="shrink-0 border-b px-4 py-2.5">
      <div class="flex items-center gap-2">
        <Card.Title class="text-sm">{$translate(titleKey)}</Card.Title>
        <span class="text-muted-foreground text-xs">{sectionModels.length}</span>
        {#if local}<div class="ml-auto flex items-center gap-2">{@render unlistedToggle()}</div>{/if}
      </div>
    </Card.Header>
    <Card.Content class="p-0">
      {#if sectionModels.length === 0}
        <div class="text-muted-foreground px-4 py-6 text-center text-sm">
          {$translate("models.noAvailable", { section: $translate(titleKey) })}
        </div>
      {:else}
        <div class="overflow-x-auto">
          <table class="w-full min-w-[48rem] text-left text-sm">
            <thead class="bg-muted/30 text-muted-foreground border-b text-xs">
              <tr>
                <th class="px-4 py-2.5 font-medium">{$translate("models.modelID")}</th>
                <th class="px-4 py-2.5 font-medium">{$translate("controlPlane.modelName")}</th>
                <th class="px-4 py-2.5 font-medium">{$translate("controlPlane.modelDescription")}</th>
                <th class="px-4 py-2.5 font-medium">{$translate("modelFiles.status")}</th>
                <th class="px-4 py-2.5 text-right font-medium">{$translate("modelFiles.actions")}</th>
              </tr>
            </thead>
            <tbody class="divide-y">
              {#each sectionModels as model (model.id)}
                <tr class="hover:bg-muted/30 transition-colors">
                  <td class="max-w-60 px-4 py-3">
                    <a href="/models/{encodeURIComponent(model.id)}" use:link class="flex min-w-0 items-center gap-2 text-sm font-medium hover:underline">
                      {#if local}<span class={`size-2 shrink-0 rounded-full ${statusDotColor(model)}`}></span>{/if}
                      <code class="truncate">{model.id}</code>
                    </a>
                  </td>
                  <td class="max-w-64 px-4 py-3">
                    <div class="truncate">{model.name || model.id}</div>
                    {#if model.aliases?.length}
                      <div class="text-muted-foreground mt-1 flex flex-wrap gap-1">
                        {#each model.aliases as alias (alias)}<span class="bg-muted rounded px-1.5 py-0.5 text-[0.625rem]">{alias}</span>{/each}
                      </div>
                    {/if}
                  </td>
                  <td class="text-muted-foreground max-w-80 px-4 py-3 text-xs">
                    <span class="block truncate" title={model.description}>{model.description || "—"}</span>
                    {#if $showCapabilityTags}
                      {@const badges = listCapabilityBadges(model)}
                      {#if badges.length > 0}
                        <div class="mt-1 flex flex-wrap gap-1">
                          {#each badges as badge (badge.key)}
                            <Tag class={`px-1.5 text-[0.625rem] ${capabilityBadgeClass[badge.key] ?? ""}`}>
                              {capabilityMessageKeys[badge.key] ? $translate(capabilityMessageKeys[badge.key]) : badge.label ?? badge.key}
                            </Tag>
                          {/each}
                        </div>
                      {/if}
                    {/if}
                  </td>
                  <td class="px-4 py-3">
                    <div class="flex items-center gap-2 text-xs"><span class={`size-2 rounded-full ${statusDotColor(model)}`}></span><span>{$translate(`status.model.${model.state}`)}</span></div>
                    {#if model.disabled}<Tag class="mt-1 border-destructive/40 bg-destructive/10 px-1.5 text-[0.625rem] uppercase text-destructive">{$translate("models.disabled")}</Tag>{/if}
{#if model.maintenance}<Tag class="mt-1 border-amber-500/40 bg-amber-500/10 px-1.5 text-[0.625rem] uppercase text-amber-600 dark:text-amber-400">{$translate("models.maintenance")}</Tag>{/if}
                    {#if model.unlisted}<Tag class="mt-1 px-1.5 text-[0.625rem] uppercase">{$translate("models.unlisted")}</Tag>{/if}
                  </td>
                  <td class="px-4 py-3 text-right">
                    {#if local}
                      <div class="inline-flex items-center justify-end gap-2"><ModelLoadButton {model} /><Button variant="outline" size="icon-sm" aria-label={$translate("settingsCenter.model.editAction", { id: model.id })} title={$translate("settingsCenter.model.editAction", { id: model.id })} onclick={() => openModelSettings(model.id)}><Pencil class="size-3.5" aria-hidden="true" /></Button><Button variant="outline" size="icon-sm" aria-label={$translate("settingsCenter.model.deleteAction", { id: model.id })} title={$translate("settingsCenter.model.deleteAction", { id: model.id })} onclick={() => openModelDelete(model.id)}><Trash2 class="size-3.5" aria-hidden="true" /></Button></div>
                    {:else}<span class="text-muted-foreground">—</span>{/if}
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {/if}
    </Card.Content>
  </Card.Root>
{/snippet}

{#snippet unlistedToggle()}
  <Label.Root for="show-unlisted-toggle" class="text-sm">
    {$translate("models.showUnlisted")}
  </Label.Root>
  <Switch.Root
    id="show-unlisted-toggle"
    checked={$showUnlisted}
    onCheckedChange={(v) => showUnlisted.set(v)}
  />
  <span class="text-muted-foreground text-xs">
    {$translate("models.unlistedCount", { count: $models.filter((m) => m.unlisted).length })}
  </span>
{/snippet}

<div class="flex h-full flex-col gap-4 overflow-y-auto p-2">
  <Card.Root class="shrink-0 gap-0 overflow-hidden py-0">
    <Card.Header class="shrink-0 gap-3 border-b px-4 py-3">
      <div class="flex items-center gap-2">
        <SquareStack class="size-5" />
        <Card.Title class="text-lg">{$translate("models.title")}</Card.Title>
        <span class="text-muted-foreground text-sm">
          {$translate("models.visibleCount", { visible: visibleModels.length, total: $models.length })}
        </span>
        <span class="text-muted-foreground text-xs uppercase tracking-wide">
          {$translate("models.readyCount", { count: readyCount })}
        </span>
        <div class="ml-auto flex items-center gap-2">
          <Button variant="outline" size="sm" onclick={() => (createDialogOpen = true)}>
            <Plus class="size-3.5" aria-hidden="true" />
            {$translate("models.addConfig")}
          </Button>
          <Button
            variant="outline"
            size="sm"
            onclick={handleUnloadAll}
            disabled={!anyLoaded || unloadingAll}
          >
            {#if unloadingAll}
              <Loader2 class="size-3.5 animate-spin" />
            {:else}
              <PowerOff class="size-3.5" />
            {/if}
            {$translate("models.unloadAll")}
          </Button>
        </div>
      </div>
      <div class="relative max-w-xl">
        <Search class="text-muted-foreground pointer-events-none absolute top-1/2 left-3 size-4 -translate-y-1/2" aria-hidden="true" />
        <Input class="pl-9" aria-label={$translate("settingsCenter.model.searchLabel")} placeholder={$translate("settingsCenter.model.searchPlaceholder")} value={modelQuery} oninput={(event) => (modelQuery = event.currentTarget.value)} />
      </div>
    </Card.Header>
  </Card.Root>

  <div class="flex min-h-0 shrink-0 flex-col gap-4">
    {#if $profiles.length > 0}
      <Card.Root class="shrink-0 gap-0 overflow-hidden py-0">
        <Card.Header class="shrink-0 border-b px-4 py-2.5">
          <div class="flex items-center gap-2">
            <Card.Title class="text-sm">{$translate("models.profiles")}</Card.Title>
            {#if selectedProfile}
              <Tag>{$activeProfile}</Tag>
              <Tag class="bg-success/15 text-success">{$translate("models.active")}</Tag>
            {/if}
            <span class="text-muted-foreground ml-auto text-xs">
              {profileMappings.length} {$translate(profileMappings.length === 1 ? "models.mapping" : "models.mappings")}
            </span>
          </div>
          {#if selectedProfile?.description}
            <p class="text-muted-foreground text-xs">{selectedProfile.description}</p>
          {/if}
        </Card.Header>
        <Card.Content class="p-0">
          {#if !selectedProfile}
            <div class="text-muted-foreground px-4 py-6 text-center text-sm">
              {$translate("models.noActiveProfile")}
            </div>
          {:else}
            <div class="divide-y">
              {#each profileMappings as [modelID, target] (modelID)}
                <div class="hover:bg-muted/50 flex items-center gap-2 px-4 py-2.5">
                  <span class="max-w-[45%] truncate text-sm font-medium">{modelID}</span>
                  <span class="text-muted-foreground text-xs" aria-hidden="true">→</span>
                  {#if target}
                    <span class="min-w-0 truncate text-sm">{target}</span>
                  {:else}
                    <Tag class="px-1.5 text-[0.625rem] uppercase">{$translate("models.disabled")}</Tag>
                  {/if}
                </div>
              {/each}
            </div>
          {/if}
        </Card.Content>
      </Card.Root>
    {/if}

    {#if $selectorModels.length > 0}
      <Card.Root class="shrink-0 gap-0 overflow-hidden py-0">
        <Card.Header class="shrink-0 border-b px-4 py-2.5">
          <div class="flex items-center gap-2">
            <Card.Title class="text-sm">{$translate("models.selectors")}</Card.Title>
            <span class="text-muted-foreground ml-auto text-xs">
              {$selectorModels.length} {$translate($selectorModels.length === 1 ? "models.selector" : "models.selectorsCount")}
            </span>
          </div>
        </Card.Header>
        <Card.Content class="p-0">
          <div class="divide-y">
            {#each $selectorModels as selector (selector.id)}
              <div class="hover:bg-muted/50 flex items-center gap-2 px-4 py-2.5">
                <div class="min-w-0 flex-1">
                  <div class="truncate text-sm font-medium">
                    {selector.name ? `${selector.id} - ${selector.name}` : selector.id}
                  </div>
                  {#if selector.description}
                    <div class="text-muted-foreground truncate text-xs">
                      {selector.description}
                    </div>
                  {/if}
                  <div class="text-muted-foreground flex flex-wrap items-center gap-x-1 text-xs">
                    <span>{$translate("models.targets")}</span>
                    {#each selector.targets ?? [] as target, i (target)}
                      {#if i > 0}<span>,</span>{/if}
                      <a
                        href="/models/{encodeURIComponent(target)}"
                        use:link
                        class="hover:text-foreground hover:underline"
                      >{target}</a>
                    {/each}
                  </div>
                </div>
                {#if selector.strategy === "spillover" && selector.spillover}
                  <Tag>{$translate("models.spillover")} {selector.spillover}</Tag>
                {/if}
                <Tag class="px-1.5 text-[0.625rem] uppercase">{selector.strategy}</Tag>
              </div>
            {/each}
          </div>
        </Card.Content>
      </Card.Root>
    {/if}

    {@render modelSection("models.localModels", matchingLocalModels, true)}
    {@render modelSection("models.peerModels", matchingPeerModels)}
  </div>
</div>

<ModelCreateDialog bind:open={createDialogOpen} onCreated={() => void fetchPlaygroundModels()} />
<ModelDeleteDialog modelId={deleteModelID} bind:open={deleteDialogOpen} onDeleted={() => void fetchPlaygroundModels()} />
<ModelSettingsDialog modelId={settingsModelID} bind:open={settingsDialogOpen} onSaved={() => void fetchPlaygroundModels()} />
