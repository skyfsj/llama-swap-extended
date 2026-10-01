<script lang="ts">
  import type { Component } from "svelte";
  import ChatInterface from "../components/playground/ChatInterface.svelte";
  import ImageInterface from "../components/playground/ImageInterface.svelte";
  import TranslationInterface from "../components/playground/TranslationInterface.svelte";
  import ConcurrencyInterface from "../components/playground/ConcurrencyInterface.svelte";
  import * as Card from "$lib/components/ui/card/index.js";
  import { Tabs, TabsList, TabsTrigger } from "$lib/components/ui/tabs/index.js";
  import { fetchPlaygroundModels, models } from "../stores/api";
  import { selectedPlaygroundTab, playgroundTabs, type PlaygroundTab } from "../stores/playground";
  import { translate } from "../lib/i18n";

  const MODEL_REFRESH_DEBOUNCE_MS = 200;

  const tabComponents: Record<PlaygroundTab, Component> = {
    chat: ChatInterface,
    images: ImageInterface,
    translation: TranslationInterface,
    concurrency: ConcurrencyInterface,
  };

  let initializedModels = false;
  $effect(() => {
    void $models;
    if (!initializedModels) {
      initializedModels = true;
      void fetchPlaygroundModels();
      return;
    }

    const timeout = window.setTimeout(() => {
      void fetchPlaygroundModels();
    }, MODEL_REFRESH_DEBOUNCE_MS);
    return () => window.clearTimeout(timeout);
  });
</script>

<Card.Root class="pg-card flex h-full flex-col gap-0 overflow-hidden p-0">
  <Tabs
    value={$selectedPlaygroundTab}
    onValueChange={(v: string) => v && selectedPlaygroundTab.set(v as PlaygroundTab)}
    class="flex flex-1 w-full flex-col gap-0 overflow-hidden"
  >
    <div class="flex shrink-0 items-center gap-3 border-b px-3 py-2 pg-divide">
      <TabsList variant="default" class="pg-seg">
        {#each playgroundTabs as tab (tab.id)}
          <TabsTrigger value={tab.id}>{$translate(tab.labelKey)}</TabsTrigger>
        {/each}
      </TabsList>
    </div>

    <div class="relative flex-1 overflow-hidden p-3">
      {#each playgroundTabs as tab (tab.id)}
        {@const TabComponent = tabComponents[tab.id]}
        <div class="h-full" class:hidden={$selectedPlaygroundTab !== tab.id}>
          <TabComponent />
        </div>
      {/each}
    </div>
  </Tabs>
</Card.Root>
