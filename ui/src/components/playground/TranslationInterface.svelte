<script lang="ts">
  import { persistentStore } from "../../stores/persistent";
  import { streamChatCompletion } from "../../lib/chatApi";
  import { playgroundStores } from "../../stores/playgroundActivity";
  import { createPlaygroundInterface } from "../../lib/playgroundInterface";
  import { playgroundModels } from "../../stores/api";
  import { filterByCategory } from "../../lib/modelCategory";
  import {
    translationLanguages,
    translationPresets,
    templateFor,
    defaultPresetTemplate,
    languageName,
    buildTranslationPrompt,
  } from "../../lib/translationPrompts";
  import ModelSelector from "./ModelSelector.svelte";
  import ExpandableTextarea from "./ExpandableTextarea.svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Textarea } from "$lib/components/ui/textarea/index.js";
  import { Label } from "$lib/components/ui/label/index.js";
  import * as Select from "$lib/components/ui/select/index.js";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import { Copy, Check, Languages, ArrowLeftRight, X, Maximize2, Trash2, SlidersHorizontal } from "@lucide/svelte";
  import { copyText } from "../../lib/clipboard";
  import { formatDuration } from "../../lib/format";
  import { locale, translate } from "../../lib/i18n";

  const iface = createPlaygroundInterface("playground-translation-model", playgroundStores.translationGenerating);
  const selectedModelStore = iface.selectedModel;
  const busyStore = iface.busy;
  const errorStore = iface.error;

  const sourceLanguageStore = persistentStore<string>("playground-translation-source", "auto");
  const targetLanguageStore = persistentStore<string>("playground-translation-target", "en");
  const presetStore = persistentStore<string>("playground-translation-preset", "default");
  const templateStore = persistentStore<string>(
    "playground-translation-template",
    defaultPresetTemplate("zh")
  );

  let sourceText = $state("");
  let resultText = $state("");
  let isReasoning = $state(false);
  let reasoningStartTime = $state(0);
  let reasoningTimeMs = $state(0);
  let elapsedMs = $state(0);
  let copied = $state(false);
  let showSettings = $state(false);

  const hasModels = $derived(
    filterByCategory(
      $playgroundModels.filter(
        (m) => m.playgroundType === "model" || m.playgroundType === "peer"
      ),
      "translation"
    ).length > 0
  );

  const currentPreset = $derived(
    translationPresets.find((p) => p.id === $presetStore) ?? translationPresets[0]
  );
  const sourceLanguage = $derived(
    translationLanguages.find((l) => l.code === $sourceLanguageStore)
  );
  const targetLanguage = $derived(
    translationLanguages.find((l) => l.code === $targetLanguageStore) ?? translationLanguages[1]
  );
  const targetLanguageDisplay = $derived(languageName(targetLanguage, $locale));
  const sourceLanguageDisplay = $derived(
    sourceLanguage ? languageName(sourceLanguage, $locale) : $translate("playground.translation.autoDetect")
  );
  const canTranslate = $derived(
    sourceText.trim().length > 0 && !!$selectedModelStore && !$busyStore
  );

  function handlePresetChange(id: string) {
    presetStore.set(id);
    const preset = translationPresets.find((p) => p.id === id);
    if (preset) templateStore.set(templateFor(preset, $locale));
  }

  function swapLanguages() {
    const previousSource = $sourceLanguageStore;
    const previousTarget = $targetLanguageStore;
    sourceLanguageStore.set(previousTarget);
    // "Detect language" only lives on the source side; when it swaps over, the
    // target falls back to the UI locale's language instead of duplicating.
    targetLanguageStore.set(
      previousSource === "auto"
        ? $locale.startsWith("zh")
          ? "zh"
          : "en"
        : previousSource
    );
  }

  async function translateText() {
    if (!canTranslate) return;
    resultText = "";
    reasoningTimeMs = 0;
    elapsedMs = 0;
    const startedAt = Date.now();
    await iface.run(async (signal) => {
      const prompt = buildTranslationPrompt(
        $templateStore,
        targetLanguageDisplay,
        sourceText,
        sourceLanguage ? languageName(sourceLanguage, $locale) : undefined
      );
      const stream = streamChatCompletion(
        $selectedModelStore,
        [{ role: "user", content: prompt }],
        signal,
        // Hy-MT2 recommended sampling for the 1.8B/7B class.
        { temperature: 0.7, max_tokens: 4096, include_usage: true }
      );
      for await (const chunk of stream) {
        if (chunk.done) break;
        if (chunk.reasoning_content) {
          if (!isReasoning) {
            isReasoning = true;
            reasoningStartTime = Date.now();
          }
          continue;
        }
        if (chunk.content) {
          if (isReasoning) {
            reasoningTimeMs = Date.now() - reasoningStartTime;
            isReasoning = false;
          }
          resultText += chunk.content;
        }
      }
      elapsedMs = Date.now() - startedAt;
    });
    isReasoning = false;
  }

  async function copyResult() {
    if (await copyText(resultText)) {
      copied = true;
      setTimeout(() => (copied = false), 2000);
    }
  }

  function handleKeyDown(event: KeyboardEvent) {
    if ((event.metaKey || event.ctrlKey) && event.key === "Enter") {
      event.preventDefault();
      void translateText();
    }
  }
