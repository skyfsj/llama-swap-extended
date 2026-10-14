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
  import { playgroundStores } from "../stores/playgroundActivity";
  import { translate } from "../lib/i18n";
  import { MessagesSquare, Image as ImageIcon, Languages, Gauge } from "@lucide/svelte";

  const MODEL_REFRESH_DEBOUNCE_MS = 200;

  const tabComponents: Record<PlaygroundTab, Component> = {
    chat: ChatInterface,
    images: ImageInterface,
    translation: TranslationInterface,
    concurrency: ConcurrencyInterface,
  };

  const tabIcons: Record<PlaygroundTab, Component> = {
    chat: MessagesSquare,
    translation: Languages,
    images: ImageIcon,
    concurrency: Gauge,
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

  const { chatStreaming, imageGenerating, translationGenerating, concurrencyRunning } = playgroundStores;

  // Track any background tab that is running while user is on another tab
  let bgRunningTab = $derived.by(() => {
    if ($selectedPlaygroundTab !== "chat" && $chatStreaming) {
      return { id: "chat" as PlaygroundTab, labelKey: "playground.tabs.chat" };
    }
    if ($selectedPlaygroundTab !== "images" && $imageGenerating) {
      return { id: "images" as PlaygroundTab, labelKey: "playground.tabs.images" };
    }
    if ($selectedPlaygroundTab !== "translation" && $translationGenerating) {
      return { id: "translation" as PlaygroundTab, labelKey: "playground.tabs.translation" };
    }
    if ($selectedPlaygroundTab !== "concurrency" && $concurrencyRunning) {
      return { id: "concurrency" as PlaygroundTab, labelKey: "playground.tabs.concurrency" };
    }
    return null;
  });
</script>

<Card.Root class="pg-card flex h-full flex-col gap-0 overflow-hidden p-0 border border-border/50 shadow-sm">
  <Tabs
    value={$selectedPlaygroundTab}
    onValueChange={(v: string) => v && selectedPlaygroundTab.set(v as PlaygroundTab)}
    class="flex flex-1 w-full flex-col gap-0 overflow-hidden"
  >
    <div class="flex shrink-0 items-center justify-between border-b px-3 py-2 pg-divide bg-muted/20">
      <TabsList variant="default" class="pg-seg">
        {#each playgroundTabs as tab (tab.id)}
          {@const Icon = tabIcons[tab.id]}
          {@const isBusy =
            tab.id === "chat"
              ? $chatStreaming
              : tab.id === "images"
                ? $imageGenerating
                : tab.id === "translation"
                  ? $translationGenerating
                  : $concurrencyRunning}
          <TabsTrigger value={tab.id} class="relative flex items-center gap-1.5 px-3">
            <Icon class="size-3.5 shrink-0" />
            <span>{$translate(tab.labelKey)}</span>
            {#if isBusy}
              <span class="relative flex size-2 ml-0.5">
                <span class="absolute inline-flex h-full w-full animate-ping rounded-full bg-primary opacity-75"></span>
                <span class="relative inline-flex size-2 rounded-full bg-primary"></span>
              </span>
            {/if}
          </TabsTrigger>
        {/each}
      </TabsList>

      {#if bgRunningTab}
        <button
          type="button"
          onclick={() => selectedPlaygroundTab.set(bgRunningTab.id)}
          class="flex items-center gap-1.5 rounded-full bg-primary/10 hover:bg-primary/20 text-primary border border-primary/20 px-2.5 py-1 text-xs transition-colors cursor-pointer"
          title={$translate(bgRunningTab.labelKey)}
        >
          <span class="size-1.5 rounded-full bg-primary animate-pulse"></span>
          <span class="font-medium">{$translate(bgRunningTab.labelKey)}</span>
          <span class="text-muted-foreground text-[10px]">({$translate("playground.concurrency.runStatusRunning")})</span>
        </button>
      {/if}
    </div>

    <div class="relative flex-1 overflow-hidden">
      {#each playgroundTabs as tab (tab.id)}
        {@const TabComponent = tabComponents[tab.id]}
        <div
          class="h-full"
          class:hidden={$selectedPlaygroundTab !== tab.id}
          class:p-3={tab.id !== "concurrency"}
          class:sm:p-4={tab.id !== "concurrency"}
        >
          <TabComponent />
        </div>
      {/each}
    </div>
  </Tabs>
</Card.Root>
