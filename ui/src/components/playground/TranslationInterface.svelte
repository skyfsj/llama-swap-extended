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
  import { Copy, Check, Languages, ArrowLeftRight, Settings, X, Maximize2 } from "@lucide/svelte";
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
</script>

<div class="flex flex-col h-full">
  <!-- Toolbar: model + settings (holds the prompt template) -->
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
    <Button
      variant="ghost"
      size="icon"
      class="pg-tool ml-auto"
      onclick={() => (showSettings = true)}
      title={$translate("playground.translation.settings")}
    >
      <Settings />
    </Button>
  </div>

  {#if !hasModels && !$selectedModelStore}
    <div class="text-muted-foreground flex flex-1 items-center justify-center text-center text-sm">
      {$translate("playground.translation.noModels")}
    </div>
  {:else}
    <!-- Language bar: source ⇄ target, scenario preset -->
    <div class="mb-3 flex shrink-0 flex-wrap items-center gap-2 border-b pg-divide px-1 py-2">
      <Select.Root
        type="single"
        value={$sourceLanguageStore}
        onValueChange={(v) => v && sourceLanguageStore.set(v)}
        disabled={$busyStore}
      >
        <Select.Trigger class="pg-trigger h-8 w-36 border-0 bg-transparent shadow-none">
          <span class="flex items-center gap-1.5">
            <Languages class="size-3.5" style="color: var(--pg-ink-3)" />
            {sourceLanguageDisplay}
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
        size="icon"
        class="pg-tool"
        onclick={swapLanguages}
        disabled={$busyStore}
        title={$translate("playground.translation.swapLanguages")}
      >
        <ArrowLeftRight />
      </Button>

      <Select.Root
        type="single"
        value={$targetLanguageStore}
        onValueChange={(v) => v && targetLanguageStore.set(v)}
        disabled={$busyStore}
      >
        <Select.Trigger class="pg-trigger h-8 w-36 border-0 bg-transparent shadow-none">
          <span class="flex items-center gap-1.5">
            <Languages class="size-3.5" style="color: var(--pg-ink-3)" />
            {targetLanguageDisplay}
          </span>
        </Select.Trigger>
        <Select.Content class="max-h-[60vh]">
          {#each translationLanguages as lang (lang.code)}
            <Select.Item value={lang.code}>{languageName(lang, $locale)}</Select.Item>
          {/each}
        </Select.Content>
      </Select.Root>

      <div class="ml-auto flex items-center gap-2">
        <span class="pg-hint hidden sm:inline">{$translate("playground.translation.scenario")}</span>
        <Select.Root
          type="single"
          value={$presetStore}
          onValueChange={(v) => v && handlePresetChange(v)}
          disabled={$busyStore}
        >
          <Select.Trigger class="pg-trigger h-8 w-36">
            {$translate(currentPreset.labelKey)}
          </Select.Trigger>
          <Select.Content>
            {#each translationPresets as preset (preset.id)}
              <Select.Item value={preset.id}>{$translate(preset.labelKey)}</Select.Item>
            {/each}
          </Select.Content>
        </Select.Root>
      </div>
    </div>

    <!-- Two-pane: source and result -->
    <div class="grid min-h-0 flex-1 grid-cols-1 gap-3 md:grid-cols-2">
      <div class="pg-composer flex min-h-0 flex-col p-1.5">
        <ExpandableTextarea
          bare
          fill
          bind:value={sourceText}
          placeholder={$translate("playground.translation.sourcePlaceholder")}
          rows={6}
          disabled={$busyStore}
        >
          {#snippet toolbar({ expand })}
            <span class="pg-chip">{sourceText.length}{$translate("playground.chat.characters")}</span>
            <div class="ml-auto flex items-center gap-1">
              <Button
                variant="ghost"
                size="icon-sm"
                class="pg-tool"
                onclick={expand}
                disabled={$busyStore}
                title={$translate("playground.expandable.expandToEdit")}
              >
                <Maximize2 />
              </Button>
            </div>
          {/snippet}
        </ExpandableTextarea>
      </div>

      <div class="pg-panel flex min-h-0 flex-col p-4">
        <div class="pg-scroll min-h-0 flex-1 overflow-y-auto">
          {#if $busyStore && !resultText}
            <div class="text-muted-foreground flex h-full items-center justify-center">
              <div class="text-center">
                <div class="inline-block w-8 h-8 border-4 border-primary border-t-transparent rounded-full animate-spin mb-2"></div>
                <p>{$translate("playground.translation.translating")}</p>
              </div>
            </div>
          {:else if $errorStore}
            <div class="text-destructive p-2">
              <p class="font-medium">{$translate("common.error")}</p>
              <p class="mt-1 text-sm">{$errorStore}</p>
            </div>
          {:else if resultText}
            <div class="whitespace-pre-wrap text-[0.9375rem] leading-relaxed">{resultText}{#if $busyStore}<span class="pg-caret"></span>{/if}</div>
          {:else}
            <div class="text-muted-foreground flex h-full items-center justify-center text-center text-sm">
              {$translate("playground.translation.empty")}
            </div>
          {/if}
        </div>
        {#if resultText && !$busyStore}
          <div class="mt-2 flex shrink-0 items-center gap-1.5">
            <Button
              variant="ghost"
              size="icon-xs"
              class="pg-tool"
              onclick={copyResult}
              title={copied ? $translate("common.copied") : $translate("playground.translation.copy")}
            >
              {#if copied}
                <Check class="text-success" />
              {:else}
                <Copy />
              {/if}
            </Button>
            <span class="pg-chip">{resultText.length}{$translate("playground.chat.characters")}</span>
            {#if elapsedMs > 0}
              <span class="pg-chip">{formatDuration(elapsedMs, { precision: 1, subSecondMs: true })}</span>
            {/if}
            {#if reasoningTimeMs > 0}
              <span class="pg-chip">{$translate("capture.reasoning")} {formatDuration(reasoningTimeMs, { precision: 1, subSecondMs: true })}</span>
            {/if}
          </div>
        {/if}
      </div>
    </div>

    <!-- Action row -->
    <div class="mt-3 flex shrink-0 items-center justify-end gap-2">
      {#if $busyStore}
        <Button variant="outline" class="pg-control pg-control--danger" onclick={() => iface.cancel()}>
          <X />
          {$translate("common.cancel")}
        </Button>
      {:else}
        <Button class="pg-action" onclick={translateText} disabled={!canTranslate}>
          <Languages />
          {$translate("playground.translation.translate")}
        </Button>
      {/if}
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