</script>

<div class="flex flex-col h-full">
  <!-- Unified Toolbar: Model, Language Switcher, Scenario, Settings -->
  <div class="shrink-0 flex flex-wrap items-center gap-2 mb-3">
    <ModelSelector
      bind:value={$selectedModelStore}
      placeholder={$translate("playground.translation.modelPlaceholder")}
      disabled={$busyStore}
      category="translation"
      extraCategory="chat"
      extraCategoryLabelKey="playground.modelSelector.groupChat"
      groupLabelKey="playground.modelSelector.groupTranslation"
    />

    <!-- Languages Selector -->
    <div class="flex items-center gap-1 rounded-lg border border-border/60 bg-muted/30 p-0.5">
      <Select.Root
        type="single"
        value={$sourceLanguageStore}
        onValueChange={(v) => v && sourceLanguageStore.set(v)}
        disabled={$busyStore}
      >
        <Select.Trigger class="pg-trigger h-8 min-w-[7.5rem] border-0 bg-transparent shadow-none px-2 text-xs">
          <span class="flex items-center gap-1 truncate">
            <Languages class="size-3.5 shrink-0 opacity-60" />
            <span class="truncate">{sourceLanguageDisplay}</span>
          </span>
        </Select.Trigger>
        <Select.Content class="max-h-[60vh]">
          <Select.Item value="auto">{$translate("playground.translation.autoDetect")}</Select.Item>
          {#each translationLanguages as lang (lang.code)}
            <Select.Item value={lang.code}>{languageName(lang, $locale)}</Select.Item>
          {/each}
        </Select.Content>
      </Select.Root>

      <Button
        variant="ghost"
        size="icon-xs"
        class="size-7 shrink-0 text-muted-foreground hover:text-foreground"
        onclick={swapLanguages}
        disabled={$busyStore}
        title={$translate("playground.translation.swapLanguages")}
      >
        <ArrowLeftRight class="size-3.5" />
      </Button>

      <Select.Root
        type="single"
        value={$targetLanguageStore}
        onValueChange={(v) => v && targetLanguageStore.set(v)}
        disabled={$busyStore}
      >
        <Select.Trigger class="pg-trigger h-8 min-w-[7.5rem] border-0 bg-transparent shadow-none px-2 text-xs">
          <span class="flex items-center gap-1 truncate">
            <Languages class="size-3.5 shrink-0 opacity-60" />
            <span class="truncate">{targetLanguageDisplay}</span>
          </span>
        </Select.Trigger>
        <Select.Content class="max-h-[60vh]">
          {#each translationLanguages as lang (lang.code)}
            <Select.Item value={lang.code}>{languageName(lang, $locale)}</Select.Item>
          {/each}
        </Select.Content>
      </Select.Root>
    </div>

    <!-- Scenario Preset -->
    <div class="hidden sm:flex items-center gap-1">
      <Select.Root
        type="single"
        value={$presetStore}
        onValueChange={(v) => v && handlePresetChange(v)}
        disabled={$busyStore}
      >
        <Select.Trigger class="pg-trigger h-8 w-32 text-xs">
          <span class="truncate">{$translate(currentPreset.labelKey)}</span>
        </Select.Trigger>
        <Select.Content>
          {#each translationPresets as preset (preset.id)}
            <Select.Item value={preset.id}>{$translate(preset.labelKey)}</Select.Item>
          {/each}
        </Select.Content>
      </Select.Root>
    </div>

    <!-- Settings Button -->
    <Button
      variant="outline"
      size="icon"
      class="pg-control ml-auto"
      onclick={() => (showSettings = true)}
      title={$translate("playground.translation.settings")}
    >
      <SlidersHorizontal class="size-3.5" />
    </Button>
  </div>

  {#if !hasModels && !$selectedModelStore}
    <div class="text-muted-foreground flex flex-1 items-center justify-center text-center text-sm">
      {$translate("playground.translation.noModels")}
    </div>
  {:else}
    <!-- Two-pane: source and result -->
    <div class="grid min-h-0 flex-1 grid-cols-1 gap-3 md:grid-cols-2">
      <!-- Source Pane -->
      <div class="pg-panel flex min-h-0 flex-col overflow-hidden border border-border/50 shadow-xs">
        <div class="flex shrink-0 items-center justify-between border-b px-3 py-2 pg-divide bg-muted/20">
          <span class="flex items-center gap-1.5 text-xs font-medium text-foreground">
            <Languages class="size-3.5 text-primary" />
            <span>{sourceLanguageDisplay}</span>
          </span>
          <div class="flex items-center gap-1.5">
            <span class="pg-chip">{sourceText.length} {$translate("playground.chat.characters")}</span>
            {#if sourceText.length > 0}
              <Button
                variant="ghost"
                size="icon-xs"
                class="pg-tool text-muted-foreground hover:text-destructive"
                onclick={() => (sourceText = "")}
                disabled={$busyStore}
                title={$translate("common.clear")}
              >
                <Trash2 class="size-3.5" />
              </Button>
            {/if}
          </div>
        </div>

        <div class="min-h-0 flex-1 p-2 flex flex-col">
          <ExpandableTextarea
            bare
            fill
            bind:value={sourceText}
            placeholder={$translate("playground.translation.sourcePlaceholder")}
            rows={8}
            disabled={$busyStore}
            onkeydown={handleKeyDown}
          >
            {#snippet toolbar({ expand })}
              <div class="flex w-full items-center justify-between pt-1">
                <Button
                  variant="ghost"
                  size="icon-sm"
                  class="pg-tool"
                  onclick={expand}
                  disabled={$busyStore}
                  title={$translate("playground.expandable.expandToEdit")}
                >
                  <Maximize2 class="size-3.5" />
                </Button>

                <div class="flex items-center gap-2">
                  <span class="text-[11px] text-muted-foreground hidden sm:inline">
                    <kbd class="rounded border bg-muted px-1.5 py-0.5 font-mono text-[10px]">⌘/Ctrl + Enter</kbd>
                  </span>
                  {#if $busyStore}
                    <Button variant="outline" class="pg-control pg-control--danger gap-1.5" onclick={() => iface.cancel()}>
                      <X class="size-3.5" />
                      {$translate("common.cancel")}
                    </Button>
                  {:else}
                    <Button class="pg-action gap-1.5" onclick={translateText} disabled={!canTranslate}>
                      <Languages class="size-3.5" />
                      {$translate("playground.translation.translate")}
                    </Button>
                  {/if}
                </div>
              </div>
            {/snippet}
          </ExpandableTextarea>
        </div>
      </div>

      <!-- Result Pane -->
      <div class="pg-panel flex min-h-0 flex-col overflow-hidden border border-border/50 shadow-xs">
        <div class="flex shrink-0 items-center justify-between border-b px-3 py-2 pg-divide bg-muted/20">
          <span class="flex items-center gap-1.5 text-xs font-medium text-foreground">
            <Languages class="size-3.5 text-primary" />
            <span>{targetLanguageDisplay}</span>
          </span>
          <div class="flex items-center gap-1.5">
            {#if resultText}
              <Button
                variant="ghost"
                size="icon-xs"
                class="pg-tool"
                onclick={copyResult}
                title={copied ? $translate("common.copied") : $translate("playground.translation.copy")}
              >
                {#if copied}
                  <Check class="size-3.5 text-success" />
                {:else}
                  <Copy class="size-3.5" />
                {/if}
              </Button>
              <span class="pg-chip">{resultText.length} {$translate("playground.chat.characters")}</span>
              {#if elapsedMs > 0}
                <span class="pg-chip">{formatDuration(elapsedMs, { precision: 1, subSecondMs: true })}</span>
              {/if}
              {#if reasoningTimeMs > 0}
                <span class="pg-chip">{$translate("capture.reasoning")} {formatDuration(reasoningTimeMs, { precision: 1, subSecondMs: true })}</span>
              {/if}
            {/if}
          </div>
        </div>

        <div class="pg-scroll min-h-0 flex-1 overflow-y-auto p-3.5">
          {#if $busyStore && !resultText}
            <div class="text-muted-foreground flex h-full flex-col items-center justify-center gap-2">
              <div class="inline-block size-6 border-2 border-primary border-t-transparent rounded-full animate-spin"></div>
              <p class="text-xs">{$translate("playground.translation.translating")}</p>
            </div>
          {:else if $errorStore}
            <div class="text-destructive p-3 rounded-lg bg-destructive/10 border border-destructive/20 text-xs">
              <p class="font-medium">{$translate("common.error")}</p>
              <p class="mt-1">{$errorStore}</p>
            </div>
          {:else if resultText}
            <div class="whitespace-pre-wrap text-[0.9375rem] leading-relaxed select-text font-normal">
              {resultText}{#if $busyStore}<span class="pg-caret"></span>{/if}
            </div>
          {:else}
            <div class="text-muted-foreground flex h-full flex-col items-center justify-center gap-2 text-center text-xs">
              <Languages class="size-8 opacity-25 text-primary" />
              <p>{$translate("playground.translation.empty")}</p>
            </div>
          {/if}
        </div>
      </div>
    </div>
  {/if}

  <!-- Settings dialog: prompt template -->
  <Dialog.Root bind:open={showSettings}>
    <Dialog.Content class="pg-float max-w-2xl">
      <Dialog.Header>
        <Dialog.Title>{$translate("playground.translation.settings")}</Dialog.Title>
      </Dialog.Header>

      <div class="space-y-4">
        <div>
          <Label class="mb-1">{$translate("playground.translation.scenario")}</Label>
          <Select.Root
            type="single"
            value={$presetStore}
            onValueChange={(v) => v && handlePresetChange(v)}
          >
            <Select.Trigger class="pg-trigger w-full">
              {$translate(currentPreset.labelKey)}
            </Select.Trigger>
            <Select.Content>
              {#each translationPresets as preset (preset.id)}
                <Select.Item value={preset.id}>{$translate(preset.labelKey)}</Select.Item>
              {/each}
            </Select.Content>
          </Select.Root>
        </div>
        <div>
          <Label class="mb-1">{$translate("playground.translation.promptTemplate")}</Label>
          <Textarea
            class="resize-none font-mono text-[13px]"
            rows={10}
            bind:value={$templateStore}
            spellcheck={false}
          />
          <p class="pg-hint mt-1">{$translate("playground.translation.promptHint")}</p>
        </div>
      </div>

      <Dialog.Footer>
        <Button variant="outline" class="pg-control" onclick={() => (showSettings = false)}>
          {$translate("common.done")}
        </Button>
      </Dialog.Footer>
    </Dialog.Content>
  </Dialog.Root>
</div>
